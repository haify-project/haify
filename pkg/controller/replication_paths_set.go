package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// SetReplicationPaths replaces node's extra replication addresses. Nothing is
// rewritten here; RewriteForReplicationPaths applies them to the resources.
func (nm *NodeManager) SetReplicationPaths(ctx context.Context, ref string, addrs []string) (*NodeInfo, error) {
	addrs, err := validateReplicationPaths(addrs)
	if err != nil {
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
		return nil, fmt.Errorf("node %q is not registered", ref)
	}
	for _, a := range addrs {
		for _, n := range nm.nodes {
			if a == n.Address || a == replicationOf(n) {
				nm.mu.Unlock()
				return nil, fmt.Errorf("%s is already %s's address", a, n.Name)
			}
			if n != node && slices.Contains(n.ReplicationPaths, a) {
				nm.mu.Unlock()
				return nil, fmt.Errorf("%s is already a replication path of %s", a, n.Name)
			}
		}
	}
	node.ReplicationPaths = addrs
	snapshot := *node
	nm.mu.Unlock()

	if nm.controller.db != nil {
		if err := nm.controller.db.SaveNode(ctx, nodeRecord(&snapshot)); err != nil {
			return nil, fmt.Errorf("save node: %w", err)
		}
	}
	return &snapshot, nil
}

// RewriteForReplicationPaths rewrites and adjusts every resource node takes
// part in, so its configs carry the node's current paths. DRBD adds or drops
// a path on a live connection without disconnecting it.
func (rm *ResourceManager) RewriteForReplicationPaths(ctx context.Context, node string) (done, failed []string) {
	if rm.controller.db == nil {
		return nil, nil
	}
	all, err := rm.controller.db.ListResources(ctx)
	if err != nil {
		return nil, []string{"list resources: " + err.Error()}
	}
	for _, r := range all {
		if !nodeTakesPart(node, r.Nodes, r.DisklessNodes, r.DisklessClients, r.DRNode) {
			continue
		}
		if err := rm.rewriteResourceConfig(ctx, r.Name, func(c string) (string, error) { return c, nil }); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", r.Name, err))
			continue
		}
		done = append(done, r.Name)
	}
	sort.Strings(done)
	return done, failed
}

func (s *Server) SetNodeReplicationPaths(ctx context.Context, req *haifypb.SetNodeReplicationPathsRequest) (*haifypb.SetNodeAddressResponse, error) {
	node, err := s.nodes.SetReplicationPaths(ctx, req.Node, req.Addresses)
	if err != nil {
		return &haifypb.SetNodeAddressResponse{Success: false, Message: err.Error()}, nil
	}
	// The guard first: a config with a path the node has no address for would
	// otherwise keep the connection down (replication_paths_guard.go).
	var failed []string
	if err := s.resources.setPathsGuard(ctx, node.Address, len(node.ReplicationPaths) > 0); err != nil {
		failed = append(failed, fmt.Sprintf("path guard on %s: %v", node.Name, err))
	}
	done, rfailed := s.resources.RewriteForReplicationPaths(ctx, node.Name)
	failed = append(failed, rfailed...)
	msg := fmt.Sprintf("%s has no extra replication path", node.Name)
	if len(node.ReplicationPaths) > 0 {
		msg = fmt.Sprintf("%s replicates over %s and %s", node.Name, replicationOf(node), strings.Join(node.ReplicationPaths, ", "))
	}
	return &haifypb.SetNodeAddressResponse{Success: len(failed) == 0, Message: msg, Resources: done, Failed: failed}, nil
}
