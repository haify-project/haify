package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
)

// Resource snapshots: one snapshot of every volume on every diskful replica,
// taken at the same instant.
//
// A volume snapshot (snapshots.go) is taken on one node. Rolling a replicated
// volume back to it means making every other replica resync the whole volume
// from that one, and losing that node loses the snapshot. A resource snapshot
// is on every replica instead, and it is the same data on all of them:
//
//   - I/O is suspended on every diskful node (the Primary first) for the few
//     seconds the snapshots take, so no write is half-way between replicas.
//     A transient systemd timer on each node resumes I/O after
//     resourceSnapshotWatchdog whatever happens to the controller, which
//     resumes it itself as soon as the snapshots exist.
//   - Each snapshot carries the replica's DRBD metadata with its data (the
//     metadata is internal, at the end of the backing volume). Rolling every
//     replica back together therefore brings back matching generation
//     identifiers too, and DRBD resyncs nothing.
//
// The backing snapshots are named "<backing volume>_snap_<name>" (ZFS:
// "<dataset>@snap_<name>"), so two disks of one VM can each have a snapshot of
// the same name.

const resourceSnapshotWatchdog = time.Minute

var resourceSnapshotNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,39}$`)

// snapTarget is one volume's backing store and how to snapshot it.
type snapTarget struct {
	vol *database.Volume
	zfs bool
}

func (t snapTarget) lvName(name string) string { return t.vol.VolumeName + "_snap_" + name }
func (t snapTarget) zfsName(name string) string {
	return t.vol.Pool + "/" + t.vol.VolumeName + "@snap_" + name
}

// resourceSnapshotTargets loads what a resource snapshot covers.
func (rm *ResourceManager) resourceSnapshotTargets(ctx context.Context, resource, name string) ([]snapTarget, []string, error) {
	log := rm.resourcesLog()
	if rm.deployment == nil || rm.controller.db == nil {
		log.Debug("resourceSnapshotTargets: no deployment client or database", zap.String("resource", resource))
		return nil, nil, fmt.Errorf("deployment client or database not available")
	}
	if !resourceSnapshotNameRe.MatchString(name) {
		log.Debug("resourceSnapshotTargets: invalid snapshot name", zap.String("snapshot", name))
		return nil, nil, fmt.Errorf("%q is not a valid snapshot name (letters, digits, _ and -, at most 40)", name)
	}
	dbRes, err := rm.controller.db.GetResource(ctx, resource)
	if err != nil || dbRes == nil {
		log.Debug("resourceSnapshotTargets: resource not found", zap.String("resource", resource), zap.Error(err))
		return nil, nil, fmt.Errorf("resource %q not found", resource)
	}
	if dbRes.Encrypted {
		log.Debug("resourceSnapshotTargets: encrypted resource; refusing", zap.String("resource", resource))
		return nil, nil, fmt.Errorf("%s is encrypted; snapshot its volumes one by one", resource)
	}
	vols, err := rm.controller.db.ListVolumes(ctx, resource)
	if err != nil || len(vols) == 0 {
		log.Debug("resourceSnapshotTargets: no volumes", zap.String("resource", resource), zap.Error(err))
		return nil, nil, fmt.Errorf("resource %q has no volumes recorded", resource)
	}
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		log.Debug("resourceSnapshotTargets: no hosts", zap.String("resource", resource), zap.Error(err))
		return nil, nil, err
	}
	targets := make([]snapTarget, 0, len(vols))
	for _, v := range vols {
		targets = append(targets, snapTarget{vol: v, zfs: isZFSDevice(v.Device)})
	}
	log.Debug("resourceSnapshotTargets: targets", zap.String("resource", resource), zap.String("snapshot", name),
		zap.Int("volumes", len(targets)), zap.Strings("hosts", hosts))
	return targets, hosts, nil
}

// CreateResourceSnapshot snapshots every volume of resource on every diskful
// replica, with I/O suspended across all of them.
func (rm *ResourceManager) CreateResourceSnapshot(ctx context.Context, resource, name string) error {
	log := rm.resourcesLog()
	log.Debug("CreateResourceSnapshot: start", zap.String("resource", resource), zap.String("snapshot", name))
	targets, hosts, err := rm.resourceSnapshotTargets(ctx, resource, name)
	if err != nil {
		return err
	}
	watchdog, err := rm.suspendForSnapshot(ctx, resource, hosts)
	if err != nil {
		log.Debug("CreateResourceSnapshot: suspend failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	defer rm.resumeAfterSnapshot(resource, hosts, watchdog)

	var made []func()
	undo := func() {
		log.Debug("CreateResourceSnapshot: undoing partial snapshot", zap.String("resource", resource), zap.Int("made", len(made)))
		for _, u := range made {
			u()
		}
	}
	for _, t := range targets {
		for _, host := range hosts {
			if err := rm.snapshotOne(ctx, host, t, name); err != nil {
				log.Debug("CreateResourceSnapshot: snapshot failed", zap.String("volume", t.vol.VolumeName), zap.String("host", host), zap.Error(err))
				undo()
				return fmt.Errorf("snapshot %s on %s: %w", t.vol.VolumeName, host, err)
			}
			h, tt := host, t
			made = append(made, func() { _ = rm.deleteOne(context.Background(), h, tt, name) })
		}
	}
	rm.controller.logger.Info("Resource snapshot taken", zap.String("resource", resource),
		zap.String("snapshot", name), zap.Strings("hosts", hosts))
	return nil
}

// suspendForSnapshot arms each node's resume watchdog, then suspends I/O,
// the Primary first so nothing new is replicated while the others stop. It
// returns the watchdog's unit name, for resumeAfterSnapshot to disarm.
func (rm *ResourceManager) suspendForSnapshot(ctx context.Context, resource string, hosts []string) (string, error) {
	log := rm.resourcesLog()
	ordered := append([]string(nil), hosts...)
	if info, err := rm.GetResource(ctx, resource); err == nil {
		for node, st := range info.NodeStates {
			if strings.EqualFold(st.Role, "Primary") {
				primary := rm.controller.ResolveHost(node)
				log.Debug("suspendForSnapshot: Primary suspends first", zap.String("resource", resource), zap.String("host", primary))
				ordered = append([]string{primary}, without(ordered, primary)...)
			}
		}
	} else {
		log.Debug("suspendForSnapshot: status unavailable; configured order", zap.String("resource", resource), zap.Error(err))
	}
	unit := fmt.Sprintf("haify-snapshot-resume-%s-%d", resource, time.Now().UnixNano())
	for i, host := range ordered {
		cmd := fmt.Sprintf("sudo systemd-run --unit=%s "+collectFlag+" --on-active=%d drbdadm resume-io %s && sudo drbdadm suspend-io %s",
			unit, int(resourceSnapshotWatchdog.Seconds()), resource, resource)
		start := time.Now()
		err := rm.execAllSuccess(ctx, []string{host}, cmd, "suspend I/O on "+host)
		log.Debug("suspendForSnapshot: armed watchdog and suspended I/O", zap.String("resource", resource), zap.String("host", host),
			zap.String("unit", unit), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			rm.resumeAfterSnapshot(resource, ordered[:i+1], unit)
			return "", err
		}
	}
	return unit, nil
}

// resumeAfterSnapshot resumes I/O everywhere and disarms the watchdog; it runs
// on its own context so a cancelled request cannot leave a resource suspended
// until the watchdog. A watchdog left armed would fire a minute later and
// resume I/O in the middle of whatever snapshot is suspending it then.
func (rm *ResourceManager) resumeAfterSnapshot(resource string, hosts []string, watchdog string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := fmt.Sprintf("sudo drbdadm resume-io %s; rc=$?; sudo systemctl stop %s.timer >/dev/null 2>&1; exit $rc", resource, watchdog)
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, hosts, cmd)
	rm.resourcesLog().Debug("resumeAfterSnapshot: resumed I/O and disarmed watchdog", zap.String("resource", resource),
		zap.Strings("hosts", hosts), zap.String("unit", watchdog), zap.Array("failed_hosts", resourcesFailedHosts{res}),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		rm.controller.logger.Warn("Resuming I/O after a snapshot failed; the watchdog resumes it",
			zap.String("resource", resource), zap.Error(err))
	}
}

func (rm *ResourceManager) snapshotOne(ctx context.Context, host string, t snapTarget, name string) error {
	log := rm.resourcesLog()
	start := time.Now()
	if t.zfs {
		err := execFailure(rm.deployment.ZFSSnapshot(ctx, []string{host}, t.vol.Pool+"/"+t.vol.VolumeName, "snap_"+name))
		log.Debug("snapshotOne: zfs snapshot", zap.String("host", host), zap.String("volume", t.vol.VolumeName),
			zap.String("snapshot", name), zap.Duration("took", time.Since(start)), zap.Error(err))
		return err
	}
	thin, err := rm.deployment.LVIsThin(ctx, host, t.vol.Pool, t.vol.VolumeName)
	if err != nil {
		log.Debug("snapshotOne: could not tell whether LV is thin", zap.String("host", host), zap.String("volume", t.vol.VolumeName), zap.Error(err))
		return err
	}
	if thin {
		err = execFailure(rm.deployment.LVCreateThinSnapshot(ctx, []string{host}, t.vol.Pool, t.vol.VolumeName, t.lvName(name)))
	} else {
		err = execFailure(rm.deployment.LVCreateSnapshot(ctx, []string{host}, t.vol.Pool, t.vol.VolumeName, t.lvName(name),
			cowSize(uint64(max(t.vol.SizeGB, 0)))))
	}
	log.Debug("snapshotOne: LVM snapshot", zap.String("host", host), zap.String("volume", t.vol.VolumeName),
		zap.String("snapshot", t.lvName(name)), zap.Bool("thin", thin), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

func (rm *ResourceManager) deleteOne(ctx context.Context, host string, t snapTarget, name string) error {
	start := time.Now()
	var err error
	if t.zfs {
		err = execFailure(rm.deployment.ZFSDestroySnapshot(ctx, []string{host}, t.zfsName(name)))
	} else {
		err = execFailure(rm.deployment.LVRemoveSnapshot(ctx, []string{host}, t.vol.Pool, t.lvName(name)))
	}
	rm.resourcesLog().Debug("deleteOne: deleted snapshot", zap.String("host", host), zap.String("volume", t.vol.VolumeName),
		zap.String("snapshot", name), zap.Bool("zfs", t.zfs), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// DeleteResourceSnapshot removes a resource snapshot from every replica.
func (rm *ResourceManager) DeleteResourceSnapshot(ctx context.Context, resource, name string) error {
	log := rm.resourcesLog()
	log.Debug("DeleteResourceSnapshot: start", zap.String("resource", resource), zap.String("snapshot", name))
	targets, hosts, err := rm.resourceSnapshotTargets(ctx, resource, name)
	if err != nil {
		return err
	}
	var failed []string
	for _, t := range targets {
		for _, host := range hosts {
			if err := rm.deleteOne(ctx, host, t, name); err != nil && !strings.Contains(strings.ToLower(err.Error()), "not found") {
				failed = append(failed, fmt.Sprintf("%s on %s: %v", t.vol.VolumeName, host, err))
			}
		}
	}
	if len(failed) > 0 {
		log.Debug("DeleteResourceSnapshot: not deleted everywhere", zap.String("resource", resource), zap.Int("failed", len(failed)))
		return fmt.Errorf("snapshot %s of %s not deleted everywhere: %s", name, resource, strings.Join(failed, "; "))
	}
	log.Debug("DeleteResourceSnapshot: deleted", zap.String("resource", resource), zap.String("snapshot", name))
	return nil
}

// RollbackResourceSnapshot brings every replica back to a resource snapshot
// together, so they come up with matching data and metadata and nothing is
// resynced. The snapshot is kept: an LVM merge consumes it, so it is taken
// again from the restored volume, which is exactly its content.
func (rm *ResourceManager) RollbackResourceSnapshot(ctx context.Context, resource, name string) error {
	log := rm.resourcesLog()
	log.Debug("RollbackResourceSnapshot: start", zap.String("resource", resource), zap.String("snapshot", name))
	targets, hosts, err := rm.resourceSnapshotTargets(ctx, resource, name)
	if err != nil {
		return err
	}
	if info, err := rm.GetResource(ctx, resource); err == nil {
		for node, st := range info.NodeStates {
			if strings.EqualFold(st.Role, "Primary") {
				log.Debug("RollbackResourceSnapshot: resource is Primary; refusing", zap.String("resource", resource), zap.String("node", node))
				return fmt.Errorf("%s is Primary on %s; stop what uses it before rolling it back", resource, node)
			}
		}
	} else {
		log.Debug("RollbackResourceSnapshot: status unavailable; not checking roles", zap.String("resource", resource), zap.Error(err))
	}
	if err := rm.assertSnapshotOnEveryReplica(ctx, targets, hosts, name); err != nil {
		log.Debug("RollbackResourceSnapshot: snapshot missing on a replica", zap.String("resource", resource), zap.Error(err))
		return err
	}
	grown := rm.volumesGrownSinceSnapshot(ctx, targets, hosts[0], name)
	all := append(append([]string(nil), hosts...), rm.disklessParticipantHosts(ctx, resource)...)
	log.Debug("RollbackResourceSnapshot: taking resource down", zap.String("resource", resource), zap.Strings("hosts", all),
		zap.Int("grown", len(grown)))
	if err := rm.execAllSuccess(ctx, all, "sudo drbdadm down "+resource, "take "+resource+" down for the rollback"); err != nil {
		log.Debug("RollbackResourceSnapshot: down failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	var rollbackErr error
	for _, t := range targets {
		for _, host := range hosts {
			if err := rm.rollbackOne(ctx, host, t, name); err != nil {
				rollbackErr = fmt.Errorf("roll %s back on %s: %w", t.vol.VolumeName, host, err)
				break
			}
		}
		if rollbackErr != nil {
			break
		}
	}
	if rollbackErr == nil {
		rollbackErr = rm.clearGrownSizes(ctx, resource, grown)
	}
	log.Debug("RollbackResourceSnapshot: rolled back; bringing resource up", zap.String("resource", resource), zap.Error(rollbackErr))
	// Up again whatever happened: a replica left down is worse than one left
	// at the wrong point, which DRBD can still resync.
	if err := rm.execAllSuccess(ctx, all, "sudo drbdadm up "+resource, "bring "+resource+" up after the rollback"); err != nil && rollbackErr == nil {
		rollbackErr = err
	}
	if rollbackErr == nil {
		rollbackErr = rm.growBack(ctx, resource, hosts, grown)
	}
	log.Debug("RollbackResourceSnapshot: done", zap.String("resource", resource), zap.String("snapshot", name), zap.Error(rollbackErr))
	return rollbackErr
}

func (rm *ResourceManager) rollbackOne(ctx context.Context, host string, t snapTarget, name string) error {
	log := rm.resourcesLog()
	start := time.Now()
	if t.zfs {
		err := execFailure(rm.deployment.ZFSRollback(ctx, []string{host}, t.vol.Pool+"/"+t.vol.VolumeName, "snap_"+name))
		log.Debug("rollbackOne: zfs rollback", zap.String("host", host), zap.String("volume", t.vol.VolumeName),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return err
	}
	if err := rm.execAllSuccess(ctx, []string{host},
		fmt.Sprintf("sudo lvconvert --merge -y %s/%s", t.vol.Pool, t.lvName(name)), "merge"); err != nil {
		log.Debug("rollbackOne: merge failed", zap.String("host", host), zap.String("volume", t.vol.VolumeName), zap.Error(err))
		return err
	}
	log.Debug("rollbackOne: merged; retaking snapshot", zap.String("host", host), zap.String("volume", t.vol.VolumeName),
		zap.Duration("took", time.Since(start)))
	return rm.snapshotOne(ctx, host, t, name)
}

// assertSnapshotOnEveryReplica refuses a rollback that would leave a replica
// at the current data: then it would not be a rollback of the resource.
func (rm *ResourceManager) assertSnapshotOnEveryReplica(ctx context.Context, targets []snapTarget, hosts []string, name string) error {
	log := rm.resourcesLog()
	for _, t := range targets {
		cmd := fmt.Sprintf("sudo lvs --noheadings -o lv_name %s/%s", t.vol.Pool, t.lvName(name))
		if t.zfs {
			cmd = "sudo zfs list -H -o name " + t.zfsName(name)
		}
		start := time.Now()
		res, err := rm.deployment.Exec(ctx, hosts, cmd)
		log.Debug("assertSnapshotOnEveryReplica: looked for snapshot", zap.String("volume", t.vol.VolumeName), zap.String("snapshot", name),
			zap.Strings("hosts", hosts), zap.Array("failed_hosts", resourcesFailedHosts{res}), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return err
		}
		if !res.AllSuccess() {
			return fmt.Errorf("snapshot %s of %s is missing on %v; it is not a snapshot of every replica", name,
				t.vol.VolumeName, res.FailedHosts())
		}
	}
	return nil
}

// ListResourceSnapshots names the resource snapshots of resource, from its
// first diskful node.
func (rm *ResourceManager) ListResourceSnapshots(ctx context.Context, resource string) ([]string, error) {
	log := rm.resourcesLog()
	targets, hosts, err := rm.resourceSnapshotTargets(ctx, resource, "x")
	if err != nil {
		return nil, err
	}
	t := targets[0]
	cmd := fmt.Sprintf("sudo lvs --noheadings -o lv_name %s 2>/dev/null", t.vol.Pool)
	prefix := t.vol.VolumeName + "_snap_"
	if t.zfs {
		cmd = fmt.Sprintf("sudo zfs list -H -t snapshot -d 1 -o name %s/%s 2>/dev/null", t.vol.Pool, t.vol.VolumeName)
		prefix = t.vol.Pool + "/" + t.vol.VolumeName + "@snap_"
	}
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, hosts[:1], cmd)
	log.Debug("ListResourceSnapshots: listed snapshots", zap.String("resource", resource), zap.String("host", hosts[0]),
		zap.Bool("zfs", t.zfs), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range execLines(res, hosts[0]) {
		if n, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			names = append(names, n)
		}
	}
	log.Debug("ListResourceSnapshots: found", zap.String("resource", resource), zap.Strings("snapshots", names))
	return names, nil
}
