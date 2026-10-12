package controller

import (
	"context"
	"strings"

	pb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/rbac"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The RBAC RPCs mirror the REST routes in rbac_routes.go. They exist so the
// CLI reaches them over the gRPC listener, which is the one [tls] protects:
// a user-management call carries an admin's bearer token and, for `user add`,
// returns a new one.
//
// The authorization interceptor classifies these as the "system" object, so
// by default only admin may call the mutating ones. Each handler still checks
// CanAdmin itself, because a custom policy granting system:read (every viewer
// has it) must not expose the user list, and one granting system:write must
// not hand out user management.
//
// Tokens are never traced here: only whether one was given.

var errAdminRequired = status.Error(codes.PermissionDenied, "administrator role required")

// rbacCaller resolves the calling user. The identity interceptor has normally
// done it already; the token is the fallback for a call that skipped it.
func rbacCaller(ctx context.Context, engine *rbac.Engine) (string, bool) {
	if name := userFromContext(ctx); name != "" {
		zap.L().Debug("rbacCaller: user from context", zap.String("user", name))
		return name, true
	}
	user, ok := engine.ResolveUser(bearerToken(ctx))
	zap.L().Debug("rbacCaller: resolved from bearer token", zap.String("user", user), zap.Bool("resolved", ok))
	return user, ok
}

// rbacRequireAdmin returns nil when the caller holds the administrator role.
func rbacRequireAdmin(ctx context.Context, engine *rbac.Engine) error {
	user, ok := rbacCaller(ctx, engine)
	if !ok {
		zap.L().Debug("rbacRequireAdmin: caller not authenticated")
		return errUnauthenticated
	}
	if !engine.CanAdmin(user) {
		zap.L().Debug("rbacRequireAdmin: caller is not an administrator", zap.String("user", user))
		return errAdminRequired
	}
	zap.L().Debug("rbacRequireAdmin: caller is an administrator", zap.String("user", user))
	return nil
}

// GetRbacWhoami reports the caller's identity and role.
func (s *Server) GetRbacWhoami(ctx context.Context, _ *pb.GetRbacWhoamiRequest) (*pb.GetRbacWhoamiResponse, error) {
	log := s.serverLog()
	engine := s.ctrl.rbac
	if engine == nil {
		log.Debug("GetRbacWhoami: rbac disabled")
		return &pb.GetRbacWhoamiResponse{Enabled: false}, nil
	}
	user, ok := rbacCaller(ctx, engine)
	if !ok {
		log.Debug("GetRbacWhoami: caller not authenticated")
		return nil, errUnauthenticated
	}
	role := engine.RoleOf(user)
	log.Debug("GetRbacWhoami: caller", zap.String("user", user), zap.String("role", role))
	return &pb.GetRbacWhoamiResponse{
		Enabled:  true,
		User:     user,
		Role:     role,
		CanAdmin: engine.CanAdmin(user),
	}, nil
}

// ListRbacPolicies returns the effective policy and user assignments. Admin only.
func (s *Server) ListRbacPolicies(ctx context.Context, _ *pb.ListRbacPoliciesRequest) (*pb.ListRbacPoliciesResponse, error) {
	log := s.serverLog()
	engine := s.ctrl.rbac
	if engine == nil {
		log.Debug("ListRbacPolicies: rbac disabled")
		return &pb.ListRbacPoliciesResponse{Enabled: false}, nil
	}
	if err := rbacRequireAdmin(ctx, engine); err != nil {
		log.Debug("ListRbacPolicies: refused", zap.Error(err))
		return nil, err
	}
	resp := &pb.ListRbacPoliciesResponse{Enabled: true}
	for _, p := range engine.EffectivePolicies() {
		resp.Policies = append(resp.Policies, &pb.RbacPolicy{Role: p.Role, Object: p.Object, Action: p.Action})
	}
	for _, u := range engine.Users() {
		log.Debug("ListRbacPolicies: user", zap.String("user", u.Name), zap.String("role", u.Role))
		resp.Users = append(resp.Users, &pb.RbacUser{Name: u.Name, Role: u.Role})
	}
	log.Debug("ListRbacPolicies: listed", zap.Int("policies", len(resp.Policies)), zap.Int("users", len(resp.Users)))
	return resp, nil
}

// CreateRbacUser adds a user and returns its token, the only time it is shown.
func (s *Server) CreateRbacUser(ctx context.Context, req *pb.CreateRbacUserRequest) (*pb.CreateRbacUserResponse, error) {
	log := s.serverLog()
	log.Debug("CreateRbacUser: adding user", zap.String("user", req.GetName()), zap.String("role", req.GetRole()),
		zap.Bool("has_token", req.GetToken() != ""))
	engine := s.ctrl.rbac
	if engine == nil {
		log.Debug("CreateRbacUser: rbac disabled")
		return &pb.CreateRbacUserResponse{Message: "RBAC is not enabled"}, nil
	}
	if err := rbacRequireAdmin(ctx, engine); err != nil {
		log.Debug("CreateRbacUser: refused", zap.Error(err))
		return nil, err
	}
	token, err := engine.CreateUser(strings.TrimSpace(req.GetName()), strings.TrimSpace(req.GetToken()), req.GetRole())
	if err != nil {
		log.Debug("CreateRbacUser: failed", zap.String("user", req.GetName()), zap.Error(err))
		return &pb.CreateRbacUserResponse{Enabled: true, Message: err.Error()}, nil
	}
	log.Debug("CreateRbacUser: added", zap.String("user", req.GetName()), zap.Bool("has_token", token != ""))
	return &pb.CreateRbacUserResponse{Success: true, Enabled: true, Token: token}, nil
}

// DeleteRbacUser removes a runtime-managed user. Admin only.
func (s *Server) DeleteRbacUser(ctx context.Context, req *pb.DeleteRbacUserRequest) (*pb.DeleteRbacUserResponse, error) {
	log := s.serverLog()
	log.Debug("DeleteRbacUser: removing user", zap.String("user", req.GetName()))
	engine := s.ctrl.rbac
	if engine == nil {
		log.Debug("DeleteRbacUser: rbac disabled")
		return &pb.DeleteRbacUserResponse{Message: "RBAC is not enabled"}, nil
	}
	if err := rbacRequireAdmin(ctx, engine); err != nil {
		log.Debug("DeleteRbacUser: refused", zap.Error(err))
		return nil, err
	}
	if err := engine.DeleteUser(req.GetName()); err != nil {
		log.Debug("DeleteRbacUser: failed", zap.String("user", req.GetName()), zap.Error(err))
		return &pb.DeleteRbacUserResponse{Enabled: true, Message: err.Error()}, nil
	}
	log.Debug("DeleteRbacUser: removed", zap.String("user", req.GetName()))
	return &pb.DeleteRbacUserResponse{Success: true, Enabled: true}, nil
}

// SetRbacUserRole changes a user's role. Admin only.
func (s *Server) SetRbacUserRole(ctx context.Context, req *pb.SetRbacUserRoleRequest) (*pb.SetRbacUserRoleResponse, error) {
	log := s.serverLog()
	log.Debug("SetRbacUserRole: changing role", zap.String("user", req.GetName()), zap.String("role", req.GetRole()))
	engine := s.ctrl.rbac
	if engine == nil {
		log.Debug("SetRbacUserRole: rbac disabled")
		return &pb.SetRbacUserRoleResponse{Message: "RBAC is not enabled"}, nil
	}
	if err := rbacRequireAdmin(ctx, engine); err != nil {
		log.Debug("SetRbacUserRole: refused", zap.Error(err))
		return nil, err
	}
	if err := engine.SetRole(req.GetName(), req.GetRole()); err != nil {
		log.Debug("SetRbacUserRole: failed", zap.String("user", req.GetName()), zap.Error(err))
		return &pb.SetRbacUserRoleResponse{Enabled: true, Message: err.Error()}, nil
	}
	log.Debug("SetRbacUserRole: changed", zap.String("user", req.GetName()), zap.String("role", req.GetRole()))
	return &pb.SetRbacUserRoleResponse{Success: true, Enabled: true}, nil
}
