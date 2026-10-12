package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/event"
)

// Retention counts snapshots; it does not look at the pool they fill. On a
// thin pool every snapshot holds the blocks written since it was taken, so a
// busy volume's history can outgrow the pool while staying well inside its
// policy. The pool fills, the next snapshot cannot be created, and retention,
// which is measured from the newest snapshot there is, stops removing
// anything: the pool stays full. The replica is what pays — its writes fail
// and DRBD drops the disk. That is what happened to one replica of a
// production resource, with the three other nodes at 79–90% on the same
// road.
//
// So after retention, a pool still past the near-full threshold gives up its
// oldest scheduled snapshots of the volume, one at a time, until it is below
// it. A snapshot is a convenience; the replica is the data.

// snapshotsKeptUnderPressure is how many of a volume's newest scheduled
// snapshots survive even a pool that stays full: removing the last ones would
// not free enough to matter and leaves nothing to restore from.
const snapshotsKeptUnderPressure = 2

// relieveThinPool removes the volume's oldest scheduled snapshots on host
// while its thin pool is past ThinPoolNearFullPercent (data or metadata).
//
// Snapshots still inside the schedule's lock window are never removed
// (snapshot_lock.go): a pool filling fast is what ransomware rewriting a
// volume looks like, and the oldest snapshots are the clean ones. Every
// removal is announced, because history disappearing to make room is worth
// knowing about; and a pool that stays full once only locked snapshots are
// left raises a critical event, because its replica is next.
func (sm *ScheduleManager) relieveThinPool(ctx context.Context, host, node, resource string, vol *ResourceVolumeInfo, lock time.Duration) {
	if isZFSDevice(vol.Device) || sm.controller.storage == nil {
		ctrlLogOrGlobal(sm.controller).Debug("relieveThinPool: ZFS volume or no storage manager, nothing to relieve",
			zap.String("resource", resource), zap.String("device", vol.Device))
		return
	}
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("relieveThinPool: checking pool", zap.String("node", node), zap.String("pool", vol.Pool),
		zap.String("resource", resource), zap.Duration("lock", lock))
	usage, err := sm.controller.storage.readThinUsage(ctx, host, vol.Pool)
	if err != nil || usage == nil || !overThinThreshold(usage) {
		if usage != nil {
			log.Debug("relieveThinPool: pool below threshold", zap.String("pool", vol.Pool),
				zap.Float64("data_percent", usage.DataPercent), zap.Float64("metadata_percent", usage.MetaPercent))
		} else {
			log.Debug("relieveThinPool: pool usage not read", zap.String("pool", vol.Pool), zap.Error(err))
		}
		return
	}
	snaps, err := sm.listScheduledSnaps(ctx, host, vol)
	if err != nil {
		log.Debug("relieveThinPool: scheduled snapshots not listed", zap.String("pool", vol.Pool), zap.Error(err))
		return
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].TS.Before(snaps[j].TS) })
	// The newest few always stay; of the rest, only the unlocked may go.
	keep := len(snaps) - snapshotsKeptUnderPressure
	if keep < 0 {
		keep = 0
	}
	candidates := unlockedSnaps(snaps[:keep], lock, lockNow())
	log.Debug("relieveThinPool: pool over threshold", zap.String("pool", vol.Pool), zap.Float64("data_percent", usage.DataPercent),
		zap.Float64("metadata_percent", usage.MetaPercent), zap.Int("snapshots", len(snaps)), zap.Int("candidates", len(candidates)))
	for len(candidates) > 0 && overThinThreshold(usage) {
		oldest := candidates[0]
		start := time.Now()
		if _, err := sm.controller.deployment.LVRemoveSnapshot(ctx, []string{host}, vol.Pool, oldest.Name); err != nil {
			log.Warn("Snapshot schedule: could not remove a snapshot to free a full pool",
				zap.String("node", node), zap.String("snapshot", oldest.Name), zap.Error(err))
			return
		}
		log.Debug("relieveThinPool: lvremove done", zap.String("host", host), zap.String("snapshot", oldest.Name),
			zap.Duration("took", time.Since(start)))
		log.Warn("Snapshot schedule: removed the oldest snapshot to free a full thin pool",
			zap.String("node", node), zap.String("pool", vol.Pool), zap.String("snapshot", oldest.Name),
			zap.Float64("data_percent", usage.DataPercent), zap.Float64("metadata_percent", usage.MetaPercent))
		sm.publishPoolEvent(event.TypePoolSnapshotsRemoved, event.StatusInfo, event.SeverityWarning, node, vol.Pool, resource,
			fmt.Sprintf("thin pool %s on %s is %.0f%% full: removed snapshot %s of %s to make room. "+
				"A sudden fill is what a volume being encrypted looks like; check before more history goes",
				vol.Pool, node, usage.DataPercent, oldest.Name, resource))
		candidates = candidates[1:]
		if usage, err = sm.controller.storage.readThinUsage(ctx, host, vol.Pool); err != nil || usage == nil {
			log.Debug("relieveThinPool: pool usage not re-read, stopping", zap.String("pool", vol.Pool), zap.Error(err))
			return
		}
	}
	if overThinThreshold(usage) && lock > 0 {
		log.Debug("relieveThinPool: still full with only locked or newest snapshots left", zap.String("pool", vol.Pool),
			zap.Float64("data_percent", usage.DataPercent))
		sm.publishPoolEvent(event.TypePoolSnapshotsLocked, event.StatusInfo, event.SeverityCritical, node, vol.Pool, resource,
			fmt.Sprintf("thin pool %s on %s is %.0f%% full and the snapshots of %s left are locked or the newest; "+
				"extend the pool now, or its replica's writes will fail and DRBD will drop it",
				vol.Pool, node, usage.DataPercent, resource))
		return
	}
	log.Debug("relieveThinPool: done", zap.String("pool", vol.Pool), zap.Bool("still_full", overThinThreshold(usage)))
}

func (sm *ScheduleManager) publishPoolEvent(t event.Type, status event.Status, sev event.Severity, node, pool, resource, msg string) {
	if sm.controller.events == nil {
		ctrlLogOrGlobal(sm.controller).Debug("publishPoolEvent: no event bus, dropped", zap.String("type", string(t)))
		return
	}
	ctrlLogOrGlobal(sm.controller).Debug("publishPoolEvent: publishing", zap.String("type", string(t)),
		zap.String("severity", string(sev)), zap.String("node", node), zap.String("pool", pool), zap.String("resource", resource))
	sm.controller.events.Publish(event.Event{
		Type: t, Severity: sev, Status: status, Resource: pool, Node: node, Message: msg,
		Details: map[string]string{"resource": resource},
	})
}

func overThinThreshold(u *PoolThinInfo) bool {
	return u.OutOfSpace || u.DataPercent >= ThinPoolNearFullPercent || u.MetaPercent >= ThinPoolNearFullPercent
}
