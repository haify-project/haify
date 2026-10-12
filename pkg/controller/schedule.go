package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
)

// schedSnapMarker separates a backing volume name from the UTC timestamp in a
// scheduled snapshot's name. Snapshots carrying it are managed by the
// scheduler; manual snapshots are left untouched by retention.
const schedSnapMarker = "_sched_"

// snapTimeLayout is the UTC timestamp embedded in scheduled snapshot names.
// It uses only characters valid in both LVM LV names and ZFS snapshot names.
const snapTimeLayout = "20060102T150405Z"

// buildSnapName names a scheduled snapshot of backingVolume taken at ts, e.g.
// "data_data_sched_20240101T020000Z".
func buildSnapName(backingVolume string, ts time.Time) string {
	return backingVolume + schedSnapMarker + ts.UTC().Format(snapTimeLayout)
}

// parseSnapName splits a snapshot name into its backing volume and timestamp.
// ok is false for names that are not scheduler-managed.
func parseSnapName(name string) (backingVolume string, ts time.Time, ok bool) {
	idx := strings.LastIndex(name, schedSnapMarker)
	if idx < 0 {
		return "", time.Time{}, false
	}
	prefix := name[:idx]
	tsStr := name[idx+len(schedSnapMarker):]
	parsed, err := time.ParseInLocation(snapTimeLayout, tsStr, time.UTC)
	if err != nil || prefix == "" {
		zap.L().Debug("parseSnapName: marker but no valid timestamp", zap.String("snapshot", name), zap.Error(err))
		return "", time.Time{}, false
	}
	return prefix, parsed, true
}

// scheduledSnap is one snapshot considered by retention.
type scheduledSnap struct {
	Name string
	TS   time.Time
}

// gfsHasRetention reports whether a policy keeps anything at all.
func gfsHasRetention(p database.GFSPolicy) bool {
	return p.Hourly > 0 || p.Daily > 0 || p.Weekly > 0 || p.Monthly > 0 || p.Yearly > 0
}

// selectExpiredSnapshots applies a GFS retention policy and returns the
// snapshots that should be deleted. For each time bucket it keeps the newest
// snapshot in each distinct period, up to that bucket's count; a snapshot kept
// by any bucket survives. An all-zero policy is treated as "keep everything"
// so a misconfiguration can never wipe a resource's snapshots.
func selectExpiredSnapshots(snaps []scheduledSnap, policy database.GFSPolicy) []scheduledSnap {
	if !gfsHasRetention(policy) || len(snaps) == 0 {
		zap.L().Debug("selectExpiredSnapshots: nothing to expire", zap.Bool("has_retention", gfsHasRetention(policy)),
			zap.Int("snapshots", len(snaps)))
		return nil
	}

	sorted := append([]scheduledSnap(nil), snaps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TS.After(sorted[j].TS) })

	kept := make(map[string]bool, len(sorted))
	keepBucket := func(count int, keyFn func(time.Time) string) {
		if count <= 0 {
			return
		}
		seen := make(map[string]bool, count)
		for _, s := range sorted {
			k := keyFn(s.TS.UTC())
			if seen[k] {
				continue
			}
			seen[k] = true
			kept[s.Name] = true
			if len(seen) >= count {
				break
			}
		}
	}

	keepBucket(policy.Hourly, func(t time.Time) string { return t.Format("2006010215") })
	keepBucket(policy.Daily, func(t time.Time) string { return t.Format("20060102") })
	keepBucket(policy.Weekly, func(t time.Time) string {
		y, w := t.ISOWeek()
		return fmt.Sprintf("%04d-%02d", y, w)
	})
	keepBucket(policy.Monthly, func(t time.Time) string { return t.Format("200601") })
	keepBucket(policy.Yearly, func(t time.Time) string { return t.Format("2006") })

	var expired []scheduledSnap
	for _, s := range sorted {
		if !kept[s.Name] {
			expired = append(expired, s)
		}
	}
	zap.L().Debug("selectExpiredSnapshots: applied policy", zap.Int("snapshots", len(sorted)), zap.Int("kept", len(kept)),
		zap.Int("expired", len(expired)), zap.Int("hourly", policy.Hourly), zap.Int("daily", policy.Daily),
		zap.Int("weekly", policy.Weekly), zap.Int("monthly", policy.Monthly), zap.Int("yearly", policy.Yearly))
	return expired
}

// ScheduleManager runs cron-driven snapshot and backup schedules on the active
// controller and prunes old snapshots and backups per each schedule's GFS
// retention policy.
type ScheduleManager struct {
	controller *Controller
	mu         sync.Mutex
	cron       *cron.Cron
	started    bool
	// verifying is set while a verify sweep runs, so a slow one is not
	// joined by the next.
	verifying atomic.Bool
}

// NewScheduleManager creates a snapshot schedule manager.
func NewScheduleManager(c *Controller) *ScheduleManager {
	return &ScheduleManager{controller: c}
}

// scheduleLog is the logger the scheduler traces through: the controller's,
// or the global one for a manager built without one (tests build them bare).
func (sm *ScheduleManager) scheduleLog() *zap.Logger {
	if sm != nil && sm.controller != nil && sm.controller.logger != nil {
		return sm.controller.logger
	}
	return zap.L()
}

// Start loads persisted schedules and begins firing them. Safe to call once on
// the active controller; a no-op if already started.
func (sm *ScheduleManager) Start(ctx context.Context) error {
	log := sm.scheduleLog()
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.started {
		log.Debug("ScheduleManager.Start: already started")
		return nil
	}
	if err := sm.rebuildLocked(ctx); err != nil {
		log.Debug("ScheduleManager.Start: rebuild failed", zap.Error(err))
		return err
	}
	sm.cron.Start()
	sm.started = true
	sm.controller.logger.Info("Snapshot scheduler started")
	return nil
}

// Stop halts all scheduled snapshots. Safe to call when not started.
func (sm *ScheduleManager) Stop() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if !sm.started {
		sm.scheduleLog().Debug("ScheduleManager.Stop: not started")
		return
	}
	if sm.cron != nil {
		sm.cron.Stop()
	}
	sm.started = false
	sm.controller.logger.Info("Snapshot scheduler stopped")
}

// CreateSchedule validates and persists a snapshot schedule, then reloads the
// cron so it takes effect immediately. One schedule per resource: the schedule
// name is the resource name.
//
// lockDays, when set, locks the snapshots the schedule takes (snapshot_lock.go);
// nil keeps the lock of the schedule being replaced.
func (sm *ScheduleManager) CreateSchedule(ctx context.Context, resource, cronExpr string, policy database.GFSPolicy, enabled bool, lockDays *int) error {
	log := sm.scheduleLog()
	log.Debug("ScheduleManager.CreateSchedule: start", zap.String("resource", resource), zap.String("cron", cronExpr),
		zap.Bool("enabled", enabled), zap.Bool("lock_given", lockDays != nil))
	if err := validateCron(cronExpr); err != nil {
		return err
	}
	if !gfsHasRetention(policy) {
		log.Debug("ScheduleManager.CreateSchedule: policy keeps nothing", zap.String("resource", resource))
		return fmt.Errorf("retention policy keeps nothing: set at least one of hourly/daily/weekly/monthly/yearly")
	}
	if sm.controller.db == nil {
		log.Debug("ScheduleManager.CreateSchedule: no database")
		return fmt.Errorf("database not available")
	}
	if _, err := sm.controller.db.GetResource(ctx, resource); err != nil {
		log.Debug("ScheduleManager.CreateSchedule: resource not found", zap.String("resource", resource), zap.Error(err))
		return fmt.Errorf("resource %q not found", resource)
	}
	old, oerr := sm.controller.db.GetSnapshotSchedule(ctx, resource)
	lock := 0
	if old != nil {
		lock = old.LockDays
	}
	if lockDays != nil {
		lock = *lockDays
	}
	log.Debug("ScheduleManager.CreateSchedule: existing schedule", zap.String("resource", resource), zap.Bool("replacing", old != nil),
		zap.Int("lock_days", lock), zap.NamedError("lookup_error", oerr))
	if err := checkScheduleLockChange(old, lock, lockNow()); err != nil {
		log.Debug("ScheduleManager.CreateSchedule: lock change refused", zap.String("resource", resource), zap.Error(err))
		return err
	}
	s := &database.SnapshotSchedule{
		Name:     resource,
		Resource: resource,
		Cron:     cronExpr,
		Enabled:  enabled,
		Keep:     policy,
		LockDays: lock,
	}
	if old != nil {
		// The last run is what says how long the newest snapshot stays
		// locked, and a freeze outlives a replaced schedule too.
		s.LastRun, s.CreatedAt = old.LastRun, old.CreatedAt
		s.FrozenUntil, s.FrozenAt, s.FrozenReason = old.FrozenUntil, old.FrozenAt, old.FrozenReason
	}
	err := sm.controller.db.SaveSnapshotSchedule(ctx, s)
	log.Debug("ScheduleManager.CreateSchedule: saved schedule", zap.String("resource", resource), zap.Error(err))
	if err != nil {
		return fmt.Errorf("save schedule: %w", err)
	}
	sm.controller.logger.Info("Created snapshot schedule",
		zap.String("resource", resource), zap.String("cron", cronExpr), zap.Bool("enabled", enabled))
	return sm.reload(ctx)
}

// DeleteSchedule removes a schedule. Existing snapshots are left in place;
// only future scheduled snapshots and pruning stop.
func (sm *ScheduleManager) DeleteSchedule(ctx context.Context, name string) error {
	log := sm.scheduleLog()
	log.Debug("ScheduleManager.DeleteSchedule: start", zap.String("schedule", name))
	if sm.controller.db == nil {
		log.Debug("ScheduleManager.DeleteSchedule: no database")
		return fmt.Errorf("database not available")
	}
	if s, err := sm.controller.db.GetSnapshotSchedule(ctx, name); err == nil && s != nil {
		if until := scheduleLockedUntil(s, lockNow()); !until.IsZero() {
			log.Debug("ScheduleManager.DeleteSchedule: snapshots still locked", zap.String("schedule", name), zap.Time("until", until))
			return fmt.Errorf("the schedule of %s locks its snapshots and the newest stay locked until %s; it can be deleted after that",
				s.Resource, until.UTC().Format(time.RFC3339))
		}
	}
	err := sm.controller.db.DeleteSnapshotSchedule(ctx, name)
	log.Debug("ScheduleManager.DeleteSchedule: deleted schedule", zap.String("schedule", name), zap.Error(err))
	if err != nil {
		return fmt.Errorf("delete schedule: %w", err)
	}
	sm.controller.logger.Info("Deleted snapshot schedule", zap.String("name", name))
	return sm.reload(ctx)
}

// ListSchedules returns all schedules.
func (sm *ScheduleManager) ListSchedules(ctx context.Context) ([]*database.SnapshotSchedule, error) {
	log := sm.scheduleLog()
	if sm.controller.db == nil {
		log.Debug("ScheduleManager.ListSchedules: no database")
		return nil, fmt.Errorf("database not available")
	}
	schedules, err := sm.controller.db.ListSnapshotSchedules(ctx)
	log.Debug("ScheduleManager.ListSchedules: listed", zap.Int("count", len(schedules)), zap.Error(err))
	return schedules, err
}

// NextRun returns the next fire time of a cron expression after now, or the
// zero time if the expression is invalid.
func NextRun(cronExpr string, now time.Time) time.Time {
	sched, err := cron.ParseStandard(cronExpr)
	if err != nil {
		zap.L().Debug("NextRun: invalid cron", zap.String("cron", cronExpr), zap.Error(err))
		return time.Time{}
	}
	return sched.Next(now)
}

// validateCron checks a standard 5-field cron expression.
func validateCron(cronExpr string) error {
	if _, err := cron.ParseStandard(cronExpr); err != nil {
		zap.L().Debug("validateCron: invalid cron", zap.String("cron", cronExpr), zap.Error(err))
		return fmt.Errorf("invalid cron expression %q: %w", cronExpr, err)
	}
	return nil
}

// reload rebuilds cron entries from the database. Called after any schedule
// change so edits take effect without a controller restart.
func (sm *ScheduleManager) reload(ctx context.Context) error {
	log := sm.scheduleLog()
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if !sm.started {
		log.Debug("ScheduleManager.reload: not started; nothing to reload")
		return nil
	}
	old := sm.cron
	if err := sm.rebuildLocked(ctx); err != nil {
		log.Debug("ScheduleManager.reload: rebuild failed", zap.Error(err))
		return err
	}
	sm.cron.Start()
	if old != nil {
		old.Stop()
	}
	log.Debug("ScheduleManager.reload: cron replaced", zap.Int("entries", len(sm.cron.Entries())))
	return nil
}

// rebuildLocked constructs a fresh (stopped) cron populated with every enabled
// schedule. The caller holds sm.mu and starts the cron.
func (sm *ScheduleManager) rebuildLocked(ctx context.Context) error {
	log := sm.scheduleLog()
	c := cron.New()
	schedules, err := sm.controller.db.ListSnapshotSchedules(ctx)
	log.Debug("ScheduleManager.rebuildLocked: loaded snapshot schedules", zap.Int("count", len(schedules)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("load snapshot schedules: %w", err)
	}
	for _, s := range schedules {
		if !s.Enabled {
			log.Debug("ScheduleManager.rebuildLocked: schedule disabled", zap.String("schedule", s.Name))
			continue
		}
		name := s.Name
		if _, err := c.AddFunc(s.Cron, func() { sm.runSchedule(name) }); err != nil {
			sm.controller.logger.Warn("Skipping snapshot schedule with invalid cron",
				zap.String("schedule", name),
				zap.String("cron", s.Cron),
				zap.Error(err))
		} else {
			log.Debug("ScheduleManager.rebuildLocked: scheduled", zap.String("schedule", name), zap.String("cron", s.Cron))
		}
	}
	if err := sm.addBackupSchedules(ctx, c); err != nil {
		log.Debug("ScheduleManager.rebuildLocked: backup schedules failed", zap.Error(err))
		return err
	}
	if spec := sm.verifySchedule(); spec != "" {
		if _, err := c.AddFunc(spec, sm.runVerifySweep); err != nil {
			sm.controller.logger.Warn("Skipping the verify schedule: invalid cron",
				zap.String("cron", spec), zap.Error(err))
		}
	}
	if spec := sm.trimSchedule(); spec != "" {
		if _, err := c.AddFunc(spec, sm.runTrimSweep); err != nil {
			sm.controller.logger.Warn("Skipping the trim schedule: invalid cron", zap.String("cron", spec), zap.Error(err))
		}
	}
	if spec := sm.inspectSchedule(); spec != "" {
		if _, err := c.AddFunc(spec, sm.runInspectionTick); err != nil {
			sm.controller.logger.Warn("Skipping the inspection schedule: invalid cron",
				zap.String("cron", spec), zap.Error(err))
		}
	}
	sm.cron = c
	log.Debug("ScheduleManager.rebuildLocked: built cron", zap.Int("entries", len(c.Entries())))
	return nil
}

func (sm *ScheduleManager) verifySchedule() string {
	if sm.controller.config == nil {
		return ""
	}
	spec := strings.TrimSpace(sm.controller.config.Storage.VerifySchedule)
	sm.scheduleLog().Debug("ScheduleManager.verifySchedule: verify cron", zap.String("cron", spec))
	return spec
}
