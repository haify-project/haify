package database

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// ==================== SNAPSHOT SCHEDULE ====================

// GFSPolicy is a grandfather-father-son retention policy: how many snapshots
// to keep in each time bucket. The most recent snapshot in each distinct
// hour/day/week/month/year is retained, up to the count for that bucket; a
// snapshot kept by any bucket survives. Zero in a field disables that bucket.
type GFSPolicy struct {
	Hourly  int
	Daily   int
	Weekly  int
	Monthly int
	Yearly  int
}

// SnapshotSchedule is a cron-driven snapshot policy for one resource. The
// active controller snapshots the resource's volumes on every diskful node on
// each cron tick, then prunes old scheduled snapshots per Keep. One schedule
// per resource (Name is the resource name).
type SnapshotSchedule struct {
	Name     string // unique; equals the target resource name
	Resource string
	Cron     string
	Enabled  bool
	Keep     GFSPolicy
	// LockDays locks every snapshot the schedule takes for that many days:
	// nothing haify does deletes it before then (pkg/controller/snapshot_lock.go).
	LockDays int `json:",omitempty"`
	// FrozenUntil, when in the future, freezes the schedule: it still takes
	// snapshots, but prunes none, and every scheduled snapshot of the
	// resource is locked until then (pkg/controller/snapshot_freeze.go).
	FrozenUntil  time.Time `json:",omitempty"`
	FrozenAt     time.Time `json:",omitempty"`
	FrozenReason string    `json:",omitempty"`
	LastRun      time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// SaveSnapshotSchedule saves or updates a snapshot schedule
func (db *DB) SaveSnapshotSchedule(ctx context.Context, s *SnapshotSchedule) error {
	db.log().Debug("DB.SaveSnapshotSchedule: saving schedule",
		zap.String("schedule", s.Name), zap.String("resource", s.Resource), zap.String("cron", s.Cron),
		zap.Bool("enabled", s.Enabled), zap.Int("lock_days", s.LockDays), zap.Time("frozen_until", s.FrozenUntil))
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now

	data, err := json.Marshal(s)
	if err != nil {
		db.log().Debug("DB.SaveSnapshotSchedule: marshal failed", zap.String("schedule", s.Name), zap.Error(err))
		return fmt.Errorf("failed to marshal snapshot schedule: %w", err)
	}

	start := time.Now()
	err = db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(schedulesBucket))
		return b.Put([]byte(s.Name), data)
	})
	db.log().Debug("DB.SaveSnapshotSchedule: put", zap.String("bucket", schedulesBucket), zap.String("key", s.Name),
		zap.Int("size_bytes", len(data)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// GetSnapshotSchedule retrieves a snapshot schedule by name
func (db *DB) GetSnapshotSchedule(ctx context.Context, name string) (*SnapshotSchedule, error) {
	db.log().Debug("DB.GetSnapshotSchedule: reading schedule", zap.String("schedule", name))
	db.mu.RLock()
	defer db.mu.RUnlock()

	var s SnapshotSchedule
	found, size := false, 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(schedulesBucket))
		data := b.Get([]byte(name))
		if data == nil {
			return fmt.Errorf("snapshot schedule not found")
		}
		found, size = true, len(data)
		return json.Unmarshal(data, &s)
	})
	db.log().Debug("DB.GetSnapshotSchedule: get", zap.String("bucket", schedulesBucket), zap.String("key", name),
		zap.Bool("found", found), zap.Int("size_bytes", size),
		zap.Duration("took", time.Since(start)), zap.Error(err))

	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ListSnapshotSchedules lists all snapshot schedules
func (db *DB) ListSnapshotSchedules(ctx context.Context) ([]*SnapshotSchedule, error) {
	db.log().Debug("DB.ListSnapshotSchedules: listing schedules")
	db.mu.RLock()
	defer db.mu.RUnlock()

	var schedules []*SnapshotSchedule
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(schedulesBucket))
		return b.ForEach(func(k, v []byte) error {
			var s SnapshotSchedule
			if err := json.Unmarshal(v, &s); err != nil {
				db.log().Debug("DB.ListSnapshotSchedules: decoding entry failed", zap.ByteString("key", k), zap.Error(err))
				return err
			}
			schedules = append(schedules, &s)
			return nil
		})
	})
	db.log().Debug("DB.ListSnapshotSchedules: scan", zap.String("bucket", schedulesBucket),
		zap.Int("count", len(schedules)), zap.Duration("took", time.Since(start)), zap.Error(err))

	return schedules, err
}

// DeleteSnapshotSchedule deletes a snapshot schedule by name
func (db *DB) DeleteSnapshotSchedule(ctx context.Context, name string) error {
	db.log().Debug("DB.DeleteSnapshotSchedule: deleting schedule", zap.String("schedule", name))
	db.mu.Lock()
	defer db.mu.Unlock()

	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(schedulesBucket))
		return b.Delete([]byte(name))
	})
	db.log().Debug("DB.DeleteSnapshotSchedule: delete", zap.String("bucket", schedulesBucket), zap.String("key", name),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}
