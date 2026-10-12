package controller

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"go.uber.org/zap"
)

// A node's address is the key it is registered under, in memory and in the
// database, and a literal in the DRBD config of every resource it takes part
// in and in the other nodes' /etc/hosts. When a node was renumbered — a DHCP
// lease that moved, a subnet change — none of that followed: the controller
// kept dialling the old address, re-registering added a second entry under the
// new one beside the stale first, and DRBD kept connecting to an address the
// node no longer had.
//
// This file validates a renumbering and moves the registry and database;
// node_address_files.go updates /etc/hosts and known_hosts on the nodes, and
// node_address_resources.go rewrites the DRBD configs.

// AddressMove asks for one node to be renumbered.
type AddressMove struct {
	Node               string
	Address            string
	ReplicationAddress string // empty keeps the node's current arrangement
}

// NodeMove is one node's renumbering as carried out.
type NodeMove struct {
	Node               string
	Hostname           string
	OldAddress         string
	Address            string
	OldReplication     string // the address DRBD used before
	ReplicationAddress string // the address DRBD uses now
}

// NodeAddressChange is what SetNodeAddresses did.
type NodeAddressChange struct {
	Moves        []NodeMove
	Resources    []string
	Failed       []string // "<resource>: <reason>"
	WANResources []string // resources whose WAN proxy must be rebuilt
}

// SetNodeAddress renumbers one registered node; see SetNodeAddresses.
func (nm *NodeManager) SetNodeAddress(ctx context.Context, nodeRef, address, replicationAddress string) (*NodeAddressChange, error) {
	nm.controller.traceLogger().Debug("NodeManager.SetNodeAddress: start", zap.String("node", nodeRef),
		zap.String("address", address), zap.String("replication_address", replicationAddress))
	return nm.SetNodeAddresses(ctx, []AddressMove{{Node: nodeRef, Address: address, ReplicationAddress: replicationAddress}})
}

// SetNodeAddresses renumbers registered nodes together. Each must already
// answer on its new address and be the same machine (same hostname), and no
// two nodes may end up on one address. A replication address, when given,
// moves DRBD traffic there; otherwise a node whose replication ran on its
// management address follows it, and one with its own replication network
// keeps it.
//
// Several at once is not a convenience. When a DHCP server hands every node a
// new lease, renumbering them one by one cannot work: the first node's
// resources are rewritten through peers the controller still knows only by
// their dead addresses, and when two nodes trade addresses the configs end up
// with both peers on one address in between. Here every check runs before
// anything changes, and the registry takes all the new addresses before any
// config is rewritten.
func (nm *NodeManager) SetNodeAddresses(ctx context.Context, moves []AddressMove) (*NodeAddressChange, error) {
	log := nm.controller.traceLogger()
	log.Debug("NodeManager.SetNodeAddresses: start", zap.Int("count", len(moves)))
	if len(moves) == 0 {
		log.Debug("NodeManager.SetNodeAddresses: refusing, no move given")
		return nil, fmt.Errorf("no node to renumber")
	}

	nm.mu.RLock()
	byName := make(map[string]*NodeInfo, len(nm.nodes))
	for _, n := range nm.nodes {
		c := *n
		byName[n.Name] = &c
		byName[n.Address] = &c
	}
	var all []NodeInfo
	for _, n := range nm.nodes {
		all = append(all, *n)
	}
	nm.mu.RUnlock()

	// Resolve and validate every move before touching anything.
	var resolved []NodeMove
	seen := map[string]bool{}
	for _, m := range moves {
		address := strings.TrimSpace(m.Address)
		repl := strings.TrimSpace(m.ReplicationAddress)
		log.Debug("NodeManager.SetNodeAddresses: validating move", zap.String("node", m.Node),
			zap.String("address", address), zap.String("replication_address", repl))
		if net.ParseIP(address) == nil {
			log.Debug("NodeManager.SetNodeAddresses: refusing, address is not an IP", zap.String("address", address))
			return nil, fmt.Errorf("%q is not an IP address", address)
		}
		if repl != "" && net.ParseIP(repl) == nil {
			log.Debug("NodeManager.SetNodeAddresses: refusing, replication address is not an IP", zap.String("replication_address", repl))
			return nil, fmt.Errorf("replication address %q is not an IP address", repl)
		}
		node := byName[strings.TrimSpace(m.Node)]
		if node == nil {
			log.Debug("NodeManager.SetNodeAddresses: refusing, node not registered", zap.String("node", m.Node))
			return nil, fmt.Errorf("node %q is not registered", m.Node)
		}
		if seen[node.Name] {
			log.Debug("NodeManager.SetNodeAddresses: refusing, node named twice", zap.String("node", node.Name))
			return nil, fmt.Errorf("node %s is named twice", node.Name)
		}
		seen[node.Name] = true
		mv := NodeMove{Node: node.Name, Hostname: node.Hostname, OldAddress: node.Address, Address: address,
			OldReplication: replicationOf(node)}
		switch {
		case repl != "":
			mv.ReplicationAddress = repl
			log.Debug("NodeManager.SetNodeAddresses: replication moves to the address given", zap.String("node", node.Name))
		case node.ReplicationAddress != "" && node.ReplicationAddress != node.Address:
			mv.ReplicationAddress = node.ReplicationAddress
			log.Debug("NodeManager.SetNodeAddresses: replication keeps its own network", zap.String("node", node.Name),
				zap.String("replication_address", mv.ReplicationAddress))
		default:
			mv.ReplicationAddress = address
			log.Debug("NodeManager.SetNodeAddresses: replication follows the management address", zap.String("node", node.Name))
		}
		if mv.OldAddress == mv.Address && mv.OldReplication == mv.ReplicationAddress {
			log.Debug("NodeManager.SetNodeAddresses: node already on these addresses, skipped", zap.String("node", node.Name))
			continue // already there
		}
		resolved = append(resolved, mv)
	}
	if len(resolved) == 0 {
		log.Debug("NodeManager.SetNodeAddresses: refusing, nothing to change")
		return nil, fmt.Errorf("nothing to change: every node already uses the address given")
	}

	// No two nodes on one address once everything has moved.
	final := map[string]string{} // address -> node
	for _, n := range all {
		addr, repl := n.Address, replicationOf(&n)
		for _, mv := range resolved {
			if mv.Node == n.Name {
				addr, repl = mv.Address, mv.ReplicationAddress
			}
		}
		for _, a := range []string{addr, repl} {
			if other, taken := final[a]; taken && other != n.Name {
				log.Debug("NodeManager.SetNodeAddresses: refusing, address would be shared",
					zap.String("address", a), zap.String("node", n.Name), zap.String("other", other))
				return nil, fmt.Errorf("%s would belong to both %s and %s", a, other, n.Name)
			}
			final[a] = n.Name
		}
	}

	// Same machine, or nothing moves: a new address that answers with some
	// other host's name is a typo or a reused lease, and renumbering onto it
	// would point every replica of every resource at the wrong machine.
	for _, mv := range resolved {
		start := time.Now()
		res, err := nm.controller.deployment.Exec(ctx, []string{mv.Address}, "hostname")
		if err != nil {
			log.Debug("NodeManager.SetNodeAddresses: hostname probe at the new address failed to run",
				zap.String("node", mv.Node), zap.String("host", mv.Address), zap.Duration("took", time.Since(start)), zap.Error(err))
			return nil, fmt.Errorf("reach %s at %s: %w", mv.Node, mv.Address, err)
		}
		log.Debug("NodeManager.SetNodeAddresses: hostname probe at the new address ran",
			zap.String("node", mv.Node), zap.String("host", mv.Address), zap.Bool("success", res.AllSuccess()),
			zap.Duration("took", time.Since(start)))
		var got string
		for _, hr := range res.Hosts {
			if !hr.Success {
				log.Debug("NodeManager.SetNodeAddresses: new address does not answer", zap.String("node", mv.Node),
					zap.String("host", mv.Address))
				return nil, fmt.Errorf("reach %s at %s: %s", mv.Node, mv.Address, hostFailure(hr))
			}
			got = strings.TrimSpace(hr.Output)
		}
		if mv.Hostname != "" && got != mv.Hostname {
			log.Debug("NodeManager.SetNodeAddresses: refusing, another machine answers at the new address",
				zap.String("node", mv.Node), zap.String("host", mv.Address), zap.String("hostname", mv.Hostname), zap.String("answered", got))
			return nil, fmt.Errorf("%s answers as %q, but node %s is %q; not renumbering onto another machine",
				mv.Address, got, mv.Node, mv.Hostname)
		}
	}

	nm.rekey(ctx, resolved, byName)
	nm.updateKnownHosts(ctx, resolved)
	for _, mv := range resolved {
		nm.updateHostsFiles(ctx, mv.Hostname, mv.OldAddress, mv.Address)
		nm.controller.logger.Info("Node renumbered",
			zap.String("node", mv.Node),
			zap.String("from", mv.OldAddress), zap.String("to", mv.Address),
			zap.String("replication_from", mv.OldReplication),
			zap.String("replication_to", mv.ReplicationAddress))
	}
	log.Debug("NodeManager.SetNodeAddresses: done", zap.Int("moved", len(resolved)))
	return &NodeAddressChange{Moves: resolved}, nil
}

func replicationOf(n *NodeInfo) string {
	if n.ReplicationAddress != "" {
		return n.ReplicationAddress
	}
	return n.Address
}

// rekey moves the nodes to their new addresses everywhere the controller keeps
// them: the registry, the host list and name map every lookup goes through,
// and the database, where the address is the key. Every old key goes before
// any new one arrives, and every mapping is applied once to the original
// values: two nodes trading addresses must not clobber each other halfway.
func (nm *NodeManager) rekey(ctx context.Context, moves []NodeMove, byName map[string]*NodeInfo) {
	log := nm.controller.traceLogger()
	log.Debug("NodeManager.rekey: start", zap.Int("count", len(moves)))
	oldToNew := make(map[string]string, len(moves))
	updated := make([]NodeInfo, 0, len(moves))
	for _, mv := range moves {
		oldToNew[mv.OldAddress] = mv.Address
		u := *byName[mv.Node]
		u.Address = mv.Address
		u.ReplicationAddress = ""
		if mv.ReplicationAddress != mv.Address {
			u.ReplicationAddress = mv.ReplicationAddress
		}
		if u.State != NodeStateMaintenance {
			u.State = NodeStateOnline
		}
		log.Debug("NodeManager.rekey: node record moved", zap.String("node", mv.Node), zap.String("from", mv.OldAddress),
			zap.String("to", mv.Address), zap.String("replication_address", u.ReplicationAddress), zap.String("state", string(u.State)))
		updated = append(updated, u)
	}

	nm.mu.Lock()
	for _, mv := range moves {
		delete(nm.nodes, mv.OldAddress)
	}
	for i := range updated {
		n := updated[i]
		nm.nodes[n.Address] = &n
	}
	nm.mu.Unlock()

	c := nm.controller
	c.hostsLock.Lock()
	for i, h := range c.hosts {
		if nw, ok := oldToNew[h]; ok {
			c.hosts[i] = nw
		}
	}
	for k, v := range c.hostsMap {
		if nw, ok := oldToNew[v]; ok {
			c.hostsMap[k] = nw
		}
	}
	newAddr := make(map[string]bool, len(updated))
	for _, n := range updated {
		newAddr[n.Address] = true
	}
	for old := range oldToNew {
		if !newAddr[old] { // on a swap, one node's old address is another's new one
			delete(c.hostsMap, old)
		} else {
			log.Debug("NodeManager.rekey: old address now belongs to another node, kept in the name map", zap.String("address", old))
		}
	}
	for _, n := range updated {
		c.hostsMap[n.Name] = n.Address
		if n.Hostname != "" {
			c.hostsMap[n.Hostname] = n.Address
		}
	}
	if c.gateway != nil {
		c.gateway.SetHosts(c.hosts)
	}
	hosts := append([]string(nil), c.hosts...)
	c.hostsLock.Unlock()
	if c.resources != nil {
		c.resources.SetHosts(hosts)
	}
	log.Debug("NodeManager.rekey: registry and host list updated", zap.Strings("hosts", hosts))

	if c.db != nil {
		for _, mv := range moves {
			start := time.Now()
			err := c.db.DeleteNode(ctx, mv.OldAddress)
			log.Debug("NodeManager.rekey: deleted old node record", zap.String("address", mv.OldAddress),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			if err != nil {
				c.logger.Warn("Failed to remove the old node record", zap.String("address", mv.OldAddress), zap.Error(err))
			}
		}
		for i := range updated {
			start := time.Now()
			err := c.db.SaveNode(ctx, nodeRecord(&updated[i]))
			log.Debug("NodeManager.rekey: saved renumbered node record", zap.String("node", updated[i].Name),
				zap.String("address", updated[i].Address), zap.Duration("took", time.Since(start)), zap.Error(err))
			if err != nil {
				c.logger.Error("Failed to save the renumbered node", zap.String("node", updated[i].Name), zap.Error(err))
			}
		}
		// A pool created by address records it as its node.
		start := time.Now()
		pools, err := c.db.ListPools(ctx)
		log.Debug("NodeManager.rekey: listed pool records", zap.Int("count", len(pools)),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err == nil {
			for _, p := range pools {
				if nw, ok := oldToNew[p.Node]; ok {
					p.Node = nw
					start := time.Now()
					saveErr := c.db.SavePool(ctx, p)
					log.Debug("NodeManager.rekey: moved pool record to the new address", zap.String("pool", p.Name),
						zap.String("node", nw), zap.Duration("took", time.Since(start)), zap.Error(saveErr))
				}
			}
		}
	} else {
		log.Debug("NodeManager.rekey: no database, registry only")
	}
}
