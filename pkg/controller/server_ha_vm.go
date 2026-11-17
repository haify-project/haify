package controller

import (
	"context"
	"fmt"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
)

// API handlers for HA guests: the live move and the guest's definition.

// evictHaLive starts a live move and answers as soon as it is under way.
func (s *Server) evictHaLive(ctx context.Context, req *haifypb.EvictHaRequest) *haifypb.EvictHaResponse {
	log := s.serverLog()
	rec, err := s.resources.StartLiveEvict(ctx, req.Resource, req.TargetNode, time.Duration(req.TimeoutSeconds)*time.Second)
	if err != nil {
		log.Debug("EvictHa: live move refused", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.EvictHaResponse{Success: false, Message: err.Error()}
	}
	log.Debug("EvictHa: live move started", zap.String("resource", req.Resource), zap.String("source", rec.SourceNode),
		zap.String("target", rec.TargetNode))
	return &haifypb.EvictHaResponse{Success: true, Started: true, SourceNode: rec.SourceNode, TargetNode: rec.TargetNode,
		Message: fmt.Sprintf("moving %s from %s to %s without stopping it", rec.Domain, rec.SourceNode, rec.TargetNode)}
}

// GetHaMigration reports the live move of a resource's guest.
func (s *Server) GetHaMigration(ctx context.Context, req *haifypb.GetHaMigrationRequest) (*haifypb.GetHaMigrationResponse, error) {
	log := s.serverLog()
	log.Debug("GetHaMigration: reading", zap.String("resource", req.Resource))
	rec, err := s.resources.GetHaMigration(ctx, req.Resource)
	if err != nil {
		log.Debug("GetHaMigration: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.GetHaMigrationResponse{Success: false, Message: err.Error()}, nil
	}
	if rec == nil {
		log.Debug("GetHaMigration: none", zap.String("resource", req.Resource))
		return &haifypb.GetHaMigrationResponse{Success: true, Message: "no live move of " + req.Resource + " has run"}, nil
	}
	log.Debug("GetHaMigration: read", zap.String("resource", req.Resource), zap.String("phase", rec.Phase), zap.Bool("done", rec.Done))
	return &haifypb.GetHaMigrationResponse{Success: true, Message: rec.Message, Migration: haMigrationPB(rec)}, nil
}

func haMigrationPB(rec *database.HaMigration) *haifypb.HaMigration {
	return &haifypb.HaMigration{
		Resource:        rec.Resource,
		Domain:          rec.Domain,
		SourceNode:      rec.SourceNode,
		TargetNode:      rec.TargetNode,
		Phase:           rec.Phase,
		Message:         rec.Message,
		Done:            rec.Done,
		Outcome:         rec.Outcome,
		ActiveNode:      rec.ActiveNode,
		ProgressPercent: uint32(rec.Progress),
		StartedUnix:     rec.StartedAt.Unix(),
		UpdatedUnix:     rec.UpdatedAt.Unix(),
		Steps:           rec.Steps,
	}
}

// GetVmDefinition reports a guest's definition across its replicas.
func (s *Server) GetVmDefinition(ctx context.Context, req *haifypb.GetVmDefinitionRequest) (*haifypb.VmDefinitionResponse, error) {
	log := s.serverLog()
	log.Debug("GetVmDefinition: reading", zap.String("resource", req.Resource))
	st, err := s.resources.GetVMDefinition(ctx, req.Resource)
	if err != nil {
		log.Debug("GetVmDefinition: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.VmDefinitionResponse{Success: false, Message: err.Error(), Resource: req.Resource}, nil
	}
	log.Debug("GetVmDefinition: read", zap.String("resource", req.Resource), zap.Bool("consistent", st.Consistent))
	return vmDefinitionPB(st), nil
}

// SyncVmDefinition spreads a guest's definition to its replicas.
func (s *Server) SyncVmDefinition(ctx context.Context, req *haifypb.SyncVmDefinitionRequest) (*haifypb.VmDefinitionResponse, error) {
	log := s.serverLog()
	log.Debug("SyncVmDefinition: syncing", zap.String("resource", req.Resource), zap.String("from_node", req.FromNode))
	st, err := s.resources.SyncVMDefinition(ctx, req.Resource, req.FromNode)
	if err != nil {
		log.Debug("SyncVmDefinition: failed", zap.String("resource", req.Resource), zap.Error(err))
		resp := &haifypb.VmDefinitionResponse{Resource: req.Resource}
		if st != nil {
			// Some replicas took the definition; say which did not.
			resp = vmDefinitionPB(st)
		}
		resp.Success, resp.Message = false, err.Error()
		return resp, nil
	}
	log.Debug("SyncVmDefinition: synced", zap.String("resource", req.Resource), zap.Bool("consistent", st.Consistent))
	return vmDefinitionPB(st), nil
}

func vmDefinitionPB(st *VMDefinitionStatus) *haifypb.VmDefinitionResponse {
	resp := &haifypb.VmDefinitionResponse{
		Success:     true,
		Message:     st.Message,
		Resource:    st.Resource,
		Domain:      st.Domain,
		Uuid:        st.UUID,
		SourceNode:  st.SourceNode,
		UpdatedUnix: st.UpdatedAt.Unix(),
		Consistent:  st.Consistent,
	}
	for _, n := range st.Nodes {
		resp.Nodes = append(resp.Nodes, &haifypb.VmDefinitionNode{Node: n.Node, State: n.State, Uuid: n.UUID,
			Running: n.Running, Detail: n.Detail})
	}
	return resp
}
