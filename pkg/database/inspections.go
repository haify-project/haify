package database

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// Inspection reports, keyed by a sequence number that is also the report id
// an operator types (`haify inspect show 42`). Records are the reports' JSON as
// pkg/inspect encodes them; this package does not interpret them. They live
// on the metadata volume so the history, and the growth trends computed from
// it, survive a Self-HA failover.

const inspectionsBucket = "inspections"

// AppendInspection stores a report under the next id and drops the oldest
// beyond keep. build receives the id so the record can carry it.
func (db *DB) AppendInspection(ctx context.Context, keep int, build func(id uint64) ([]byte, error)) (uint64, error) {
	db.log().Debug("DB.AppendInspection: appending inspection report", zap.Int("keep", keep))
	db.mu.Lock()
	defer db.mu.Unlock()
	var id uint64
	size, dropped := 0, 0
	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(inspectionsBucket))
		if err != nil {
			return err
		}
		if id, err = b.NextSequence(); err != nil {
			return err
		}
		data, err := build(id)
		if err != nil {
			db.log().Debug("DB.AppendInspection: building report failed", zap.Uint64("id", id), zap.Error(err))
			return err
		}
		size = len(data)
		if err := b.Put(seqKey(id), data); err != nil {
			return err
		}
		if keep <= 0 {
			db.log().Debug("DB.AppendInspection: no keep limit, nothing to prune")
			return nil
		}
		var keys [][]byte
		c := b.Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			keys = append(keys, append([]byte(nil), k...))
		}
		for i := 0; i < len(keys)-keep; i++ {
			err := b.Delete(keys[i])
			db.log().Debug("DB.AppendInspection: delete", zap.String("bucket", inspectionsBucket),
				zap.Uint64("id", binary.BigEndian.Uint64(keys[i])), zap.Error(err))
			if err != nil {
				return err
			}
			dropped++
		}
		return nil
	})
	db.log().Debug("DB.AppendInspection: put", zap.String("bucket", inspectionsBucket), zap.Uint64("id", id),
		zap.Int("size_bytes", size), zap.Int("dropped", dropped),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return id, err
}

// GetInspection returns one stored report.
func (db *DB) GetInspection(ctx context.Context, id uint64) ([]byte, error) {
	db.log().Debug("DB.GetInspection: reading inspection report", zap.Uint64("id", id))
	db.mu.RLock()
	defer db.mu.RUnlock()
	var out []byte
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(inspectionsBucket))
		if b == nil {
			return fmt.Errorf("inspection %d not found", id)
		}
		v := b.Get(seqKey(id))
		if v == nil {
			return fmt.Errorf("inspection %d not found", id)
		}
		out = append([]byte(nil), v...)
		return nil
	})
	db.log().Debug("DB.GetInspection: get", zap.String("bucket", inspectionsBucket), zap.Uint64("id", id),
		zap.Bool("found", out != nil), zap.Int("size_bytes", len(out)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return out, err
}

// ListInspections returns up to limit stored reports, newest first, with
// their ids.
func (db *DB) ListInspections(ctx context.Context, limit int) ([]uint64, [][]byte, error) {
	db.log().Debug("DB.ListInspections: listing inspection reports", zap.Int("limit", limit))
	db.mu.RLock()
	defer db.mu.RUnlock()
	var ids []uint64
	var out [][]byte
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(inspectionsBucket))
		if b == nil {
			db.log().Debug("DB.ListInspections: inspections bucket absent", zap.String("bucket", inspectionsBucket))
			return nil
		}
		c := b.Cursor()
		for k, v := c.Last(); k != nil && (limit <= 0 || len(out) < limit); k, v = c.Prev() {
			ids = append(ids, binary.BigEndian.Uint64(k))
			out = append(out, append([]byte(nil), v...))
		}
		return nil
	})
	db.log().Debug("DB.ListInspections: scan", zap.String("bucket", inspectionsBucket), zap.Int("count", len(out)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return ids, out, err
}
