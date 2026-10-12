package controller

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/gateway"
	"go.uber.org/zap"
)

func (s *Server) CreateNFSGateway(ctx context.Context, req *haifypb.CreateNFSGatewayRequest) (*haifypb.CreateNFSGatewayResponse, error) {
	log := s.serverLog()
	log.Debug("CreateNFSGateway: creating", zap.String("resource", req.Resource), zap.String("service_ip", req.ServiceIp),
		zap.String("export_path", req.ExportPath), zap.Strings("allowed_ips", req.AllowedIps), zap.String("fs_type", req.FsType))
	if err := s.ctrl.assertPromoterAllowed(ctx, req.Resource, "an NFS gateway"); err != nil {
		log.Debug("CreateNFSGateway: promoter not allowed on resource", zap.String("resource", req.Resource), zap.Error(err))
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	nfsMgr := gateway.NewNFSManager(s.gateway)
	start := time.Now()
	resp, err := nfsMgr.CreateNFSGateway(ctx, req)
	if err != nil {
		log.Debug("CreateNFSGateway: gateway manager failed", zap.String("resource", req.Resource),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return resp, err
	}
	log.Debug("CreateNFSGateway: gateway created on nodes", zap.String("resource", req.Resource), zap.Duration("took", time.Since(start)))

	// Generate gateway name from resource
	gwName := req.Resource + "-nfs"

	// Save to database
	if s.ctrl.db != nil {
		gw := &database.Gateway{
			Name:     gwName,
			Resource: req.Resource,
			Type:     database.GatewayTypeNFS,
			Config: map[string]interface{}{
				"service_ip":       req.ServiceIp,
				"service_host":     gatewayServiceHost(req.ServiceIp),
				"export_path":      req.ExportPath,
				"export_directory": gatewayExportDirectory(req.Resource, req.ExportPath),
				"allowed_ips":      req.AllowedIps,
				"fs_type":          req.FsType,
				"options":          req.Options,
			},
			Status: "configured",
		}
		if err := s.ctrl.db.SaveGateway(ctx, gw); err != nil {
			s.ctrl.logger.Error("Failed to save gateway to database", zap.Error(err))
		} else {
			log.Debug("CreateNFSGateway: saved gateway record", zap.String("gateway", gwName))
		}
	} else {
		log.Debug("CreateNFSGateway: no database, record not saved", zap.String("gateway", gwName))
	}

	return resp, nil
}

func (s *Server) CreateISCSIGateway(ctx context.Context, req *haifypb.CreateISCSIGatewayRequest) (*haifypb.CreateISCSIGatewayResponse, error) {
	log := s.serverLog()
	// CHAP credentials are logged as present or not, never their values.
	log.Debug("CreateISCSIGateway: creating", zap.String("resource", req.Resource), zap.String("service_ip", req.ServiceIp),
		zap.String("iqn", req.Iqn), zap.Strings("allowed_initiators", req.AllowedInitiators),
		zap.Bool("has_chap_user", req.Username != ""), zap.Bool("has_chap_password", req.Password != ""),
		zap.String("implementation", req.Implementation))
	if err := s.ctrl.assertPromoterAllowed(ctx, req.Resource, "an iSCSI gateway"); err != nil {
		log.Debug("CreateISCSIGateway: promoter not allowed on resource", zap.String("resource", req.Resource), zap.Error(err))
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	iscsiMgr := gateway.NewISCSIManager(s.gateway)
	start := time.Now()
	resp, err := iscsiMgr.CreateISCSIGateway(ctx, req)
	if err != nil {
		log.Debug("CreateISCSIGateway: gateway manager failed", zap.String("resource", req.Resource),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return resp, err
	}
	log.Debug("CreateISCSIGateway: gateway created on nodes", zap.String("resource", req.Resource), zap.Duration("took", time.Since(start)))

	// Generate gateway name from resource
	gwName := req.Resource + "-iscsi"

	// Save to database
	if s.ctrl.db != nil {
		gw := &database.Gateway{
			Name:     gwName,
			Resource: req.Resource,
			Type:     database.GatewayTypeISCSI,
			Config: map[string]interface{}{
				"service_ip":         req.ServiceIp,
				"service_host":       gatewayServiceHost(req.ServiceIp),
				"iqn":                req.Iqn,
				"allowed_initiators": req.AllowedInitiators,
				"username":           req.Username,
				"password":           req.Password,
				"implementation":     req.Implementation,
				"options":            req.Options,
			},
			Status: "configured",
		}
		if err := s.ctrl.db.SaveGateway(ctx, gw); err != nil {
			s.ctrl.logger.Error("Failed to save gateway to database", zap.Error(err))
		} else {
			log.Debug("CreateISCSIGateway: saved gateway record", zap.String("gateway", gwName))
		}
	} else {
		log.Debug("CreateISCSIGateway: no database, record not saved", zap.String("gateway", gwName))
	}

	return resp, nil
}

func (s *Server) CreateNVMeGateway(ctx context.Context, req *haifypb.CreateNVMeGatewayRequest) (*haifypb.CreateNVMeGatewayResponse, error) {
	log := s.serverLog()
	log.Debug("CreateNVMeGateway: creating", zap.String("resource", req.Resource), zap.String("service_ip", req.ServiceIp),
		zap.String("nqn", req.Nqn), zap.String("transport_type", req.TransportType))
	if err := s.ctrl.assertPromoterAllowed(ctx, req.Resource, "an NVMe-oF gateway"); err != nil {
		log.Debug("CreateNVMeGateway: promoter not allowed on resource", zap.String("resource", req.Resource), zap.Error(err))
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	nvmeMgr := gateway.NewNVMeManager(s.gateway)
	start := time.Now()
	resp, err := nvmeMgr.CreateNVMeGateway(ctx, req)
	if err != nil {
		log.Debug("CreateNVMeGateway: gateway manager failed", zap.String("resource", req.Resource),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return resp, err
	}
	log.Debug("CreateNVMeGateway: gateway created on nodes", zap.String("resource", req.Resource), zap.Duration("took", time.Since(start)))

	// Generate gateway name from resource
	gwName := req.Resource + "-nvme"

	// Save to database
	if s.ctrl.db != nil {
		gw := &database.Gateway{
			Name:     gwName,
			Resource: req.Resource,
			Type:     database.GatewayTypeNVMEOF,
			Config: map[string]interface{}{
				"service_ip":     req.ServiceIp,
				"service_host":   gatewayServiceHost(req.ServiceIp),
				"nqn":            req.Nqn,
				"transport_type": req.TransportType,
				"options":        req.Options,
			},
			Status: "configured",
		}
		if err := s.ctrl.db.SaveGateway(ctx, gw); err != nil {
			s.ctrl.logger.Error("Failed to save gateway to database", zap.Error(err))
		} else {
			log.Debug("CreateNVMeGateway: saved gateway record", zap.String("gateway", gwName))
		}
	} else {
		log.Debug("CreateNVMeGateway: no database, record not saved", zap.String("gateway", gwName))
	}

	return resp, nil
}

func (s *Server) DeleteGateway(ctx context.Context, req *haifypb.DeleteGatewayRequest) (*haifypb.DeleteGatewayResponse, error) {
	log := s.serverLog()
	log.Debug("DeleteGateway: deleting", zap.String("resource", req.Id))
	start := time.Now()
	err := s.gateway.DeleteGateway(ctx, req.Id)
	if err != nil {
		log.Debug("DeleteGateway: failed", zap.String("resource", req.Id), zap.Duration("took", time.Since(start)), zap.Error(err))
		return &haifypb.DeleteGatewayResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("DeleteGateway: removed from nodes", zap.String("resource", req.Id), zap.Duration("took", time.Since(start)))

	// Delete from database
	if s.ctrl.db != nil {
		if err := s.ctrl.db.DeleteGatewayByResource(ctx, req.Id); err != nil {
			s.ctrl.logger.Error("Failed to delete gateway from database", zap.Error(err))
		} else {
			log.Debug("DeleteGateway: deleted gateway record", zap.String("resource", req.Id))
		}
	}

	return &haifypb.DeleteGatewayResponse{
		Success: true,
		Message: "Gateway deleted successfully",
	}, nil
}

func (s *Server) GetGateway(ctx context.Context, req *haifypb.GetGatewayRequest) (*haifypb.GetGatewayResponse, error) {
	log := s.serverLog()
	log.Debug("GetGateway: looking up", zap.String("resource", req.Id))
	gw, err := s.getGatewayInfo(ctx, req.Id)
	if err != nil {
		log.Debug("GetGateway: failed", zap.String("resource", req.Id), zap.Error(err))
		return &haifypb.GetGatewayResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("GetGateway: found", zap.String("resource", req.Id), zap.String("gateway", gw.Name), zap.String("type", gw.Type),
		zap.String("state", gw.State))
	return &haifypb.GetGatewayResponse{
		Success: true,
		Message: "Gateway found",
		Gateway: s.enrichGatewayInfo(ctx, gw),
	}, nil
}

func (s *Server) ListGateways(ctx context.Context, req *haifypb.ListGatewaysRequest) (*haifypb.ListGatewaysResponse, error) {
	log := s.serverLog()
	log.Debug("ListGateways: listing")
	gateways, err := s.listGatewayInfos(ctx)
	if err != nil {
		log.Debug("ListGateways: failed", zap.Error(err))
		return &haifypb.ListGatewaysResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	var pbGateways []*haifypb.GatewayInfo
	for _, gw := range gateways {
		log.Debug("ListGateways: gateway", zap.String("gateway", gw.Name), zap.String("type", gw.Type),
			zap.String("resource", gw.Resource), zap.String("state", gw.State))
		pbGateways = append(pbGateways, s.enrichGatewayInfo(ctx, gw))
	}

	log.Debug("ListGateways: listed", zap.Int("count", len(pbGateways)))
	return &haifypb.ListGatewaysResponse{
		Success:  true,
		Message:  "Gateways listed successfully",
		Gateways: pbGateways,
	}, nil
}

func (s *Server) StartGateway(ctx context.Context, req *haifypb.StartGatewayRequest) (*haifypb.StartGatewayResponse, error) {
	log := s.serverLog()
	log.Debug("StartGateway: starting", zap.String("resource", req.Id))
	start := time.Now()
	err := s.gateway.StartGateway(ctx, req.Id)
	if err != nil {
		log.Debug("StartGateway: failed", zap.String("resource", req.Id), zap.Duration("took", time.Since(start)), zap.Error(err))
		return &haifypb.StartGatewayResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("StartGateway: started", zap.String("resource", req.Id), zap.Duration("took", time.Since(start)))
	if s.ctrl.db != nil {
		if gw, err := s.ctrl.db.GetGatewayByResource(ctx, req.Id); err == nil {
			gw.Status = "started"
			if err := s.ctrl.db.SaveGateway(ctx, gw); err != nil {
				s.ctrl.logger.Error("Failed to update gateway status in database", zap.Error(err))
			} else {
				log.Debug("StartGateway: record marked started", zap.String("gateway", gw.Name))
			}
		} else {
			log.Debug("StartGateway: no gateway record to update", zap.String("resource", req.Id), zap.Error(err))
		}
	}
	return &haifypb.StartGatewayResponse{
		Success: true,
		Message: "Gateway started successfully",
	}, nil
}

func (s *Server) StopGateway(ctx context.Context, req *haifypb.StopGatewayRequest) (*haifypb.StopGatewayResponse, error) {
	log := s.serverLog()
	log.Debug("StopGateway: stopping", zap.String("resource", req.Id))
	start := time.Now()
	err := s.gateway.StopGateway(ctx, req.Id)
	if err != nil {
		log.Debug("StopGateway: failed", zap.String("resource", req.Id), zap.Duration("took", time.Since(start)), zap.Error(err))
		return &haifypb.StopGatewayResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("StopGateway: stopped", zap.String("resource", req.Id), zap.Duration("took", time.Since(start)))
	if s.ctrl.db != nil {
		if gw, err := s.ctrl.db.GetGatewayByResource(ctx, req.Id); err == nil {
			gw.Status = "stopped"
			gw.ActiveNode = ""
			if err := s.ctrl.db.SaveGateway(ctx, gw); err != nil {
				s.ctrl.logger.Error("Failed to update gateway status in database", zap.Error(err))
			} else {
				log.Debug("StopGateway: record marked stopped", zap.String("gateway", gw.Name))
			}
		} else {
			log.Debug("StopGateway: no gateway record to update", zap.String("resource", req.Id), zap.Error(err))
		}
	}
	return &haifypb.StopGatewayResponse{
		Success: true,
		Message: "Gateway stopped successfully",
	}, nil
}
