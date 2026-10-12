package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// Lookups over the registered nodes: by name, hostname or address, for SSH
// (management address), for DRBD (hostname, replication address) and for
// listing.

// GetNodeAddressByName gets node address by node name
func (nm *NodeManager) GetNodeAddressByName(name string) string {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	for addr, node := range nm.nodes {
		if node.Name == name || node.Hostname == name || addr == name {
			loggerOfController(nm.controller).Debug("NodeManager.GetNodeAddressByName: found",
				zap.String("node", name), zap.String("address", addr))
			return addr
		}
	}
	loggerOfController(nm.controller).Debug("NodeManager.GetNodeAddressByName: no registered node matches", zap.String("node", name))
	return ""
}

// GetDRBDNameByRef returns the name DRBD must see for a node, i.e. the node's
// real `uname -n` hostname captured at registration.
//
// This exists because a DRBD `.res` file only applies to a host whose hostname
// matches one of its `on <name>` sections — drbdadm otherwise fails the whole
// resource with "'<res>' not defined in your config (for this host)". Writing
// the Haify node *name* there silently works only while operators happen to
// register nodes under their hostname (node1, node2, ...); register the
// same host as "node-a" while it calls itself "vm-node-a" and every resource
// create fails.
//
// Unknown refs fall back to the ref itself, so callers that already pass a
// hostname keep working.
func (nm *NodeManager) GetDRBDNameByRef(ref string) string {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	log := loggerOfController(nm.controller)
	for addr, node := range nm.nodes {
		if node.Name == ref || node.Hostname == ref || addr == ref {
			if node.Hostname != "" {
				log.Debug("NodeManager.GetDRBDNameByRef: using the registered hostname",
					zap.String("ref", ref), zap.String("hostname", node.Hostname))
				return node.Hostname
			}
			log.Debug("NodeManager.GetDRBDNameByRef: node has no hostname, using the ref", zap.String("ref", ref))
			return ref
		}
	}
	log.Debug("NodeManager.GetDRBDNameByRef: unknown ref, using it as is", zap.String("ref", ref))
	return ref
}

// GetReplicationAddressByName returns the address DRBD should use to reach a
// node: its dedicated replication address when one was registered, otherwise the
// management address.
//
// This is deliberately a separate lookup from GetNodeAddressByName: that one
// answers "where do I SSH to?", this one answers "what goes in the .res file?".
// Conflating them is what forced replication onto the management network.
func (nm *NodeManager) GetReplicationAddressByName(name string) string {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	log := loggerOfController(nm.controller)
	for addr, node := range nm.nodes {
		if node.Name == name || node.Hostname == name || addr == name {
			if r := strings.TrimSpace(node.ReplicationAddress); r != "" {
				log.Debug("NodeManager.GetReplicationAddressByName: dedicated replication address",
					zap.String("node", name), zap.String("address", r))
				return r
			}
			log.Debug("NodeManager.GetReplicationAddressByName: replication shares the management address",
				zap.String("node", name), zap.String("address", addr))
			return addr
		}
	}
	log.Debug("NodeManager.GetReplicationAddressByName: no registered node matches", zap.String("node", name))
	return ""
}

// GetNodeNameByAddress resolves an address (or name/hostname) to the node's
// canonical name. Returns "" when no registered node matches. It is the inverse
// of GetNodeAddressByName and tolerates being handed a name already.
func (nm *NodeManager) GetNodeNameByAddress(address string) string {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	for addr, node := range nm.nodes {
		if addr == address || node.Name == address || node.Hostname == address {
			loggerOfController(nm.controller).Debug("NodeManager.GetNodeNameByAddress: found",
				zap.String("address", address), zap.String("node", node.Name))
			return node.Name
		}
	}
	loggerOfController(nm.controller).Debug("NodeManager.GetNodeNameByAddress: no registered node matches", zap.String("address", address))
	return ""
}

// GetNode gets node information
func (nm *NodeManager) GetNode(ctx context.Context, address string) (*NodeInfo, error) {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	log := loggerOfController(nm.controller)
	resolved := address
	if node := nm.nodes[resolved]; node != nil {
		log.Debug("NodeManager.GetNode: found by address", zap.String("address", address), zap.String("node", node.Name))
		return node, nil
	}

	for addr, node := range nm.nodes {
		if node.Name == address || node.Hostname == address {
			resolved = addr
			break
		}
	}

	node := nm.nodes[resolved]
	if node == nil {
		log.Debug("NodeManager.GetNode: node not found", zap.String("node", address))
		return nil, fmt.Errorf("node not found: %s", address)
	}

	log.Debug("NodeManager.GetNode: found by name", zap.String("node", address), zap.String("address", resolved))
	return node, nil
}

// ListNodes lists all nodes
func (nm *NodeManager) ListNodes(ctx context.Context) ([]*NodeInfo, error) {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	nodes := make([]*NodeInfo, 0, len(nm.nodes))
	for _, node := range nm.nodes {
		nodes = append(nodes, node)
	}

	// Sorted by name, because a map range gives a different order on almost
	// every call and the UI redraws this list on each poll: rows jump around
	// while being read, and a reordered list is indistinguishable from one
	// where something actually changed.
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	loggerOfController(nm.controller).Debug("NodeManager.ListNodes: listed", zap.Int("count", len(nodes)))
	return nodes, nil
}
