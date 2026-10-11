package controller

import (
	"context"
	"fmt"
	"strings"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
)

func (s *Server) CreateResource(ctx context.Context, req *haifypb.CreateResourceRequest) (*haifypb.CreateResourceResponse, error) {
	if req.Profile != "" {
		if s.ctrl == nil || s.ctrl.db == nil {
			return &haifypb.CreateResourceResponse{Success: false, Message: "database not available"}, nil
		}
		profile, err := s.ctrl.db.GetResourceProfile(ctx, req.Profile)
		if err != nil {
			return &haifypb.CreateResourceResponse{Success: false, Message: err.Error()}, nil
		}
		applyResourceProfile(req, profile)
	}

	// Prefer the explicit multi-volume list; fall back to the single-volume
	// size_gb/pool shorthand when it is empty (older clients, CLI).
	volumes := make([]VolumeSpec, 0, len(req.Volumes))
	for _, v := range req.Volumes {
		volumes = append(volumes, VolumeSpec{SizeGB: v.SizeGb, Pool: v.Pool, SizeBytes: v.SizeBytes})
	}
	if len(volumes) == 0 {
		volumes = append(volumes, VolumeSpec{SizeGB: req.SizeGb, Pool: req.Pool, SizeBytes: req.SizeBytes})
	}

	// WAN master switch: the dr_* / wan_port fields only apply when --wan is set.
	// Reject a partial request clearly instead of silently ignoring the DR
	// fields; when --wan is off we pass nil so the LAN path is unchanged.
	var wan *WANSpec
	if req.Wan {
		wan = &WANSpec{
			DRNode:        req.DrNode,
			DREndpoint:    req.DrEndpoint,
			WANPort:       req.WanPort,
			EgressAddress: strings.TrimSpace(req.WanEgressAddress),
		}
	} else if req.DrNode != "" || req.DrEndpoint != "" || req.WanPort != 0 {
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
			return &haifypb.CreateResourceResponse{
				Success: false,
				Message: "WAN resources require an explicit --nodes primary; auto-placement is LAN-only",
			}, nil
		}
		pool, total, perr := singlePoolTotal(volumes)
		if perr != nil {
			return &haifypb.CreateResourceResponse{Success: false, Message: perr.Error()}, nil
		}
		replicas := int(req.Replicas)
		if replicas == 0 {
			replicas = 2
		}
		placed, warn, perr := s.resources.selectPlacementNodes(ctx, pool, total, replicas, req.ReplicasOnDifferent, req.ReplicasOnSame, req.DoNotPlaceWith)
		if perr != nil {
			return &haifypb.CreateResourceResponse{Success: false, Message: perr.Error()}, nil
		}
		nodes, placementWarning = placed, warn
	}

	err := s.resources.CreateResourceWithVolumesMetadata(ctx, req.Name, req.Port, nodes, req.Protocol, req.StorageType, req.DrbdOptions, volumes, wan, ResourceMetadata{
		Labels:  req.Labels,
		Profile: req.Profile,
		Encrypt: req.Encrypt,
	})
	if err != nil {
		return &haifypb.CreateResourceResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	msg := "Resource created successfully"
	if placementWarning != "" {
		msg += "; warning: " + placementWarning
	}
	return &haifypb.CreateResourceResponse{
		Success: true,
		Message: msg,
	}, nil
}

func applyResourceProfile(req *haifypb.CreateResourceRequest, profile *database.ResourceProfile) {
	if req.Protocol == "" {
		req.Protocol = profile.Protocol
	}
	if req.StorageType == "" {
		req.StorageType = profile.StorageType
	}
	if req.Pool == "" {
		req.Pool = profile.Pool
	}
	for _, volume := range req.Volumes {
		if volume.Pool == "" {
			volume.Pool = profile.Pool
		}
	}
	if req.Replicas == 0 {
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
		return "", 0, fmt.Errorf("no volumes to place")
	}
	pool := volumes[0].Pool
	var total uint32
	for _, v := range volumes {
		if v.Pool != pool {
			return "", 0, fmt.Errorf("auto-placement requires all volumes in one pool (got %q and %q); pass --nodes to place manually", pool, v.Pool)
		}
		total += v.SizeGB
	}
	return pool, total, nil
}

func (s *Server) AdoptResource(ctx context.Context, req *haifypb.AdoptResourceRequest) (*haifypb.AdoptResourceResponse, error) {
	result, err := s.resources.AdoptResource(ctx, req.Name, req.Nodes, req.Port, req.Protocol)
	if err != nil {
		return &haifypb.AdoptResourceResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
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
	err := s.resources.DeleteResource(ctx, req.Name, true)
	if err != nil {
		return &haifypb.DeleteResourceResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.DeleteResourceResponse{
		Success: true,
		Message: "Resource deleted successfully",
	}, nil
}

func (s *Server) GetResource(ctx context.Context, req *haifypb.GetResourceRequest) (*haifypb.GetResourceResponse, error) {
	resource, err := s.resources.GetResource(ctx, req.Name)
	if err != nil {
		return &haifypb.GetResourceResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	var pbVolumes []*haifypb.VolumeInfo
	for _, v := range resource.Volumes {
		pbVolumes = append(pbVolumes, &haifypb.VolumeInfo{
			VolumeId:      v.VolumeID,
			Device:        v.Device,
			SizeGb:        v.SizeGB,
			SizeBytes:     v.SizeBytes,
			Pool:          v.Pool,
			BackingVolume: v.BackingVolume,
			Encrypted:     v.Encrypted,
		})
	}

	// Build node states map
	nodeStates := make(map[string]*haifypb.NodeResourceState)
	for node, state := range resource.NodeStates {
		nodeStates[node] = &haifypb.NodeResourceState{
			Role:             state.Role,
			DiskState:        state.DiskState,
			ReplicationState: state.Replication,
			SyncPercent:      state.SyncPercent,
			Connection:       state.Connection,
			Tls:              state.TLS,
			WrittenKib:       state.WrittenKiB,
			ReadKib:          state.ReadKiB,
			SentKib:          state.SentKiB,
			ReceivedKib:      state.ReceivedKiB,
		}
	}

	return &haifypb.GetResourceResponse{
		Success: true,
		Message: "Resource found",
		Resource: &haifypb.ResourceInfo{
			Name:            resource.Name,
			Port:            resource.Port,
			Protocol:        resource.Protocol,
			Nodes:           resource.Nodes,
			Role:            resource.Role,
			Volumes:         pbVolumes,
			NodeStates:      nodeStates,
			DisklessNodes:   resource.DisklessNodes,
			DisklessClients: resource.DisklessClients,
			QuorumRisk:      resource.QuorumRisk,
			FaultDomainRisk: resource.FaultDomainRisk,
			WanMode:         resource.WANMode,
			DrNode:          resource.DRNode,
			Encrypted:       resource.Encrypted,
			Labels:          resource.Labels,
			Profile:         resource.Profile,
		},
	}, nil
}

func (s *Server) ListResources(ctx context.Context, req *haifypb.ListResourcesRequest) (*haifypb.ListResourcesResponse, error) {
	resources, err := s.resources.ListResources(ctx)
	if err != nil {
		return &haifypb.ListResourcesResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	var pbResources []*haifypb.ResourceInfo
	for _, r := range resources {
		if req.GetProfile() != "" && r.Profile != req.GetProfile() {
			continue
		}
		var pbVolumes []*haifypb.VolumeInfo
		for _, v := range r.Volumes {
			pbVolumes = append(pbVolumes, &haifypb.VolumeInfo{
				VolumeId:      v.VolumeID,
				Device:        v.Device,
				SizeGb:        v.SizeGB,
				SizeBytes:     v.SizeBytes,
				Pool:          v.Pool,
				BackingVolume: v.BackingVolume,
				Encrypted:     v.Encrypted,
			})
		}
		pbResources = append(pbResources, &haifypb.ResourceInfo{
			Name:            r.Name,
			Port:            r.Port,
			Protocol:        r.Protocol,
			Nodes:           r.Nodes,
			Role:            r.Role,
			Volumes:         pbVolumes,
			DisklessNodes:   r.DisklessNodes,
			DisklessClients: r.DisklessClients,
			QuorumRisk:      r.QuorumRisk,
			FaultDomainRisk: r.FaultDomainRisk,
			WanMode:         r.WANMode,
			DrNode:          r.DRNode,
			Encrypted:       r.Encrypted,
			Labels:          r.Labels,
			Profile:         r.Profile,
		})
	}

	return &haifypb.ListResourcesResponse{
		Success:   true,
		Message:   "Resources listed successfully",
		Resources: pbResources,
	}, nil
}

func (s *Server) RepairResource(ctx context.Context, req *haifypb.RepairResourceRequest) (*haifypb.RepairResourceResponse, error) {
	if err := s.resources.RepairResourceConfig(ctx, req.Name); err != nil {
		return &haifypb.RepairResourceResponse{Success: false, Message: err.Error()}, nil
	}
	// Promoters belong on exactly the primary-site replicas; a replica added
	// or removed by an older version, or a DR node given one, is put right.
	if err := s.resources.SyncPromoters(ctx, req.Name); err != nil {
		return &haifypb.RepairResourceResponse{Success: false,
			Message: "resource config reconciled and applied, but its promoters could not be placed: " + err.Error()}, nil
	}
	return &haifypb.RepairResourceResponse{Success: true,
		Message: "Resource config reconciled on every participant and applied; promoters on the primary-site replicas only"}, nil
}

func (s *Server) UpdateResourceOptions(ctx context.Context, req *haifypb.UpdateResourceOptionsRequest) (*haifypb.UpdateResourceOptionsResponse, error) {
	if err := s.resources.SetOptions(ctx, req.Name, req.Options); err != nil {
		return &haifypb.UpdateResourceOptionsResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.UpdateResourceOptionsResponse{
		Success: true,
		Message: "Resource options updated and applied",
	}, nil
}

func (s *Server) ResourceStatus(ctx context.Context, req *haifypb.ResourceStatusRequest) (*haifypb.ResourceStatusResponse, error) {
	// Get resource detailed status
	resource, err := s.resources.GetResource(ctx, req.Name)
	if err != nil {
		return &haifypb.ResourceStatusResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	// Convert to status format with detailed node states
	status := &haifypb.ResourceStatus{
		Name:       resource.Name,
		Role:       resource.Role,
		Nodes:      resource.Nodes,
		Encrypted:  resource.Encrypted,
		NodeStates: make(map[string]*haifypb.NodeResourceState),
	}

	// Convert node states from endpoint key to hostname key
	for endpoint, nodeState := range resource.NodeStates {
		// Get hostname for this endpoint
		nodeInfo, err := s.nodes.GetNode(ctx, endpoint)
		hostname := endpoint
		if err == nil && nodeInfo.Hostname != "" {
			hostname = nodeInfo.Hostname
		}

		nodeName := endpoint
		if err == nil && nodeInfo.Name != "" {
			nodeName = nodeInfo.Name
		}
		status.NodeStates[hostname] = &haifypb.NodeResourceState{
			Role:             nodeState.Role,
			DiskState:        nodeState.DiskState,
			ReplicationState: nodeState.Replication,
			SyncPercent:      nodeState.SyncPercent,
			Node:             nodeName,
			Connection:       nodeState.Connection,
			Tls:              nodeState.TLS,
			WrittenKib:       nodeState.WrittenKiB,
			ReadKib:          nodeState.ReadKiB,
			SentKib:          nodeState.SentKiB,
			ReceivedKib:      nodeState.ReceivedKiB,
		}
	}

	for _, v := range resource.Volumes {
		status.Volumes = append(status.Volumes, &haifypb.VolumeInfo{
			VolumeId:      v.VolumeID,
			Device:        v.Device,
			SizeGb:        v.SizeGB,
			SizeBytes:     v.SizeBytes,
			Pool:          v.Pool,
			BackingVolume: v.BackingVolume,
			Encrypted:     v.Encrypted,
		})
	}

	// Quorum arithmetic — the number that decides whether this resource keeps
	// serving. Best effort: it is derived from a live probe, and a resource that
	// is down should still report the rest of its status.
	if q, qerr := s.resources.Quorum(ctx, req.Name); qerr == nil && q != nil {
		status.Quorum = &haifypb.QuorumInfo{
			Members:   int32(q.Members),
			Required:  int32(q.Required),
			Online:    int32(q.Online),
			HasQuorum: q.HasQuorum,
			Tolerated: int32(q.Tolerated),
		}
	}

	// WAN replication view (nil for a LAN resource, so the fields stay zero).
	if wan, werr := s.resources.WANStatus(ctx, req.Name); werr == nil && wan != nil {
		status.Wan = true
		status.DrNode = wan.DRNode
		status.DrEndpoint = wan.DREndpoint
		status.WanPort = uint32(wan.WANPort)
		status.WanProxy = wan.ProxyState
		status.WanReachable = wan.WANReachable
		// Left nil when the proxy published nothing, so the client can tell
		// "unknown" from "no backlog".
		if m := wan.Metrics; m != nil {
			status.WanMetrics = &haifypb.WANMetrics{
				BufferUsedBytes:   m.BufferUsedBytes,
				BufferCapBytes:    m.BufferCapBytes,
				BufferFillPercent: m.BufferFillPercent,
				DrbdToWanBytes:    m.DRBDToWANBytes,
				WanWireBytes:      m.WANWireBytes,
				WanToDrbdBytes:    m.WANToDRBDBytes,
				FramesSent:        m.FramesSent,
				CompressionRatio:  m.CompressionRat,
				WanConnected:      m.WANConnected,
				Reconnects:        m.Reconnects,
				RingFullEvents:    m.RingFullEvents,
			}
		}
	}

	return &haifypb.ResourceStatusResponse{
		Success: true,
		Message: "Resource status retrieved",
		Status:  status,
	}, nil
}

func (s *Server) SetPrimary(ctx context.Context, req *haifypb.SetPrimaryRequest) (*haifypb.SetPrimaryResponse, error) {
	var err error
	if req.QuorumGuarded {
		// Quorum-guarded promote: try normal, escalate to --force only if the
		// node holds DRBD quorum, refuse otherwise. `force` is ignored here.
		err = s.resources.PromoteForNode(ctx, req.Resource, req.Node)
	} else {
		err = s.resources.SetPrimary(ctx, req.Resource, req.Node, req.Force)
	}
	if err != nil {
		return &haifypb.SetPrimaryResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.SetPrimaryResponse{
		Success: true,
		Message: "Resource set to Primary successfully",
	}, nil
}

func (s *Server) SetSecondary(ctx context.Context, req *haifypb.SetSecondaryRequest) (*haifypb.SetSecondaryResponse, error) {
	err := s.resources.SetSecondary(ctx, req.Resource, req.Node)
	if err != nil {
		return &haifypb.SetSecondaryResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.SetSecondaryResponse{
		Success: true,
		Message: "Resource set to Secondary successfully",
	}, nil
}

// SetDualPrimary toggles allow-two-primaries for the Proxmox live-migration
// window. See ResourceManager.SetDualPrimary for the safety rules (WAN refused,
// disable idempotent + verified).
func (s *Server) SetDualPrimary(ctx context.Context, req *haifypb.SetDualPrimaryRequest) (*haifypb.SetDualPrimaryResponse, error) {
	if err := s.resources.SetDualPrimaryOn(ctx, req.Resource, req.Enable, req.Nodes); err != nil {
		return &haifypb.SetDualPrimaryResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	msg := "Dual-primary disabled"
	if req.Enable {
		msg = "Dual-primary enabled for the live-migration window"
	}
	return &haifypb.SetDualPrimaryResponse{
		Success: true,
		Message: msg,
	}, nil
}
