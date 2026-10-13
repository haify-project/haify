package database

import (
	"context"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// Per-resource write-rate history (pkg/controller/write_history.go), one
// opaque JSON document per resource: the days of DRBD write rates that WAN
// sizing (haify wan sizing) reads. It lives here so it survives a controller
// restart and moves with the controller on failover.

const writeHistoryBucket = "write_history"

// SaveWriteHistory stores resource's history.
func (db *DB) SaveWriteHistory(ctx context.Context, resource string, data []byte) error {
	db.log().Debug("DB.SaveWriteHistory: saving history", zap.String("resource", resource), zap.Int("size_bytes", len(data)))
	buf := append([]byte(nil), data...)
	db.mu.Lock()
	defer db.mu.Unlock()
	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(writeHistoryBucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(resource), buf)
	})
	db.log().Debug("DB.SaveWriteHistory: put", zap.String("bucket", writeHistoryBucket), zap.String("key", resource),
		zap.Int("size_bytes", len(buf)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// LoadWriteHistory returns resource's history, or nil when none is stored.
func (db *DB) LoadWriteHistory(ctx context.Context, resource string) ([]byte, error) {
	db.log().Debug("DB.LoadWriteHistory: loading history", zap.String("resource", resource))
	db.mu.RLock()
	defer db.mu.RUnlock()
	var out []byte
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(writeHistoryBucket))
		if b == nil {
			db.log().Debug("DB.LoadWriteHistory: bucket absent", zap.String("bucket", writeHistoryBucket))
			return nil
		}
		if v := b.Get([]byte(resource)); v != nil {
			out = append([]byte(nil), v...)
		}
		return nil
	})
	db.log().Debug("DB.LoadWriteHistory: get", zap.String("bucket", writeHistoryBucket), zap.String("key", resource),
		zap.Bool("found", out != nil), zap.Int("size_bytes", len(out)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return out, err
}
