package database

import (
	"context"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// The cluster's totals over time (pkg/controller/cluster_history.go), one
// opaque JSON document, kept here so the dashboard's charts survive a
// controller restart and move with the controller on failover.

const (
	clusterHistoryBucket = "cluster_history"
	clusterHistoryKey    = "samples"
)

// SaveClusterHistory stores the cluster history document.
func (db *DB) SaveClusterHistory(ctx context.Context, data []byte) error {
	db.log().Debug("DB.SaveClusterHistory: saving", zap.Int("size_bytes", len(data)))
	buf := append([]byte(nil), data...)
	db.mu.Lock()
	defer db.mu.Unlock()
	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(clusterHistoryBucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(clusterHistoryKey), buf)
	})
	db.log().Debug("DB.SaveClusterHistory: put", zap.String("bucket", clusterHistoryBucket),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// LoadClusterHistory returns the cluster history document, nil when none is
// stored.
func (db *DB) LoadClusterHistory(ctx context.Context) ([]byte, error) {
	db.log().Debug("DB.LoadClusterHistory: loading")
	db.mu.RLock()
	defer db.mu.RUnlock()
	var out []byte
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(clusterHistoryBucket))
		if b == nil {
			return nil
		}
		if v := b.Get([]byte(clusterHistoryKey)); v != nil {
			out = append([]byte(nil), v...)
		}
		return nil
	})
	db.log().Debug("DB.LoadClusterHistory: get", zap.Bool("found", out != nil), zap.Int("size_bytes", len(out)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return out, err
}
