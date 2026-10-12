package database

import (
	"time"

	"context"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// Cluster events (degrade, failover, node loss) used to live only in the
// controller's memory, so a controller restart emptied the history — and under
// Self-HA every failover is a controller restart. The history was gone exactly
// when someone opened it to see what had just happened. Kept here, on the
// replicated metadata volume, it moves with the controller like the audit
// trail does.
//
// Records are the events' JSON as the event package encodes them; this
// package does not interpret them.

const eventBucket = "events"

// AppendEventRecord stores one event, dropping the oldest beyond retention.
func (db *DB) AppendEventRecord(ctx context.Context, record []byte, retention int) error {
	db.log().Debug("DB.AppendEventRecord: appending event", zap.Int("size_bytes", len(record)), zap.Int("retention", retention))
	db.mu.Lock()
	defer db.mu.Unlock()
	var seq uint64
	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(eventBucket))
		if err != nil {
			return err
		}
		seq, err = b.NextSequence()
		if err != nil {
			return err
		}
		if err := b.Put(seqKey(seq), record); err != nil {
			return err
		}
		if retention > 0 && auditLen(b, seq) > retention+pruneSlack {
			db.log().Debug("DB.AppendEventRecord: history past retention, pruning", zap.Int("retention", retention))
			_, err := pruneAudit(b, retention, time.Time{})
			return err
		}
		return nil
	})
	db.log().Debug("DB.AppendEventRecord: put", zap.String("bucket", eventBucket), zap.Uint64("seq", seq),
		zap.Int("size_bytes", len(record)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// RecentEventRecords returns up to limit of the newest stored events, oldest
// first.
func (db *DB) RecentEventRecords(ctx context.Context, limit int) ([][]byte, error) {
	db.log().Debug("DB.RecentEventRecords: reading recent events", zap.Int("limit", limit))
	db.mu.RLock()
	defer db.mu.RUnlock()
	var out [][]byte
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(eventBucket))
		if b == nil {
			db.log().Debug("DB.RecentEventRecords: event bucket absent", zap.String("bucket", eventBucket))
			return nil
		}
		c := b.Cursor()
		for k, v := c.Last(); k != nil && (limit <= 0 || len(out) < limit); k, v = c.Prev() {
			out = append(out, append([]byte(nil), v...))
		}
		return nil
	})
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	db.log().Debug("DB.RecentEventRecords: scan", zap.String("bucket", eventBucket), zap.Int("count", len(out)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return out, err
}
