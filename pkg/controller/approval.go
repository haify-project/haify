package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/haify-project/haify/pkg/config"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/event"
)

// Two-person approval ([rbac.approval]).
//
// RBAC decides what a role may do, and an admin may do everything — so one
// stolen admin token could delete the backup target, the backups and the
// snapshots, and nothing in haify would object. With approval on, each call on
// the list runs only after a second user approved that exact call:
//
//  1. Alice makes the call. It fails with FAILED_PRECONDITION, naming request
//     <id>, which is now pending and raises an approval.requested event.
//  2. Bob, a different user with the approve right (admin, security-officer),
//     reads the request's arguments and runs `haify approval approve <id>`.
//  3. Alice repeats the identical call within the TTL. It runs, once.
//
// "Identical" is the method plus the deterministic encoding of its arguments,
// so an approval for deleting one backup cannot be spent deleting another.
// Creating users and changing roles are on the default list: otherwise the
// stolen token would simply mint its own second approver.

// approvalGate holds the requests and decides whether a call may run.
type approvalGate struct {
	db      *database.DB
	methods map[string]bool
	ttl     time.Duration
	events  *event.Bus
	log     *zap.Logger
	now     func() time.Time

	// mu makes check-then-consume atomic, so one approval runs one call even
	// when the same call arrives twice at once.
	mu sync.Mutex
}

func newApprovalGate(cfg config.ApprovalConfig, db *database.DB, events *event.Bus, log *zap.Logger) *approvalGate {
	if log == nil {
		log = zap.L()
	}
	log.Debug("newApprovalGate: building approval gate", zap.Bool("enabled", cfg.Enabled),
		zap.Bool("has_db", db != nil), zap.Int("ttl_minutes", cfg.TTLMinutes))
	if !cfg.Enabled || db == nil {
		log.Debug("newApprovalGate: approval gate off", zap.Bool("enabled", cfg.Enabled), zap.Bool("has_db", db != nil))
		return nil
	}
	methods := map[string]bool{}
	for _, m := range cfg.ApprovalMethods() {
		methods[strings.TrimSpace(m)] = true
		log.Debug("newApprovalGate: method needs approval", zap.String("method", strings.TrimSpace(m)))
	}
	ttl := time.Duration(cfg.TTLMinutes) * time.Minute
	if ttl <= 0 {
		log.Debug("newApprovalGate: no TTL configured, using one hour", zap.Int("ttl_minutes", cfg.TTLMinutes))
		ttl = time.Hour
	}
	log.Debug("newApprovalGate: approval gate on", zap.Int("count", len(methods)), zap.Duration("ttl", ttl))
	return &approvalGate{db: db, methods: methods, ttl: ttl, events: events, log: log, now: time.Now}
}

// requestDigest identifies one call: its method and exact arguments.
func requestDigest(method string, req proto.Message) (string, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		zap.L().Debug("requestDigest: marshal failed", zap.String("method", method), zap.Error(err))
		return "", err
	}
	sum := sha256.Sum256(append([]byte(method+"\x00"), b...))
	return hex.EncodeToString(sum[:]), nil
}

// maskedRequest renders the arguments for the approver with every field that
// looks like a credential replaced: the request list is readable by anyone
// who may read the cluster.
func maskedRequest(req proto.Message) string {
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(req)
	if err != nil {
		zap.L().Debug("maskedRequest: marshal failed, showing empty arguments", zap.Error(err))
		return "{}"
	}
	var m any
	if err := json.Unmarshal(b, &m); err != nil {
		zap.L().Debug("maskedRequest: decode failed, showing empty arguments", zap.Error(err))
		return "{}"
	}
	maskSecrets(m)
	out, _ := json.Marshal(m)
	return string(out)
}

func maskSecrets(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "secret") || strings.Contains(lk, "password") || strings.Contains(lk, "token") ||
				strings.Contains(lk, "passphrase") || strings.HasSuffix(lk, "key") {
				zap.L().Debug("maskSecrets: masking field", zap.String("field", k))
				t[k] = "***"
				continue
			}
			maskSecrets(val)
		}
	case []any:
		for _, val := range t {
			maskSecrets(val)
		}
	}
}

// check lets method run for user when an approval for exactly this call is
// waiting, and consumes it. Otherwise it records (or finds) a pending request
// and returns the FAILED_PRECONDITION error that names it.
func (g *approvalGate) check(ctx context.Context, method, user string, req proto.Message) error {
	if g == nil || !g.methods[method] {
		return nil
	}
	log := g.log
	if log == nil {
		log = zap.L()
	}
	log.Debug("approvalGate.check: method needs approval", zap.String("method", method), zap.String("user", user))
	if weakens, ok := conditionalApproval[method]; ok && !weakens(ctx, g.db, req) {
		log.Debug("approvalGate.check: call does not weaken retention, no approval needed", zap.String("method", method))
		return nil
	}
	if user == "" {
		log.Debug("approvalGate.check: caller has no identity, denied", zap.String("method", method))
		return status.Error(codes.PermissionDenied, method+" needs a second person's approval, and the caller has no identity")
	}
	digest, err := requestDigest(method, req)
	if err != nil {
		log.Debug("approvalGate.check: request digest failed", zap.String("method", method), zap.Error(err))
		return status.Errorf(codes.Internal, "approval: %v", err)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	start := time.Now()
	all, err := g.db.ListApprovals(ctx)
	log.Debug("approvalGate.check: listed approvals", zap.Int("count", len(all)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return status.Errorf(codes.Internal, "approval: %v", err)
	}
	var pending *database.Approval
	for _, a := range all {
		if a.Digest != digest || a.Requester != user {
			continue
		}
		st := a.EffectiveState(now)
		log.Debug("approvalGate.check: matching request found", zap.String("id", a.ID), zap.String("state", st))
		switch st {
		case database.ApprovalApproved:
			a.State = database.ApprovalUsed
			start := time.Now()
			if err := g.db.SaveApproval(ctx, a); err != nil {
				log.Debug("approvalGate.check: marking approval used failed", zap.String("id", a.ID),
					zap.Duration("took", time.Since(start)), zap.Error(err))
				return status.Errorf(codes.Internal, "approval: %v", err)
			}
			log.Debug("approvalGate.check: approval consumed", zap.String("id", a.ID), zap.Duration("took", time.Since(start)))
			g.log.Info("Running an approved call", zap.String("method", method), zap.String("id", a.ID),
				zap.String("requester", user), zap.String("approver", a.DecidedBy))
			return nil
		case database.ApprovalPending:
			pending = a
		}
	}
	if pending == nil {
		pending = &database.Approval{
			ID: newApprovalID(), Method: method, Digest: digest, Request: maskedRequest(req),
			Requester: user, CreatedAt: now, ExpiresAt: now.Add(g.ttl), State: database.ApprovalPending,
		}
		start := time.Now()
		if err := g.db.SaveApproval(ctx, pending); err != nil {
			log.Debug("approvalGate.check: saving new request failed", zap.String("id", pending.ID),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			return status.Errorf(codes.Internal, "approval: %v", err)
		}
		log.Debug("approvalGate.check: new request recorded", zap.String("id", pending.ID),
			zap.String("method", method), zap.Time("expires_at", pending.ExpiresAt), zap.Duration("took", time.Since(start)))
		pruneErr := g.db.PruneApprovals(ctx, now.AddDate(0, 0, -30))
		log.Debug("approvalGate.check: pruned old approvals", zap.Error(pruneErr))
		if g.events != nil {
			log.Debug("approvalGate.check: publishing approval.requested", zap.String("id", pending.ID))
			g.events.Publish(event.Event{
				Type: event.TypeApprovalRequested, Severity: event.SeverityWarning, Status: event.StatusInfo,
				Message: fmt.Sprintf("%s asks to run %s %s; approve with `haify approval approve %s`",
					user, method, pending.Request, pending.ID),
				Details: map[string]string{"id": pending.ID, "method": method, "requester": user},
			})
		}
	}
	log.Debug("approvalGate.check: call held for approval", zap.String("method", method), zap.String("id", pending.ID))
	return status.Errorf(codes.FailedPrecondition,
		"%s needs a second person's approval: request %s is pending until %s. Another user with the approve right "+
			"runs `haify approval approve %s`; then repeat this exact call",
		method, pending.ID, pending.ExpiresAt.UTC().Format(time.RFC3339), pending.ID)
}

// decide approves or rejects request id as user.
func (g *approvalGate) decide(ctx context.Context, id, user string, approve bool) (*database.Approval, error) {
	if g == nil {
		zap.L().Debug("approvalGate.decide: approval not enabled", zap.String("id", id))
		return nil, fmt.Errorf("two-person approval is not enabled ([rbac.approval])")
	}
	log := g.log
	if log == nil {
		log = zap.L()
	}
	log.Debug("approvalGate.decide: deciding request", zap.String("id", id), zap.String("user", user), zap.Bool("approve", approve))
	g.mu.Lock()
	defer g.mu.Unlock()
	start := time.Now()
	a, err := g.db.GetApproval(ctx, id)
	log.Debug("approvalGate.decide: read request", zap.String("id", id), zap.Bool("found", err == nil),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	now := g.now()
	if st := a.EffectiveState(now); st != database.ApprovalPending {
		log.Debug("approvalGate.decide: request not pending", zap.String("id", id), zap.String("state", st))
		return a, fmt.Errorf("request %s is %s, not pending", id, st)
	}
	if approve && a.Requester == user {
		log.Debug("approvalGate.decide: requester cannot approve own request", zap.String("id", id), zap.String("user", user))
		return a, fmt.Errorf("request %s was made by %s, and a request cannot be approved by the user who made it", id, user)
	}
	a.DecidedBy, a.DecidedAt = user, now
	if approve {
		a.State = database.ApprovalApproved
		// The requester gets a full TTL to make the call.
		a.ExpiresAt = now.Add(g.ttl)
	} else {
		a.State = database.ApprovalRejected
	}
	start = time.Now()
	err = g.db.SaveApproval(ctx, a)
	log.Debug("approvalGate.decide: saved decision", zap.String("id", id), zap.String("state", a.State),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return a, err
}

func newApprovalID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// approvalUnaryInterceptor runs after the RBAC interceptors: the caller is
// known and allowed the call; the gate decides whether it also needs a
// second person.
func approvalUnaryInterceptor(g *approvalGate) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		msg, ok := req.(proto.Message)
		if !ok {
			zap.L().Debug("approvalUnaryInterceptor: request is not a proto message, not gated", zap.String("method", info.FullMethod))
		}
		if ok {
			if err := g.check(ctx, shortMethod(info.FullMethod), userFromContext(ctx), msg); err != nil {
				return nil, err
			}
		}
		return handler(ctx, req)
	}
}
