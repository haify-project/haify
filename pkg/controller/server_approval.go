package controller

import (
	"context"
	"time"

	pb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/rbac"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The approval RPCs (approval.go). The authorization interceptor classifies
// ListApprovals as approval:read and the other two as approval:approve, which
// admin and security-officer hold. Each handler checks again, because a call
// that reaches the server without the interceptors (a test, an in-process
// client) must not decide a request on nobody's behalf.

func approvalInfo(a *database.Approval, now time.Time) *pb.ApprovalInfo {
	if a == nil {
		return nil
	}
	info := &pb.ApprovalInfo{
		Id: a.ID, Method: a.Method, Request: a.Request, Requester: a.Requester,
		CreatedAt: a.CreatedAt.UTC().Format(time.RFC3339), ExpiresAt: a.ExpiresAt.UTC().Format(time.RFC3339),
		State: a.EffectiveState(now), DecidedBy: a.DecidedBy,
	}
	if !a.DecidedAt.IsZero() {
		info.DecidedAt = a.DecidedAt.UTC().Format(time.RFC3339)
	}
	return info
}

// ListApprovals lists the requests, open ones only unless include_closed.
func (s *Server) ListApprovals(ctx context.Context, req *pb.ListApprovalsRequest) (*pb.ListApprovalsResponse, error) {
	log := s.serverLog()
	log.Debug("ListApprovals: listing", zap.Bool("include_closed", req.GetIncludeClosed()))
	g := s.ctrl.approvals
	if g == nil {
		log.Debug("ListApprovals: two-person approval not enabled")
		return &pb.ListApprovalsResponse{Enabled: false}, nil
	}
	start := time.Now()
	all, err := g.db.ListApprovals(ctx)
	if err != nil {
		log.Debug("ListApprovals: database read failed", zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, status.Errorf(codes.Internal, "list approvals: %v", err)
	}
	log.Debug("ListApprovals: read from database", zap.Int("count", len(all)), zap.Duration("took", time.Since(start)))
	now := g.now()
	resp := &pb.ListApprovalsResponse{Enabled: true}
	for _, a := range all {
		st := a.EffectiveState(now)
		if !req.GetIncludeClosed() && st != database.ApprovalPending && st != database.ApprovalApproved {
			log.Debug("ListApprovals: skipping closed request", zap.String("id", a.ID), zap.String("state", st))
			continue
		}
		// The request body is not logged: it can carry secrets.
		log.Debug("ListApprovals: request", zap.String("id", a.ID), zap.String("method", a.Method),
			zap.String("requester", a.Requester), zap.String("state", st))
		resp.Approvals = append(resp.Approvals, approvalInfo(a, now))
	}
	log.Debug("ListApprovals: listed", zap.Int("count", len(resp.Approvals)))
	return resp, nil
}

// approver resolves a caller allowed to decide requests.
func (s *Server) approver(ctx context.Context) (string, error) {
	log := s.serverLog()
	engine := s.ctrl.rbac
	if engine == nil {
		log.Debug("approver: rbac disabled, approval impossible")
		return "", status.Error(codes.FailedPrecondition, "two-person approval needs [rbac] enabled")
	}
	user, ok := rbacCaller(ctx, engine)
	if !ok {
		log.Debug("approver: caller not authenticated")
		return "", errUnauthenticated
	}
	if allowed, _ := engine.Enforce(user, "approval", rbac.ActApprove); !allowed {
		log.Debug("approver: caller lacks the approve right", zap.String("user", user))
		return "", status.Error(codes.PermissionDenied, "approving needs the approve right (admin or security-officer)")
	}
	log.Debug("approver: caller may decide", zap.String("user", user))
	return user, nil
}

// ApproveRequest approves a pending request made by someone else.
func (s *Server) ApproveRequest(ctx context.Context, req *pb.ApproveRequestRequest) (*pb.ApproveRequestResponse, error) {
	log := s.serverLog()
	log.Debug("ApproveRequest: approving", zap.String("id", req.GetId()))
	user, err := s.approver(ctx)
	if err != nil {
		log.Debug("ApproveRequest: caller may not approve", zap.String("id", req.GetId()), zap.Error(err))
		return nil, err
	}
	a, err := s.ctrl.approvals.decide(ctx, req.GetId(), user, true)
	if err != nil {
		log.Debug("ApproveRequest: decide failed", zap.String("id", req.GetId()), zap.String("user", user), zap.Error(err))
		return &pb.ApproveRequestResponse{Success: false, Message: err.Error(), Approval: approvalInfo(a, time.Now())}, nil
	}
	log.Debug("ApproveRequest: approved", zap.String("id", req.GetId()), zap.String("user", user),
		zap.String("requester", a.Requester), zap.String("method", a.Method))
	return &pb.ApproveRequestResponse{Success: true, Approval: approvalInfo(a, time.Now()),
		Message: a.Requester + " may now repeat the call within " + s.ctrl.approvals.ttl.String()}, nil
}

// RejectRequest closes a pending request without running it.
func (s *Server) RejectRequest(ctx context.Context, req *pb.RejectRequestRequest) (*pb.RejectRequestResponse, error) {
	log := s.serverLog()
	log.Debug("RejectRequest: rejecting", zap.String("id", req.GetId()))
	user, err := s.approver(ctx)
	if err != nil {
		log.Debug("RejectRequest: caller may not reject", zap.String("id", req.GetId()), zap.Error(err))
		return nil, err
	}
	a, err := s.ctrl.approvals.decide(ctx, req.GetId(), user, false)
	if err != nil {
		log.Debug("RejectRequest: decide failed", zap.String("id", req.GetId()), zap.String("user", user), zap.Error(err))
		return &pb.RejectRequestResponse{Success: false, Message: err.Error(), Approval: approvalInfo(a, time.Now())}, nil
	}
	log.Debug("RejectRequest: rejected", zap.String("id", req.GetId()), zap.String("user", user))
	return &pb.RejectRequestResponse{Success: true, Message: "rejected", Approval: approvalInfo(a, time.Now())}, nil
}
