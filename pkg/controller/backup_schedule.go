package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/event"
)

// Scheduled backups. They ride the snapshot scheduler's cron, so they run only
// on the active controller and move with it on a Self-HA failover; a backup
// cut off by that failover is marked failed on startup (ReconcileInterrupted)
// and the next tick takes a fresh one.
//
// Retention is the snapshot GFS policy applied to the schedule's completed
// backups, with one rule on top: an incremental is useless without every
// backup down to its full one, so whatever the policy keeps, its whole chain
// is kept too. That is why a daily schedule keeping 7 can hold up to a month
// of backups — the full one at the start of the chain stays until no kept
// incremental is built on it.

// scheduledBackupTimeout bounds one scheduled run, upload included.
const scheduledBackupTimeout = 24 * time.Hour

// backupRuns guards against a slow run being joined by the next tick: a
// backup of a large volume can outlast an hourly cron.
var backupRuns sync.Map

// CreateBackupSchedule validates and stores a schedule, replacing any earlier
// one for the same resource and target, and reloads the cron.
func (sm *ScheduleManager) CreateBackupSchedule(ctx context.Context, resource, target, cronExpr string,
	policy database.GFSPolicy, enabled bool) (*database.BackupSchedule, error) {
	log := sm.controller.traceLogger()
	log.Debug("ScheduleManager.CreateBackupSchedule: start", zap.String("resource", resource), zap.String("target", target),
		zap.String("cron", cronExpr), zap.Bool("enabled", enabled))
	if err := validateCron(cronExpr); err != nil {
		log.Debug("ScheduleManager.CreateBackupSchedule: invalid cron", zap.String("cron", cronExpr), zap.Error(err))
		return nil, err
	}
	if !gfsHasRetention(policy) {
		log.Debug("ScheduleManager.CreateBackupSchedule: policy keeps nothing")
		return nil, fmt.Errorf("retention policy keeps nothing: set at least one of hourly/daily/weekly/monthly/yearly")
	}
	db := sm.controller.db
	if db == nil {
		log.Debug("ScheduleManager.CreateBackupSchedule: no database")
		return nil, fmt.Errorf("database not available")
	}
	if _, err := db.GetResource(ctx, resource); err != nil {
		log.Debug("ScheduleManager.CreateBackupSchedule: resource not found", zap.String("resource", resource), zap.Error(err))
		return nil, fmt.Errorf("resource %q not found", resource)
	}
	if _, err := db.GetBackupTarget(ctx, target); err != nil {
		log.Debug("ScheduleManager.CreateBackupSchedule: target not found", zap.String("target", target), zap.Error(err))
		return nil, err
	}
	s := &database.BackupSchedule{
		Name:     database.BackupScheduleName(resource, target),
		Resource: resource, Target: target, Cron: cronExpr, Enabled: enabled, Keep: policy,
	}
	if old, err := db.GetBackupSchedule(ctx, s.Name); err == nil {
		log.Debug("ScheduleManager.CreateBackupSchedule: replacing existing schedule, keeping its history", zap.String("schedule", s.Name))
		s.CreatedAt, s.LastRun, s.LastBackup, s.LastError = old.CreatedAt, old.LastRun, old.LastBackup, old.LastError
	}
	start := time.Now()
	if err := db.SaveBackupSchedule(ctx, s); err != nil {
		log.Debug("ScheduleManager.CreateBackupSchedule: db save failed", zap.String("schedule", s.Name),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, fmt.Errorf("save backup schedule: %w", err)
	}
	log.Debug("ScheduleManager.CreateBackupSchedule: db saved", zap.String("schedule", s.Name), zap.Duration("took", time.Since(start)))
	sm.controller.logger.Info("Saved backup schedule",
		zap.String("schedule", s.Name), zap.String("cron", cronExpr), zap.Bool("enabled", enabled))
	return s, sm.reload(ctx)
}

// DeleteBackupSchedule stops a schedule. The backups it made stay.
func (sm *ScheduleManager) DeleteBackupSchedule(ctx context.Context, name string) error {
	log := sm.controller.traceLogger()
	if sm.controller.db == nil {
		log.Debug("ScheduleManager.DeleteBackupSchedule: no database")
		return fmt.Errorf("database not available")
	}
	start := time.Now()
	if err := sm.controller.db.DeleteBackupSchedule(ctx, name); err != nil {
		log.Debug("ScheduleManager.DeleteBackupSchedule: db delete failed", zap.String("schedule", name),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return err
	}
	log.Debug("ScheduleManager.DeleteBackupSchedule: db deleted", zap.String("schedule", name), zap.Duration("took", time.Since(start)))
	sm.controller.logger.Info("Deleted backup schedule", zap.String("schedule", name))
	return sm.reload(ctx)
}

// ListBackupSchedules returns every backup schedule.
func (sm *ScheduleManager) ListBackupSchedules(ctx context.Context) ([]*database.BackupSchedule, error) {
	log := sm.controller.traceLogger()
	if sm.controller.db == nil {
		log.Debug("ScheduleManager.ListBackupSchedules: no database")
		return nil, fmt.Errorf("database not available")
	}
	start := time.Now()
	out, err := sm.controller.db.ListBackupSchedules(ctx)
	log.Debug("ScheduleManager.ListBackupSchedules: db list", zap.Int("count", len(out)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return out, err
}

// addBackupSchedules registers every enabled backup schedule on c.
func (sm *ScheduleManager) addBackupSchedules(ctx context.Context, c *cron.Cron) error {
	log := sm.controller.traceLogger()
	start := time.Now()
	schedules, err := sm.controller.db.ListBackupSchedules(ctx)
	log.Debug("ScheduleManager.addBackupSchedules: db list", zap.Int("count", len(schedules)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("load backup schedules: %w", err)
	}
	for _, s := range schedules {
		if !s.Enabled {
			log.Debug("ScheduleManager.addBackupSchedules: schedule disabled, skipped", zap.String("schedule", s.Name))
			continue
		}
		name := s.Name
		if _, err := c.AddFunc(s.Cron, func() { sm.runBackupTick(name) }); err != nil {
			sm.controller.logger.Warn("Skipping backup schedule with invalid cron",
				zap.String("schedule", name), zap.String("cron", s.Cron), zap.Error(err))
			continue
		}
		log.Debug("ScheduleManager.addBackupSchedules: schedule registered", zap.String("schedule", name), zap.String("cron", s.Cron))
	}
	return nil
}

// RunBackupSchedule takes one backup for a schedule, then prunes. It returns
// the schedule as it stands after the run, carrying the run's outcome. A run
// still going when the next tick fires makes that tick a no-op.
func (sm *ScheduleManager) RunBackupSchedule(ctx context.Context, name string) (*database.BackupSchedule, error) {
	trace := sm.controller.traceLogger()
	if _, busy := backupRuns.LoadOrStore(name, true); busy {
		trace.Debug("ScheduleManager.RunBackupSchedule: already running, skipped", zap.String("schedule", name))
		return nil, fmt.Errorf("backup schedule %q is already running", name)
	}
	defer backupRuns.Delete(name)

	db, log := sm.controller.db, sm.controller.logger
	if db == nil {
		trace.Debug("ScheduleManager.RunBackupSchedule: no database")
		return nil, fmt.Errorf("database not available")
	}
	start := time.Now()
	s, err := db.GetBackupSchedule(ctx, name)
	trace.Debug("ScheduleManager.RunBackupSchedule: db get schedule", zap.String("schedule", name),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}

	log.Info("Running backup schedule", zap.String("schedule", name))
	rec, runErr := sm.controller.backups.createBackup(ctx, s.Resource, s.Target, "", false, s.Name, "")
	failedBefore := s.LastError != ""
	s.LastRun = time.Now()
	trace.Debug("ScheduleManager.RunBackupSchedule: backup run finished", zap.String("schedule", name),
		zap.Bool("failed_before", failedBefore), zap.Error(runErr))
	if runErr != nil {
		s.LastBackup, s.LastError = "", runErr.Error()
		sm.publishBackupEvent(s, event.StatusFiring, event.SeverityWarning,
			fmt.Sprintf("scheduled backup of %s to %s failed: %v", s.Resource, s.Target, runErr))
	} else {
		s.LastBackup, s.LastError = rec.ID, ""
		if failedBefore {
			sm.publishBackupEvent(s, event.StatusResolved, event.SeverityInfo,
				fmt.Sprintf("scheduled backup of %s to %s completed again (%s)", s.Resource, s.Target, rec.ID))
		}
	}
	if err := db.SaveBackupSchedule(context.WithoutCancel(ctx), s); err != nil {
		log.Warn("Backup schedule: failed to record the run", zap.String("schedule", name), zap.Error(err))
	}
	if runErr == nil {
		trace.Debug("ScheduleManager.RunBackupSchedule: pruning", zap.String("schedule", name))
		sm.pruneBackups(ctx, s)
	}
	return s, nil
}

// runBackupTick is the cron entry for a schedule.
func (sm *ScheduleManager) runBackupTick(name string) {
	log := sm.controller.traceLogger()
	log.Debug("ScheduleManager.runBackupTick: tick", zap.String("schedule", name))
	ctx, cancel := context.WithTimeout(context.Background(), scheduledBackupTimeout)
	defer cancel()
	if s, err := sm.controller.db.GetBackupSchedule(ctx, name); err != nil || !s.Enabled {
		log.Debug("ScheduleManager.runBackupTick: schedule gone or disabled, skipping", zap.String("schedule", name), zap.Error(err))
		return
	}
	if _, err := sm.RunBackupSchedule(ctx, name); err != nil {
		sm.controller.logger.Warn("Backup schedule: tick skipped", zap.String("schedule", name), zap.Error(err))
	}
}

func (sm *ScheduleManager) publishBackupEvent(s *database.BackupSchedule, status event.Status, sev event.Severity, msg string) {
	if sm.controller.events == nil {
		sm.controller.traceLogger().Debug("ScheduleManager.publishBackupEvent: notifications off, not published",
			zap.String("schedule", s.Name))
		return
	}
	sm.controller.traceLogger().Debug("ScheduleManager.publishBackupEvent: publishing", zap.String("schedule", s.Name),
		zap.String("status", string(status)), zap.String("severity", string(sev)))
	sm.controller.events.Publish(event.Event{
		Type: event.TypeBackupFailed, Severity: sev, Status: status,
		Resource: s.Resource, Message: msg,
		Details: map[string]string{"target": s.Target, "schedule": s.Name},
	})
}

// pruneBackups deletes the schedule's own backups its policy no longer keeps,
// newest first so no backup is deleted while a later one depends on it, and
// drops its failed records older than the newest completed backup: their
// objects were removed when they failed, and the success after them
// supersedes them. Backups taken by hand, or by another schedule, are never
// touched, though a kept backup of this schedule still keeps them when it is
// built on them.
func (sm *ScheduleManager) pruneBackups(ctx context.Context, s *database.BackupSchedule) {
	log := sm.controller.logger
	trace := sm.controller.traceLogger()
	start := time.Now()
	backups, err := sm.controller.db.ListBackups(ctx, s.Resource, s.Target)
	trace.Debug("ScheduleManager.pruneBackups: db list backups", zap.String("schedule", s.Name),
		zap.Int("count", len(backups)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		log.Warn("Backup schedule: list backups failed", zap.String("schedule", s.Name), zap.Error(err))
		return
	}
	var newest time.Time
	for _, b := range backups {
		if b.State == database.BackupStateCompleted && b.StartedAt.After(newest) {
			newest = b.StartedAt
		}
	}
	doomed := selectExpiredBackups(backups, s.Keep, s.Name)
	for _, b := range backups {
		if b.State == database.BackupStateFailed && b.Schedule == s.Name && b.StartedAt.Before(newest) {
			trace.Debug("ScheduleManager.pruneBackups: superseded failed record", zap.String("backup", b.ID))
			doomed = append(doomed, b)
		}
	}
	trace.Debug("ScheduleManager.pruneBackups: selected for deletion", zap.String("schedule", s.Name), zap.Int("count", len(doomed)))
	now := lockNow()
	for _, b := range doomed {
		// A locked backup cannot go yet; a later run prunes it once the lock
		// has expired.
		if lockedNow(b, now) {
			trace.Debug("ScheduleManager.pruneBackups: still locked, kept for now", zap.String("backup", b.ID),
				zap.Time("retain_until", b.RetainUntil))
			continue
		}
		if err := sm.controller.backups.DeleteBackup(ctx, b.ID, "", false); err != nil {
			log.Warn("Backup schedule: prune failed", zap.String("backup", b.ID), zap.Error(err))
			continue
		}
		log.Info("Backup schedule: pruned backup", zap.String("schedule", s.Name), zap.String("backup", b.ID))
	}
}

// selectExpiredBackups returns the completed backups of schedule that policy
// does not keep, newest first. GFS counts only the schedule's own backups; a
// backup is kept when GFS selects it or when a kept backup is built on it,
// directly or further down its chain, whoever took it. Running and failed
// records are never selected: one is in flight and the other holds no data.
func selectExpiredBackups(backups []*database.Backup, policy database.GFSPolicy, schedule string) []*database.Backup {
	byID := make(map[string]*database.Backup, len(backups))
	var snaps []scheduledSnap
	for _, b := range backups {
		if b.State != database.BackupStateCompleted {
			continue
		}
		byID[b.ID] = b
		if b.Schedule == schedule {
			snaps = append(snaps, scheduledSnap{Name: b.ID, TS: b.StartedAt})
		}
	}
	candidates := selectExpiredSnapshots(snaps, policy)
	expired := make(map[string]bool, len(candidates))
	for _, e := range candidates {
		expired[e.Name] = true
	}
	for id, b := range byID {
		if expired[id] {
			continue
		}
		for cur := b; cur.Kind == database.BackupKindIncremental; {
			parent := byID[cur.Parent]
			if parent == nil {
				break
			}
			if expired[parent.ID] {
				zap.L().Debug("selectExpiredBackups: kept as the base of a kept backup", zap.String("backup", parent.ID),
					zap.String("kept_by", id))
			}
			delete(expired, parent.ID)
			cur = parent
		}
	}
	var out []*database.Backup
	for _, c := range candidates {
		if expired[c.Name] {
			out = append(out, byID[c.Name])
		}
	}
	zap.L().Debug("selectExpiredBackups: decided", zap.String("schedule", schedule), zap.Int("completed", len(byID)),
		zap.Int("own", len(snaps)), zap.Int("candidates", len(candidates)), zap.Int("expired", len(out)))
	return out
}
