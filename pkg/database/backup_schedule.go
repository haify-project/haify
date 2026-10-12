package database

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// BackupSchedule ships a resource to a target on a cron, and prunes the
// backups it made there per Keep. One schedule per resource and target.
type BackupSchedule struct {
	// Name is BackupScheduleName(Resource, Target).
	Name     string
	Resource string
	Target   string
	Cron     string
	Enabled  bool
	Keep     GFSPolicy
	LastRun  time.Time
	// LastBackup is the id the last run produced; LastError why it produced
	// none. Exactly one of them is set once the schedule has run.
	LastBackup string
	LastError  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// BackupScheduleName is the key of the schedule backing resource up to target.
func BackupScheduleName(resource, target string) string {
	return resource + "@" + target
}

// SaveBackupSchedule saves or updates a backup schedule.
func (db *DB) SaveBackupSchedule(ctx context.Context, s *BackupSchedule) error {
	db.log().Debug("DB.SaveBackupSchedule: saving schedule",
		zap.String("schedule", s.Name), zap.String("resource", s.Resource), zap.String("target", s.Target),
		zap.String("cron", s.Cron), zap.Bool("enabled", s.Enabled),
		zap.String("last_backup", s.LastBackup), zap.Bool("has_last_error", s.LastError != ""))
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	data, err := json.Marshal(s)
	if err != nil {
		db.log().Debug("DB.SaveBackupSchedule: marshal failed", zap.String("schedule", s.Name), zap.Error(err))
		return fmt.Errorf("failed to marshal backup schedule: %w", err)
	}
	start := time.Now()
	err = db.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(backupSchedulesBucket)).Put([]byte(s.Name), data)
	})
	db.log().Debug("DB.SaveBackupSchedule: put", zap.String("bucket", backupSchedulesBucket), zap.String("key", s.Name),
		zap.Int("size_bytes", len(data)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// GetBackupSchedule retrieves a backup schedule by name.
func (db *DB) GetBackupSchedule(ctx context.Context, name string) (*BackupSchedule, error) {
	db.log().Debug("DB.GetBackupSchedule: reading schedule", zap.String("schedule", name))
	db.mu.RLock()
	defer db.mu.RUnlock()

	var s BackupSchedule
	found, size := false, 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte(backupSchedulesBucket)).Get([]byte(name))
		if data == nil {
			return fmt.Errorf("backup schedule %q not found", name)
		}
		found, size = true, len(data)
		return json.Unmarshal(data, &s)
	})
	db.log().Debug("DB.GetBackupSchedule: get", zap.String("bucket", backupSchedulesBucket), zap.String("key", name),
		zap.Bool("found", found), zap.Int("size_bytes", size),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ListBackupSchedules lists all backup schedules, name-ordered.
func (db *DB) ListBackupSchedules(ctx context.Context) ([]*BackupSchedule, error) {
	db.log().Debug("DB.ListBackupSchedules: listing schedules")
	db.mu.RLock()
	defer db.mu.RUnlock()

	schedules := make([]*BackupSchedule, 0)
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(backupSchedulesBucket)).ForEach(func(k, v []byte) error {
			var s BackupSchedule
			if err := json.Unmarshal(v, &s); err != nil {
				db.log().Debug("DB.ListBackupSchedules: decoding entry failed", zap.ByteString("key", k), zap.Error(err))
				return err
			}
			schedules = append(schedules, &s)
			return nil
		})
	})
	db.log().Debug("DB.ListBackupSchedules: scan", zap.String("bucket", backupSchedulesBucket),
		zap.Int("count", len(schedules)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	sort.Slice(schedules, func(i, j int) bool { return schedules[i].Name < schedules[j].Name })
	return schedules, nil
}

// DeleteBackupSchedule deletes a backup schedule by name.
func (db *DB) DeleteBackupSchedule(ctx context.Context, name string) error {
	db.log().Debug("DB.DeleteBackupSchedule: deleting schedule", zap.String("schedule", name))
	db.mu.Lock()
	defer db.mu.Unlock()

	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(backupSchedulesBucket))
		if b.Get([]byte(name)) == nil {
			db.log().Debug("DB.DeleteBackupSchedule: schedule not found", zap.String("bucket", backupSchedulesBucket),
				zap.String("key", name))
			return fmt.Errorf("backup schedule %q not found", name)
		}
		return b.Delete([]byte(name))
	})
	db.log().Debug("DB.DeleteBackupSchedule: delete", zap.String("bucket", backupSchedulesBucket), zap.String("key", name),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}
