package controller

// Host names and addresses: the hostname/address maps the controller resolves
// nodes through, and the node registry it loads from the database at start.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// initHostsMapping initializes hostname to address mapping
func (c *Controller) initHostsMapping() {
	log := c.traceLogger()
	// Get hostnames from all hosts
	if len(c.hosts) == 0 {
		log.Debug("Controller.initHostsMapping: no hosts, nothing to map")
		return
	}
	log.Debug("Controller.initHostsMapping: resolving hostnames", zap.Strings("hosts", c.hosts))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, host := range c.hosts {
		start := time.Now()
		result, err := c.deployment.Exec(ctx, []string{host}, "hostname")
		if err != nil || !result.AllSuccess() {
			log.Debug("Controller.initHostsMapping: hostname lookup failed",
				zap.String("host", host), zap.Duration("took", time.Since(start)), zap.Error(err))
		}
		if err == nil && result.AllSuccess() {
			log.Debug("Controller.initHostsMapping: hostname lookup succeeded",
				zap.String("host", host), zap.Duration("took", time.Since(start)))
			for h, r := range result.Hosts {
				if r.Success && r.Output != "" {
					hostname := r.Output
					c.hostsMap[hostname] = h
					c.logger.Debug("Host mapping",
						zap.String("hostname", hostname),
						zap.String("address", h))
				}
			}
		}
	}
}

// loadHostsFromDatabase loads hosts from registered nodes in database
func (c *Controller) loadHostsFromDatabase(ctx context.Context) error {
	log := c.traceLogger()
	log.Debug("Controller.loadHostsFromDatabase: listing registered nodes")
	nodes, err := c.nodes.ListNodes(ctx)
	if err != nil {
		log.Debug("Controller.loadHostsFromDatabase: listing nodes failed", zap.Error(err))
		return err
	}

	if len(nodes) == 0 {
		log.Debug("Controller.loadHostsFromDatabase: no registered nodes")
		return fmt.Errorf("no nodes found in database")
	}

	var hosts []string
	for _, node := range nodes {
		if node.LocalAgent {
			// Reached through its agent, never over SSH.
			log.Debug("Controller.loadHostsFromDatabase: local-agent node, not an SSH host",
				zap.String("node", node.Name), zap.String("host", node.Address))
			continue
		}
		log.Debug("Controller.loadHostsFromDatabase: adding node host",
			zap.String("node", node.Name), zap.String("host", node.Address))
		hosts = append(hosts, node.Address)
	}

	c.hosts = hosts
	c.logger.Info("Loaded hosts from database", zap.Strings("hosts", hosts))
	return nil
}

// NodeName is the reverse of ResolveHost: the node name registered for an
// address, or the address itself when none is. Messages meant for an operator
// name nodes the way every command takes them.
//
// The registered node name wins. The hosts map also carries each node's
// hostname as an alias of the same address, and taking whichever map entry
// came first returned one name on one call and the other on the next: alerts
// keyed by node (pool.data_near_full) then cleared as "no longer exists" and
// fired again under the other name every poll.
func (c *Controller) NodeName(address string) string {
	log := c.traceLogger()
	if c.nodes != nil {
		if name := c.nodes.GetNodeNameByAddress(address); name != "" {
			log.Debug("Controller.NodeName: registered node name", zap.String("address", address), zap.String("node", name))
			return name
		}
	}
	c.hostsLock.RLock()
	defer c.hostsLock.RUnlock()
	best := ""
	for name, addr := range c.hostsMap {
		if addr == address && name != address && (best == "" || name < best) {
			best = name
		}
	}
	if best != "" {
		log.Debug("Controller.NodeName: name from hosts map", zap.String("address", address), zap.String("node", best))
		return best
	}
	log.Debug("Controller.NodeName: no name known, using address", zap.String("address", address))
	return address
}

// ResolveHost resolves a hostname to an address
func (c *Controller) ResolveHost(hostOrAddr string) string {
	c.hostsLock.RLock()
	defer c.hostsLock.RUnlock()

	log := c.traceLogger()
	// Try to resolve hostname to address first
	if addr, ok := c.hostsMap[hostOrAddr]; ok {
		log.Debug("Controller.ResolveHost: resolved from hosts map", zap.String("host", hostOrAddr), zap.String("address", addr))
		return addr
	}

	// If it's already an address in our hosts list, return as is
	for _, addr := range c.hosts {
		if addr == hostOrAddr {
			log.Debug("Controller.ResolveHost: already a known address", zap.String("host", hostOrAddr))
			return hostOrAddr
		}
	}

	// Return as-is (might be a hostname that SSH can resolve)
	log.Debug("Controller.ResolveHost: unknown, passing through", zap.String("host", hostOrAddr))
	return hostOrAddr
}

// NormalizeHost converts an address to hostname if available
// Used for display purposes to avoid showing duplicates
func (c *Controller) NormalizeHost(addrOrHost string) string {
	c.hostsLock.RLock()
	defer c.hostsLock.RUnlock()

	log := c.traceLogger()
	// If it's already in hosts list, return as is (prefer hostnames over IPs)
	for _, host := range c.hosts {
		if host == addrOrHost {
			log.Debug("Controller.NormalizeHost: already in hosts list", zap.String("host", addrOrHost))
			return host
		}
	}

	// Reverse lookup: check if this address maps to a hostname
	for hostname, addr := range c.hostsMap {
		if addr == addrOrHost {
			log.Debug("Controller.NormalizeHost: reverse-mapped to hostname",
				zap.String("address", addrOrHost), zap.String("hostname", hostname))
			return hostname
		}
	}

	// Return as-is
	log.Debug("Controller.NormalizeHost: no mapping, unchanged", zap.String("host", addrOrHost))
	return addrOrHost
}

// ==================== DATABASE ====================

// loadFromDatabase loads nodes and gateways from database
func (c *Controller) loadFromDatabase(ctx context.Context) error {
	log := c.traceLogger()
	// Load nodes
	start := time.Now()
	dbNodes, err := c.db.ListNodes(ctx)
	log.Debug("Controller.loadFromDatabase: listed nodes", zap.Int("count", len(dbNodes)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("failed to load nodes: %w", err)
	}

	for _, dbNode := range dbNodes {
		var labels map[string]string
		if dbNode.Labels != "" {
			if err := json.Unmarshal([]byte(dbNode.Labels), &labels); err != nil {
				c.logger.Warn("Failed to decode node labels; ignoring",
					zap.String("node", dbNode.Name), zap.Error(err))
			}
		}
		c.nodes.mu.Lock()
		c.nodes.nodes[dbNode.Address] = &NodeInfo{
			Name:               dbNode.Name,
			Address:            dbNode.Address,
			ReplicationAddress: dbNode.ReplicationAddress,
			ReplicationPaths:   dbNode.ReplicationPaths,
			Hostname:           dbNode.Hostname,
			State:              NodeState(dbNode.State),
			LastSeen:           dbNode.LastSeen,
			Version:            dbNode.Version,
			Capacity:           make(map[string]interface{}),
			Labels:             labels,
			OfflineSince:       dbNode.OfflineSince,
			LocalAgent:         dbNode.LocalAgent,
		}
		loaded := c.nodes.nodes[dbNode.Address]
		c.nodes.mu.Unlock()
		if loaded.LocalAgent {
			c.nodes.markLocalAgent(loaded)
		}

		// Build hostname -> IP address mapping for DRBD config
		log.Debug("Controller.loadFromDatabase: mapping node names",
			zap.String("node", dbNode.Name), zap.String("hostname", dbNode.Hostname),
			zap.String("address", dbNode.Address), zap.Int("labels", len(labels)))
		if dbNode.Hostname != "" && dbNode.Address != "" {
			c.hostsLock.Lock()
			c.hostsMap[dbNode.Hostname] = dbNode.Address
			c.hostsLock.Unlock()
		}
		// Also map name -> IP if different from hostname
		if dbNode.Name != "" && dbNode.Name != dbNode.Hostname && dbNode.Address != "" {
			c.hostsLock.Lock()
			c.hostsMap[dbNode.Name] = dbNode.Address
			c.hostsLock.Unlock()
		}

		c.logger.Debug("Loaded node from database",
			zap.String("name", dbNode.Name),
			zap.String("address", dbNode.Address))
	}

	// Load gateways
	start = time.Now()
	dbGateways, err := c.db.ListGateways(ctx)
	log.Debug("Controller.loadFromDatabase: listed gateways", zap.Int("count", len(dbGateways)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("failed to load gateways: %w", err)
	}

	for _, dbGateway := range dbGateways {
		c.logger.Debug("Loaded gateway from database",
			zap.String("name", dbGateway.Name),
			zap.String("type", string(dbGateway.Type)),
			zap.String("resource", dbGateway.Resource))
	}

	c.logger.Info("Loaded data from database",
		zap.Int("nodes", len(dbNodes)),
		zap.Int("gateways", len(dbGateways)))

	return nil
}
