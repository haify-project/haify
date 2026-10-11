package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/event"
)

// Taking a disk out of a pool, or swapping it for another, while the pool
// stays in use (`haify pool remove-disk`, `haify pool replace-disk`).
//
// The data on the disk moves first (pvmove), and only then does the disk
// leave the volume group. Moving a full disk takes hours, so the work runs on
// the node as a transient systemd unit that writes its outcome to a status
// file, and the controller follows it as a storage job that survives a
// failover.

const (
	diskJobPoll   = 15 * time.Second
	diskJobStatus = "/var/lib/haify-jobs"
)

// pvInfo is one physical volume of a group.
type pvInfo struct {
	Name      string
	SizeBytes uint64
	UsedBytes uint64
}

func parsePVs(out string) []pvInfo {
	var pvs []pvInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) != 3 {
			continue
		}
		size, _ := strconv.ParseUint(strings.TrimSpace(f[1]), 10, 64)
		used, _ := strconv.ParseUint(strings.TrimSpace(f[2]), 10, 64)
		pvs = append(pvs, pvInfo{Name: strings.TrimSpace(f[0]), SizeBytes: size, UsedBytes: used})
	}
	return pvs
}

// checkRemovable says why disk cannot leave a group made of pvs, or "".
func checkRemovable(pvs []pvInfo, disk string) string {
	var target *pvInfo
	var freeElsewhere uint64
	for i := range pvs {
		if pvs[i].Name == disk {
			target = &pvs[i]
			continue
		}
		freeElsewhere += pvs[i].SizeBytes - pvs[i].UsedBytes
	}
	switch {
	case target == nil:
		return fmt.Sprintf("%s is not a disk of this pool", disk)
	case len(pvs) < 2:
		return fmt.Sprintf("%s is the pool's only disk; delete the pool instead", disk)
	case target.UsedBytes > freeElsewhere:
		return fmt.Sprintf("%s holds %s but the other disks have only %s free; add a disk first",
			disk, formatBytes(target.UsedBytes), formatBytes(freeElsewhere))
	}
	return ""
}

// diskJobScript is what runs on the node. Each step is idempotent enough to
// be repeated if the unit is restarted after a controller failover.
func diskJobScript(j *database.StorageJob) string {
	vg, old := j.Pool, j.Disk
	status := fmt.Sprintf("%s/%s.status", diskJobStatus, j.ID)
	var b strings.Builder
	fmt.Fprintf(&b, "set -u\nmkdir -p %s\nfail() { echo \"failed: $1\" > %s; exit 1; }\n", diskJobStatus, status)
	if j.Kind == database.JobReplaceDisk {
		nw := j.NewDisk
		fmt.Fprintf(&b, "pvs %s >/dev/null 2>&1 || pvcreate -y %s || fail 'pvcreate %s'\n", nw, nw, nw)
		fmt.Fprintf(&b, "pvs --noheadings -o vg_name %s | grep -qw %s || vgextend %s %s || fail 'vgextend'\n", nw, vg, vg, nw)
		fmt.Fprintf(&b, "used=$(pvs --noheadings --nosuffix --units b -o pv_used %s | tr -d ' ')\n", old)
		fmt.Fprintf(&b, "[ \"${used:-0}\" = 0 ] || pvmove -i 15 %s %s || fail 'pvmove'\n", old, nw)
	} else {
		fmt.Fprintf(&b, "used=$(pvs --noheadings --nosuffix --units b -o pv_used %s | tr -d ' ')\n", old)
		fmt.Fprintf(&b, "[ \"${used:-0}\" = 0 ] || pvmove -i 15 %s || fail 'pvmove'\n", old)
	}
	fmt.Fprintf(&b, "vgreduce %s %s || fail 'vgreduce'\npvremove -y %s || true\necho done > %s\n", vg, old, old, status)
	return b.String()
}

func newJobID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// poolPVs lists the disks of vg on host.
func (sm *StorageManager) poolPVs(ctx context.Context, host, vg string) ([]pvInfo, error) {
	cmd := fmt.Sprintf("sudo pvs --noheadings --nosuffix --units b --separator '|' -o pv_name,pv_size,pv_used -S vg_name=%s", vg)
	res, err := sm.controller.deployment.Exec(ctx, []string{host}, cmd)
	if err != nil {
		return nil, err
	}
	hr := res.Hosts[host]
	if hr == nil || !hr.Success || strings.TrimSpace(hr.Output) == "" {
		return nil, fmt.Errorf("pool %s not found on that node", vg)
	}
	return parsePVs(hr.Output), nil
}

// StartDiskJob begins removing disk from pool on node, or replacing it with
// newDisk when that is set; wipe lets a new disk that holds old data be
// erased. It returns once the work is running.
func (sm *StorageManager) StartDiskJob(ctx context.Context, pool, node, disk, newDisk string, wipe bool) (string, error) {
	c := sm.controller
	pool = normalizeManagedName(pool)
	host := c.ResolveHost(node)
	if pool == "" || node == "" || disk == "" {
		return "", fmt.Errorf("pool, node and disk are required")
	}
	jobs, _ := c.db.ListStorageJobs(ctx)
	for _, j := range jobs {
		if j.State == database.JobRunning && j.Pool == pool && j.Node == node {
			return "", fmt.Errorf("job %s is already moving data in %s on %s", j.ID, pool, node)
		}
	}
	pvs, err := sm.poolPVs(ctx, host, pool)
	if err != nil {
		return "", err
	}
	kind := database.JobRemoveDisk
	if newDisk != "" {
		kind = database.JobReplaceDisk
		for _, p := range pvs {
			if p.Name == newDisk {
				return "", fmt.Errorf("%s is already a disk of %s", newDisk, pool)
			}
		}
		if !containsPV(pvs, disk) {
			return "", fmt.Errorf("%s is not a disk of %s on %s", disk, pool, node)
		}
		paths, err := sm.prepareDisks(ctx, host, node, []string{newDisk}, wipe)
		if err != nil {
			return "", err
		}
		newDisk = paths[0]
	} else if why := checkRemovable(pvs, disk); why != "" {
		return "", fmt.Errorf("%s", why)
	}
	j := &database.StorageJob{ID: newJobID(), Kind: kind, State: database.JobRunning, Pool: pool, Node: node,
		Disk: disk, NewDisk: newDisk, Progress: "starting", StartedAt: time.Now()}
	if err := c.db.SaveStorageJob(ctx, j); err != nil {
		return "", err
	}
	if err := sm.launchDiskJob(ctx, host, j); err != nil {
		j.State, j.Message = database.JobFailed, err.Error()
		_ = c.db.SaveStorageJob(ctx, j)
		return "", err
	}
	go sm.followDiskJob(c.ctx, j)
	return j.ID, nil
}

func containsPV(pvs []pvInfo, name string) bool {
	for _, p := range pvs {
		if p.Name == name {
			return true
		}
	}
	return false
}

func (sm *StorageManager) launchDiskJob(ctx context.Context, host string, j *database.StorageJob) error {
	script := base64Std(diskJobScript(j))
	cmd := fmt.Sprintf("sudo systemd-run --unit=haify-disk-%s --collect /bin/bash -c 'echo %s | base64 -d | /bin/bash'", j.ID, script)
	return execFailure(sm.controller.deployment.Exec(ctx, []string{host}, cmd))
}

// diskJobState reads a job's status file and progress on its node.
func (sm *StorageManager) diskJobState(ctx context.Context, host string, j *database.StorageJob) (status, progress string, err error) {
	cmd := fmt.Sprintf("cat %s/%s.status 2>/dev/null; echo '--'; systemctl is-active haify-disk-%s 2>/dev/null; echo '--'; "+
		"sudo lvs -a --noheadings -o lv_name,copy_percent,sync_percent %s 2>/dev/null | awk '$1 ~ /pvmove/ {print \"moving \" $2 \"%%\"} $3 != \"\" && $3 != \"100.00\" {print $1 \" syncing \" $3 \"%%\"}' | head -3",
		diskJobStatus, j.ID, j.ID, j.Pool)
	res, err := sm.controller.deployment.Exec(ctx, []string{host}, cmd)
	if err != nil {
		return "", "", err
	}
	hr := res.Hosts[host]
	if hr == nil {
		return "", "", fmt.Errorf("no answer from %s", host)
	}
	parts := strings.SplitN(hr.Output, "--", 3)
	if len(parts) < 3 {
		return "", "", fmt.Errorf("unreadable job state")
	}
	status = strings.TrimSpace(parts[0])
	if status == "" && strings.TrimSpace(parts[1]) != "active" {
		status = "failed: the job stopped without finishing; see `journalctl -u haify-disk-" + j.ID + "` on " + j.Node
	}
	return status, strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimSpace(parts[2]), "\n", "; ")), " "), nil
}

// followDiskJob polls a running job until it finishes.
func (sm *StorageManager) followDiskJob(ctx context.Context, j *database.StorageJob) {
	c := sm.controller
	host := c.ResolveHost(j.Node)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(diskJobPoll):
		}
		status, progress, err := sm.diskJobState(ctx, host, j)
		if err != nil {
			continue // the node may be briefly unreachable; the job runs there regardless
		}
		if status == "" {
			if progress != "" && progress != j.Progress {
				j.Progress = progress
				_ = c.db.SaveStorageJob(ctx, j)
			}
			continue
		}
		sm.finishDiskJob(ctx, j, status)
		return
	}
}

func (sm *StorageManager) finishDiskJob(ctx context.Context, j *database.StorageJob, status string) {
	c := sm.controller
	sev := event.SeverityInfo
	verb := "removed from"
	if j.Kind == database.JobReplaceDisk {
		verb = "replaced by " + j.NewDisk + " in"
	}
	if status == "done" {
		j.State, j.Progress = database.JobDone, "done"
		j.Message = fmt.Sprintf("%s %s pool %s on %s; its data moved off first", j.Disk, verb, j.Pool, j.Node)
		sm.recordPoolDiskChange(ctx, j)
	} else {
		sev = event.SeverityWarning
		j.State = database.JobFailed
		j.Message = fmt.Sprintf("moving data off %s in pool %s on %s %s; the disk is still part of the pool",
			j.Disk, j.Pool, j.Node, strings.TrimPrefix(status, "failed: "))
	}
	_ = c.db.SaveStorageJob(ctx, j)
	c.logger.Info("Disk job finished", zap.String("job", j.ID), zap.String("state", j.State), zap.String("message", j.Message))
	if c.events != nil {
		c.events.Publish(event.Event{Type: event.TypePoolDiskMoved, Severity: sev, Status: event.StatusInfo,
			Node: j.Node, Message: j.Message, Details: map[string]string{"pool": j.Pool, "job": j.ID}})
	}
}

// recordPoolDiskChange keeps the pool record's device list in step.
func (sm *StorageManager) recordPoolDiskChange(ctx context.Context, j *database.StorageJob) {
	db := sm.controller.db
	p, err := db.GetPool(ctx, j.Pool)
	if err != nil || p == nil {
		return
	}
	var devs []string
	for _, d := range splitCSV(p.Devices) {
		if d != j.Disk {
			devs = append(devs, d)
		}
	}
	if j.NewDisk != "" {
		devs = append(devs, j.NewDisk)
	}
	p.Devices = strings.Join(devs, ",")
	p.UpdatedAt = time.Now()
	_ = db.SavePool(ctx, p)
}

// resumeDiskJobs follows the disk jobs a previous controller started.
func (sm *StorageManager) resumeDiskJobs(ctx context.Context) {
	jobs, err := sm.controller.db.ListStorageJobs(ctx)
	if err != nil {
		return
	}
	for _, j := range jobs {
		if j.State == database.JobRunning && (j.Kind == database.JobRemoveDisk || j.Kind == database.JobReplaceDisk) {
			go sm.followDiskJob(sm.controller.ctx, j)
		}
	}
}
