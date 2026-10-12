package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/haify-project/haify/pkg/event"
)

// Moving a volume to another pool while the resource stays in service
// (`haify resource move-volume`): from an HDD pool to an SSD pool, off a pool
// that is being retired.
//
// One node at a time: the node's copy is detached, a fresh volume is made in
// the new pool and attached, and DRBD copies the data back from a peer. The
// resource keeps serving throughout — a Primary whose own copy is detached
// reads and writes through its peers — and is never more than one copy short.
// A thin target stays thin: the resync sends zeros as discards. The move ends
// by writing the new pool into the resource's config and database record.
//
// The old volume's LVM snapshots cannot come along and are deleted with it,
// so a resource whose snapshots are locked is refused.

const (
	volumeMovePoll    = 10 * time.Second
	volumeMoveTimeout = 72 * time.Hour
)

// MoveVolume starts moving volume volID of resource to pool. A failed move
// is resumed by asking again with the same pool.
func (rm *ResourceManager) MoveVolume(ctx context.Context, resource string, volID int, pool string) (string, error) {
	c := rm.controller
	log := c.traceLogger()
	log.Debug("MoveVolume: checking", zap.String("resource", resource), zap.Int("volume", volID), zap.String("pool", pool))
	pool = normalizeManagedName(pool)
	r, err := c.db.GetResource(ctx, resource)
	if err != nil || r == nil {
		log.Debug("MoveVolume: resource not found", zap.String("resource", resource), zap.Error(err))
		return "", fmt.Errorf("resource %q not found", resource)
	}
	vol, err := rm.dbVolume(ctx, resource, volID)
	if err != nil {
		return "", err
	}
	nodes := splitCSV(r.Nodes)
	switch {
	case vol.Pool == pool:
		log.Debug("MoveVolume: already in the pool", zap.String("pool", pool))
		return "", fmt.Errorf("volume %d of %s is already in %s", volID, resource, pool)
	case r.Encrypted:
		log.Debug("MoveVolume: encrypted, refusing", zap.String("resource", resource))
		return "", fmt.Errorf("%s is encrypted; moving an encrypted volume is not supported", resource)
	case r.MoveFrom != "":
		log.Debug("MoveVolume: a replica move is under way, refusing", zap.String("from", r.MoveFrom), zap.String("to", r.MoveTo))
		return "", fmt.Errorf("%s is moving a replica (%s → %s); wait for it to finish", resource, r.MoveFrom, r.MoveTo)
	case len(nodes) < 2:
		log.Debug("MoveVolume: one copy only, refusing", zap.Strings("nodes", nodes))
		return "", fmt.Errorf("%s has one copy; its data would have nowhere to come back from", resource)
	}
	if err := c.assertResourceUnlocked(ctx, resource, "moving the volume to another pool"); err != nil {
		return "", err
	}
	for _, n := range nodes {
		start := time.Now()
		res, err := rm.deployment.Exec(ctx, []string{c.ResolveHost(n)}, "sudo vgs "+pool)
		if err != nil || !res.AllSuccess() {
			log.Debug("MoveVolume: target pool missing on node", zap.String("node", n), zap.String("pool", pool),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			return "", fmt.Errorf("pool %s does not exist on %s", pool, n)
		}
		log.Debug("MoveVolume: target pool present", zap.String("node", n), zap.Duration("took", time.Since(start)))
	}
	jobs, _ := c.db.ListStorageJobs(ctx)
	var done []string
	for _, j := range jobs {
		if j.Kind != database.JobMoveVolume || j.Resource != resource {
			continue
		}
		if j.State == database.JobRunning {
			log.Debug("MoveVolume: a move is already running", zap.String("job", j.ID))
			return "", fmt.Errorf("job %s is already moving a volume of %s", j.ID, resource)
		}
		if j.State == database.JobFailed && j.VolumeID == volID && j.TargetPool == pool && len(done) == 0 {
			log.Debug("MoveVolume: resuming a failed move", zap.String("job", j.ID), zap.Strings("done", j.Done))
			done = j.Done // pick up where the last attempt stopped
		}
	}
	j := &database.StorageJob{ID: newJobID(), Kind: database.JobMoveVolume, State: database.JobRunning,
		Resource: resource, VolumeID: volID, FromPool: vol.Pool, TargetPool: pool,
		Nodes: rm.secondariesFirst(ctx, resource, nodes), Done: done, Progress: "starting", StartedAt: time.Now()}
	if err := c.db.SaveStorageJob(ctx, j); err != nil {
		log.Debug("MoveVolume: job not saved", zap.Error(err))
		return "", err
	}
	log.Debug("MoveVolume: job started", zap.String("job", j.ID), zap.String("from_pool", vol.Pool), zap.String("pool", pool),
		zap.Strings("order", j.Nodes))
	go rm.runVolumeMove(c.ctx, j)
	return j.ID, nil
}

func (rm *ResourceManager) dbVolume(ctx context.Context, resource string, volID int) (*database.Volume, error) {
	log := rm.controller.traceLogger()
	vols, err := rm.controller.db.ListVolumes(ctx, resource)
	if err != nil {
		log.Debug("dbVolume: volume list failed", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	for _, v := range vols {
		if v.VolumeID == volID {
			log.Debug("dbVolume: found", zap.String("resource", resource), zap.Int("volume", volID), zap.String("pool", v.Pool))
			return v, nil
		}
	}
	log.Debug("dbVolume: no such volume", zap.String("resource", resource), zap.Int("volume", volID))
	return nil, fmt.Errorf("%s has no volume %d", resource, volID)
}

// secondariesFirst orders nodes so the Primary's copy moves last: until then
// it keeps serving from its own disk.
func (rm *ResourceManager) secondariesFirst(ctx context.Context, resource string, nodes []string) []string {
	var primary string
	if info, err := rm.GetResource(ctx, resource); err == nil {
		for n, st := range info.NodeStates {
			if strings.EqualFold(st.Role, "Primary") {
				primary = rm.controller.NodeName(n)
			}
		}
	}
	var out []string
	for _, n := range nodes {
		if n != primary {
			out = append(out, n)
		}
	}
	if contains(nodes, primary) {
		out = append(out, primary)
	}
	rm.controller.traceLogger().Debug("secondariesFirst: order", zap.String("resource", resource), zap.String("primary", primary),
		zap.Strings("order", out))
	return out
}

func (rm *ResourceManager) runVolumeMove(ctx context.Context, j *database.StorageJob) {
	c := rm.controller
	log := c.traceLogger()
	log.Debug("runVolumeMove: running", zap.String("job", j.ID), zap.String("resource", j.Resource), zap.Int("volume", j.VolumeID),
		zap.Strings("nodes", j.Nodes), zap.Strings("done", j.Done))
	for _, n := range j.Nodes {
		if contains(j.Done, n) {
			log.Debug("runVolumeMove: node already done", zap.String("node", n))
			continue
		}
		j.Progress = fmt.Sprintf("moving %s (%d of %d)", n, len(j.Done)+1, len(j.Nodes))
		_ = c.db.SaveStorageJob(ctx, j)
		start := time.Now()
		log.Debug("runVolumeMove: moving node", zap.String("node", n), zap.String("progress", j.Progress))
		if err := rm.moveVolumeOnNode(ctx, j, n); err != nil {
			log.Debug("runVolumeMove: node failed", zap.String("node", n), zap.Duration("took", time.Since(start)), zap.Error(err))
			rm.finishVolumeMove(ctx, j, fmt.Errorf("on %s: %w", n, err))
			return
		}
		j.Done = append(j.Done, n)
		_ = c.db.SaveStorageJob(ctx, j)
		log.Debug("runVolumeMove: node moved", zap.String("node", n), zap.Duration("took", time.Since(start)))
	}
	rm.finishVolumeMove(ctx, j, rm.commitVolumeMove(ctx, j))
}

// moveVolumeOnNode replaces node's copy of the volume with one in the target
// pool and waits for DRBD to fill it.
func (rm *ResourceManager) moveVolumeOnNode(ctx context.Context, j *database.StorageJob, node string) error {
	c := rm.controller
	log := c.traceLogger()
	host := c.ResolveHost(node)
	vol, err := rm.dbVolume(ctx, j.Resource, j.VolumeID)
	if err != nil {
		return err
	}
	target := fmt.Sprintf("%s/%d", j.Resource, j.VolumeID)
	log.Debug("moveVolumeOnNode: waiting for a peer to be UpToDate", zap.String("node", node), zap.String("target", target))
	if err := rm.waitPeerUpToDate(ctx, j.Resource, j.VolumeID, node); err != nil {
		return err
	}
	size, err := rm.deployment.LVSizeBytes(ctx, host, j.FromPool, vol.VolumeName)
	if err != nil {
		log.Debug("moveVolumeOnNode: old size not read", zap.String("host", host), zap.Error(err))
		return fmt.Errorf("read the size of %s/%s: %w", j.FromPool, vol.VolumeName, err)
	}
	// A little larger than the old volume: fresh metadata may be sized for
	// more peers than the old copy's, and a copy smaller than its peers is
	// refused when it connects.
	newSize := size + size/256 + 64<<20
	log.Debug("moveVolumeOnNode: creating the new copy", zap.String("host", host), zap.String("pool", j.TargetPool),
		zap.String("volume", vol.VolumeName), zap.Uint64("old_size_bytes", size), zap.Uint64("size_bytes", newSize))
	if err := rm.createBackingVolumeOn(ctx, host, j.TargetPool, vol.VolumeName, size+size/256+64<<20); err != nil {
		log.Debug("moveVolumeOnNode: new copy not created", zap.Error(err))
		return fmt.Errorf("create %s/%s: %w", j.TargetPool, vol.VolumeName, err)
	}
	if err := execFailure(rm.deployment.DRBDDetach(ctx, host, target)); err != nil {
		log.Debug("moveVolumeOnNode: detach failed", zap.String("host", host), zap.Error(err))
		return fmt.Errorf("detach: %w", err)
	}
	log.Debug("moveVolumeOnNode: detached, pointing the config at the new copy", zap.String("host", host))
	if err := rm.writeHostVolumeDisk(ctx, j, node, "/dev/"+j.TargetPool+"/"+vol.VolumeName); err != nil {
		log.Debug("moveVolumeOnNode: config not rewritten", zap.Error(err))
		return err
	}
	if err := execFailure(rm.deployment.Exec(ctx, []string{host},
		fmt.Sprintf("sudo drbdadm create-md --max-peers=%d --force %s", deployment.DefaultMaxPeers, target))); err != nil {
		log.Debug("moveVolumeOnNode: create-md failed", zap.String("host", host), zap.Error(err))
		return fmt.Errorf("create metadata: %w", err)
	}
	if err := execFailure(rm.deployment.DRBDAttach(ctx, host, target)); err != nil {
		log.Debug("moveVolumeOnNode: attach failed", zap.String("host", host), zap.Error(err))
		return fmt.Errorf("attach: %w", err)
	}
	log.Debug("moveVolumeOnNode: attached, waiting for the resync", zap.String("host", host), zap.String("target", target))
	if err := rm.waitLocalUpToDate(ctx, host, target); err != nil {
		return err
	}
	// The old copy and its snapshots go.
	old := fmt.Sprintf("for s in $(sudo lvs --noheadings -o lv_name -S origin=%s %s 2>/dev/null); do sudo lvremove -f %s/$s; done; sudo lvremove -f %s/%s",
		vol.VolumeName, j.FromPool, j.FromPool, j.FromPool, vol.VolumeName)
	if err := execFailure(rm.deployment.Exec(ctx, []string{host}, old)); err != nil {
		c.logger.Warn("Volume moved, but the old copy could not be removed", zap.String("node", node), zap.Error(err))
	} else {
		log.Debug("moveVolumeOnNode: old copy removed", zap.String("host", host), zap.String("pool", j.FromPool))
	}
	return nil
}

// writeHostVolumeDisk points node's copy at disk in the config every member
// holds.
func (rm *ResourceManager) writeHostVolumeDisk(ctx context.Context, j *database.StorageJob, node, disk string) error {
	rm.controller.traceLogger().Debug("writeHostVolumeDisk: rewriting", zap.String("resource", j.Resource),
		zap.String("node", node), zap.Int("volume", j.VolumeID), zap.String("disk", disk))
	return rm.editResourceConfig(ctx, j.Resource, func(conf string) (string, error) {
		minor := -1
		for _, v := range parseResourceConfigVolumes(conf) {
			if v.VolumeID == j.VolumeID {
				minor = v.Minor
			}
		}
		if minor < 0 {
			rm.controller.traceLogger().Debug("writeHostVolumeDisk: volume not in the config", zap.Int("volume", j.VolumeID))
			return "", fmt.Errorf("volume %d is not in the config", j.VolumeID)
		}
		return setHostVolumeDisk(conf, rm.controller.nodes.GetDRBDNameByRef(node), j.VolumeID, minor, disk)
	})
}

// editResourceConfig rewrites a resource's .res on every member.
func (rm *ResourceManager) editResourceConfig(ctx context.Context, resource string, edit func(string) (string, error)) error {
	log := rm.controller.traceLogger()
	r, err := rm.controller.db.GetResource(ctx, resource)
	if err != nil || r == nil {
		log.Debug("editResourceConfig: resource not found", zap.String("resource", resource), zap.Error(err))
		return fmt.Errorf("resource %q not found", resource)
	}
	var hosts []string
	for _, n := range splitCSV(r.Nodes) {
		hosts = append(hosts, rm.controller.ResolveHost(n))
	}
	path := "/etc/drbd.d/" + resource + ".res"
	conf, err := rm.readResourceConfig(ctx, hosts, path, resource)
	if err != nil {
		log.Debug("editResourceConfig: config not read", zap.Error(err))
		return err
	}
	conf, err = edit(conf)
	if err != nil {
		log.Debug("editResourceConfig: edit refused", zap.Error(err))
		return err
	}
	targets := append(hosts, rm.disklessParticipantHosts(ctx, resource)...)
	start := time.Now()
	if _, err := rm.deployment.DistributeConfig(ctx, targets, conf, path); err != nil {
		log.Debug("editResourceConfig: distribution failed", zap.Strings("hosts", targets), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("distribute the config: %w", err)
	}
	// The config body is not traced: it can hold a shared-secret.
	log.Debug("editResourceConfig: distributed", zap.String("resource", resource), zap.Strings("hosts", targets),
		zap.Int("bytes", len(conf)), zap.Duration("took", time.Since(start)))
	return nil
}

// waitPeerUpToDate waits until a member other than node has the volume
// UpToDate, the copy node's will be rebuilt from.
func (rm *ResourceManager) waitPeerUpToDate(ctx context.Context, resource string, vol int, node string) error {
	log := rm.controller.traceLogger()
	deadline := time.Now().Add(10 * time.Minute)
	for {
		if info, err := rm.GetResource(ctx, resource); err == nil {
			for n, st := range info.NodeStates {
				if rm.controller.NodeName(n) != node && st.DiskState == "UpToDate" {
					log.Debug("waitPeerUpToDate: peer UpToDate", zap.String("resource", resource), zap.String("peer", n))
					return nil
				}
			}
			log.Debug("waitPeerUpToDate: no peer UpToDate yet", zap.String("resource", resource), zap.String("node", node))
		} else {
			log.Debug("waitPeerUpToDate: status not read", zap.String("resource", resource), zap.Error(err))
		}
		if time.Now().After(deadline) {
			log.Debug("waitPeerUpToDate: deadline passed", zap.String("resource", resource))
			return fmt.Errorf("no other copy of %s is UpToDate to rebuild from", resource)
		}
		select {
		case <-ctx.Done():
			log.Debug("waitPeerUpToDate: cancelled", zap.Error(ctx.Err()))
			return ctx.Err()
		case <-time.After(volumeMovePoll):
		}
	}
}

// waitLocalUpToDate waits for host's copy of target (res/vol) to finish its
// resync.
func (rm *ResourceManager) waitLocalUpToDate(ctx context.Context, host, target string) error {
	log := rm.controller.traceLogger()
	deadline := time.Now().Add(volumeMoveTimeout)
	for {
		res, err := rm.deployment.Exec(ctx, []string{host}, "sudo drbdadm dstate "+target)
		if err == nil && res.Hosts[host] != nil {
			dstate := strings.TrimSpace(res.Hosts[host].Output)
			if strings.HasPrefix(dstate, "UpToDate/") {
				log.Debug("waitLocalUpToDate: UpToDate", zap.String("host", host), zap.String("target", target))
				return nil
			}
			log.Debug("waitLocalUpToDate: still syncing", zap.String("host", host), zap.String("target", target), zap.String("dstate", dstate))
		} else {
			log.Debug("waitLocalUpToDate: dstate not read", zap.String("host", host), zap.Error(err))
		}
		if time.Now().After(deadline) {
			log.Debug("waitLocalUpToDate: deadline passed", zap.String("host", host), zap.String("target", target))
			return fmt.Errorf("the new copy did not finish syncing within %s", volumeMoveTimeout)
		}
		select {
		case <-ctx.Done():
			log.Debug("waitLocalUpToDate: cancelled", zap.Error(ctx.Err()))
			return ctx.Err()
		case <-time.After(volumeMovePoll):
		}
	}
}

// commitVolumeMove makes the new pool the volume's pool for every node.
func (rm *ResourceManager) commitVolumeMove(ctx context.Context, j *database.StorageJob) error {
	log := rm.controller.traceLogger()
	log.Debug("commitVolumeMove: committing", zap.String("job", j.ID), zap.String("resource", j.Resource), zap.String("pool", j.TargetPool))
	vol, err := rm.dbVolume(ctx, j.Resource, j.VolumeID)
	if err != nil {
		return err
	}
	disk := "/dev/" + j.TargetPool + "/" + vol.VolumeName
	err = rm.editResourceConfig(ctx, j.Resource, func(conf string) (string, error) {
		out, err := setResourceVolumeDisk(conf, j.VolumeID, disk)
		if err != nil {
			return "", err
		}
		for _, n := range j.Nodes {
			out = removeHostVolumeDisk(out, rm.controller.nodes.GetDRBDNameByRef(n), j.VolumeID)
		}
		return out, nil
	})
	if err != nil {
		log.Debug("commitVolumeMove: config not committed", zap.Error(err))
		return err
	}
	vol.Pool, vol.Device = j.TargetPool, disk
	err = rm.controller.db.SaveVolume(ctx, vol)
	log.Debug("commitVolumeMove: volume record saved", zap.String("disk", disk), zap.Error(err))
	return err
}

func (rm *ResourceManager) finishVolumeMove(ctx context.Context, j *database.StorageJob, err error) {
	c := rm.controller
	sev := event.SeverityInfo
	if err == nil {
		j.State, j.Progress = database.JobDone, "done"
		j.Message = fmt.Sprintf("volume %d of %s moved from %s to %s on %s", j.VolumeID, j.Resource, j.FromPool, j.TargetPool, strings.Join(j.Nodes, ", "))
	} else {
		sev = event.SeverityWarning
		j.State = database.JobFailed
		j.Message = fmt.Sprintf("moving volume %d of %s to %s stopped %v; moved so far: %s. Ask again to carry on from there",
			j.VolumeID, j.Resource, j.TargetPool, err, strings.Join(j.Done, ", "))
	}
	serr := c.db.SaveStorageJob(context.WithoutCancel(ctx), j)
	c.traceLogger().Debug("finishVolumeMove: job saved", zap.String("job", j.ID), zap.String("state", j.State), zap.Error(serr))
	c.logger.Info("Volume move finished", zap.String("job", j.ID), zap.String("state", j.State), zap.String("message", j.Message))
	if c.events != nil {
		c.events.Publish(event.Event{Type: event.TypeVolumeMoved, Severity: sev, Status: event.StatusInfo,
			Resource: j.Resource, Message: j.Message, Details: map[string]string{"job": j.ID, "pool": j.TargetPool}})
	}
}

// resumeVolumeMoves carries on with the moves a previous controller started.
func (rm *ResourceManager) resumeVolumeMoves(ctx context.Context) {
	log := rm.controller.traceLogger()
	jobs, err := rm.controller.db.ListStorageJobs(ctx)
	if err != nil {
		log.Debug("resumeVolumeMoves: job list failed", zap.Error(err))
		return
	}
	resumed := 0
	for _, j := range jobs {
		if j.Kind == database.JobMoveVolume && j.State == database.JobRunning {
			log.Debug("resumeVolumeMoves: resuming", zap.String("job", j.ID), zap.String("resource", j.Resource))
			resumed++
			go rm.runVolumeMove(rm.controller.ctx, j)
		}
	}
	log.Debug("resumeVolumeMoves: done", zap.Int("resumed", resumed))
}
