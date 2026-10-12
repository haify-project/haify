package controller

import (
	"context"
	"fmt"
	"strings"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
)

// Resource lifecycle and role RPCs; the read-only views (get, list, status)
// are in server_resources_query.go. DRBD options can carry a shared-secret,
// so traces log how many there are, never their values.

func (s *Server) CreateResource(ctx context.Context, req *haifypb.CreateResourceRequest) (*haifypb.CreateResourceResponse, error) {
	log := s.serverLog()
	log.Debug("CreateResource: creating", zap.String("resource", req.Name), zap.Uint32("port", req.Port),
		zap.Strings("nodes", req.Nodes), zap.String("protocol", req.Protocol), zap.String("storage_type", req.StorageType),
		zap.String("pool", req.Pool), zap.Uint32("size_gb", req.SizeGb), zap.Uint64("size_bytes", req.SizeBytes),
		zap.Int("volumes", len(req.Volumes)), zap.Uint32("replicas", req.Replicas), zap.String("profile", req.Profile),
		zap.Bool("wan", req.Wan), zap.Bool("encrypt", req.Encrypt), zap.Int("drbd_options", len(req.DrbdOptions)))
	if req.Profile != "" {
		if s.ctrl == nil || s.ctrl.db == nil {
			log.Debug("CreateResource: profile given but no database", zap.String("profile", req.Profile))
			return &haifypb.CreateResourceResponse{Success: false, Message: "database not available"}, nil
		}
		profile, err := s.ctrl.db.GetResourceProfile(ctx, req.Profile)
		if err != nil {
			log.Debug("CreateResource: profile not found", zap.String("profile", req.Profile), zap.Error(err))
			return &haifypb.CreateResourceResponse{Success: false, Message: err.Error()}, nil
		}
		log.Debug("CreateResource: applying profile", zap.String("profile", profile.Name))
		applyResourceProfile(req, profile)
	}

	// Prefer the explicit multi-volume list; fall back to the single-volume
	// size_gb/pool shorthand when it is empty (older clients, CLI).
	volumes := make([]VolumeSpec, 0, len(req.Volumes))
	for i, v := range req.Volumes {
		log.Debug("CreateResource: volume", zap.Int("volume", i), zap.Uint32("size_gb", v.SizeGb),
			zap.Uint64("size_bytes", v.SizeBytes), zap.String("pool", v.Pool))
		volumes = append(volumes, VolumeSpec{SizeGB: v.SizeGb, Pool: v.Pool, SizeBytes: v.SizeBytes})
	}
	if len(volumes) == 0 {
		log.Debug("CreateResource: no volume list, using the single-volume shorthand", zap.Uint32("size_gb", req.SizeGb),
			zap.String("pool", req.Pool))
		volumes = append(volumes, VolumeSpec{SizeGB: req.SizeGb, Pool: req.Pool, SizeBytes: req.SizeBytes})
	}

	// WAN master switch: the dr_* / wan_port fields only apply when --wan is set.
	// Reject a partial request clearly instead of silently ignoring the DR
	// fields; when --wan is off we pass nil so the LAN path is unchanged.
	var wan *WANSpec
	if req.Wan {
		log.Debug("CreateResource: WAN mode", zap.String("dr_node", req.DrNode), zap.String("dr_endpoint", req.DrEndpoint),
			zap.Uint32("wan_port", req.WanPort), zap.String("egress_address", req.WanEgressAddress))
		wan = &WANSpec{
			DRNode:        req.DrNode,
			DREndpoint:    req.DrEndpoint,
			WANPort:       req.WanPort,
			EgressAddress: strings.TrimSpace(req.WanEgressAddress),
		}
	} else if req.DrNode != "" || req.DrEndpoint != "" || req.WanPort != 0 {
		log.Debug("CreateResource: DR fields without WAN mode, refusing", zap.String("dr_node", req.DrNode))
		return &haifypb.CreateResourceResponse{
			Success: false,
			Message: "dr_node/dr_endpoint/wan_port require --wan (WAN mode is off)",
		}, nil
	}

	// Auto-placement: no explicit node list means "pick for me". Choose the
	// nodes with the most free space in the target pool. Not available for WAN
	// (which needs an explicit primary + DR endpoint), and all volumes must
	// share one pool so there is a single capacity target to place against.
	nodes := req.Nodes
	var placementWarning string
	if len(nodes) == 0 {
		if wan != nil {
			log.Debug("CreateResource: WAN resource without nodes, refusing auto-placement")
			return &haifypb.CreateResourceResponse{
				Success: false,
				Message: "WAN resources require an explicit --nodes primary; auto-placement is LAN-only",
			}, nil
		}
		pool, total, perr := singlePoolTotal(volumes)
		if perr != nil {
			log.Debug("CreateResource: volumes cannot be auto-placed", zap.Error(perr))
			return &haifypb.CreateResourceResponse{Success: false, Message: perr.Error()}, nil
		}
		replicas := int(req.Replicas)
		if replicas == 0 {
			log.Debug("CreateResource: no replica count, defaulting to 2")
			replicas = 2
		}
		log.Debug("CreateResource: auto-placing", zap.String("pool", pool), zap.Uint32("size_gb", total), zap.Int("replicas", replicas),
			zap.Strings("on_different", req.ReplicasOnDifferent), zap.Strings("on_same", req.ReplicasOnSame),
			zap.Strings("do_not_place_with", req.DoNotPlaceWith))
		placed, warn, perr := s.resources.selectPlacementNodes(ctx, pool, total, replicas, req.ReplicasOnDifferent, req.ReplicasOnSame, req.DoNotPlaceWith)
		if perr != nil {
			log.Debug("CreateResource: auto-placement failed", zap.Error(perr))
			return &haifypb.CreateResourceResponse{Success: false, Message: perr.Error()}, nil
		}
		nodes, placementWarning = placed, warn
		log.Debug("CreateResource: auto-placed", zap.Strings("nodes", nodes), zap.String("warning", placementWarning))
	}

	err := s.resources.CreateResourceWithVolumesMetadata(ctx, req.Name, req.Port, nodes, req.Protocol, req.StorageType, req.DrbdOptions, volumes, wan, ResourceMetadata{
		Labels:  req.Labels,
		Profile: req.Profile,
		Encrypt: req.Encrypt,
	})
	if err != nil {
		log.Debug("CreateResource: failed", zap.String("resource", req.Name), zap.Strings("nodes", nodes), zap.Error(err))
		return &haifypb.CreateResourceResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	msg := "Resource created successfully"
	if placementWarning != "" {
		msg += "; warning: " + placementWarning
	}
	log.Debug("CreateResource: created", zap.String("resource", req.Name), zap.Strings("nodes", nodes),
		zap.Bool("placement_warning", placementWarning != ""))
	return &haifypb.CreateResourceResponse{
		Success: true,
		Message: msg,
	}, nil
}

func applyResourceProfile(req *haifypb.CreateResourceRequest, profile *database.ResourceProfile) {
	log := zap.L()
	log.Debug("applyResourceProfile: filling request from profile", zap.String("resource", req.Name), zap.String("profile", profile.Name))
	if req.Protocol == "" {
		log.Debug("applyResourceProfile: protocol from profile", zap.String("protocol", profile.Protocol))
		req.Protocol = profile.Protocol
	}
	if req.StorageType == "" {
		log.Debug("applyResourceProfile: storage type from profile", zap.String("storage_type", profile.StorageType))
		req.StorageType = profile.StorageType
	}
	if req.Pool == "" {
		log.Debug("applyResourceProfile: pool from profile", zap.String("pool", profile.Pool))
		req.Pool = profile.Pool
	}
	for i, volume := range req.Volumes {
		if volume.Pool == "" {
			log.Debug("applyResourceProfile: volume pool from profile", zap.Int("volume", i), zap.String("pool", profile.Pool))
			volume.Pool = profile.Pool
		}
	}
	if req.Replicas == 0 {
		log.Debug("applyResourceProfile: replicas from profile", zap.Int("replicas", profile.Replicas))
		req.Replicas = uint32(profile.Replicas)
	}
	if len(req.ReplicasOnDifferent) == 0 {
		req.ReplicasOnDifferent = append([]string(nil), profile.OnDifferent...)
	}
	if len(req.ReplicasOnSame) == 0 {
		req.ReplicasOnSame = append([]string(nil), profile.OnSame...)
	}
	req.DrbdOptions = mergeStringMaps(profile.DRBDOptions, req.DrbdOptions)
	req.Labels = mergeStringMaps(profile.Labels, req.Labels)
	req.Profile = profile.Name
	log.Debug("applyResourceProfile: applied", zap.String("resource", req.Name), zap.Int("drbd_options", len(req.DrbdOptions)),
		zap.Int("labels", len(req.Labels)))
}

func mergeStringMaps(defaults, overrides map[string]string) map[string]string {
	if len(defaults) == 0 && len(overrides) == 0 {
		return nil
	}
	merged := make(map[string]string, len(defaults)+len(overrides))
	for key, value := range defaults {
		merged[key] = value
	}
	for key, value := range overrides {
		merged[key] = value
	}
	return merged
}

// singlePoolTotal returns the shared pool and total size of the volumes, or an
// error if they do not all target one pool (auto-placement needs a single
// capacity target). An empty per-volume pool is allowed only when every volume
// omits it — the controller's own default pool selection then applies.
func singlePoolTotal(volumes []VolumeSpec) (string, uint32, error) {
	if len(volumes) == 0 {
		zap.L().Debug("singlePoolTotal: no volumes")
		return "", 0, fmt.Errorf("no volumes to place")
	}
	pool := volumes[0].Pool
	var total uint32
	for _, v := range volumes {
		if v.Pool != pool {
			zap.L().Debug("singlePoolTotal: volumes in different pools", zap.String("pool", pool), zap.String("other_pool", v.Pool))
			return "", 0, fmt.Errorf("auto-placement requires all volumes in one pool (got %q and %q); pass --nodes to place manually", pool, v.Pool)
		}
		total += v.SizeGB
	}
	zap.L().Debug("singlePoolTotal: one pool", zap.String("pool", pool), zap.Uint32("size_gb", total), zap.Int("volumes", len(volumes)))
	return pool, total, nil
}

func (s *Server) AdoptResource(ctx context.Context, req *haifypb.AdoptResourceRequest) (*haifypb.AdoptResourceResponse, error) {
	log := s.serverLog()
	log.Debug("AdoptResource: adopting", zap.String("resource", req.Name), zap.Strings("nodes", req.Nodes),
		zap.Uint32("port", req.Port), zap.String("protocol", req.Protocol))
	result, err := s.resources.AdoptResource(ctx, req.Name, req.Nodes, req.Port, req.Protocol)
	if err != nil {
		log.Debug("AdoptResource: failed", zap.String("resource", req.Name), zap.Error(err))
		return &haifypb.AdoptResourceResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("AdoptResource: adopted", zap.String("resource", req.Name), zap.Strings("nodes", result.Nodes),
		zap.Uint32("port", result.Port), zap.Int("volumes", result.Volumes))
	return &haifypb.AdoptResourceResponse{
		Success:  true,
		Message:  "Resource adopted successfully",
		Nodes:    result.Nodes,
		Port:     result.Port,
		Protocol: result.Protocol,
		Volumes:  uint32(result.Volumes),
	}, nil
}

func (s *Server) DeleteResource(ctx context.Context, req *haifypb.DeleteResourceRequest) (*haifypb.DeleteResourceResponse, error) {
	log := s.serverLog()
	log.Debug("DeleteResource: deleting", zap.String("resource", req.Name))
	err := s.resources.DeleteResource(ctx, req.Name, true)
	if err != nil {
		log.Debug("DeleteResource: failed", zap.String("resource", req.Name), zap.Error(err))
		return &haifypb.DeleteResourceResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("DeleteResource: deleted", zap.String("resource", req.Name))
	return &haifypb.DeleteResourceResponse{
		Success: true,
		Message: "Resource deleted successfully",
	}, nil
}

func (s *Server) RepairResource(ctx context.Context, req *haifypb.RepairResourceRequest) (*haifypb.RepairResourceResponse, error) {
	log := s.serverLog()
	log.Debug("RepairResource: reconciling config", zap.String("resource", req.Name))
	if err := s.resources.RepairResourceConfig(ctx, req.Name); err != nil {
		log.Debug("RepairResource: config reconcile failed", zap.String("resource", req.Name), zap.Error(err))
		return &haifypb.RepairResourceResponse{Success: false, Message: err.Error()}, nil
	}
	// Promoters belong on exactly the primary-site replicas; a replica added
	// or removed by an older version, or a DR node given one, is put right.
	log.Debug("RepairResource: config applied, placing promoters", zap.String("resource", req.Name))
	if err := s.resources.SyncPromoters(ctx, req.Name); err != nil {
		log.Debug("RepairResource: promoter placement failed", zap.String("resource", req.Name), zap.Error(err))
		return &haifypb.RepairResourceResponse{Success: false,
			Message: "resource config reconciled and applied, but its promoters could not be placed: " + err.Error()}, nil
	}
	log.Debug("RepairResource: repaired", zap.String("resource", req.Name))
	return &haifypb.RepairResourceResponse{Success: true,
		Message: "Resource config reconciled on every participant and applied; promoters on the primary-site replicas only"}, nil
}

func (s *Server) UpdateResourceOptions(ctx context.Context, req *haifypb.UpdateResourceOptionsRequest) (*haifypb.UpdateResourceOptionsResponse, error) {
	log := s.serverLog()
	log.Debug("UpdateResourceOptions: setting options", zap.String("resource", req.Name), zap.Int("options", len(req.Options)))
	if err := s.resources.SetOptions(ctx, req.Name, req.Options); err != nil {
		log.Debug("UpdateResourceOptions: failed", zap.String("resource", req.Name), zap.Error(err))
		return &haifypb.UpdateResourceOptionsResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("UpdateResourceOptions: applied", zap.String("resource", req.Name))
	return &haifypb.UpdateResourceOptionsResponse{
		Success: true,
		Message: "Resource options updated and applied",
	}, nil
}

func (s *Server) SetPrimary(ctx context.Context, req *haifypb.SetPrimaryRequest) (*haifypb.SetPrimaryResponse, error) {
	log := s.serverLog()
	log.Debug("SetPrimary: promoting", zap.String("resource", req.Resource), zap.String("node", req.Node),
		zap.Bool("force", req.Force), zap.Bool("quorum_guarded", req.QuorumGuarded))
	var err error
	if req.QuorumGuarded {
		// Quorum-guarded promote: try normal, escalate to --force only if the
		// node holds DRBD quorum, refuse otherwise. `force` is ignored here.
		log.Debug("SetPrimary: quorum-guarded promote, force ignored", zap.String("resource", req.Resource))
		err = s.resources.PromoteForNode(ctx, req.Resource, req.Node)
	} else {
		err = s.resources.SetPrimary(ctx, req.Resource, req.Node, req.Force)
	}
	if err != nil {
		log.Debug("SetPrimary: failed", zap.String("resource", req.Resource), zap.String("node", req.Node), zap.Error(err))
		return &haifypb.SetPrimaryResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("SetPrimary: promoted", zap.String("resource", req.Resource), zap.String("node", req.Node))
	return &haifypb.SetPrimaryResponse{
		Success: true,
		Message: "Resource set to Primary successfully",
	}, nil
}

func (s *Server) SetSecondary(ctx context.Context, req *haifypb.SetSecondaryRequest) (*haifypb.SetSecondaryResponse, error) {
	log := s.serverLog()
	log.Debug("SetSecondary: demoting", zap.String("resource", req.Resource), zap.String("node", req.Node))
	err := s.resources.SetSecondary(ctx, req.Resource, req.Node)
	if err != nil {
		log.Debug("SetSecondary: failed", zap.String("resource", req.Resource), zap.String("node", req.Node), zap.Error(err))
		return &haifypb.SetSecondaryResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("SetSecondary: demoted", zap.String("resource", req.Resource), zap.String("node", req.Node))
	return &haifypb.SetSecondaryResponse{
		Success: true,
		Message: "Resource set to Secondary successfully",
	}, nil
}

// SetDualPrimary toggles allow-two-primaries for the Proxmox live-migration
// window. See ResourceManager.SetDualPrimary for the safety rules (WAN refused,
// disable idempotent + verified).
func (s *Server) SetDualPrimary(ctx context.Context, req *haifypb.SetDualPrimaryRequest) (*haifypb.SetDualPrimaryResponse, error) {
	log := s.serverLog()
	log.Debug("SetDualPrimary: toggling", zap.String("resource", req.Resource), zap.Bool("enable", req.Enable), zap.Strings("nodes", req.Nodes))
	if err := s.resources.SetDualPrimaryOn(ctx, req.Resource, req.Enable, req.Nodes); err != nil {
		log.Debug("SetDualPrimary: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.SetDualPrimaryResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	msg := "Dual-primary disabled"
	if req.Enable {
		msg = "Dual-primary enabled for the live-migration window"
	}
	log.Debug("SetDualPrimary: done", zap.String("resource", req.Resource), zap.Bool("enable", req.Enable))
	return &haifypb.SetDualPrimaryResponse{
		Success: true,
		Message: msg,
	}, nil
}
