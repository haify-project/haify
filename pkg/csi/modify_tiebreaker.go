package csi

import (
	"context"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A volume with two diskful replicas needs a third vote, a diskless quorum
// tiebreaker, or losing either node suspends its I/O and the HA failover
// controller, which acts only where the survivors keep quorum, cannot move its
// Pods. CreateVolume gets one from the controller. A shrink to two can leave a
// volume without one: when it grew from two to three, its tiebreaker became
// the third diskful replica, and removing a replica takes the whole node out.
// ensureTiebreaker puts the vote back.

// needsTiebreaker reports whether r is down to two diskful replicas with no
// tiebreaker, ignoring the nodes in leaving (replicas being removed now).
func needsTiebreaker(r *haifypb.ResourceInfo, diskful int, leaving []string) bool {
	if diskful != 2 {
		return false
	}
	for _, n := range r.GetDisklessNodes() {
		if !containsNode(leaving, n) {
			return false
		}
	}
	return true
}

// tiebreakerCandidate picks the node for a new tiebreaker: preferably one of
// preferred (a node whose replica was just removed, so it is known to reach
// the others), else any online node that holds no part of the volume.
func tiebreakerCandidate(r *haifypb.ResourceInfo, nodes []*haifypb.NodeInfo, preferred []string) string {
	online := map[string]bool{}
	for _, n := range nodes {
		online[n.GetName()] = n.GetState() == "" || n.GetState() == "online"
	}
	for i := len(preferred) - 1; i >= 0; i-- {
		if online[preferred[i]] {
			return preferred[i]
		}
	}
	taken := append(append(append([]string{}, r.GetNodes()...), r.GetDisklessNodes()...), r.GetDisklessClients()...)
	for _, n := range nodes {
		if online[n.GetName()] && !containsNode(taken, n.GetName()) {
			return n.GetName()
		}
	}
	return ""
}

// ensureTiebreaker gives r a tiebreaker when it needs one. Having no node to
// put it on is not an error: the replica change stands, and the controller
// keeps flagging the volume as a quorum risk.
func (s *controllerServer) ensureTiebreaker(ctx context.Context, mod volumeModifier, r *haifypb.ResourceInfo,
	diskful int, nodes []*haifypb.NodeInfo, removed []string) error {
	name := r.GetName()
	if !needsTiebreaker(r, diskful, removed) {
		s.log.Debug("controllerServer.ensureTiebreaker: not needed", zap.String("resource", name), zap.Int("diskful", diskful))
		return nil
	}
	node := tiebreakerCandidate(r, nodes, removed)
	if node == "" {
		s.log.Warn("volume has two replicas and no node to hold its quorum tiebreaker; losing either replica's node suspends its I/O",
			zap.String("volume", name))
		return nil
	}
	start := time.Now()
	_, msg, err := mod.SetTiebreaker(ctx, name, node)
	s.log.Debug("controllerServer.ensureTiebreaker: set tiebreaker", zap.String("resource", name), zap.String("node", node),
		zap.String("message", msg), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return status.Errorf(codes.Internal, "put the quorum tiebreaker of %q on %s: %v", name, node, err)
	}
	s.log.Info("quorum tiebreaker added after a replica change", zap.String("volume", name), zap.String("node", node))
	return nil
}
