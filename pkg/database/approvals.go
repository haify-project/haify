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

// Two-person approval requests ([rbac.approval], pkg/controller/approval.go).
// They live in the database so a request made before a failover can be
// approved, and the approved call made, on the controller that took over.

const approvalsBucket = "approvals"

// Approval states. A pending or approved request past ExpiresAt is expired;
// that is computed when it is read, never stored.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
	ApprovalUsed     = "used"
	ApprovalExpired  = "expired"
)

// Approval is one call waiting for, or holding, a second person's approval.
type Approval struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	// Digest identifies the exact call: method and arguments. Only that call
	// is approved.
	Digest string `json:"digest"`
	// Request is the call's arguments as JSON, secrets masked, for the
	// approver to read.
	Request   string    `json:"request"`
	Requester string    `json:"requester"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	State     string    `json:"state"`
	DecidedBy string    `json:"decided_by,omitempty"`
	DecidedAt time.Time `json:"decided_at,omitempty"`
}

// EffectiveState is State, or expired once a still-open request has run out.
func (a *Approval) EffectiveState(now time.Time) string {
	if (a.State == ApprovalPending || a.State == ApprovalApproved) && now.After(a.ExpiresAt) {
		return ApprovalExpired
	}
	return a.State
}

// SaveApproval creates or replaces a request.
func (db *DB) SaveApproval(ctx context.Context, a *Approval) error {
	// Request holds the call's arguments: only its size is traced.
	db.log().Debug("DB.SaveApproval: saving approval",
		zap.String("approval", a.ID), zap.String("method", a.Method), zap.String("requester", a.Requester),
		zap.String("state", a.State), zap.String("decided_by", a.DecidedBy), zap.Time("expires_at", a.ExpiresAt))
	data, err := json.Marshal(a)
	if err != nil {
		db.log().Debug("DB.SaveApproval: marshal failed", zap.String("approval", a.ID), zap.Error(err))
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	start := time.Now()
	err = db.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(approvalsBucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(a.ID), data)
	})
	db.log().Debug("DB.SaveApproval: put", zap.String("bucket", approvalsBucket), zap.String("key", a.ID),
		zap.Int("size_bytes", len(data)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// GetApproval returns one request.
func (db *DB) GetApproval(ctx context.Context, id string) (*Approval, error) {
	db.log().Debug("DB.GetApproval: reading approval", zap.String("approval", id))
	db.mu.RLock()
	defer db.mu.RUnlock()
	var a *Approval
	size := 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(approvalsBucket))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(id))
		if v == nil {
			return nil
		}
		size = len(v)
		a = &Approval{}
		return json.Unmarshal(v, a)
	})
	db.log().Debug("DB.GetApproval: get", zap.String("bucket", approvalsBucket), zap.String("key", id),
		zap.Bool("found", a != nil), zap.Int("size_bytes", size),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err == nil && a == nil {
		err = fmt.Errorf("no approval request %q", id)
	}
	return a, err
}

// ListApprovals returns every request, newest first.
func (db *DB) ListApprovals(ctx context.Context) ([]*Approval, error) {
	db.log().Debug("DB.ListApprovals: listing approvals")
	db.mu.RLock()
	defer db.mu.RUnlock()
	var out []*Approval
	unreadable := 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(approvalsBucket))
		if b == nil {
			db.log().Debug("DB.ListApprovals: approvals bucket absent", zap.String("bucket", approvalsBucket))
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var a Approval
			if json.Unmarshal(v, &a) == nil {
				out = append(out, &a)
			} else {
				unreadable++
			}
			return nil
		})
	})
	db.log().Debug("DB.ListApprovals: scan", zap.String("bucket", approvalsBucket), zap.Int("count", len(out)),
		zap.Int("unreadable", unreadable), zap.Duration("took", time.Since(start)), zap.Error(err))
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, err
}

// PruneApprovals drops requests that closed (or expired) before cutoff. The
// audit trail, not this list, is the record of what was approved.
func (db *DB) PruneApprovals(ctx context.Context, cutoff time.Time) error {
	db.log().Debug("DB.PruneApprovals: pruning approvals", zap.Time("cutoff", cutoff))
	db.mu.Lock()
	defer db.mu.Unlock()
	dropped := 0
	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(approvalsBucket))
		if b == nil {
			db.log().Debug("DB.PruneApprovals: approvals bucket absent", zap.String("bucket", approvalsBucket))
			return nil
		}
		var doomed [][]byte
		err := b.ForEach(func(k, v []byte) error {
			var a Approval
			if json.Unmarshal(v, &a) == nil && a.ExpiresAt.Before(cutoff) {
				doomed = append(doomed, append([]byte(nil), k...))
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, k := range doomed {
			err := b.Delete(k)
			db.log().Debug("DB.PruneApprovals: delete", zap.String("bucket", approvalsBucket),
				zap.ByteString("key", k), zap.Error(err))
			if err != nil {
				return err
			}
		}
		dropped = len(doomed)
		return nil
	})
	db.log().Debug("DB.PruneApprovals: done", zap.Int("count", dropped),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}
