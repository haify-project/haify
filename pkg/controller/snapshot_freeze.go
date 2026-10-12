package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
)

// Freezing a snapshot schedule.
//
// When a volume is being rewritten wholesale — what encryption looks like —
// the history worth having is the snapshots from before it started, and the
// schedule's own retention is what would delete them: the GFS policy ages
// them out, and a thin pool filling with the rewritten blocks makes the
// scheduler remove the oldest. A freeze stops both. A frozen schedule keeps
// taking snapshots and removes none, and until the freeze ends every
// scheduled snapshot of the resource is locked against deletion through haify
// and the schedule and resource cannot be deleted (snapshot_lock.go).
//
// The write-anomaly detector freezes a schedule when it fires
// (write_anomaly.go); an operator can freeze one by hand. Ending a freeze
// early is a weakening of protection, and is on the two-person approval
// list.

// defaultFreeze is how long a freeze lasts when nobody says.
const defaultFreeze = 7 * 24 * time.Hour

// scheduleFrozenUntil is when s's freeze ends, or zero when it is not frozen.
func scheduleFrozenUntil(s *database.SnapshotSchedule, now time.Time) time.Time {
	if s == nil || !s.FrozenUntil.After(now) {
		return time.Time{}
	}
	return s.FrozenUntil
}

// resourceFrozenUntil is when resource's schedule freeze ends, and why it
// froze; zero when it is not frozen.
func (c *Controller) resourceFrozenUntil(ctx context.Context, resource string) (time.Time, string) {
	log := ctrlLogOrGlobal(c)
	if c.db == nil || resource == "" {
		log.Debug("resourceFrozenUntil: no database or resource, not frozen", zap.String("resource", resource))
		return time.Time{}, ""
	}
	s, err := c.db.GetSnapshotSchedule(ctx, resource)
	if err != nil || s == nil {
		log.Debug("resourceFrozenUntil: no schedule, not frozen", zap.String("resource", resource), zap.Error(err))
		return time.Time{}, ""
	}
	until := scheduleFrozenUntil(s, lockNow())
	log.Debug("resourceFrozenUntil: checked", zap.String("resource", resource), zap.Time("until", until),
		zap.String("reason", s.FrozenReason))
	return until, s.FrozenReason
}

// FreezeSchedule freezes resource's schedule for d (defaultFreeze when zero).
// A freeze is only ever extended here, never shortened.
func (sm *ScheduleManager) FreezeSchedule(ctx context.Context, resource string, d time.Duration, reason string) (time.Time, error) {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("FreezeSchedule: freezing", zap.String("resource", resource), zap.Duration("duration", d), zap.String("reason", reason))
	if sm.controller.db == nil {
		log.Debug("FreezeSchedule: no database")
		return time.Time{}, fmt.Errorf("database not available")
	}
	if d <= 0 {
		log.Debug("FreezeSchedule: no duration, using the default", zap.Duration("duration", defaultFreeze))
		d = defaultFreeze
	}
	if d > time.Duration(maxSnapshotLockDays)*24*time.Hour {
		log.Debug("FreezeSchedule: too long", zap.Duration("duration", d), zap.Int("max_days", maxSnapshotLockDays))
		return time.Time{}, fmt.Errorf("a freeze lasts at most %d days", maxSnapshotLockDays)
	}
	s, err := sm.controller.db.GetSnapshotSchedule(ctx, resource)
	if err != nil || s == nil {
		log.Debug("FreezeSchedule: no schedule", zap.String("resource", resource), zap.Error(err))
		return time.Time{}, fmt.Errorf("resource %s has no snapshot schedule to freeze", resource)
	}
	now := lockNow()
	until := now.Add(d)
	if s.FrozenUntil.After(until) {
		log.Debug("FreezeSchedule: already frozen longer, keeping the later end", zap.Time("until", s.FrozenUntil))
		until = s.FrozenUntil
	}
	if s.FrozenUntil.Before(now) {
		log.Debug("FreezeSchedule: new freeze, recording its start", zap.Time("frozen_at", now))
		s.FrozenAt = now
	}
	s.FrozenUntil, s.FrozenReason = until, reason
	if err := sm.controller.db.SaveSnapshotSchedule(ctx, s); err != nil {
		log.Debug("FreezeSchedule: save failed", zap.String("resource", resource), zap.Error(err))
		return time.Time{}, fmt.Errorf("save schedule: %w", err)
	}
	sm.controller.logger.Warn("Snapshot schedule frozen", zap.String("resource", resource),
		zap.Time("until", until), zap.String("reason", reason))
	return until, nil
}

// UnfreezeSchedule ends resource's freeze now.
func (sm *ScheduleManager) UnfreezeSchedule(ctx context.Context, resource string) error {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("UnfreezeSchedule: unfreezing", zap.String("resource", resource))
	if sm.controller.db == nil {
		log.Debug("UnfreezeSchedule: no database")
		return fmt.Errorf("database not available")
	}
	s, err := sm.controller.db.GetSnapshotSchedule(ctx, resource)
	if err != nil || s == nil {
		log.Debug("UnfreezeSchedule: no schedule", zap.String("resource", resource), zap.Error(err))
		return fmt.Errorf("resource %s has no snapshot schedule", resource)
	}
	if scheduleFrozenUntil(s, lockNow()).IsZero() {
		log.Debug("UnfreezeSchedule: not frozen", zap.String("resource", resource))
		return fmt.Errorf("the snapshot schedule of %s is not frozen", resource)
	}
	s.FrozenUntil, s.FrozenReason = time.Time{}, ""
	if err := sm.controller.db.SaveSnapshotSchedule(ctx, s); err != nil {
		log.Debug("UnfreezeSchedule: save failed", zap.String("resource", resource), zap.Error(err))
		return fmt.Errorf("save schedule: %w", err)
	}
	sm.controller.logger.Warn("Snapshot schedule unfrozen", zap.String("resource", resource))
	return nil
}

// snapshotResource snapshots every volume of res on every diskful node at ts,
// holding the ZFS ones when the schedule locks.
func (sm *ScheduleManager) snapshotResource(ctx context.Context, res *ResourceInfo, s *database.SnapshotSchedule, ts time.Time) {
	log := ctrlLogOrGlobal(sm.controller)
	locked := s.LockDays > 0 || !scheduleFrozenUntil(s, lockNow()).IsZero()
	log.Debug("snapshotResource: snapshotting", zap.String("resource", res.Name), zap.Strings("nodes", res.Nodes),
		zap.Int("volumes", len(res.Volumes)), zap.Time("ts", ts), zap.Bool("locked", locked))
	for _, node := range res.Nodes {
		host := sm.controller.ResolveHost(node)
		for _, vol := range res.Volumes {
			log.Debug("snapshotResource: volume", zap.String("node", node), zap.String("host", host),
				zap.String("pool", vol.Pool), zap.String("volume", vol.BackingVolume))
			sm.snapshotVolume(ctx, host, node, vol, ts)
			if (s.LockDays > 0 || !scheduleFrozenUntil(s, lockNow()).IsZero()) && isZFSDevice(vol.Device) {
				sm.controller.holdLockedZFSSnapshot(ctx, host,
					fmt.Sprintf("%s/%s@%s", vol.Pool, vol.BackingVolume, buildSnapName(vol.BackingVolume, ts)))
			}
		}
	}
}

// SnapshotNow takes one scheduled snapshot of resource immediately, outside
// its cron, and records nothing else: no pruning, no last-run update.
func (sm *ScheduleManager) SnapshotNow(ctx context.Context, resource string) error {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("SnapshotNow: snapshotting now", zap.String("resource", resource))
	if sm.controller.db == nil {
		log.Debug("SnapshotNow: no database")
		return fmt.Errorf("database not available")
	}
	s, err := sm.controller.db.GetSnapshotSchedule(ctx, resource)
	if err != nil || s == nil {
		log.Debug("SnapshotNow: no schedule", zap.String("resource", resource), zap.Error(err))
		return fmt.Errorf("resource %s has no snapshot schedule", resource)
	}
	res, err := sm.controller.resources.GetResource(ctx, resource)
	if err != nil {
		log.Debug("SnapshotNow: resource not read", zap.String("resource", resource), zap.Error(err))
		return err
	}
	sm.snapshotResource(ctx, res, s, time.Now().UTC())
	log.Debug("SnapshotNow: done", zap.String("resource", resource))
	return nil
}
