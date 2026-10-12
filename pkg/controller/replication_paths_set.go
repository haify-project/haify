package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// replicationPathsLog is the logger the node manager traces replication paths
// through: the controller's, or the global one for a manager built without
// one (tests build them bare).
func (nm *NodeManager) replicationPathsLog() *zap.Logger {
	if nm != nil && nm.controller != nil && nm.controller.logger != nil {
		return nm.controller.logger
	}
	return zap.L()
}

// SetReplicationPaths replaces node's extra replication addresses. Nothing is
// rewritten here; RewriteForReplicationPaths applies them to the resources.
func (nm *NodeManager) SetReplicationPaths(ctx context.Context, ref string, addrs []string) (*NodeInfo, error) {
	log := nm.replicationPathsLog()
	log.Debug("NodeManager.SetReplicationPaths: start", zap.String("node", ref), zap.Strings("addresses", addrs))
	addrs, err := validateReplicationPaths(addrs)
	if err != nil {
		log.Debug("NodeManager.SetReplicationPaths: invalid addresses", zap.String("node", ref), zap.Error(err))
		return nil, err
	}
	nm.mu.Lock()
	var node *NodeInfo
	for addr, n := range nm.nodes {
		if n.Name == ref || addr == ref || n.Hostname == ref {
			node = n
		}
	}
	if node == nil {
		nm.mu.Unlock()
		log.Debug("NodeManager.SetReplicationPaths: node not registered", zap.String("node", ref))
		return nil, fmt.Errorf("node %q is not registered", ref)
	}
	for _, a := range addrs {
		for _, n := range nm.nodes {
			if a == n.Address || a == replicationOf(n) {
				nm.mu.Unlock()
				log.Debug("NodeManager.SetReplicationPaths: address is a node's address", zap.String("address", a), zap.String("owner", n.Name))
				return nil, fmt.Errorf("%s is already %s's address", a, n.Name)
			}
			if n != node && slices.Contains(n.ReplicationPaths, a) {
				nm.mu.Unlock()
				log.Debug("NodeManager.SetReplicationPaths: address is another node's path", zap.String("address", a), zap.String("owner", n.Name))
				return nil, fmt.Errorf("%s is already a replication path of %s", a, n.Name)
			}
		}
	}
	node.ReplicationPaths = addrs
	snapshot := *node
	nm.mu.Unlock()

	if nm.controller.db != nil {
		err := nm.controller.db.SaveNode(ctx, nodeRecord(&snapshot))
		log.Debug("NodeManager.SetReplicationPaths: saved node", zap.String("node", snapshot.Name), zap.Error(err))
		if err != nil {
			return nil, fmt.Errorf("save node: %w", err)
		}
	} else {
		log.Debug("NodeManager.SetReplicationPaths: no database; not persisted", zap.String("node", snapshot.Name))
	}
	log.Debug("NodeManager.SetReplicationPaths: paths set", zap.String("node", snapshot.Name), zap.Strings("addresses", addrs))
	return &snapshot, nil
}

// RewriteForReplicationPaths rewrites and adjusts every resource node takes
// part in, so its configs carry the node's current paths. DRBD adds or drops
// a path on a live connection without disconnecting it.
func (rm *ResourceManager) RewriteForReplicationPaths(ctx context.Context, node string) (done, failed []string) {
	log := rm.resourcesLog()
	log.Debug("RewriteForReplicationPaths: start", zap.String("node", node))
	if rm.controller.db == nil {
		log.Debug("RewriteForReplicationPaths: no database; nothing to rewrite", zap.String("node", node))
		return nil, nil
	}
	all, err := rm.controller.db.ListResources(ctx)
	log.Debug("RewriteForReplicationPaths: listed resources", zap.Int("count", len(all)), zap.Error(err))
	if err != nil {
		return nil, []string{"list resources: " + err.Error()}
	}
	for _, r := range all {
		if !nodeTakesPart(node, r.Nodes, r.DisklessNodes, r.DisklessClients, r.DRNode) {
			log.Debug("RewriteForReplicationPaths: node not a member; skipping", zap.String("resource", r.Name), zap.String("node", node))
			continue
		}
		if err := rm.rewriteResourceConfig(ctx, r.Name, func(c string) (string, error) { return c, nil }); err != nil {
			log.Debug("RewriteForReplicationPaths: rewrite failed", zap.String("resource", r.Name), zap.Error(err))
			failed = append(failed, fmt.Sprintf("%s: %v", r.Name, err))
			continue
		}
		log.Debug("RewriteForReplicationPaths: rewritten", zap.String("resource", r.Name))
		done = append(done, r.Name)
	}
	sort.Strings(done)
	log.Debug("RewriteForReplicationPaths: done", zap.String("node", node), zap.Int("done", len(done)), zap.Int("failed", len(failed)))
	return done, failed
}

func (s *Server) SetNodeReplicationPaths(ctx context.Context, req *haifypb.SetNodeReplicationPathsRequest) (*haifypb.SetNodeAddressResponse, error) {
	log := s.nodes.replicationPathsLog()
	node, err := s.nodes.SetReplicationPaths(ctx, req.Node, req.Addresses)
	if err != nil {
		log.Debug("SetNodeReplicationPaths: set failed", zap.String("node", req.Node), zap.Error(err))
		return &haifypb.SetNodeAddressResponse{Success: false, Message: err.Error()}, nil
	}
	// The guard first: a config with a path the node has no address for would
	// otherwise keep the connection down (replication_paths_guard.go).
	var failed []string
	guard := len(node.ReplicationPaths) > 0
	if err := s.resources.setPathsGuard(ctx, node.Address, guard); err != nil {
		log.Debug("SetNodeReplicationPaths: path guard failed", zap.String("node", node.Name), zap.Bool("on", guard), zap.Error(err))
		failed = append(failed, fmt.Sprintf("path guard on %s: %v", node.Name, err))
	}
	done, rfailed := s.resources.RewriteForReplicationPaths(ctx, node.Name)
	log.Debug("SetNodeReplicationPaths: resources rewritten", zap.String("node", node.Name),
		zap.Int("done", len(done)), zap.Int("failed", len(rfailed)))
	failed = append(failed, rfailed...)
	msg := fmt.Sprintf("%s has no extra replication path", node.Name)
	if len(node.ReplicationPaths) > 0 {
		msg = fmt.Sprintf("%s replicates over %s and %s", node.Name, replicationOf(node), strings.Join(node.ReplicationPaths, ", "))
	}
	return &haifypb.SetNodeAddressResponse{Success: len(failed) == 0, Message: msg, Resources: done, Failed: failed}, nil
}
