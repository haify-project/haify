package database

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// A live move of an HA guest (`ha evict --live`). While one runs, the
// resource's promoter is disabled on every replica and DRBD allows two
// Primaries; a controller that restarts in the middle has to know that, or
// the guest is left with nothing watching it. The record is written before
// the first change on a node and is what a starting controller picks up.

const haMigrationsBucket = "ha_migrations"

// HaMigration is the last live move of one resource's guest.
type HaMigration struct {
	Resource   string
	Domain     string
	SourceNode string
	TargetNode string
	// AgentUnit is the systemd unit that runs the guest's OCF agent on the
	// source node, read before the promoter was disabled.
	AgentUnit string `json:",omitempty"`
	// Phase, Message, Outcome, ActiveNode and Steps are what the API shows.
	Phase      string
	Message    string
	Outcome    string   `json:",omitempty"`
	ActiveNode string   `json:",omitempty"`
	Steps      []string `json:",omitempty"`
	Progress   int      `json:",omitempty"`
	Done       bool
	// Deadline is when the memory copy is given up.
	Deadline  time.Time
	StartedAt time.Time
	UpdatedAt time.Time
}

// SaveHaMigration creates or replaces a resource's migration record.
func (db *DB) SaveHaMigration(ctx context.Context, m *HaMigration) error {
	if m == nil || m.Resource == "" {
		db.log().Debug("DB.SaveHaMigration: refusing a record without a resource", zap.Bool("nil_record", m == nil))
		return fmt.Errorf("resource is required")
	}
	db.log().Debug("DB.SaveHaMigration: saving", zap.String("resource", m.Resource), zap.String("phase", m.Phase),
		zap.String("source_node", m.SourceNode), zap.String("target_node", m.TargetNode), zap.Bool("done", m.Done),
		zap.String("outcome", m.Outcome))
	db.mu.Lock()
	defer db.mu.Unlock()

	m.UpdatedAt = time.Now()
	if m.StartedAt.IsZero() {
		m.StartedAt = m.UpdatedAt
	}
	data, err := json.Marshal(m)
	if err != nil {
		db.log().Debug("DB.SaveHaMigration: marshal failed", zap.String("resource", m.Resource), zap.Error(err))
		return fmt.Errorf("failed to marshal the migration record: %w", err)
	}
	start := time.Now()
	err = db.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(haMigrationsBucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(m.Resource), data)
	})
	db.log().Debug("DB.SaveHaMigration: put", zap.String("bucket", haMigrationsBucket), zap.String("key", m.Resource),
		zap.Int("size_bytes", len(data)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// GetHaMigration returns the migration record of resource, or nil.
func (db *DB) GetHaMigration(ctx context.Context, resource string) (*HaMigration, error) {
	db.log().Debug("DB.GetHaMigration: reading", zap.String("resource", resource))
	db.mu.RLock()
	defer db.mu.RUnlock()

	var m *HaMigration
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(haMigrationsBucket))
		if b == nil {
			return nil
		}
		data := b.Get([]byte(resource))
		if data == nil {
			return nil
		}
		m = &HaMigration{}
		return json.Unmarshal(data, m)
	})
	db.log().Debug("DB.GetHaMigration: get", zap.String("bucket", haMigrationsBucket), zap.String("key", resource),
		zap.Bool("found", m != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("failed to read the migration record of %s: %w", resource, err)
	}
	return m, nil
}

// ListHaMigrations returns every migration record.
func (db *DB) ListHaMigrations(ctx context.Context) ([]*HaMigration, error) {
	db.log().Debug("DB.ListHaMigrations: listing")
	db.mu.RLock()
	defer db.mu.RUnlock()

	var out []*HaMigration
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(haMigrationsBucket))
		if b == nil {
			db.log().Debug("DB.ListHaMigrations: bucket absent", zap.String("bucket", haMigrationsBucket))
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var m HaMigration
			if err := json.Unmarshal(v, &m); err != nil {
				return fmt.Errorf("migration record %s: %w", k, err)
			}
			out = append(out, &m)
			return nil
		})
	})
	db.log().Debug("DB.ListHaMigrations: scan", zap.String("bucket", haMigrationsBucket), zap.Int("count", len(out)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return out, err
}

// DeleteHaMigration removes a resource's migration record.
func (db *DB) DeleteHaMigration(ctx context.Context, resource string) error {
	db.log().Debug("DB.DeleteHaMigration: deleting", zap.String("resource", resource))
	db.mu.Lock()
	defer db.mu.Unlock()
	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(haMigrationsBucket))
		if b == nil {
			return nil
		}
		return b.Delete([]byte(resource))
	})
	db.log().Debug("DB.DeleteHaMigration: delete", zap.String("bucket", haMigrationsBucket), zap.String("key", resource),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}
