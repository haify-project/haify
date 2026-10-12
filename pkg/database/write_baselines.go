package database

import (
	"context"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// Learned write-rate baselines (pkg/controller/write_anomaly.go), one opaque
// JSON document per resource. They take weeks to learn by hour of the week,
// so they live here rather than in memory, and move with the controller on
// failover.

const writeBaselinesBucket = "write_baselines"

// SaveWriteBaseline stores resource's baseline.
func (db *DB) SaveWriteBaseline(ctx context.Context, resource string, data []byte) error {
	db.log().Debug("DB.SaveWriteBaseline: saving baseline", zap.String("resource", resource), zap.Int("size_bytes", len(data)))
	buf := append([]byte(nil), data...)
	db.mu.Lock()
	defer db.mu.Unlock()
	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(writeBaselinesBucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(resource), buf)
	})
	db.log().Debug("DB.SaveWriteBaseline: put", zap.String("bucket", writeBaselinesBucket), zap.String("key", resource),
		zap.Int("size_bytes", len(buf)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// LoadWriteBaseline returns resource's baseline, or nil when none is stored.
func (db *DB) LoadWriteBaseline(ctx context.Context, resource string) ([]byte, error) {
	db.log().Debug("DB.LoadWriteBaseline: loading baseline", zap.String("resource", resource))
	db.mu.RLock()
	defer db.mu.RUnlock()
	var out []byte
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(writeBaselinesBucket))
		if b == nil {
			db.log().Debug("DB.LoadWriteBaseline: bucket absent", zap.String("bucket", writeBaselinesBucket))
			return nil
		}
		if v := b.Get([]byte(resource)); v != nil {
			out = append([]byte(nil), v...)
		}
		return nil
	})
	db.log().Debug("DB.LoadWriteBaseline: get", zap.String("bucket", writeBaselinesBucket), zap.String("key", resource),
		zap.Bool("found", out != nil), zap.Int("size_bytes", len(out)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return out, err
}
