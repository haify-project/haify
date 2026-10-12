package controller

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// Self-healing RPCs (self_heal_*.go).

func (s *Server) MoveReplica(ctx context.Context, req *haifypb.MoveReplicaRequest) (*haifypb.MoveReplicaResponse, error) {
	log := s.serverLog()
	log.Debug("MoveReplica: moving", zap.String("resource", req.Resource), zap.String("from", req.From), zap.String("to", req.To))
	if err := s.resources.MoveReplica(ctx, req.Resource, req.From, req.To); err != nil {
		log.Debug("MoveReplica: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.MoveReplicaResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("MoveReplica: started", zap.String("resource", req.Resource), zap.String("from", req.From), zap.String("to", req.To))
	return &haifypb.MoveReplicaResponse{Success: true, Message: fmt.Sprintf(
		"a replica of %s is being added on %s; the one on %s is removed once it is in sync", req.Resource, req.To, req.From)}, nil
}

func (s *Server) PlanRebalance(ctx context.Context, req *haifypb.PlanRebalanceRequest) (*haifypb.PlanRebalanceResponse, error) {
	log := s.serverLog()
	log.Debug("PlanRebalance: planning", zap.Uint32("max_moves", req.MaxMoves), zap.Bool("apply", req.Apply))
	moves, notes, err := s.resources.PlanRebalance(ctx, int(req.MaxMoves))
	if err != nil {
		log.Debug("PlanRebalance: planning failed", zap.Error(err))
		return &haifypb.PlanRebalanceResponse{Success: false, Message: err.Error()}, nil
	}
	resp := &haifypb.PlanRebalanceResponse{Success: true, Notes: notes}
	for _, m := range moves {
		log.Debug("PlanRebalance: move", zap.String("resource", m.Resource), zap.String("from", m.From),
			zap.String("to", m.To), zap.Any("size_gb", m.SizeGB))
		resp.Moves = append(resp.Moves, &haifypb.RebalanceMove{Resource: m.Resource, From: m.From, To: m.To, SizeGb: m.SizeGB})
	}
	switch {
	case len(moves) == 0:
		log.Debug("PlanRebalance: balanced, nothing to move", zap.Int("notes", len(notes)))
		resp.Message = "the nodes are balanced; nothing to move"
	case req.Apply:
		log.Debug("PlanRebalance: applying moves", zap.Int("moves", len(moves)))
		if err := s.resources.ApplyRebalance(moves); err != nil {
			log.Debug("PlanRebalance: apply failed", zap.Error(err))
			return &haifypb.PlanRebalanceResponse{Success: false, Message: err.Error(), Moves: resp.Moves}, nil
		}
		resp.Message = fmt.Sprintf("running %d move(s), one at a time", len(moves))
	default:
		log.Debug("PlanRebalance: proposal only", zap.Int("moves", len(moves)))
		resp.Message = fmt.Sprintf("%d move(s) proposed; nothing changed", len(moves))
	}
	return resp, nil
}

func (s *Server) MarkNodeLost(ctx context.Context, req *haifypb.MarkNodeLostRequest) (*haifypb.MarkNodeLostResponse, error) {
	log := s.serverLog()
	log.Debug("MarkNodeLost: marking lost", zap.String("node", req.Node))
	done, err := s.resources.NodeLost(ctx, req.Node)
	if err != nil {
		log.Debug("MarkNodeLost: failed", zap.String("node", req.Node), zap.Strings("resources", done), zap.Error(err))
		return &haifypb.MarkNodeLostResponse{Success: false, Message: err.Error(), Resources: done}, nil
	}
	log.Debug("MarkNodeLost: done", zap.String("node", req.Node), zap.Strings("resources", done))
	return &haifypb.MarkNodeLostResponse{Success: true, Resources: done,
		Message: fmt.Sprintf("%s is marked lost; %d replica(s) removed", req.Node, len(done))}, nil
}

func (s *Server) RestoreNode(ctx context.Context, req *haifypb.RestoreNodeRequest) (*haifypb.RestoreNodeResponse, error) {
	log := s.serverLog()
	log.Debug("RestoreNode: restoring", zap.String("node", req.Node), zap.Bool("dry_run", req.DryRun))
	plan, err := s.resources.NodeRestore(ctx, req.Node, req.DryRun)
	if err != nil {
		log.Debug("RestoreNode: failed", zap.String("node", req.Node), zap.Strings("plan", plan), zap.Error(err))
		return &haifypb.RestoreNodeResponse{Success: false, Message: err.Error(), Plan: plan}, nil
	}
	msg := fmt.Sprintf("%s is restored and may take replicas again", req.Node)
	if req.DryRun {
		msg = "dry run; nothing changed"
	}
	log.Debug("RestoreNode: done", zap.String("node", req.Node), zap.Bool("dry_run", req.DryRun), zap.Strings("plan", plan))
	return &haifypb.RestoreNodeResponse{Success: true, Message: msg, Plan: plan}, nil
}

func (s *Server) SetHaPreferredNodes(ctx context.Context, req *haifypb.SetHaPreferredNodesRequest) (*haifypb.SetHaPreferredNodesResponse, error) {
	log := s.serverLog()
	log.Debug("SetHaPreferredNodes: setting", zap.String("resource", req.Resource), zap.Strings("nodes", req.Nodes),
		zap.String("policy", req.Policy))
	if err := s.resources.SetHaPreferredNodes(ctx, req.Resource, req.Nodes, req.Policy); err != nil {
		log.Debug("SetHaPreferredNodes: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.SetHaPreferredNodesResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("SetHaPreferredNodes: set", zap.String("resource", req.Resource))
	return &haifypb.SetHaPreferredNodesResponse{Success: true,
		Message: fmt.Sprintf("preferred nodes of %s set; the nodes that would take over use them now", req.Resource)}, nil
}
