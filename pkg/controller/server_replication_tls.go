package controller

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

func (s *Server) SetupReplicationTLS(ctx context.Context, req *haifypb.SetupReplicationTLSRequest) (*haifypb.ReplicationTLSResponse, error) {
	log := s.serverLog()
	log.Debug("SetupReplicationTLS: setting up", zap.Strings("nodes", req.Nodes))
	states, err := s.resources.SetupReplicationTLS(ctx, req.Nodes)
	if err != nil {
		log.Debug("SetupReplicationTLS: failed", zap.Strings("nodes", req.Nodes), zap.Error(err))
		return &haifypb.ReplicationTLSResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("SetupReplicationTLS: done", zap.Int("nodes", len(states)))
	return tlsResponse(states), nil
}

func (s *Server) GetReplicationTLSStatus(ctx context.Context, req *haifypb.GetReplicationTLSStatusRequest) (*haifypb.ReplicationTLSResponse, error) {
	log := s.serverLog()
	log.Debug("GetReplicationTLSStatus: reading", zap.Strings("nodes", req.Nodes))
	states, err := s.resources.ReplicationTLSStatus(ctx, req.Nodes)
	if err != nil {
		log.Debug("GetReplicationTLSStatus: failed", zap.Strings("nodes", req.Nodes), zap.Error(err))
		return &haifypb.ReplicationTLSResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("GetReplicationTLSStatus: read", zap.Int("nodes", len(states)))
	return tlsResponse(states), nil
}

func (s *Server) SetResourceTLS(ctx context.Context, req *haifypb.SetResourceTLSRequest) (*haifypb.SetResourceTLSResponse, error) {
	log := s.serverLog()
	log.Debug("SetResourceTLS: switching", zap.String("resource", req.Resource), zap.Bool("enabled", req.Enabled))
	n, err := s.resources.SetResourceTLS(ctx, req.Resource, req.Enabled)
	if err != nil {
		log.Debug("SetResourceTLS: failed", zap.String("resource", req.Resource), zap.Int("links_changed", n), zap.Error(err))
		return &haifypb.SetResourceTLSResponse{Success: false, Message: err.Error(), LinksChanged: uint32(n)}, nil
	}
	state := "off"
	if req.Enabled {
		state = "on"
	}
	log.Debug("SetResourceTLS: switched", zap.String("resource", req.Resource), zap.String("state", state), zap.Int("links_changed", n))
	return &haifypb.SetResourceTLSResponse{
		Success: true, LinksChanged: uint32(n),
		Message: fmt.Sprintf("TLS is %s for every connection of %s (%d link(s) switched)", state, req.Resource, n),
	}, nil
}

func tlsResponse(states []NodeTLSState) *haifypb.ReplicationTLSResponse {
	out := &haifypb.ReplicationTLSResponse{Success: true}
	ready := 0
	for _, st := range states {
		zap.L().Debug("tlsResponse: node", zap.String("node", st.Node), zap.Bool("ready", st.Ready),
			zap.String("problem", st.Problem), zap.Time("expires", st.Expires))
		info := &haifypb.NodeTLSInfo{Node: st.Node, Ready: st.Ready, Problem: st.Problem}
		if !st.Expires.IsZero() {
			info.Expires = st.Expires.UTC().Format("2006-01-02")
		}
		if st.Ready {
			ready++
		}
		out.Nodes = append(out.Nodes, info)
	}
	out.Message = fmt.Sprintf("%d of %d node(s) ready for encrypted replication", ready, len(states))
	zap.L().Debug("tlsResponse: summary", zap.Int("ready", ready), zap.Int("nodes", len(states)))
	return out
}
