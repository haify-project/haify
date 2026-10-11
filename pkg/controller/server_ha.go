package controller

import (
	"context"
	"strings"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Server) MakeHa(ctx context.Context, req *haifypb.MakeHaRequest) (*haifypb.MakeHaResponse, error) {
	var ocfAgents []OcfAgentSpec
	for _, a := range req.OcfAgents {
		if a == nil {
			continue
		}
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
			continue
		}
		if ocf := it.GetOcf(); ocf != nil {
			startItems = append(startItems, HaStartItem{Ocf: &OcfAgentSpec{
				Provider: ocf.Provider,
				Name:     ocf.Name,
				Instance: ocf.Instance,
				Params:   ocf.Params,
			}})
		} else if unit := strings.TrimSpace(it.GetSystemdUnit()); unit != "" {
			startItems = append(startItems, HaStartItem{SystemdUnit: unit})
		}
	}
	configPath, err := s.resources.MakeHa(ctx, req.Resource, req.Services, req.MountPoint, req.Fstype, req.Vip, ocfAgents, startItems)
	if err != nil {
		return &haifypb.MakeHaResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.MakeHaResponse{
		Success:    true,
		Message:    "HA configuration created successfully",
		ConfigPath: configPath,
	}, nil
}

// ListResourceAgents lists the OCF resource agents available on the nodes.
func (s *Server) ListResourceAgents(ctx context.Context, req *haifypb.ListResourceAgentsRequest) (*haifypb.ListResourceAgentsResponse, error) {
	agents, err := s.resources.ListResourceAgents(ctx)
	if err != nil {
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
	return resp, nil
}

// GetResourceAgentMetadata returns an OCF agent's parsed meta-data parameter schema.
func (s *Server) GetResourceAgentMetadata(ctx context.Context, req *haifypb.GetResourceAgentMetadataRequest) (*haifypb.GetResourceAgentMetadataResponse, error) {
	meta, err := s.resources.GetResourceAgentMetadata(ctx, req.Provider, req.Name)
	if err != nil {
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
	return resp, nil
}

// GetHaToml reads a resource's drbd-reactor promoter TOML.
func (s *Server) GetHaToml(ctx context.Context, req *haifypb.GetHaTomlRequest) (*haifypb.GetHaTomlResponse, error) {
	path, content, err := s.resources.GetHaToml(ctx, req.Resource)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &haifypb.GetHaTomlResponse{
		Resource: req.Resource,
		Path:     path,
		Content:  content,
	}, nil
}

// SyncHaToml writes an edited promoter TOML to all resource nodes and reloads drbd-reactor.
func (s *Server) SyncHaToml(ctx context.Context, req *haifypb.SyncHaTomlRequest) (*haifypb.SyncHaTomlResponse, error) {
	message, err := s.resources.SyncHaToml(ctx, req.Resource, req.Content)
	if err != nil {
		return &haifypb.SyncHaTomlResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.SyncHaTomlResponse{
		Success: true,
		Message: message,
	}, nil
}

func (s *Server) EnableSelfHa(ctx context.Context, req *haifypb.EnableSelfHaRequest) (*haifypb.EnableSelfHaResponse, error) {
	handoffLog, err := s.resources.EnableSelfHa(ctx, req.Vip, req.Pool, req.SizeGb, req.Port, req.Nodes)
	if err != nil {
		return &haifypb.EnableSelfHaResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.EnableSelfHaResponse{
		Success:    true,
		Message:    "Self-HA handoff started; the controller will restart under drbd-reactor management",
		Resource:   SelfHaResource,
		HandoffLog: handoffLog,
	}, nil
}

func (s *Server) DisableSelfHa(ctx context.Context, req *haifypb.DisableSelfHaRequest) (*haifypb.DisableSelfHaResponse, error) {
	if err := s.resources.DisableSelfHa(ctx, req.Node); err != nil {
		return &haifypb.DisableSelfHaResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.DisableSelfHaResponse{
		Success: true,
		Message: "Self-HA disable started; the controller will restart standalone on " + req.Node,
	}, nil
}

func (s *Server) GetSelfHaStatus(ctx context.Context, req *haifypb.GetSelfHaStatusRequest) (*haifypb.GetSelfHaStatusResponse, error) {
	status, err := s.resources.GetSelfHaStatus(ctx)
	if err != nil {
		return &haifypb.GetSelfHaStatusResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
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
	err := s.resources.EvictHa(ctx, req.Resource)
	if err != nil {
		return &haifypb.EvictHaResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.EvictHaResponse{
		Success: true,
		Message: "HA resource evicted successfully",
	}, nil
}

func (s *Server) DeleteHa(ctx context.Context, req *haifypb.DeleteHaRequest) (*haifypb.DeleteHaResponse, error) {
	err := s.resources.RemoveHa(ctx, req.Resource)
	if err != nil {
		return &haifypb.DeleteHaResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.DeleteHaResponse{
		Success: true,
		Message: "HA configuration deleted successfully",
	}, nil
}

func (s *Server) GetHa(ctx context.Context, req *haifypb.GetHaRequest) (*haifypb.GetHaResponse, error) {
	haCfg, err := s.resources.GetHaConfig(ctx, req.Resource)
	if err != nil {
		return &haifypb.GetHaResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	return &haifypb.GetHaResponse{
		Success: true,
		Message: "HA configuration found",
		Config:  haConfigInfo(haCfg),
	}, nil
}

func (s *Server) ListHa(ctx context.Context, req *haifypb.ListHaRequest) (*haifypb.ListHaResponse, error) {
	haConfigs, err := s.resources.ListHaConfigs(ctx)
	if err != nil {
		return &haifypb.ListHaResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	var pbConfigs []*haifypb.HaConfigInfo
	for _, cfg := range haConfigs {
		pbConfigs = append(pbConfigs, haConfigInfo(cfg))
	}

	return &haifypb.ListHaResponse{
		Success: true,
		Message: "HA configurations listed successfully",
		Configs: pbConfigs,
	}, nil
}

func (s *Server) GetHaStatus(ctx context.Context, req *haifypb.GetHaStatusRequest) (*haifypb.GetHaStatusResponse, error) {
	promoters, err := s.resources.GetHaStatus(ctx, req.Resource)
	if err != nil {
		return &haifypb.GetHaStatusResponse{Success: false, Message: err.Error()}, nil
	}

	var pbPromoters []*haifypb.HaPromoterStatus
	for _, p := range promoters {
		pbPromoters = append(pbPromoters, haPromoterStatusPB(p))
	}

	return &haifypb.GetHaStatusResponse{
		Success:   true,
		Message:   "HA status retrieved successfully",
		Promoters: pbPromoters,
	}, nil
}

// haPromoterStatusPB renders a promoter's status for the API.
func haPromoterStatusPB(p *HaPromoterStatus) *haifypb.HaPromoterStatus {
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
		pb.Standbys = append(pb.Standbys, &haifypb.HaStandby{
			Node: sb.Node, Ready: sb.Ready, Reasons: sb.Reasons, DiskState: sb.DiskState,
		})
	}
	for _, o := range p.Outside {
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
