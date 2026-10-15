package csi

import (
	"context"
	"fmt"
	"strings"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
)

// releaseTaints takes the failover taint off every node that has come back:
// Kubernetes reports it Ready, and DRBD has reconnected it to the volumes it
// holds a replica of. Ready alone is not enough, because a node whose kubelet
// is up while its replicas are still disconnected is exactly where a Pod
// should not be scheduled yet.
//
// The test reads nothing remembered from the sweep that tainted the node, so a
// replica of the plugin that took over the Lease in between releases taints
// the previous one set.
func (h *HAFailover) releaseTaints(ctx context.Context, view *haClusterView, haifyNodes []*haifypb.NodeInfo, losses map[string]*haNodeLoss) {
	h.log.Debug("HAFailover.releaseTaints: start", zap.Int("nodes", len(view.nodes)), zap.Int("lost_nodes", len(losses)))
	var resources []*haifypb.ResourceInfo
	listed := false
	for _, name := range haSortedKeys(view.nodes) {
		node := view.nodes[name]
		if !haHasTaint(node) {
			continue
		}
		if ctx.Err() != nil {
			h.log.Debug("HAFailover.releaseTaints: stopped leading")
			return
		}
		switch {
		case losses[name] != nil:
			h.log.Debug("HAFailover.releaseTaints: node still lost", zap.String("node", name))
			continue
		case !haNodeReady(node):
			h.log.Debug("HAFailover.releaseTaints: node not Ready yet", zap.String("node", name))
			continue
		}
		if !listed {
			start := time.Now()
			var err error
			resources, err = h.backend.ListResources(ctx)
			h.log.Debug("HAFailover.releaseTaints: listed resources", zap.Int("count", len(resources)),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			if err != nil {
				return
			}
			listed = true
		}
		back, why := h.replicasBack(ctx, node, view, haifyNodes, resources)
		if !back {
			h.log.Debug("HAFailover.releaseTaints: keeping taint", zap.String("node", name), zap.String("why", why))
			continue
		}
		removed, err := h.setFailoverTaint(ctx, name, false)
		if err != nil {
			h.log.Warn("HA failover: remove taint", zap.String("node", name), zap.Error(err))
			continue
		}
		if removed {
			h.log.Info("HA failover: removed taint, node is back", zap.String("node", name), zap.String("taint", FailoverTaintKey),
				zap.String("why", why))
			h.recorder.Event(haNodeRef(name), corev1.EventTypeNormal, ReasonFailoverNodeReturned,
				fmt.Sprintf("Removed %s: %s", FailoverTaintKey, why))
		}
	}
}

// replicasBack reports whether DRBD has reconnected node to every Haify volume
// it holds a replica of, and says why not when it has not. A volume whose view
// cannot be read counts as not back.
func (h *HAFailover) replicasBack(ctx context.Context, node *corev1.Node, view *haClusterView, haifyNodes []*haifypb.NodeInfo,
	resources []*haifypb.ResourceInfo) (bool, string) {
	id := haIdentityFor(node, haifyNodes)
	checked := 0
	for _, r := range resources {
		if view.handles[r.GetName()] == nil || !haListsNode(r.GetNodes(), id) {
			continue
		}
		checked++
		callCtx, cancel := context.WithTimeout(ctx, h.cfg.CallTimeout)
		start := time.Now()
		live, err := h.backend.GetResource(callCtx, r.GetName())
		cancel()
		h.log.Debug("HAFailover.replicasBack: got resource", zap.String("node", node.Name), zap.String("resource", r.GetName()),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return false, fmt.Sprintf("cannot read %s: %v", r.GetName(), err)
		}
		if !haReconnected(live, id) {
			return false, fmt.Sprintf("%s is not reconnected to %s yet", node.Name, r.GetName())
		}
	}
	h.log.Debug("HAFailover.replicasBack: all reconnected", zap.String("node", node.Name), zap.Int("volumes", checked))
	if checked == 0 {
		return true, "Ready, and it holds no replica of a Haify volume"
	}
	return true, fmt.Sprintf("Ready, and reconnected to all %d of its Haify volumes", checked)
}

// haListsNode reports whether a resource's diskful node list names the node.
func haListsNode(nodes []string, id haNodeIdentity) bool {
	for _, n := range nodes {
		if id[strings.ToLower(strings.TrimSpace(n))] {
			return true
		}
	}
	return false
}

// haReconnected reports whether the view shows node connected: as a peer whose
// connection is Connected, or as the node that answered with at least one
// peer Connected to it.
func haReconnected(r *haifypb.ResourceInfo, id haNodeIdentity) bool {
	states := r.GetNodeStates()
	for key, st := range states {
		if !id.matches(key, st) {
			continue
		}
		if strings.EqualFold(st.GetConnection(), "Connected") {
			return true
		}
		if st.GetConnection() != "" {
			return false
		}
		for other, peer := range states {
			if other != key && strings.EqualFold(peer.GetConnection(), "Connected") {
				return true
			}
		}
		return false
	}
	return false
}
