package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
)

// Running a snapshot schedule: one tick takes a snapshot of every volume on
// every diskful node and prunes what the retention policy no longer keeps.

// runSchedule executes one schedule: snapshot the resource's volumes on every
// diskful node, then prune per the GFS policy. Per-node failures are logged
// and do not abort the rest.
func (sm *ScheduleManager) runSchedule(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	log := sm.scheduleLog()
	tick := time.Now()
	log.Debug("ScheduleManager.runSchedule: tick", zap.String("schedule", name))

	s, err := sm.controller.db.GetSnapshotSchedule(ctx, name)
	if err != nil || s == nil || !s.Enabled {
		log.Debug("ScheduleManager.runSchedule: schedule missing or disabled; skipping", zap.String("schedule", name),
			zap.Bool("found", s != nil), zap.Bool("enabled", s != nil && s.Enabled), zap.Error(err))
		return
	}

	res, err := sm.controller.resources.GetResource(ctx, s.Resource)
	if err != nil {
		log.Warn("Snapshot schedule: resource lookup failed",
			zap.String("schedule", name), zap.String("resource", s.Resource), zap.Error(err))
		return
	}
	if len(res.Volumes) == 0 || len(res.Nodes) == 0 {
		log.Warn("Snapshot schedule: resource has no diskful volumes/nodes",
			zap.String("schedule", name), zap.String("resource", s.Resource))
		return
	}

	ts := time.Now().UTC()
	log.Info("Running snapshot schedule",
		zap.String("schedule", name), zap.String("resource", s.Resource),
		zap.Int("nodes", len(res.Nodes)), zap.Int("volumes", len(res.Volumes)))

	lock := time.Duration(s.LockDays) * 24 * time.Hour
	frozen := !scheduleFrozenUntil(s, lockNow()).IsZero()
	log.Debug("ScheduleManager.runSchedule: lock and freeze", zap.String("schedule", name),
		zap.Int("lock_days", s.LockDays), zap.Bool("frozen", frozen))
	sm.snapshotResource(ctx, res, s, ts)
	for _, node := range res.Nodes {
		host := sm.controller.ResolveHost(node)
		for _, vol := range res.Volumes {
			// A frozen schedule removes nothing, not even to relieve a full
			// pool: what it froze is the history from before something
			// started rewriting the volume.
			if frozen {
				log.Debug("ScheduleManager.runSchedule: frozen; not pruning", zap.String("node", node), zap.String("volume", vol.BackingVolume))
				continue
			}
			log.Debug("ScheduleManager.runSchedule: pruning volume", zap.String("node", node), zap.String("volume", vol.BackingVolume))
			sm.pruneVolume(ctx, host, node, vol, s.Keep, lock)
			sm.relieveThinPool(ctx, host, node, s.Resource, vol, lock)
		}
	}

	s.LastRun = time.Now()
	err = sm.controller.db.SaveSnapshotSchedule(ctx, s)
	log.Debug("ScheduleManager.runSchedule: recorded last run", zap.String("schedule", name), zap.Error(err))
	if err != nil {
		log.Warn("Snapshot schedule: failed to record last run",
			zap.String("schedule", name), zap.Error(err))
	}
	log.Debug("ScheduleManager.runSchedule: tick done", zap.String("schedule", name), zap.Duration("took", time.Since(tick)))
}

// snapshotVolume takes one storage-native snapshot of vol on host.
func (sm *ScheduleManager) snapshotVolume(ctx context.Context, host, node string, vol *ResourceVolumeInfo, ts time.Time) {
	log := sm.scheduleLog()
	snapName := buildSnapName(vol.BackingVolume, ts)
	dep := sm.controller.deployment
	log.Debug("ScheduleManager.snapshotVolume: start", zap.String("node", node), zap.String("host", host),
		zap.String("volume", vol.BackingVolume), zap.String("pool", vol.Pool), zap.String("snapshot", snapName))

	start := time.Now()
	if isZFSDevice(vol.Device) {
		dataset := fmt.Sprintf("%s/%s", vol.Pool, vol.BackingVolume)
		res, err := dep.ZFSSnapshot(ctx, []string{host}, dataset, snapName)
		log.Debug("ScheduleManager.snapshotVolume: zfs snapshot", zap.String("dataset", dataset), zap.Duration("took", time.Since(start)), zap.Error(err))
		logSnapResult(log, "zfs", node, snapName, res, err)
		return
	}

	// LVM: thin snapshots size from the pool; thick snapshots need a COW size.
	thin, err := dep.LVIsThin(ctx, host, vol.Pool, vol.BackingVolume)
	log.Debug("ScheduleManager.snapshotVolume: thin check", zap.String("volume", vol.BackingVolume), zap.Bool("thin", thin), zap.Error(err))
	if err != nil {
		log.Warn("Snapshot schedule: LVIsThin failed; assuming thick",
			zap.String("node", node), zap.String("volume", vol.BackingVolume), zap.Error(err))
	}
	start = time.Now()
	if thin {
		res, e := dep.LVCreateThinSnapshot(ctx, []string{host}, vol.Pool, vol.BackingVolume, snapName)
		log.Debug("ScheduleManager.snapshotVolume: thin snapshot", zap.String("volume", vol.BackingVolume), zap.Duration("took", time.Since(start)), zap.Error(e))
		logSnapResult(log, "lvm-thin", node, snapName, res, e)
		return
	}
	// Thick LVM snapshots need an absolute COW size (-L); reserve ~20% of the
	// origin, with a floor so tiny volumes still get usable headroom.
	res, e := dep.LVCreateSnapshot(ctx, []string{host}, vol.Pool, vol.BackingVolume, snapName, cowSize(vol.SizeGB))
	log.Debug("ScheduleManager.snapshotVolume: thick snapshot", zap.String("volume", vol.BackingVolume),
		zap.String("cow_size", cowSize(vol.SizeGB)), zap.Duration("took", time.Since(start)), zap.Error(e))
	logSnapResult(log, "lvm", node, snapName, res, e)
}

// cowSize returns a copy-on-write reservation (~20% of an originGB-sized
// volume) as an lvcreate -L argument, never below 256 MiB.
func cowSize(originGB uint64) string {
	return fmt.Sprintf("%dM", cowBytes(originGB)>>20)
}

// pruneVolume lists scheduled snapshots of vol on host and deletes those the
// GFS policy no longer retains.
//
// A snapshot still inside its lock window is kept whatever the policy says; a
// later run prunes it once the lock has expired.
func (sm *ScheduleManager) pruneVolume(ctx context.Context, host, node string, vol *ResourceVolumeInfo, policy database.GFSPolicy, lock time.Duration) {
	log := sm.scheduleLog()
	snaps, err := sm.listScheduledSnaps(ctx, host, vol)
	if err != nil {
		log.Warn("Snapshot schedule: list snapshots failed",
			zap.String("node", node), zap.String("volume", vol.BackingVolume), zap.Error(err))
		return
	}
	expired := unlockedSnaps(selectExpiredSnapshots(snaps, policy), lock, lockNow())
	log.Debug("ScheduleManager.pruneVolume: retention decided", zap.String("node", node), zap.String("volume", vol.BackingVolume),
		zap.Int("snapshots", len(snaps)), zap.Int("expired", len(expired)), zap.Duration("lock", lock))
	dep := sm.controller.deployment
	for _, s := range expired {
		var derr error
		start := time.Now()
		if isZFSDevice(vol.Device) {
			snap := fmt.Sprintf("%s/%s@%s", vol.Pool, vol.BackingVolume, s.Name)
			sm.controller.releaseZFSLockHold(ctx, host, snap)
			_, derr = dep.ZFSDestroySnapshot(ctx, []string{host}, snap)
		} else {
			_, derr = dep.LVRemoveSnapshot(ctx, []string{host}, vol.Pool, s.Name)
		}
		log.Debug("ScheduleManager.pruneVolume: deleted snapshot", zap.String("node", node), zap.String("snapshot", s.Name),
			zap.Duration("took", time.Since(start)), zap.Error(derr))
		if derr != nil {
			log.Warn("Snapshot schedule: prune failed",
				zap.String("node", node), zap.String("snapshot", s.Name), zap.Error(derr))
			continue
		}
		log.Info("Snapshot schedule: pruned expired snapshot",
			zap.String("node", node), zap.String("snapshot", s.Name))
	}
}

// listScheduledSnaps returns this volume's scheduler-managed snapshots on host.
func (sm *ScheduleManager) listScheduledSnaps(ctx context.Context, host string, vol *ResourceVolumeInfo) ([]scheduledSnap, error) {
	log := sm.scheduleLog()
	dep := sm.controller.deployment
	var names []string

	start := time.Now()
	if isZFSDevice(vol.Device) {
		dataset := fmt.Sprintf("%s/%s", vol.Pool, vol.BackingVolume)
		res, err := dep.ZFSListSnapshots(ctx, []string{host}, dataset)
		log.Debug("ScheduleManager.listScheduledSnaps: listed zfs snapshots", zap.String("host", host), zap.String("dataset", dataset),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return nil, err
		}
		for _, line := range execLines(res, host) {
			// "pool/vol@snapname  used  refer  creation"
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if at := strings.Index(fields[0], "@"); at >= 0 {
				names = append(names, fields[0][at+1:])
			}
		}
	} else {
		res, err := dep.LVListSnapshots(ctx, []string{host}, vol.Pool)
		log.Debug("ScheduleManager.listScheduledSnaps: listed LVM snapshots", zap.String("host", host), zap.String("pool", vol.Pool),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return nil, err
		}
		for _, line := range execLines(res, host) {
			// Pipe-separated, because lv_time carries spaces. Splitting on
			// whitespace worked only as long as the name was first and nothing
			// downstream looked at the other columns.
			name := strings.TrimSpace(line)
			if idx := strings.IndexByte(name, '|'); idx >= 0 {
				name = strings.TrimSpace(name[:idx])
			}
			if name == "" {
				continue
			}
			names = append(names, name)
		}
	}

	var out []scheduledSnap
	for _, n := range names {
		backing, ts, ok := parseSnapName(n)
		if !ok || backing != vol.BackingVolume {
			continue
		}
		out = append(out, scheduledSnap{Name: n, TS: ts})
	}
	log.Debug("ScheduleManager.listScheduledSnaps: scheduled snapshots", zap.String("host", host), zap.String("volume", vol.BackingVolume),
		zap.Int("listed", len(names)), zap.Int("scheduled", len(out)))
	return out, nil
}

// isZFSDevice reports whether a volume's device path is a ZFS zvol.
func isZFSDevice(device string) bool {
	return strings.Contains(device, "/dev/zvol/")
}

// execLines returns the trimmed non-empty output lines for host from a result.
func execLines(res *deployment.ExecResult, host string) []string {
	if res == nil {
		return nil
	}
	var lines []string
	for h, hr := range res.Hosts {
		if h != host {
			continue
		}
		for _, raw := range strings.Split(hr.Output, "\n") {
			if t := strings.TrimSpace(raw); t != "" {
				lines = append(lines, t)
			}
		}
	}
	return lines
}

// logSnapResult records the outcome of a single snapshot attempt.
func logSnapResult(log *zap.Logger, kind, node, snapName string, res *deployment.ExecResult, err error) {
	if err != nil {
		log.Warn("Snapshot schedule: snapshot failed",
			zap.String("kind", kind), zap.String("node", node),
			zap.String("snapshot", snapName), zap.Error(err))
		return
	}
	if res != nil && !res.AllSuccess() {
		log.Warn("Snapshot schedule: snapshot failed on host",
			zap.String("kind", kind), zap.String("node", node),
			zap.String("snapshot", snapName), zap.Strings("failed", res.FailedHosts()))
		return
	}
	log.Info("Snapshot schedule: created snapshot",
		zap.String("kind", kind), zap.String("node", node), zap.String("snapshot", snapName))
}
