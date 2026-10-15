package controller

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
)

// A local-agent node is one the controller never SSHes into: a Kubernetes
// worker whose operating system takes no SSH keys and no packages (RHCOS on
// OpenShift is the case this exists for). The Haify CSI node plugin running
// there registers the node itself and becomes its agent: it polls
// GetNodeAgentState for the DRBD configuration of every resource the node is a
// diskless client of, writes it to the host's /etc/drbd.d, applies it with
// drbdadm, and changes the node's DRBD role itself.
//
// Such a node can only be a diskless client. It owns no pool, so no replica is
// placed on it, and it is never picked as a tiebreaker. Every command the
// controller would have run there is dropped by localAgentDeployment.

// localAgentTimeout is how long an agent may go without reporting before the
// node counts as offline. The CSI node plugin reports every 10 seconds.
const localAgentTimeout = 60 * time.Second

// agentConfigTTL is how long a resource's configuration read for agents is
// reused. Every agent polls every 10 seconds; without it each poll would be an
// SSH read per resource.
const agentConfigTTL = 15 * time.Second

// isLocalAgentHost reports whether host (an address, node name or hostname)
// is a local-agent node.
func (nm *NodeManager) isLocalAgentHost(host string) bool {
	nm.agentMu.RLock()
	defer nm.agentMu.RUnlock()
	return nm.agents[host]
}

// markLocalAgent records every way n can be named in the agent set.
func (nm *NodeManager) markLocalAgent(n *NodeInfo) {
	nm.agentMu.Lock()
	defer nm.agentMu.Unlock()
	for _, k := range []string{n.Address, n.Name, n.Hostname, n.ReplicationAddress} {
		if k != "" {
			nm.agents[k] = true
		}
	}
	nm.controller.traceLogger().Debug("NodeManager.markLocalAgent: recorded", zap.String("node", n.Name),
		zap.String("address", n.Address), zap.Int("keys", len(nm.agents)))
}

// unmarkLocalAgent removes n from the agent set.
func (nm *NodeManager) unmarkLocalAgent(n *NodeInfo) {
	nm.agentMu.Lock()
	defer nm.agentMu.Unlock()
	for _, k := range []string{n.Address, n.Name, n.Hostname, n.ReplicationAddress} {
		delete(nm.agents, k)
	}
	nm.controller.traceLogger().Debug("NodeManager.unmarkLocalAgent: removed", zap.String("node", n.Name),
		zap.String("address", n.Address))
}

// RegisterLocalAgentNode registers a node on behalf of the agent running on
// it. The agent supplies the node's hostname, which the controller would
// otherwise read over SSH, and its address, which is where the node's DRBD
// peers reach it.
//
// A node already registered for SSH stays SSH-managed: the controller can
// already do everything there, and the plugin then works the ordinary way.
func (nm *NodeManager) RegisterLocalAgentNode(ctx context.Context, name, address, hostname string) (*NodeInfo, error) {
	log := nm.controller.traceLogger()
	name, address, hostname = strings.TrimSpace(name), strings.TrimSpace(address), strings.TrimSpace(hostname)
	log.Debug("NodeManager.RegisterLocalAgentNode: start", zap.String("name", name), zap.String("address", address),
		zap.String("hostname", hostname))
	if name == "" || address == "" {
		log.Debug("NodeManager.RegisterLocalAgentNode: refusing, name or address missing")
		return nil, fmt.Errorf("a local-agent node needs a name and an address")
	}
	if net.ParseIP(address) == nil {
		log.Debug("NodeManager.RegisterLocalAgentNode: refusing, address is not an IP", zap.String("address", address))
		return nil, fmt.Errorf("a local-agent node registers with its IP address, got %q", address)
	}
	if hostname == "" {
		log.Debug("NodeManager.RegisterLocalAgentNode: no hostname given, using the name", zap.String("name", name))
		hostname = name
	}

	nm.mu.Lock()
	existing := nm.nodes[address]
	if existing == nil {
		for _, n := range nm.nodes {
			if n.Name == name {
				existing = n
				break
			}
		}
	}
	if existing != nil && !existing.LocalAgent {
		snapshot := *existing
		nm.mu.Unlock()
		log.Debug("NodeManager.RegisterLocalAgentNode: node is SSH-managed, leaving it so",
			zap.String("name", snapshot.Name), zap.String("address", snapshot.Address))
		return &snapshot, nil
	}
	if existing != nil && existing.Address != address {
		nm.mu.Unlock()
		log.Debug("NodeManager.RegisterLocalAgentNode: refusing, name registered at another address",
			zap.String("name", name), zap.String("registered", existing.Address), zap.String("address", address))
		return nil, fmt.Errorf("node %q is registered at %s, not %s: unregister it first", name, existing.Address, address)
	}
	node := &NodeInfo{
		Name:       name,
		Address:    address,
		Hostname:   hostname,
		State:      NodeStateOnline,
		LastSeen:   time.Now(),
		Capacity:   make(map[string]interface{}),
		LocalAgent: true,
	}
	if existing != nil {
		node.Labels = existing.Labels
		if existing.State == NodeStateMaintenance || existing.State == NodeStateEvicted {
			node.State = existing.State
		}
		if existing.Hostname != hostname {
			nm.unmarkLocalAgent(existing)
		}
	}
	nm.nodes[address] = node
	snapshot := *node
	nm.mu.Unlock()
	nm.markLocalAgent(&snapshot)

	// Names resolve to the address like any node's; the address is not added
	// to the controller's host list, which is the list of hosts it SSHes into.
	nm.controller.hostsLock.Lock()
	nm.controller.hostsMap[name] = address
	nm.controller.hostsMap[hostname] = address
	nm.controller.hostsLock.Unlock()

	if nm.controller.db != nil {
		start := time.Now()
		err := nm.controller.db.SaveNode(ctx, nodeRecord(&snapshot))
		log.Debug("NodeManager.RegisterLocalAgentNode: saved node record", zap.String("node", name),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return nil, fmt.Errorf("save node: %w", err)
		}
	}
	nm.controller.logger.Info("Local-agent node registered",
		zap.String("name", name), zap.String("address", address), zap.String("hostname", hostname),
		zap.Bool("re_registration", existing != nil))
	return &snapshot, nil
}

// agentHeartbeat records that the agent on the node at address reported in.
func (nm *NodeManager) agentHeartbeat(address string) {
	nm.controller.traceLogger().Debug("NodeManager.agentHeartbeat: agent reported", zap.String("address", address))
	nm.markHealth(address, NodeStateOnline)
}

// checkLocalAgentHealth is CheckNodeHealth for a local-agent node, which has
// nothing to probe: it is online while its agent keeps reporting.
func (nm *NodeManager) checkLocalAgentHealth(node *NodeInfo) error {
	nm.mu.RLock()
	lastSeen := node.LastSeen
	nm.mu.RUnlock()
	since := time.Since(lastSeen)
	nm.controller.traceLogger().Debug("NodeManager.checkLocalAgentHealth: last report", zap.String("node", node.Name),
		zap.Duration("since", since))
	if since > localAgentTimeout {
		nm.markHealth(node.Address, NodeStateOffline)
		return fmt.Errorf("the Haify CSI node plugin on this local-agent node last reported %s ago",
			since.Round(time.Second))
	}
	nm.markHealth(node.Address, NodeStateOnline)
	return nil
}

// AgentResource is one resource a local-agent node must have up, with the
// DRBD configuration to write for it.
type AgentResource struct {
	Name   string
	Config string
}

type agentConfigEntry struct {
	config string
	read   time.Time
}

// AgentState is what the agent on node must apply: every resource the node is
// a diskless client of. Calling it is also the agent's heartbeat.
func (rm *ResourceManager) AgentState(ctx context.Context, node string) ([]AgentResource, error) {
	log := rm.resourcesLog()
	log.Debug("ResourceManager.AgentState: start", zap.String("node", node))
	info, err := rm.controller.nodes.GetNode(ctx, node)
	if err != nil {
		log.Debug("ResourceManager.AgentState: unknown node", zap.String("node", node), zap.Error(err))
		return nil, err
	}
	if !info.LocalAgent {
		log.Debug("ResourceManager.AgentState: refusing, node is SSH-managed", zap.String("node", node))
		return nil, fmt.Errorf("node %q is managed over SSH, not by a local agent", node)
	}
	rm.controller.nodes.agentHeartbeat(info.Address)
	if rm.controller.db == nil {
		log.Debug("ResourceManager.AgentState: no database")
		return nil, fmt.Errorf("database not available")
	}

	start := time.Now()
	all, err := rm.controller.db.ListResources(ctx)
	log.Debug("ResourceManager.AgentState: listed resources", zap.Int("count", len(all)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("list resources: %w", err)
	}
	var out []AgentResource
	for _, r := range all {
		if !agentServes(r, info) {
			continue
		}
		cfg, err := rm.agentConfig(ctx, r.Name, info.Hostname)
		if err != nil {
			// Leaving the resource out would make the agent take it down, so
			// the whole answer fails and the agent keeps what it has.
			log.Debug("ResourceManager.AgentState: config unreadable, reporting the failure",
				zap.String("resource", r.Name), zap.Error(err))
			return nil, fmt.Errorf("read configuration of %s: %w", r.Name, err)
		}
		log.Debug("ResourceManager.AgentState: resource", zap.String("resource", r.Name), zap.Int("bytes", len(cfg)))
		out = append(out, AgentResource{Name: r.Name, Config: cfg})
	}
	log.Debug("ResourceManager.AgentState: done", zap.String("node", node), zap.Int("resources", len(out)))
	return out, nil
}

// agentServes reports whether node is one of r's diskless clients.
func agentServes(r *database.Resource, node *NodeInfo) bool {
	for _, c := range splitCSV(r.DisklessClients) {
		if c == node.Name || c == node.Hostname || c == node.Address {
			return true
		}
	}
	return false
}

// agentConfig is the resource's .res as the nodes that hold it have it,
// replication paths included. It is read from a diskful node and reused for
// agentConfigTTL, except that a cached copy without the agent's own `on`
// section is read again: that copy predates the attach the agent is waiting
// for.
func (rm *ResourceManager) agentConfig(ctx context.Context, resource, hostname string) (string, error) {
	log := rm.resourcesLog()
	rm.agentCfgMu.Lock()
	e, ok := rm.agentCfg[resource]
	rm.agentCfgMu.Unlock()
	if ok && time.Since(e.read) < agentConfigTTL && hasOnBlock(e.config, hostname) {
		log.Debug("ResourceManager.agentConfig: cached", zap.String("resource", resource),
			zap.Duration("age", time.Since(e.read)))
		return e.config, nil
	}
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		log.Debug("ResourceManager.agentConfig: no hosts", zap.String("resource", resource), zap.Error(err))
		return "", err
	}
	path := fmt.Sprintf("/etc/drbd.d/%s.res", resource)
	var lastErr error
	for _, h := range hosts {
		start := time.Now()
		res, err := rm.deployment.Exec(ctx, []string{h}, "cat "+path)
		out := hostOutput(res, h)
		log.Debug("ResourceManager.agentConfig: read", zap.String("resource", resource), zap.String("host", h),
			zap.Int("bytes", len(out)), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil || res == nil || !res.AllSuccess() || strings.TrimSpace(out) == "" {
			lastErr = fmt.Errorf("could not read %s on %s", path, h)
			continue
		}
		// The read came back with the replication paths collapsed
		// (resPathsDeployment); the nodes have them expanded.
		cfg := expandReplicationPaths(out, rm.controller.nodes.replicationPathsByDRBDName())
		rm.agentCfgMu.Lock()
		rm.agentCfg[resource] = agentConfigEntry{config: cfg, read: time.Now()}
		rm.agentCfgMu.Unlock()
		return cfg, nil
	}
	return "", lastErr
}

// hasOnBlock reports whether config has an `on` section for hostname.
func hasOnBlock(config, hostname string) bool {
	for _, b := range parseOnBlocks(config) {
		if b.name == hostname {
			return true
		}
	}
	return false
}
