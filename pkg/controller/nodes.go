package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
)

// NodeState represents the state of a node
type NodeState string

const (
	NodeStateOnline      NodeState = "online"
	NodeStateOffline     NodeState = "offline"
	NodeStateDegraded    NodeState = "degraded"
	NodeStateMaintenance NodeState = "maintenance"
	// NodeStateEvicted is a node self-healing moved every replica off after it
	// stayed unreachable; it gets no new replicas until `node restore` cleans
	// what it still holds (self_heal_evict.go).
	NodeStateEvicted NodeState = "evicted"
)

// NodeInfo represents node information
type NodeInfo struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	// ReplicationAddress is where DRBD talks to this node. Empty means it shares
	// Address. Keeping these separate is what lets replication ride a dedicated
	// NIC/subnet while the controller still reaches the node for SSH on the
	// management address.
	ReplicationAddress string `json:"replication_address,omitempty"`
	// ReplicationPaths are further addresses DRBD may use to reach this node,
	// one per additional network, in the same order on every node
	// (replication_paths.go).
	ReplicationPaths []string               `json:"replication_paths,omitempty"`
	Hostname         string                 `json:"hostname"`
	State            NodeState              `json:"state"`
	LastSeen         time.Time              `json:"last_seen"`
	Capacity         map[string]interface{} `json:"capacity"`
	Version          string                 `json:"version"`
	// Labels are arbitrary key=value tags (e.g. rack=A) used by placement
	// constraints such as replicas-on-different.
	Labels map[string]string `json:"labels,omitempty"`
	// OfflineSince is when the node stopped answering; zero while it answers.
	OfflineSince time.Time `json:"offline_since,omitempty"`
}

// NodeManager manages cluster nodes
type NodeManager struct {
	controller *Controller
	mu         sync.RWMutex
	nodes      map[string]*NodeInfo
}

// NewNodeManager creates a new node manager
func NewNodeManager(ctrl *Controller) *NodeManager {
	return &NodeManager{
		controller: ctrl,
		nodes:      make(map[string]*NodeInfo),
	}
}

// RegisterNode registers a new node
func (nm *NodeManager) RegisterNode(ctx context.Context, name, address string) (*NodeInfo, error) {
	nm.controller.traceLogger().Debug("NodeManager.RegisterNode: registering on the management network only",
		zap.String("name", name), zap.String("address", address))
	return nm.RegisterNodeWithReplicationAddress(ctx, name, address, "")
}

// RegisterNodeWithReplicationAddress registers a node whose DRBD replication
// traffic should use `replicationAddress` instead of the management `address`.
//
// Splitting the two is what lets replication ride a dedicated NIC/subnet: the
// controller keeps reaching the node over `address` for SSH, while generated
// .res files point DRBD at the replication address. An empty
// `replicationAddress` means "same as address" — the single-network behavior
// every previously registered node keeps.
func (nm *NodeManager) RegisterNodeWithReplicationAddress(ctx context.Context, name, address, replicationAddress string) (*NodeInfo, error) {
	log := nm.controller.logger
	replicationAddress = strings.TrimSpace(replicationAddress)
	log.Debug("NodeManager.RegisterNodeWithReplicationAddress: start",
		zap.String("name", name), zap.String("address", address),
		zap.String("replication_address", replicationAddress))
	nm.controller.logger.Info("Registering node",
		zap.String("name", name),
		zap.String("address", address),
		zap.String("replication_address", replicationAddress))

	// Check node health by executing hostname command
	start := time.Now()
	result, err := nm.controller.deployment.Exec(ctx, []string{address}, "hostname")
	if err != nil {
		log.Debug("NodeManager.RegisterNodeWithReplicationAddress: hostname probe failed to run",
			zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, fmt.Errorf("failed to connect to node: %w", err)
	}
	log.Debug("NodeManager.RegisterNodeWithReplicationAddress: hostname probe ran",
		zap.String("host", address), zap.Bool("success", result.AllSuccess()),
		zap.Strings("failed_hosts", result.FailedHosts()), zap.Duration("took", time.Since(start)))

	if !result.AllSuccess() {
		log.Debug("NodeManager.RegisterNodeWithReplicationAddress: refusing, health check failed", zap.String("host", address))
		return nil, fmt.Errorf("health check failed for node: %s", address)
	}

	// Get hostname
	hostname := name // fallback to provided name
	for _, r := range result.Hosts {
		if r.Success && r.Output != "" {
			hostname = strings.TrimSpace(r.Output)
			break
		}
	}
	log.Debug("NodeManager.RegisterNodeWithReplicationAddress: hostname resolved",
		zap.String("name", name), zap.String("hostname", hostname))

	// Preserve any labels a prior registration set, so re-registering a node
	// (e.g. after a restart or address refresh) does not wipe its rack/zone tags.
	// A drained node stays drained: re-registering is not an undrain.
	nm.mu.RLock()
	var labels map[string]string
	state := NodeStateOnline
	if existing := nm.nodes[address]; existing != nil {
		if len(existing.Labels) > 0 {
			labels = make(map[string]string, len(existing.Labels))
			for k, v := range existing.Labels {
				labels[k] = v
			}
		}
		if existing.State == NodeStateMaintenance || existing.State == NodeStateEvicted {
			state = existing.State
		}
		log.Debug("NodeManager.RegisterNodeWithReplicationAddress: re-registration keeps labels and drain state",
			zap.String("address", address), zap.Int("labels", len(labels)),
			zap.String("previous_state", string(existing.State)), zap.String("state", string(state)))
	} else {
		log.Debug("NodeManager.RegisterNodeWithReplicationAddress: first registration of this address", zap.String("address", address))
	}
	nm.mu.RUnlock()

	// Create node info
	nodeInfo := &NodeInfo{
		Name:               name,
		Address:            address,
		ReplicationAddress: replicationAddress,
		Hostname:           hostname,
		State:              state,
		LastSeen:           time.Now(),
		Version:            nm.detectNodeVersion(ctx, address),
		Capacity:           make(map[string]interface{}),
		Labels:             labels,
	}

	// Save to in-memory cache
	nm.mu.Lock()
	nm.nodes[address] = nodeInfo
	nm.mu.Unlock()

	// Update controller's hosts list if not already present
	nm.controller.hostsLock.Lock()
	found := false
	for _, h := range nm.controller.hosts {
		if h == address {
			found = true
			break
		}
	}
	if !found {
		nm.controller.hosts = append(nm.controller.hosts, address)
	}
	log.Debug("NodeManager.RegisterNodeWithReplicationAddress: host list updated",
		zap.String("address", address), zap.Bool("already_listed", found), zap.Int("hosts", len(nm.controller.hosts)))
	// Update hostsMap for resolution
	nm.controller.hostsMap[name] = address
	if hostname != "" {
		nm.controller.hostsMap[hostname] = address
	}
	nm.controller.gateway.SetHosts(nm.controller.hosts)
	nm.controller.hostsLock.Unlock()

	// Add hosts entry to all existing nodes for new node
	nm.addHostsEntry(ctx, address, hostname)

	// Save to database
	if nm.controller.db != nil {
		dbNode := &database.Node{
			Name:               nodeInfo.Name,
			Address:            nodeInfo.Address,
			ReplicationAddress: nodeInfo.ReplicationAddress,
			Hostname:           nodeInfo.Hostname,
			State:              string(nodeInfo.State),
			LastSeen:           nodeInfo.LastSeen,
			Version:            nodeInfo.Version,
		}
		if len(nodeInfo.Labels) > 0 {
			if encoded, err := json.Marshal(nodeInfo.Labels); err == nil {
				dbNode.Labels = string(encoded)
			}
		}
		dbStart := time.Now()
		err := nm.controller.db.SaveNode(ctx, dbNode)
		log.Debug("NodeManager.RegisterNodeWithReplicationAddress: saved node record",
			zap.String("address", address), zap.Duration("took", time.Since(dbStart)), zap.Error(err))
		if err != nil {
			nm.controller.logger.Error("Failed to save node to database", zap.Error(err))
		}
	} else {
		log.Debug("NodeManager.RegisterNodeWithReplicationAddress: no database, node kept in memory only", zap.String("address", address))
	}

	nm.controller.logger.Info("Node registered successfully",
		zap.String("name", name),
		zap.String("address", address),
		zap.String("hostname", hostname))

	return nodeInfo, nil
}

// SetNodeLabels sets or merges labels on a node (by name or address) and
// persists them. With replace=true the label set is replaced wholesale;
// otherwise labels are merged in and a key with an empty value is deleted.
func (nm *NodeManager) SetNodeLabels(ctx context.Context, nodeRef string, labels map[string]string, replace bool) (*NodeInfo, error) {
	log := nm.controller.traceLogger()
	resolved := nm.controller.ResolveHost(nodeRef)
	log.Debug("NodeManager.SetNodeLabels: start", zap.String("node", nodeRef), zap.String("resolved", resolved),
		zap.Any("labels", labels), zap.Bool("replace", replace))

	nm.mu.Lock()
	node := nm.nodes[resolved]
	if node == nil {
		// nodeRef may be a name/hostname that ResolveHost did not map. Only the
		// node itself is taken from the fallback: nm.nodes holds pointers, so the
		// label edits below land in the map entry without needing its key, and
		// persistence reads the address off the node record rather than the key.
		for addr, n := range nm.nodes {
			if n.Name == nodeRef || n.Hostname == nodeRef || addr == nodeRef {
				node = n
				break
			}
		}
		log.Debug("NodeManager.SetNodeLabels: resolved address not registered, matched by name",
			zap.String("node", nodeRef), zap.Bool("found", node != nil))
	}
	if node == nil {
		nm.mu.Unlock()
		log.Debug("NodeManager.SetNodeLabels: node not found", zap.String("node", nodeRef))
		return nil, fmt.Errorf("node not found: %s", nodeRef)
	}

	// replace starts from an empty set; merge keeps the existing one (lazily
	// created). The provided labels are then applied on top.
	if replace || node.Labels == nil {
		node.Labels = make(map[string]string, len(labels))
	}
	for k, v := range labels {
		k = strings.TrimSpace(k)
		if k == "" {
			log.Debug("NodeManager.SetNodeLabels: skipping empty key")
			continue
		}
		if v == "" {
			log.Debug("NodeManager.SetNodeLabels: deleting label", zap.String("key", k))
			delete(node.Labels, k) // empty value deletes the key
			continue
		}
		log.Debug("NodeManager.SetNodeLabels: setting label", zap.String("key", k), zap.String("value", v))
		node.Labels[k] = v
	}
	// Copy for return + persistence outside the lock.
	labelsCopy := make(map[string]string, len(node.Labels))
	for k, v := range node.Labels {
		labelsCopy[k] = v
	}
	snapshot := *node
	nm.mu.Unlock()

	if nm.controller.db != nil {
		encoded, err := json.Marshal(labelsCopy)
		if err != nil {
			log.Debug("NodeManager.SetNodeLabels: encoding labels failed", zap.String("node", snapshot.Name), zap.Error(err))
			return nil, fmt.Errorf("encode labels: %w", err)
		}
		// The whole record, replication address included: this used to be
		// rebuilt field by field without it, so labelling a node silently
		// moved its DRBD traffic back onto the management network at the next
		// config it took part in.
		dbNode := nodeRecord(&snapshot)
		dbNode.Labels = string(encoded)
		dbStart := time.Now()
		if err := nm.controller.db.SaveNode(ctx, dbNode); err != nil {
			log.Debug("NodeManager.SetNodeLabels: saving node record failed", zap.String("node", snapshot.Name),
				zap.String("address", snapshot.Address), zap.Duration("took", time.Since(dbStart)), zap.Error(err))
			return nil, fmt.Errorf("persist node labels: %w", err)
		}
		log.Debug("NodeManager.SetNodeLabels: saved node record", zap.String("node", snapshot.Name),
			zap.String("address", snapshot.Address), zap.Duration("took", time.Since(dbStart)))
	} else {
		log.Debug("NodeManager.SetNodeLabels: no database, labels kept in memory only", zap.String("node", snapshot.Name))
	}

	nm.controller.logger.Info("Set node labels",
		zap.String("node", snapshot.Name), zap.Any("labels", labelsCopy), zap.Bool("replace", replace))
	snapshot.Labels = labelsCopy
	return &snapshot, nil
}

// UnregisterNode unregisters a node. It refuses while any resource or gateway
// still places something on the node (see checkNodeUnreferenced).
func (nm *NodeManager) UnregisterNode(ctx context.Context, address string) error {
	log := nm.controller.traceLogger()
	nm.mu.Lock()
	defer nm.mu.Unlock()

	resolved := nm.controller.ResolveHost(address)
	log.Debug("NodeManager.UnregisterNode: start", zap.String("address", address), zap.String("resolved", resolved))
	node := nm.nodes[resolved]
	if node == nil {
		log.Debug("NodeManager.UnregisterNode: node not found", zap.String("address", address), zap.String("resolved", resolved))
		return fmt.Errorf("node not found: %s", address)
	}
	if err := nm.checkNodeUnreferenced(ctx, node, resolved); err != nil {
		log.Debug("NodeManager.UnregisterNode: refused, node still referenced",
			zap.String("node", node.Name), zap.Error(err))
		return err
	}

	nm.controller.logger.Info("Unregistering node",
		zap.String("address", address),
		zap.String("resolved", resolved),
		zap.String("name", node.Name))

	delete(nm.nodes, resolved)

	nm.controller.hostsLock.Lock()
	filteredHosts := make([]string, 0, len(nm.controller.hosts))
	for _, host := range nm.controller.hosts {
		if host != resolved {
			filteredHosts = append(filteredHosts, host)
		}
	}
	nm.controller.hosts = filteredHosts

	delete(nm.controller.hostsMap, resolved)
	if node.Name != "" {
		delete(nm.controller.hostsMap, node.Name)
	}
	if node.Hostname != "" {
		delete(nm.controller.hostsMap, node.Hostname)
	}
	nm.controller.gateway.SetHosts(nm.controller.hosts)
	nm.controller.hostsLock.Unlock()
	log.Debug("NodeManager.UnregisterNode: removed from host list and name map",
		zap.String("node", node.Name), zap.String("hostname", node.Hostname), zap.Int("hosts", len(filteredHosts)))

	// Delete from database
	if nm.controller.db != nil {
		dbStart := time.Now()
		err := nm.controller.db.DeleteNode(ctx, resolved)
		log.Debug("NodeManager.UnregisterNode: deleted node record",
			zap.String("address", resolved), zap.Duration("took", time.Since(dbStart)), zap.Error(err))
		if err != nil {
			nm.controller.logger.Error("Failed to delete node from database", zap.Error(err))
		}
	} else {
		log.Debug("NodeManager.UnregisterNode: no database to delete from", zap.String("address", resolved))
	}

	return nil
}

// addHostsEntry adds the new node's hostname and IP to /etc/hosts on all existing nodes
func (nm *NodeManager) addHostsEntry(ctx context.Context, ip, hostname string) {
	log := nm.controller.traceLogger()
	log.Debug("NodeManager.addHostsEntry: start", zap.String("ip", ip), zap.String("hostname", hostname))
	if hostname == "" || hostname == ip {
		log.Debug("NodeManager.addHostsEntry: skipped, no hostname distinct from the address",
			zap.String("ip", ip), zap.String("hostname", hostname))
		return
	}

	// Prepare hosts entry
	hostsEntry := fmt.Sprintf("%s\t%s", ip, hostname)

	// Get list of all existing nodes (excluding the new one)
	nm.mu.RLock()
	var existingNodes []string
	for _, node := range nm.nodes {
		if node.Hostname != hostname && node.Hostname != "" {
			existingNodes = append(existingNodes, node.Hostname)
		}
	}
	nm.mu.RUnlock()

	if len(existingNodes) == 0 {
		log.Debug("NodeManager.addHostsEntry: skipped, no other node to tell", zap.String("hostname", hostname))
		return
	}

	// Add to each existing node's /etc/hosts
	hostsCmd := fmt.Sprintf(
		"grep -q '%s' /etc/hosts || echo '%s' | sudo tee -a /etc/hosts > /dev/null",
		hostsEntry, hostsEntry,
	)

	start := time.Now()
	result, err := nm.controller.deployment.Exec(ctx, existingNodes, hostsCmd)
	if err != nil {
		log.Debug("NodeManager.addHostsEntry: append to /etc/hosts failed to run",
			zap.Strings("hosts", existingNodes), zap.Duration("took", time.Since(start)), zap.Error(err))
		nm.controller.logger.Warn("Failed to add hosts entry", zap.Error(err))
		return
	}
	log.Debug("NodeManager.addHostsEntry: appended to /etc/hosts",
		zap.Strings("hosts", existingNodes), zap.Bool("success", result.AllSuccess()),
		zap.Strings("failed_hosts", result.FailedHosts()), zap.Duration("took", time.Since(start)))

	for nodeHost, r := range result.Hosts {
		log.Debug("NodeManager.addHostsEntry: host result", zap.String("host", nodeHost), zap.Bool("success", r.Success))
		if !r.Success {
			nm.controller.logger.Warn("Failed to add hosts entry",
				zap.String("node", nodeHost),
				zap.String("output", r.Output))
		}
	}
}

// assertNodesOnline refuses nodes the health check last found offline. A node
// the controller does not know is left to the steps that follow, which report
// it in their own terms.
func (rm *ResourceManager) assertNodesOnline(nodes []string) error {
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.assertNodesOnline: start", zap.Strings("nodes", nodes))
	var offline []string
	for _, n := range nodes {
		addr := rm.controller.ResolveHost(n)
		rm.controller.nodes.mu.RLock()
		info := rm.controller.nodes.nodes[addr]
		rm.controller.nodes.mu.RUnlock()
		state := ""
		if info != nil {
			state = string(info.State)
		}
		log.Debug("ResourceManager.assertNodesOnline: node state", zap.String("node", n), zap.String("address", addr),
			zap.Bool("known", info != nil), zap.String("state", state))
		if info != nil && info.State == NodeStateOffline {
			offline = append(offline, n)
		}
	}
	if len(offline) > 0 {
		log.Debug("ResourceManager.assertNodesOnline: refusing, nodes offline", zap.Strings("offline", offline))
		return fmt.Errorf("node(s) %s offline: the controller cannot reach them (see haify node list and the node.unreachable alert for why)",
			strings.Join(offline, ", "))
	}
	log.Debug("ResourceManager.assertNodesOnline: all nodes usable", zap.Int("count", len(nodes)))
	return nil
}

// nodeRecord is the database form of a node.
func nodeRecord(n *NodeInfo) *database.Node {
	rec := &database.Node{
		Name:               n.Name,
		Address:            n.Address,
		ReplicationAddress: n.ReplicationAddress,
		ReplicationPaths:   n.ReplicationPaths,
		Hostname:           n.Hostname,
		State:              string(n.State),
		LastSeen:           n.LastSeen,
		Version:            n.Version,
		OfflineSince:       n.OfflineSince,
	}
	if len(n.Labels) > 0 {
		if encoded, err := json.Marshal(n.Labels); err == nil {
			rec.Labels = string(encoded)
		} else {
			zap.L().Debug("nodeRecord: labels not encoded", zap.String("node", n.Name), zap.Error(err))
		}
	}
	return rec
}
