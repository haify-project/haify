package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

func (s *Server) RegisterNode(ctx context.Context, req *haifypb.RegisterNodeRequest) (*haifypb.RegisterNodeResponse, error) {
	node, err := s.nodes.RegisterNodeWithReplicationAddress(ctx, req.Name, req.Address, req.ReplicationAddress)
	if err != nil {
		return &haifypb.RegisterNodeResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.RegisterNodeResponse{
		Success: true,
		Message: "Node registered successfully",
		Node: &haifypb.NodeInfo{
			Name:               node.Name,
			Address:            node.Address,
			ReplicationAddress: node.ReplicationAddress,
			ReplicationPaths:   node.ReplicationPaths,
			Hostname:           node.Hostname,
			State:              string(node.State),
			LastSeen:           node.LastSeen.Unix(),
			OfflineSince:       unixOrZero(node.OfflineSince),
			Version:            node.Version,
			Labels:             node.Labels,
		},
	}, nil
}

func (s *Server) UnregisterNode(ctx context.Context, req *haifypb.UnregisterNodeRequest) (*haifypb.UnregisterNodeResponse, error) {
	err := s.nodes.UnregisterNode(ctx, req.Address)
	if err != nil {
		return &haifypb.UnregisterNodeResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.UnregisterNodeResponse{
		Success: true,
		Message: "Node unregistered successfully",
	}, nil
}

func (s *Server) DrainNode(ctx context.Context, req *haifypb.DrainNodeRequest) (*haifypb.DrainNodeResponse, error) {
	moved, err := s.resources.DrainNode(ctx, req.Name)
	if err != nil {
		return &haifypb.DrainNodeResponse{Success: false, Message: err.Error(), ResourcesMoved: moved}, nil
	}
	return &haifypb.DrainNodeResponse{
		Success:        true,
		Message:        fmt.Sprintf("node %q drained; %d resource(s) moved", req.Name, len(moved)),
		ResourcesMoved: moved,
	}, nil
}

func (s *Server) UndrainNode(ctx context.Context, req *haifypb.UndrainNodeRequest) (*haifypb.UndrainNodeResponse, error) {
	if err := s.resources.UndrainNode(ctx, req.Name); err != nil {
		return &haifypb.UndrainNodeResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.UndrainNodeResponse{Success: true, Message: fmt.Sprintf("node %q returned to service", req.Name)}, nil
}

func (s *Server) GetNode(ctx context.Context, req *haifypb.GetNodeRequest) (*haifypb.GetNodeResponse, error) {
	node, err := s.nodes.GetNode(ctx, req.Address)
	if err != nil {
		return &haifypb.GetNodeResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.GetNodeResponse{
		Success: true,
		Message: "Node found",
		Node: &haifypb.NodeInfo{
			Name:               node.Name,
			Address:            node.Address,
			Hostname:           node.Hostname,
			State:              string(node.State),
			LastSeen:           node.LastSeen.Unix(),
			OfflineSince:       unixOrZero(node.OfflineSince),
			Version:            node.Version,
			Labels:             node.Labels,
			ReplicationAddress: node.ReplicationAddress,
			ReplicationPaths:   node.ReplicationPaths,
		},
	}, nil
}

func (s *Server) SetNodeAddress(ctx context.Context, req *haifypb.SetNodeAddressRequest) (*haifypb.SetNodeAddressResponse, error) {
	moves := []AddressMove{{Node: req.Node, Address: req.Address, ReplicationAddress: req.ReplicationAddress}}
	if len(req.Moves) > 0 {
		if req.Node != "" || req.Address != "" {
			return &haifypb.SetNodeAddressResponse{Success: false, Message: "give node and address, or moves, not both"}, nil
		}
		moves = moves[:0]
		for _, m := range req.Moves {
			moves = append(moves, AddressMove{Node: m.Node, Address: m.Address, ReplicationAddress: m.ReplicationAddress})
		}
	}
	change, err := s.nodes.SetNodeAddresses(ctx, moves)
	if err != nil {
		return &haifypb.SetNodeAddressResponse{Success: false, Message: err.Error()}, nil
	}
	s.resources.RenumberInResources(ctx, change)
	for _, name := range change.WANResources {
		resp, err := s.RepairWanProxy(ctx, &haifypb.RepairWanProxyRequest{Name: name})
		switch {
		case err != nil:
			change.Failed = append(change.Failed, fmt.Sprintf("%s: rebuild WAN proxy: %v", name, err))
		case !resp.Success:
			change.Failed = append(change.Failed, fmt.Sprintf("%s: rebuild WAN proxy: %s", name, resp.Message))
		}
	}
	parts := make([]string, 0, len(change.Moves))
	for _, mv := range change.Moves {
		p := fmt.Sprintf("%s moved from %s to %s", mv.Node, mv.OldAddress, mv.Address)
		if mv.ReplicationAddress != mv.Address {
			p += fmt.Sprintf(" (DRBD on %s)", mv.ReplicationAddress)
		}
		parts = append(parts, p)
	}
	msg := strings.Join(parts, "; ")
	return &haifypb.SetNodeAddressResponse{
		Success:   len(change.Failed) == 0,
		Message:   msg,
		Resources: change.Resources,
		Failed:    change.Failed,
	}, nil
}

func (s *Server) SetNodeLabels(ctx context.Context, req *haifypb.SetNodeLabelsRequest) (*haifypb.SetNodeLabelsResponse, error) {
	node, err := s.nodes.SetNodeLabels(ctx, req.Node, req.Labels, req.Replace)
	if err != nil {
		return &haifypb.SetNodeLabelsResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.SetNodeLabelsResponse{
		Success: true,
		Message: "Node labels updated",
		Node: &haifypb.NodeInfo{
			Name:               node.Name,
			Address:            node.Address,
			Hostname:           node.Hostname,
			State:              string(node.State),
			LastSeen:           node.LastSeen.Unix(),
			OfflineSince:       unixOrZero(node.OfflineSince),
			Version:            node.Version,
			Labels:             node.Labels,
			ReplicationAddress: node.ReplicationAddress,
			ReplicationPaths:   node.ReplicationPaths,
		},
	}, nil
}

func (s *Server) ListNodes(ctx context.Context, req *haifypb.ListNodesRequest) (*haifypb.ListNodesResponse, error) {
	nodes, err := s.nodes.ListNodes(ctx)
	if err != nil {
		return &haifypb.ListNodesResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	var pbNodes []*haifypb.NodeInfo
	for _, n := range nodes {
		pbNodes = append(pbNodes, &haifypb.NodeInfo{
			Name:               n.Name,
			Address:            n.Address,
			Hostname:           n.Hostname,
			State:              string(n.State),
			LastSeen:           n.LastSeen.Unix(),
			OfflineSince:       unixOrZero(n.OfflineSince),
			Version:            n.Version,
			Labels:             n.Labels,
			ReplicationAddress: n.ReplicationAddress,
		})
	}

	return &haifypb.ListNodesResponse{
		Success: true,
		Message: "Nodes listed successfully",
		Nodes:   pbNodes,
	}, nil
}

func (s *Server) HealthCheck(ctx context.Context, req *haifypb.HealthCheckRequest) (*haifypb.HealthCheckResponse, error) {
	health, err := s.nodes.HealthCheck(ctx, req.Node)
	if err != nil {
		return &haifypb.HealthCheckResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	return &haifypb.HealthCheckResponse{
		Success: true,
		Message: "Health check completed",
		Health: &haifypb.NodeHealthInfo{
			DrbdInstalled:           health.DrbdInstalled,
			DrbdVersion:             health.DrbdVersion,
			DrbdReactorInstalled:    health.DrbdReactorInstalled,
			DrbdReactorVersion:      health.DrbdReactorVersion,
			DrbdReactorRunning:      health.DrbdReactorRunning,
			ResourceAgentsInstalled: health.ResourceAgentsInstalled,
			AvailableAgents:         health.AvailableAgents,
		},
	}, nil
}

// unixOrZero is t in Unix seconds, or 0 for the zero time.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
