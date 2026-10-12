package controller

import (
	"context"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// Read-only resource RPCs: one resource, the list, and the detailed status.

func (s *Server) GetResource(ctx context.Context, req *haifypb.GetResourceRequest) (*haifypb.GetResourceResponse, error) {
	log := s.serverLog()
	log.Debug("GetResource: reading", zap.String("resource", req.Name))
	resource, err := s.resources.GetResource(ctx, req.Name)
	if err != nil {
		log.Debug("GetResource: failed", zap.String("resource", req.Name), zap.Error(err))
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
		log.Debug("GetResource: node state", zap.String("resource", req.Name), zap.String("node", node),
			zap.String("role", state.Role), zap.String("disk_state", state.DiskState), zap.String("connection", state.Connection))
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

	log.Debug("GetResource: found", zap.String("resource", resource.Name), zap.String("role", resource.Role),
		zap.Strings("nodes", resource.Nodes), zap.Int("volumes", len(pbVolumes)), zap.Bool("wan", resource.WANMode))
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
	log := s.serverLog()
	log.Debug("ListResources: listing", zap.String("profile", req.GetProfile()))
	resources, err := s.resources.ListResources(ctx)
	if err != nil {
		log.Debug("ListResources: failed", zap.Error(err))
		return &haifypb.ListResourcesResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	var pbResources []*haifypb.ResourceInfo
	for _, r := range resources {
		if req.GetProfile() != "" && r.Profile != req.GetProfile() {
			log.Debug("ListResources: skipping resource of another profile", zap.String("resource", r.Name), zap.String("profile", r.Profile))
			continue
		}
		log.Debug("ListResources: resource", zap.String("resource", r.Name), zap.String("role", r.Role),
			zap.Strings("nodes", r.Nodes), zap.Int("volumes", len(r.Volumes)))
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

	log.Debug("ListResources: listed", zap.Int("count", len(pbResources)), zap.Int("total", len(resources)))
	return &haifypb.ListResourcesResponse{
		Success:   true,
		Message:   "Resources listed successfully",
		Resources: pbResources,
	}, nil
}

func (s *Server) ResourceStatus(ctx context.Context, req *haifypb.ResourceStatusRequest) (*haifypb.ResourceStatusResponse, error) {
	log := s.serverLog()
	log.Debug("ResourceStatus: reading", zap.String("resource", req.Name))
	// Get resource detailed status
	resource, err := s.resources.GetResource(ctx, req.Name)
	if err != nil {
		log.Debug("ResourceStatus: failed", zap.String("resource", req.Name), zap.Error(err))
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
		log.Debug("ResourceStatus: node state", zap.String("endpoint", endpoint), zap.String("node", nodeName),
			zap.String("hostname", hostname), zap.Bool("registered", err == nil), zap.String("role", nodeState.Role),
			zap.String("disk_state", nodeState.DiskState), zap.String("connection", nodeState.Connection))
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
		log.Debug("ResourceStatus: quorum", zap.String("resource", req.Name), zap.Int("members", q.Members),
			zap.Int("required", q.Required), zap.Int("online", q.Online), zap.Bool("has_quorum", q.HasQuorum))
		status.Quorum = &haifypb.QuorumInfo{
			Members:   int32(q.Members),
			Required:  int32(q.Required),
			Online:    int32(q.Online),
			HasQuorum: q.HasQuorum,
			Tolerated: int32(q.Tolerated),
		}
	} else {
		log.Debug("ResourceStatus: quorum unavailable", zap.String("resource", req.Name), zap.Error(qerr))
	}

	// WAN replication view (nil for a LAN resource, so the fields stay zero).
	if wan, werr := s.resources.WANStatus(ctx, req.Name); werr == nil && wan != nil {
		log.Debug("ResourceStatus: WAN view", zap.String("resource", req.Name), zap.String("dr_node", wan.DRNode),
			zap.Any("proxy", wan.ProxyState), zap.Bool("reachable", wan.WANReachable), zap.Bool("has_metrics", wan.Metrics != nil))
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
	} else {
		log.Debug("ResourceStatus: no WAN view", zap.String("resource", req.Name), zap.Error(werr))
	}

	log.Debug("ResourceStatus: done", zap.String("resource", req.Name), zap.Int("node_states", len(status.NodeStates)),
		zap.Int("volumes", len(status.Volumes)))
	return &haifypb.ResourceStatusResponse{
		Success: true,
		Message: "Resource status retrieved",
		Status:  status,
	}, nil
}
