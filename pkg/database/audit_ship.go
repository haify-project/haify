package database

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// Shipping the audit trail off the cluster.
//
// The trail in this database is only as trustworthy as the controller host:
// whoever holds it can rewrite the file. So each entry is also sent to a
// destination outside the cluster (pkg/controller/audit_ship.go). The trail
// itself is the outbox: every destination has a cursor here, the sequence
// number of the last entry it acknowledged, and the shipper sends what comes
// after it. The cursor sits on the replicated metadata volume with the trail,
// so a controller that takes over after a failover resumes where the last one
// stopped, and entries written while a destination was down are sent once it
// is back — nothing between two acknowledged batches is skipped.

const auditShipBucket = "audit_ship"

// AuditRecord is an audit entry with its position in the trail.
type AuditRecord struct {
	Seq   uint64      `json:"seq"`
	Event *AuditEvent `json:"event"`
}

// shipDestination names a ship destination for a trace. A webhook
// destination's name carries its whole URL, whose path or query can hold a
// token, so only its host is kept.
func shipDestination(dest string) zap.Field {
	kind, rest, ok := strings.Cut(dest, ":")
	if !ok || kind != "webhook" {
		return zap.String("destination", dest)
	}
	return zap.String("destination", kind+":"+urlHost(rest))
}

// auditCutoff is the time before which entries are past their retention, or
// zero when entries are kept until the cap.
func (db *DB) auditCutoff() time.Time {
	if db.auditMaxAge <= 0 {
		return time.Time{}
	}
	return time.Now().Add(-db.auditMaxAge)
}

// AuditEventsAfter returns up to limit entries that follow seq, oldest first.
func (db *DB) AuditEventsAfter(ctx context.Context, seq uint64, limit int) ([]AuditRecord, error) {
	db.log().Debug("DB.AuditEventsAfter: reading audit entries", zap.Uint64("after_seq", seq), zap.Int("limit", limit))
	db.mu.RLock()
	defer db.mu.RUnlock()
	var out []AuditRecord
	unreadable := 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(auditBucket))
		if b == nil {
			db.log().Debug("DB.AuditEventsAfter: audit bucket absent", zap.String("bucket", auditBucket))
			return nil
		}
		c := b.Cursor()
		for k, v := c.Seek(seqKey(seq + 1)); k != nil && len(out) < limit; k, v = c.Next() {
			var ev AuditEvent
			if err := json.Unmarshal(v, &ev); err != nil {
				// Shipped as a gap would be silent; an unreadable entry still
				// has a sequence number the receiver can see is missing.
				unreadable++
				continue
			}
			out = append(out, AuditRecord{Seq: binary.BigEndian.Uint64(k), Event: &ev})
		}
		return nil
	})
	db.log().Debug("DB.AuditEventsAfter: scan", zap.String("bucket", auditBucket), zap.Uint64("after_seq", seq),
		zap.Int("count", len(out)), zap.Int("unreadable", unreadable),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return out, err
}

// AuditShipCursor is the last entry destination acknowledged; zero before the
// first.
func (db *DB) AuditShipCursor(ctx context.Context, destination string) (uint64, error) {
	if ce := db.log().Check(zap.DebugLevel, "DB.AuditShipCursor: reading ship cursor"); ce != nil {
		ce.Write(shipDestination(destination))
	}
	db.mu.RLock()
	defer db.mu.RUnlock()
	var seq uint64
	found := false
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(auditShipBucket))
		if b == nil {
			return nil
		}
		if v := b.Get([]byte(destination)); len(v) == 8 {
			seq, found = binary.BigEndian.Uint64(v), true
		}
		return nil
	})
	if ce := db.log().Check(zap.DebugLevel, "DB.AuditShipCursor: get"); ce != nil {
		ce.Write(zap.String("bucket", auditShipBucket), shipDestination(destination), zap.Bool("found", found),
			zap.Uint64("seq", seq), zap.Duration("took", time.Since(start)), zap.Error(err))
	}
	return seq, err
}

// SetAuditShipCursor records that destination acknowledged everything up to
// and including seq.
func (db *DB) SetAuditShipCursor(ctx context.Context, destination string, seq uint64) error {
	if ce := db.log().Check(zap.DebugLevel, "DB.SetAuditShipCursor: advancing ship cursor"); ce != nil {
		ce.Write(shipDestination(destination), zap.Uint64("seq", seq))
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(auditShipBucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(destination), seqKey(seq))
	})
	if ce := db.log().Check(zap.DebugLevel, "DB.SetAuditShipCursor: put"); ce != nil {
		ce.Write(zap.String("bucket", auditShipBucket), shipDestination(destination), zap.Uint64("seq", seq),
			zap.Duration("took", time.Since(start)), zap.Error(err))
	}
	return err
}

// PruneExpiredAudit drops entries past their retention, oldest first, at most
// batch of them per call, and returns how many it dropped.
func (db *DB) PruneExpiredAudit(ctx context.Context, batch int) (int, error) {
	cutoff := db.auditCutoff()
	db.log().Debug("DB.PruneExpiredAudit: pruning expired audit entries",
		zap.Int("batch", batch), zap.Time("cutoff", cutoff))
	if cutoff.IsZero() {
		db.log().Debug("DB.PruneExpiredAudit: no max age configured, nothing to prune")
		return 0, nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	dropped := 0
	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(auditBucket))
		if b == nil {
			db.log().Debug("DB.PruneExpiredAudit: audit bucket absent", zap.String("bucket", auditBucket))
			return nil
		}
		var doomed [][]byte
		c := b.Cursor()
		for k, v := c.First(); k != nil && len(doomed) < batch; k, v = c.Next() {
			var ev AuditEvent
			if json.Unmarshal(v, &ev) == nil && !ev.Timestamp.Before(cutoff) {
				break
			}
			doomed = append(doomed, append([]byte(nil), k...))
		}
		// Keys are deleted after the walk; see pruneAudit.
		for _, k := range doomed {
			if err := b.Delete(k); err != nil {
				db.log().Debug("DB.PruneExpiredAudit: delete failed",
					zap.Uint64("seq", binary.BigEndian.Uint64(k)), zap.Error(err))
				return err
			}
		}
		dropped = len(doomed)
		return nil
	})
	db.log().Debug("DB.PruneExpiredAudit: done", zap.String("bucket", auditBucket), zap.Int("count", dropped),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return dropped, err
}

// TakeAuditTruncated returns how many entries the cap dropped before their
// retention ran out since the last call, and resets the count.
func (db *DB) TakeAuditTruncated() uint64 {
	n := db.auditTruncated.Swap(0)
	db.log().Debug("DB.TakeAuditTruncated: taken", zap.Uint64("count", n))
	return n
}
