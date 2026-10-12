package controller

import (
	"context"
	"strings"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// OCF agent parameters and promoter TOML bodies can carry credentials (CHAP
// passwords, database passwords), so traces here log their count or length,
// never their contents.

func (s *Server) MakeHa(ctx context.Context, req *haifypb.MakeHaRequest) (*haifypb.MakeHaResponse, error) {
	log := s.serverLog()
	log.Debug("MakeHa: creating HA config", zap.String("resource", req.Resource), zap.Strings("services", req.Services),
		zap.String("mount_point", req.MountPoint), zap.String("fstype", req.Fstype), zap.String("vip", req.Vip),
		zap.Int("ocf_agents", len(req.OcfAgents)), zap.Int("start_items", len(req.StartItems)))
	var ocfAgents []OcfAgentSpec
	for _, a := range req.OcfAgents {
		if a == nil {
			log.Debug("MakeHa: skipping empty OCF agent")
			continue
		}
		log.Debug("MakeHa: OCF agent", zap.String("provider", a.Provider), zap.String("agent", a.Name),
			zap.String("instance", a.Instance), zap.Int("params", len(a.Params)))
		ocfAgents = append(ocfAgents, OcfAgentSpec{
			Provider: a.Provider,
			Name:     a.Name,
			Instance: a.Instance,
			Params:   a.Params,
		})
	}
	// Ordered start[] list: systemd units and OCF agents interleaved as peers.
	var startItems []HaStartItem
	for _, it := range req.StartItems {
		if it == nil {
			log.Debug("MakeHa: skipping empty start item")
			continue
		}
		if ocf := it.GetOcf(); ocf != nil {
			log.Debug("MakeHa: start item is an OCF agent", zap.String("provider", ocf.Provider), zap.String("agent", ocf.Name),
				zap.String("instance", ocf.Instance), zap.Int("params", len(ocf.Params)))
			startItems = append(startItems, HaStartItem{Ocf: &OcfAgentSpec{
				Provider: ocf.Provider,
				Name:     ocf.Name,
				Instance: ocf.Instance,
				Params:   ocf.Params,
			}})
		} else if unit := strings.TrimSpace(it.GetSystemdUnit()); unit != "" {
			log.Debug("MakeHa: start item is a systemd unit", zap.String("unit", unit))
			startItems = append(startItems, HaStartItem{SystemdUnit: unit})
		} else {
			log.Debug("MakeHa: start item has neither agent nor unit, skipped")
		}
	}
	configPath, err := s.resources.MakeHa(ctx, req.Resource, req.Services, req.MountPoint, req.Fstype, req.Vip, ocfAgents, startItems)
	if err != nil {
		log.Debug("MakeHa: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.MakeHaResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("MakeHa: created", zap.String("resource", req.Resource), zap.String("path", configPath))
	return &haifypb.MakeHaResponse{
		Success:    true,
		Message:    "HA configuration created successfully",
		ConfigPath: configPath,
	}, nil
}

// ListResourceAgents lists the OCF resource agents available on the nodes.
func (s *Server) ListResourceAgents(ctx context.Context, req *haifypb.ListResourceAgentsRequest) (*haifypb.ListResourceAgentsResponse, error) {
	log := s.serverLog()
	log.Debug("ListResourceAgents: listing")
	agents, err := s.resources.ListResourceAgents(ctx)
	if err != nil {
		log.Debug("ListResourceAgents: failed", zap.Error(err))
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &haifypb.ListResourceAgentsResponse{}
	for _, a := range agents {
		resp.Agents = append(resp.Agents, &haifypb.ResourceAgentInfo{
			Provider:  a.Provider,
			Name:      a.Name,
			Shortdesc: a.Shortdesc,
		})
	}
	log.Debug("ListResourceAgents: listed", zap.Int("count", len(resp.Agents)))
	return resp, nil
}

// GetResourceAgentMetadata returns an OCF agent's parsed meta-data parameter schema.
func (s *Server) GetResourceAgentMetadata(ctx context.Context, req *haifypb.GetResourceAgentMetadataRequest) (*haifypb.GetResourceAgentMetadataResponse, error) {
	log := s.serverLog()
	log.Debug("GetResourceAgentMetadata: reading", zap.String("provider", req.Provider), zap.String("agent", req.Name))
	meta, err := s.resources.GetResourceAgentMetadata(ctx, req.Provider, req.Name)
	if err != nil {
		log.Debug("GetResourceAgentMetadata: not found", zap.String("provider", req.Provider), zap.String("agent", req.Name), zap.Error(err))
		return nil, status.Error(codes.NotFound, err.Error())
	}
	resp := &haifypb.GetResourceAgentMetadataResponse{
		Provider:  meta.Provider,
		Name:      meta.Name,
		Version:   meta.Version,
		Shortdesc: meta.Shortdesc,
		Longdesc:  meta.Longdesc,
	}
	for _, p := range meta.Parameters {
		resp.Parameters = append(resp.Parameters, &haifypb.ResourceAgentParameter{
			Name:      p.Name,
			Required:  p.Required,
			Unique:    p.Unique,
			Type:      p.Type,
			Default:   p.Default,
			Shortdesc: p.Shortdesc,
			Longdesc:  p.Longdesc,
		})
	}
	log.Debug("GetResourceAgentMetadata: read", zap.String("agent", meta.Name), zap.String("version", meta.Version),
		zap.Int("parameters", len(resp.Parameters)))
	return resp, nil
}

// GetHaToml reads a resource's drbd-reactor promoter TOML.
func (s *Server) GetHaToml(ctx context.Context, req *haifypb.GetHaTomlRequest) (*haifypb.GetHaTomlResponse, error) {
	log := s.serverLog()
	log.Debug("GetHaToml: reading", zap.String("resource", req.Resource))
	path, content, err := s.resources.GetHaToml(ctx, req.Resource)
	if err != nil {
		log.Debug("GetHaToml: not found", zap.String("resource", req.Resource), zap.Error(err))
		return nil, status.Error(codes.NotFound, err.Error())
	}
	log.Debug("GetHaToml: read", zap.String("resource", req.Resource), zap.String("path", path), zap.Int("bytes", len(content)))
	return &haifypb.GetHaTomlResponse{
		Resource: req.Resource,
		Path:     path,
		Content:  content,
	}, nil
}

// SyncHaToml writes an edited promoter TOML to all resource nodes and reloads drbd-reactor.
func (s *Server) SyncHaToml(ctx context.Context, req *haifypb.SyncHaTomlRequest) (*haifypb.SyncHaTomlResponse, error) {
	log := s.serverLog()
	log.Debug("SyncHaToml: syncing", zap.String("resource", req.Resource), zap.Int("bytes", len(req.Content)))
	message, err := s.resources.SyncHaToml(ctx, req.Resource, req.Content)
	if err != nil {
		log.Debug("SyncHaToml: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.SyncHaTomlResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("SyncHaToml: synced", zap.String("resource", req.Resource))
	return &haifypb.SyncHaTomlResponse{
		Success: true,
		Message: message,
	}, nil
}

func (s *Server) EnableSelfHa(ctx context.Context, req *haifypb.EnableSelfHaRequest) (*haifypb.EnableSelfHaResponse, error) {
	log := s.serverLog()
	log.Debug("EnableSelfHa: enabling", zap.String("vip", req.Vip), zap.String("pool", req.Pool), zap.Uint32("size_gb", req.SizeGb),
		zap.Uint32("port", req.Port), zap.Strings("nodes", req.Nodes))
	handoffLog, err := s.resources.EnableSelfHa(ctx, req.Vip, req.Pool, req.SizeGb, req.Port, req.Nodes)
	if err != nil {
		log.Debug("EnableSelfHa: failed", zap.Error(err))
		return &haifypb.EnableSelfHaResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("EnableSelfHa: handoff started", zap.String("resource", SelfHaResource), zap.String("handoff_log", handoffLog))
	return &haifypb.EnableSelfHaResponse{
		Success:    true,
		Message:    "Self-HA handoff started; the controller will restart under drbd-reactor management",
		Resource:   SelfHaResource,
		HandoffLog: handoffLog,
	}, nil
}

func (s *Server) DisableSelfHa(ctx context.Context, req *haifypb.DisableSelfHaRequest) (*haifypb.DisableSelfHaResponse, error) {
	log := s.serverLog()
	log.Debug("DisableSelfHa: disabling", zap.String("node", req.Node))
	if err := s.resources.DisableSelfHa(ctx, req.Node); err != nil {
		log.Debug("DisableSelfHa: failed", zap.String("node", req.Node), zap.Error(err))
		return &haifypb.DisableSelfHaResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("DisableSelfHa: started", zap.String("node", req.Node))
	return &haifypb.DisableSelfHaResponse{
		Success: true,
		Message: "Self-HA disable started; the controller will restart standalone on " + req.Node,
	}, nil
}

func (s *Server) GetSelfHaStatus(ctx context.Context, req *haifypb.GetSelfHaStatusRequest) (*haifypb.GetSelfHaStatusResponse, error) {
	log := s.serverLog()
	log.Debug("GetSelfHaStatus: reading")
	status, err := s.resources.GetSelfHaStatus(ctx)
	if err != nil {
		log.Debug("GetSelfHaStatus: failed", zap.Error(err))
		return &haifypb.GetSelfHaStatusResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("GetSelfHaStatus: read", zap.Bool("enabled", status.Enabled), zap.String("resource", status.Resource),
		zap.String("vip", status.VIP), zap.Strings("nodes", status.Nodes), zap.String("active_node", status.ActiveNode))
	return &haifypb.GetSelfHaStatusResponse{
		Success:    true,
		Message:    "OK",
		Enabled:    status.Enabled,
		Resource:   status.Resource,
		Vip:        status.VIP,
		Nodes:      status.Nodes,
		ActiveNode: status.ActiveNode,
	}, nil
}

func (s *Server) EvictHa(ctx context.Context, req *haifypb.EvictHaRequest) (*haifypb.EvictHaResponse, error) {
	log := s.serverLog()
	log.Debug("EvictHa: evicting", zap.String("resource", req.Resource))
	err := s.resources.EvictHa(ctx, req.Resource)
	if err != nil {
		log.Debug("EvictHa: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.EvictHaResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("EvictHa: evicted", zap.String("resource", req.Resource))
	return &haifypb.EvictHaResponse{
		Success: true,
		Message: "HA resource evicted successfully",
	}, nil
}

func (s *Server) DeleteHa(ctx context.Context, req *haifypb.DeleteHaRequest) (*haifypb.DeleteHaResponse, error) {
	log := s.serverLog()
	log.Debug("DeleteHa: deleting", zap.String("resource", req.Resource))
	err := s.resources.RemoveHa(ctx, req.Resource)
	if err != nil {
		log.Debug("DeleteHa: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.DeleteHaResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("DeleteHa: deleted", zap.String("resource", req.Resource))
	return &haifypb.DeleteHaResponse{
		Success: true,
		Message: "HA configuration deleted successfully",
	}, nil
}

func (s *Server) GetHa(ctx context.Context, req *haifypb.GetHaRequest) (*haifypb.GetHaResponse, error) {
	log := s.serverLog()
	log.Debug("GetHa: reading", zap.String("resource", req.Resource))
	haCfg, err := s.resources.GetHaConfig(ctx, req.Resource)
	if err != nil {
		log.Debug("GetHa: not found", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.GetHaResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	log.Debug("GetHa: found", zap.String("resource", req.Resource))
	return &haifypb.GetHaResponse{
		Success: true,
		Message: "HA configuration found",
		Config:  haConfigInfo(haCfg),
	}, nil
}

func (s *Server) ListHa(ctx context.Context, req *haifypb.ListHaRequest) (*haifypb.ListHaResponse, error) {
	log := s.serverLog()
	log.Debug("ListHa: listing")
	haConfigs, err := s.resources.ListHaConfigs(ctx)
	if err != nil {
		log.Debug("ListHa: failed", zap.Error(err))
		return &haifypb.ListHaResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	var pbConfigs []*haifypb.HaConfigInfo
	for _, cfg := range haConfigs {
		pbConfigs = append(pbConfigs, haConfigInfo(cfg))
	}

	log.Debug("ListHa: listed", zap.Int("count", len(pbConfigs)))
	return &haifypb.ListHaResponse{
		Success: true,
		Message: "HA configurations listed successfully",
		Configs: pbConfigs,
	}, nil
}

func (s *Server) GetHaStatus(ctx context.Context, req *haifypb.GetHaStatusRequest) (*haifypb.GetHaStatusResponse, error) {
	log := s.serverLog()
	log.Debug("GetHaStatus: reading", zap.String("resource", req.Resource))
	promoters, err := s.resources.GetHaStatus(ctx, req.Resource)
	if err != nil {
		log.Debug("GetHaStatus: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.GetHaStatusResponse{Success: false, Message: err.Error()}, nil
	}

	var pbPromoters []*haifypb.HaPromoterStatus
	for _, p := range promoters {
		pbPromoters = append(pbPromoters, haPromoterStatusPB(p))
	}

	log.Debug("GetHaStatus: read", zap.String("resource", req.Resource), zap.Int("promoters", len(pbPromoters)))
	return &haifypb.GetHaStatusResponse{
		Success:   true,
		Message:   "HA status retrieved successfully",
		Promoters: pbPromoters,
	}, nil
}

// haPromoterStatusPB renders a promoter's status for the API.
func haPromoterStatusPB(p *HaPromoterStatus) *haifypb.HaPromoterStatus {
	zap.L().Debug("haPromoterStatusPB: promoter", zap.String("resource", p.DRBDResource), zap.String("primary", p.PrimaryOn),
		zap.String("status", p.Status), zap.String("active_node", p.ActiveNode), zap.Int("deps", len(p.Deps)),
		zap.Int("standbys", len(p.Standbys)), zap.Int("outside", len(p.Outside)))
	pb := &haifypb.HaPromoterStatus{
		DrbdResource:            p.DRBDResource,
		PrimaryOn:               p.PrimaryOn,
		Status:                  p.Status,
		Target:                  haServiceStatusPB(p.Target),
		ActiveNode:              p.ActiveNode,
		OutsideSessions:         int32(p.OutsideSessions),
		OutsideSessionProcesses: int32(p.OutsideSessionProcesses),
	}
	for _, d := range p.Deps {
		pb.Deps = append(pb.Deps, haServiceStatusPB(d))
	}
	for _, sb := range p.Standbys {
		zap.L().Debug("haPromoterStatusPB: standby", zap.String("node", sb.Node), zap.Bool("ready", sb.Ready),
			zap.String("disk_state", sb.DiskState))
		pb.Standbys = append(pb.Standbys, &haifypb.HaStandby{
			Node: sb.Node, Ready: sb.Ready, Reasons: sb.Reasons, DiskState: sb.DiskState,
		})
	}
	for _, o := range p.Outside {
		zap.L().Debug("haPromoterStatusPB: unit outside the chain", zap.String("unit", o.Unit), zap.Int("processes", o.Processes))
		pb.Outside = append(pb.Outside, &haifypb.HaOutsideService{
			Unit: o.Unit, Description: o.Description, Processes: int32(o.Processes), MountPoints: o.MountPoints,
		})
	}
	return pb
}

func haServiceStatusPB(s HaServiceStatus) *haifypb.HaServiceStatus {
	return &haifypb.HaServiceStatus{
		Name:        s.Name,
		Status:      s.Status,
		Kind:        s.Kind,
		Label:       s.Label,
		Description: s.Description,
		Since:       s.Since,
		Restarts:    int32(s.Restarts),
		SubState:    s.SubState,
		MainPid:     int32(s.MainPID),
	}
}

// haConfigInfo renders a stored HA config for the API, start order included.
func haConfigInfo(cfg *database.HaConfig) *haifypb.HaConfigInfo {
	zap.L().Debug("haConfigInfo: rendering", zap.String("resource", cfg.Resource), zap.String("vip", cfg.VIP),
		zap.Int("services", len(cfg.Services)), zap.Int("ocf_agents", len(cfg.OcfAgents)), zap.Int("start_items", len(cfg.StartItems)))
	ocf := func(a database.HaOcfAgent) *haifypb.OcfAgent {
		return &haifypb.OcfAgent{Provider: a.Provider, Name: a.Name, Instance: a.Instance, Params: a.Params}
	}
	info := &haifypb.HaConfigInfo{
		Resource:   cfg.Resource,
		Vip:        cfg.VIP,
		MountPoint: cfg.MountPoint,
		FsType:     cfg.FsType,
		Services:   cfg.Services,
	}
	for _, a := range cfg.OcfAgents {
		info.OcfAgents = append(info.OcfAgents, ocf(a))
	}
	for _, it := range cfg.StartItems {
		if it.Ocf != nil {
			info.StartItems = append(info.StartItems, &haifypb.HaStartItem{Item: &haifypb.HaStartItem_Ocf{Ocf: ocf(*it.Ocf)}})
		} else {
			info.StartItems = append(info.StartItems, &haifypb.HaStartItem{Item: &haifypb.HaStartItem_SystemdUnit{SystemdUnit: it.SystemdUnit}})
		}
	}
	return info
}
