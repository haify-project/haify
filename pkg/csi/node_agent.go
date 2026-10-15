package csi

import (
	"context"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NewLocalAgentNodeServer returns the CSI Node service for a local-agent node
// (agent.go): staging works as on any node, except that this plugin brings the
// resource up and changes its role itself instead of asking the controller to
// do it over SSH.
func NewLocalAgentNodeServer(b HaifyBackend, m Mounter, nodeName string, agent *LocalAgent, log *zap.Logger) csi.NodeServer {
	ns := NewNodeServer(b, m, nodeName, log).(*nodeServer)
	ns.agent = agent
	return ns
}

// agentAttached brings a resource this node was just attached to up here and
// waits for it to connect. Elsewhere the controller already did that.
func (s *nodeServer) agentAttached(ctx context.Context, res string) error {
	if s.agent == nil {
		return nil
	}
	start := time.Now()
	err := s.agent.Ensure(ctx, res)
	s.log.Debug("nodeServer.agentAttached: resource up on this node", zap.String("resource", res),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return status.Errorf(codes.Internal, "bring %s up on this node: %v", res, err)
	}
	return nil
}

// agentDetached takes a resource this node was just detached from down here.
// A failure is left to the agent's next round.
func (s *nodeServer) agentDetached(ctx context.Context, res string) {
	if s.agent == nil {
		return
	}
	err := s.agent.Reconcile(ctx)
	s.log.Debug("nodeServer.agentDetached: reconciled", zap.String("resource", res), zap.Error(err))
}

// promote makes this node Primary: the quorum-guarded promote on the
// controller, or drbdadm here on a local-agent node.
func (s *nodeServer) promote(ctx context.Context, res string) error {
	if s.agent != nil {
		return s.agent.Promote(ctx, res)
	}
	return s.backend.PromoteForNode(ctx, res, s.nodeName)
}

// demote makes this node Secondary, the same two ways.
func (s *nodeServer) demote(ctx context.Context, res string) error {
	if s.agent != nil {
		return s.agent.Demote(ctx, res)
	}
	return s.backend.SetSecondary(ctx, res, s.nodeName)
}
