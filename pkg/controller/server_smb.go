package controller

import (
	"context"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/gateway"
)

// SMB gateway handlers. The gateway itself is in pkg/gateway/smb.go; stop,
// start and delete are the common gateway RPCs. SMB user passwords are never
// traced, only whether one was given.

func (s *Server) CreateSMBGateway(ctx context.Context, req *haifypb.CreateSMBGatewayRequest) (*haifypb.CreateSMBGatewayResponse, error) {
	log := s.serverLog()
	log.Debug("CreateSMBGateway: creating", zap.String("resource", req.Resource), zap.String("service_ip", req.ServiceIp),
		zap.String("workgroup", req.Workgroup), zap.String("share", req.ShareName))
	if err := s.ctrl.assertPromoterAllowed(ctx, req.Resource, "an SMB gateway"); err != nil {
		log.Debug("CreateSMBGateway: promoter not allowed on resource", zap.String("resource", req.Resource), zap.Error(err))
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	start := time.Now()
	resp, err := gateway.NewSMBManager(s.gateway).CreateSMBGateway(ctx, req)
	if err != nil {
		log.Debug("CreateSMBGateway: gateway manager failed", zap.String("resource", req.Resource),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return resp, err
	}
	log.Debug("CreateSMBGateway: gateway created on nodes", zap.String("resource", req.Resource), zap.Duration("took", time.Since(start)))
	if s.ctrl.db != nil {
		gw := &database.Gateway{
			Name:     req.Resource + "-smb",
			Resource: req.Resource,
			Type:     database.GatewayTypeSMB,
			Config: map[string]interface{}{
				"service_ip":   req.ServiceIp,
				"service_host": gatewayServiceHost(req.ServiceIp),
				"workgroup":    req.Workgroup,
				"share_name":   req.ShareName,
			},
			Status: "configured",
		}
		if err := s.ctrl.db.SaveGateway(ctx, gw); err != nil {
			s.ctrl.logger.Error("Failed to save gateway to database", zap.Error(err))
		} else {
			log.Debug("CreateSMBGateway: saved gateway record", zap.String("gateway", gw.Name))
		}
	} else {
		log.Debug("CreateSMBGateway: no database, record not saved", zap.String("resource", req.Resource))
	}
	return resp, nil
}

func (s *Server) AddSMBShare(ctx context.Context, req *haifypb.AddSMBShareRequest) (*haifypb.AddSMBShareResponse, error) {
	log := s.serverLog()
	sh := req.GetShare()
	if sh == nil {
		log.Debug("AddSMBShare: no share in request", zap.String("resource", req.Resource))
		return nil, status.Error(codes.InvalidArgument, "share is required")
	}
	log.Debug("AddSMBShare: adding", zap.String("resource", req.Resource), zap.String("share", sh.Name),
		zap.String("path", sh.Path), zap.Bool("read_only", sh.ReadOnly), zap.Strings("valid_users", sh.ValidUsers))
	err := gateway.NewSMBManager(s.gateway).AddSMBShare(ctx, req.Resource, gateway.SMBShare{
		Name: sh.Name, Path: sh.Path, ReadOnly: sh.ReadOnly, ValidUsers: sh.ValidUsers,
	})
	if err != nil {
		log.Debug("AddSMBShare: failed", zap.String("resource", req.Resource), zap.String("share", sh.Name), zap.Error(err))
		return &haifypb.AddSMBShareResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("AddSMBShare: added", zap.String("resource", req.Resource), zap.String("share", sh.Name))
	return &haifypb.AddSMBShareResponse{Success: true, Message: "SMB share added"}, nil
}

func (s *Server) RemoveSMBShare(ctx context.Context, req *haifypb.RemoveSMBShareRequest) (*haifypb.RemoveSMBShareResponse, error) {
	log := s.serverLog()
	log.Debug("RemoveSMBShare: removing", zap.String("resource", req.Resource), zap.String("share", req.Name))
	if err := gateway.NewSMBManager(s.gateway).RemoveSMBShare(ctx, req.Resource, req.Name); err != nil {
		log.Debug("RemoveSMBShare: failed", zap.String("resource", req.Resource), zap.String("share", req.Name), zap.Error(err))
		return &haifypb.RemoveSMBShareResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("RemoveSMBShare: removed", zap.String("resource", req.Resource), zap.String("share", req.Name))
	return &haifypb.RemoveSMBShareResponse{Success: true, Message: "SMB share removed; its data was left in place"}, nil
}

func (s *Server) ListSMBShares(ctx context.Context, req *haifypb.ListSMBSharesRequest) (*haifypb.ListSMBSharesResponse, error) {
	log := s.serverLog()
	log.Debug("ListSMBShares: listing", zap.String("resource", req.Resource))
	shares, err := gateway.NewSMBManager(s.gateway).ListSMBShares(ctx, req.Resource)
	if err != nil {
		log.Debug("ListSMBShares: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.ListSMBSharesResponse{Success: false, Message: err.Error()}, nil
	}
	out := make([]*haifypb.SMBShareInfo, 0, len(shares))
	for _, sh := range shares {
		log.Debug("ListSMBShares: share", zap.String("share", sh.Name), zap.String("path", sh.Path), zap.Bool("read_only", sh.ReadOnly))
		out = append(out, &haifypb.SMBShareInfo{Name: sh.Name, Path: sh.Path, ReadOnly: sh.ReadOnly, ValidUsers: sh.ValidUsers})
	}
	log.Debug("ListSMBShares: listed", zap.String("resource", req.Resource), zap.Int("count", len(out)))
	return &haifypb.ListSMBSharesResponse{Success: true, Shares: out}, nil
}

func (s *Server) SetSMBUser(ctx context.Context, req *haifypb.SetSMBUserRequest) (*haifypb.SetSMBUserResponse, error) {
	log := s.serverLog()
	log.Debug("SetSMBUser: setting user", zap.String("resource", req.Resource), zap.String("user", req.User),
		zap.Bool("has_password", req.Password != ""))
	if err := gateway.NewSMBManager(s.gateway).SetSMBUser(ctx, req.Resource, req.User, req.Password); err != nil {
		log.Debug("SetSMBUser: failed", zap.String("resource", req.Resource), zap.String("user", req.User), zap.Error(err))
		return &haifypb.SetSMBUserResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("SetSMBUser: set", zap.String("resource", req.Resource), zap.String("user", req.User))
	return &haifypb.SetSMBUserResponse{Success: true, Message: "SMB user " + req.User + " set"}, nil
}

func (s *Server) RemoveSMBUser(ctx context.Context, req *haifypb.RemoveSMBUserRequest) (*haifypb.RemoveSMBUserResponse, error) {
	log := s.serverLog()
	log.Debug("RemoveSMBUser: removing user", zap.String("resource", req.Resource), zap.String("user", req.User))
	if err := gateway.NewSMBManager(s.gateway).RemoveSMBUser(ctx, req.Resource, req.User); err != nil {
		log.Debug("RemoveSMBUser: failed", zap.String("resource", req.Resource), zap.String("user", req.User), zap.Error(err))
		return &haifypb.RemoveSMBUserResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("RemoveSMBUser: removed", zap.String("resource", req.Resource), zap.String("user", req.User))
	return &haifypb.RemoveSMBUserResponse{Success: true, Message: "SMB user " + req.User + " removed"}, nil
}

func (s *Server) ListSMBUsers(ctx context.Context, req *haifypb.ListSMBUsersRequest) (*haifypb.ListSMBUsersResponse, error) {
	log := s.serverLog()
	log.Debug("ListSMBUsers: listing", zap.String("resource", req.Resource))
	users, err := gateway.NewSMBManager(s.gateway).ListSMBUsers(ctx, req.Resource)
	if err != nil {
		log.Debug("ListSMBUsers: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.ListSMBUsersResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("ListSMBUsers: listed", zap.String("resource", req.Resource), zap.Int("count", len(users)))
	return &haifypb.ListSMBUsersResponse{Success: true, Users: users}, nil
}
