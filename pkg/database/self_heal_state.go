package database

import (
	"context"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// What self-healing has to remember across a controller restart or failover
// (pkg/controller/self_heal_follow.go): one opaque JSON document per kind of
// self-healing. It lives here rather than in memory because the work it
// describes outlasts a controller: a replica added to follow its user has to
// be paired with the removal of an old one whichever controller is active
// when its sync finishes.

const selfHealStateBucket = "self_heal_state"

// SaveSelfHealState stores the document kept under key.
func (db *DB) SaveSelfHealState(ctx context.Context, key string, data []byte) error {
	db.log().Debug("DB.SaveSelfHealState: saving state", zap.String("key", key), zap.Int("size_bytes", len(data)))
	buf := append([]byte(nil), data...)
	db.mu.Lock()
	defer db.mu.Unlock()
	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(selfHealStateBucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(key), buf)
	})
	db.log().Debug("DB.SaveSelfHealState: put", zap.String("bucket", selfHealStateBucket), zap.String("key", key),
		zap.Int("size_bytes", len(buf)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// LoadSelfHealState returns the document kept under key, or nil when there is
// none.
func (db *DB) LoadSelfHealState(ctx context.Context, key string) ([]byte, error) {
	db.log().Debug("DB.LoadSelfHealState: loading state", zap.String("key", key))
	db.mu.RLock()
	defer db.mu.RUnlock()
	var out []byte
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(selfHealStateBucket))
		if b == nil {
			db.log().Debug("DB.LoadSelfHealState: bucket absent", zap.String("bucket", selfHealStateBucket))
			return nil
		}
		if v := b.Get([]byte(key)); v != nil {
			out = append([]byte(nil), v...)
		}
		return nil
	})
	db.log().Debug("DB.LoadSelfHealState: get", zap.String("bucket", selfHealStateBucket), zap.String("key", key),
		zap.Bool("found", out != nil), zap.Int("size_bytes", len(out)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return out, err
}
