package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

func (s *Server) RegisterNode(ctx context.Context, req *haifypb.RegisterNodeRequest) (*haifypb.RegisterNodeResponse, error) {
	log := s.serverLog()
	log.Debug("RegisterNode: registering", zap.String("node", req.Name), zap.String("address", req.Address),
		zap.String("replication_address", req.ReplicationAddress), zap.Bool("local_agent", req.LocalAgent))
	var node *NodeInfo
	var err error
	if req.LocalAgent {
		node, err = s.nodes.RegisterLocalAgentNode(ctx, req.Name, req.Address, req.Hostname)
	} else {
		node, err = s.nodes.RegisterNodeWithReplicationAddress(ctx, req.Name, req.Address, req.ReplicationAddress)
	}
	if err != nil {
		log.Debug("RegisterNode: failed", zap.String("node", req.Name), zap.Error(err))
		return &haifypb.RegisterNodeResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("RegisterNode: registered", zap.String("node", node.Name), zap.String("address", node.Address),
		zap.String("state", string(node.State)), zap.String("version", node.Version))
	return &haifypb.RegisterNodeResponse{
		Success: true,
		Message: "Node registered successfully",
		Node:    nodeInfoPB(node),
	}, nil
}

// GetNodeAgentState is what the agent on a local-agent node polls: the
// resources it must have up and their configuration (node_local_agent.go).
func (s *Server) GetNodeAgentState(ctx context.Context, req *haifypb.GetNodeAgentStateRequest) (*haifypb.GetNodeAgentStateResponse, error) {
	log := s.serverLog()
	log.Debug("GetNodeAgentState: agent polling", zap.String("node", req.Node))
	resources, err := s.resources.AgentState(ctx, req.Node)
	if err != nil {
		log.Debug("GetNodeAgentState: failed", zap.String("node", req.Node), zap.Error(err))
		return &haifypb.GetNodeAgentStateResponse{Success: false, Message: err.Error()}, nil
	}
	out := make([]*haifypb.AgentResource, 0, len(resources))
	for _, r := range resources {
		out = append(out, &haifypb.AgentResource{Name: r.Name, Config: r.Config})
	}
	log.Debug("GetNodeAgentState: answered", zap.String("node", req.Node), zap.Int("resources", len(out)))
	return &haifypb.GetNodeAgentStateResponse{Success: true, Resources: out}, nil
}

func (s *Server) UnregisterNode(ctx context.Context, req *haifypb.UnregisterNodeRequest) (*haifypb.UnregisterNodeResponse, error) {
	log := s.serverLog()
	log.Debug("UnregisterNode: unregistering", zap.String("address", req.Address))
	err := s.nodes.UnregisterNode(ctx, req.Address)
	if err != nil {
		log.Debug("UnregisterNode: failed", zap.String("address", req.Address), zap.Error(err))
		return &haifypb.UnregisterNodeResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("UnregisterNode: unregistered", zap.String("address", req.Address))
	return &haifypb.UnregisterNodeResponse{
		Success: true,
		Message: "Node unregistered successfully",
	}, nil
}

func (s *Server) DrainNode(ctx context.Context, req *haifypb.DrainNodeRequest) (*haifypb.DrainNodeResponse, error) {
	log := s.serverLog()
	log.Debug("DrainNode: draining", zap.String("node", req.Name))
	moved, err := s.resources.DrainNode(ctx, req.Name)
	if err != nil {
		log.Debug("DrainNode: failed", zap.String("node", req.Name), zap.Strings("moved", moved), zap.Error(err))
		return &haifypb.DrainNodeResponse{Success: false, Message: err.Error(), ResourcesMoved: moved}, nil
	}
	log.Debug("DrainNode: drained", zap.String("node", req.Name), zap.Strings("moved", moved))
	return &haifypb.DrainNodeResponse{
		Success:        true,
		Message:        fmt.Sprintf("node %q drained; %d resource(s) moved", req.Name, len(moved)),
		ResourcesMoved: moved,
	}, nil
}

func (s *Server) UndrainNode(ctx context.Context, req *haifypb.UndrainNodeRequest) (*haifypb.UndrainNodeResponse, error) {
	log := s.serverLog()
	log.Debug("UndrainNode: returning node to service", zap.String("node", req.Name))
	if err := s.resources.UndrainNode(ctx, req.Name); err != nil {
		log.Debug("UndrainNode: failed", zap.String("node", req.Name), zap.Error(err))
		return &haifypb.UndrainNodeResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("UndrainNode: done", zap.String("node", req.Name))
	return &haifypb.UndrainNodeResponse{Success: true, Message: fmt.Sprintf("node %q returned to service", req.Name)}, nil
}

func (s *Server) GetNode(ctx context.Context, req *haifypb.GetNodeRequest) (*haifypb.GetNodeResponse, error) {
	log := s.serverLog()
	log.Debug("GetNode: looking up", zap.String("address", req.Address))
	node, err := s.nodes.GetNode(ctx, req.Address)
	if err != nil {
		log.Debug("GetNode: failed", zap.String("address", req.Address), zap.Error(err))
		return &haifypb.GetNodeResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("GetNode: found", zap.String("node", node.Name), zap.String("state", string(node.State)))
	return &haifypb.GetNodeResponse{
		Success: true,
		Message: "Node found",
		Node:    nodeInfoPB(node),
	}, nil
}

func (s *Server) SetNodeAddress(ctx context.Context, req *haifypb.SetNodeAddressRequest) (*haifypb.SetNodeAddressResponse, error) {
	log := s.serverLog()
	log.Debug("SetNodeAddress: renumbering", zap.String("node", req.Node), zap.String("address", req.Address),
		zap.String("replication_address", req.ReplicationAddress), zap.Int("moves", len(req.Moves)))
	moves := []AddressMove{{Node: req.Node, Address: req.Address, ReplicationAddress: req.ReplicationAddress}}
	if len(req.Moves) > 0 {
		if req.Node != "" || req.Address != "" {
			log.Debug("SetNodeAddress: both a single node and moves given, refusing")
			return &haifypb.SetNodeAddressResponse{Success: false, Message: "give node and address, or moves, not both"}, nil
		}
		moves = moves[:0]
		for _, m := range req.Moves {
			log.Debug("SetNodeAddress: move", zap.String("node", m.Node), zap.String("address", m.Address),
				zap.String("replication_address", m.ReplicationAddress))
			moves = append(moves, AddressMove{Node: m.Node, Address: m.Address, ReplicationAddress: m.ReplicationAddress})
		}
	}
	change, err := s.nodes.SetNodeAddresses(ctx, moves)
	if err != nil {
		log.Debug("SetNodeAddress: registry update failed", zap.Int("moves", len(moves)), zap.Error(err))
		return &haifypb.SetNodeAddressResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("SetNodeAddress: registry updated, renumbering resources", zap.Int("moves", len(change.Moves)),
		zap.Strings("wan_resources", change.WANResources))
	s.resources.RenumberInResources(ctx, change)
	for _, name := range change.WANResources {
		log.Debug("SetNodeAddress: rebuilding WAN proxy", zap.String("resource", name))
		resp, err := s.RepairWanProxy(ctx, &haifypb.RepairWanProxyRequest{Name: name})
		switch {
		case err != nil:
			log.Debug("SetNodeAddress: WAN proxy rebuild errored", zap.String("resource", name), zap.Error(err))
			change.Failed = append(change.Failed, fmt.Sprintf("%s: rebuild WAN proxy: %v", name, err))
		case !resp.Success:
			log.Debug("SetNodeAddress: WAN proxy rebuild failed", zap.String("resource", name), zap.String("message", resp.Message))
			change.Failed = append(change.Failed, fmt.Sprintf("%s: rebuild WAN proxy: %s", name, resp.Message))
		default:
			log.Debug("SetNodeAddress: WAN proxy rebuilt", zap.String("resource", name))
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
	log.Debug("SetNodeAddress: done", zap.Int("moves", len(change.Moves)), zap.Strings("resources", change.Resources),
		zap.Strings("failed", change.Failed))
	return &haifypb.SetNodeAddressResponse{
		Success:   len(change.Failed) == 0,
		Message:   msg,
		Resources: change.Resources,
		Failed:    change.Failed,
	}, nil
}

func (s *Server) SetNodeLabels(ctx context.Context, req *haifypb.SetNodeLabelsRequest) (*haifypb.SetNodeLabelsResponse, error) {
	log := s.serverLog()
	log.Debug("SetNodeLabels: updating labels", zap.String("node", req.Node), zap.Any("labels", req.Labels),
		zap.Bool("replace", req.Replace))
	node, err := s.nodes.SetNodeLabels(ctx, req.Node, req.Labels, req.Replace)
	if err != nil {
		log.Debug("SetNodeLabels: failed", zap.String("node", req.Node), zap.Error(err))
		return &haifypb.SetNodeLabelsResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("SetNodeLabels: updated", zap.String("node", node.Name), zap.Int("labels", len(node.Labels)))
	return &haifypb.SetNodeLabelsResponse{
		Success: true,
		Message: "Node labels updated",
		Node:    nodeInfoPB(node),
	}, nil
}

func (s *Server) ListNodes(ctx context.Context, req *haifypb.ListNodesRequest) (*haifypb.ListNodesResponse, error) {
	log := s.serverLog()
	log.Debug("ListNodes: listing")
	nodes, err := s.nodes.ListNodes(ctx)
	if err != nil {
		log.Debug("ListNodes: failed", zap.Error(err))
		return &haifypb.ListNodesResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	var pbNodes []*haifypb.NodeInfo
	for _, n := range nodes {
		log.Debug("ListNodes: node", zap.String("node", n.Name), zap.String("address", n.Address),
			zap.String("state", string(n.State)))
		pbNodes = append(pbNodes, nodeInfoPB(n))
	}

	log.Debug("ListNodes: listed", zap.Int("count", len(pbNodes)))
	return &haifypb.ListNodesResponse{
		Success: true,
		Message: "Nodes listed successfully",
		Nodes:   pbNodes,
	}, nil
}

func (s *Server) HealthCheck(ctx context.Context, req *haifypb.HealthCheckRequest) (*haifypb.HealthCheckResponse, error) {
	log := s.serverLog()
	log.Debug("HealthCheck: checking node", zap.String("node", req.Node))
	health, err := s.nodes.HealthCheck(ctx, req.Node)
	if err != nil {
		log.Debug("HealthCheck: failed", zap.String("node", req.Node), zap.Error(err))
		return &haifypb.HealthCheckResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	log.Debug("HealthCheck: done", zap.String("node", req.Node), zap.Bool("drbd", health.DrbdInstalled),
		zap.String("drbd_version", health.DrbdVersion), zap.Bool("reactor", health.DrbdReactorInstalled),
		zap.Bool("reactor_running", health.DrbdReactorRunning), zap.Bool("resource_agents", health.ResourceAgentsInstalled),
		zap.Int("agents", len(health.AvailableAgents)))
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

// nodeInfoPB is the API form of a node.
func nodeInfoPB(n *NodeInfo) *haifypb.NodeInfo {
	return &haifypb.NodeInfo{
		Name:               n.Name,
		Address:            n.Address,
		ReplicationAddress: n.ReplicationAddress,
		ReplicationPaths:   n.ReplicationPaths,
		Hostname:           n.Hostname,
		State:              string(n.State),
		LastSeen:           n.LastSeen.Unix(),
		OfflineSince:       unixOrZero(n.OfflineSince),
		Version:            n.Version,
		Labels:             n.Labels,
		LocalAgent:         n.LocalAgent,
	}
}
