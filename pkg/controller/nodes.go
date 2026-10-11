package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
	replicationAddress = strings.TrimSpace(replicationAddress)
	nm.controller.logger.Info("Registering node",
		zap.String("name", name),
		zap.String("address", address),
		zap.String("replication_address", replicationAddress))

	// Check node health by executing hostname command
	result, err := nm.controller.deployment.Exec(ctx, []string{address}, "hostname")
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node: %w", err)
	}

	if !result.AllSuccess() {
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
		if err := nm.controller.db.SaveNode(ctx, dbNode); err != nil {
			nm.controller.logger.Error("Failed to save node to database", zap.Error(err))
		}
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
	resolved := nm.controller.ResolveHost(nodeRef)

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
	}
	if node == nil {
		nm.mu.Unlock()
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
			continue
		}
		if v == "" {
			delete(node.Labels, k) // empty value deletes the key
			continue
		}
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
			return nil, fmt.Errorf("encode labels: %w", err)
		}
		// The whole record, replication address included: this used to be
		// rebuilt field by field without it, so labelling a node silently
		// moved its DRBD traffic back onto the management network at the next
		// config it took part in.
		dbNode := nodeRecord(&snapshot)
		dbNode.Labels = string(encoded)
		if err := nm.controller.db.SaveNode(ctx, dbNode); err != nil {
			return nil, fmt.Errorf("persist node labels: %w", err)
		}
	}

	nm.controller.logger.Info("Set node labels",
		zap.String("node", snapshot.Name), zap.Any("labels", labelsCopy), zap.Bool("replace", replace))
	snapshot.Labels = labelsCopy
	return &snapshot, nil
}

// UnregisterNode unregisters a node. It refuses while any resource or gateway
// still places something on the node (see checkNodeUnreferenced).
func (nm *NodeManager) UnregisterNode(ctx context.Context, address string) error {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	resolved := nm.controller.ResolveHost(address)
	node := nm.nodes[resolved]
	if node == nil {
		return fmt.Errorf("node not found: %s", address)
	}
	if err := nm.checkNodeUnreferenced(ctx, node, resolved); err != nil {
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

	// Delete from database
	if nm.controller.db != nil {
		if err := nm.controller.db.DeleteNode(ctx, resolved); err != nil {
			nm.controller.logger.Error("Failed to delete node from database", zap.Error(err))
		}
	}

	return nil
}

// GetNodeAddressByName gets node address by node name
func (nm *NodeManager) GetNodeAddressByName(name string) string {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	for addr, node := range nm.nodes {
		if node.Name == name || node.Hostname == name || addr == name {
			return addr
		}
	}
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

	for addr, node := range nm.nodes {
		if node.Name == ref || node.Hostname == ref || addr == ref {
			if node.Hostname != "" {
				return node.Hostname
			}
			return ref
		}
	}
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

	for addr, node := range nm.nodes {
		if node.Name == name || node.Hostname == name || addr == name {
			if r := strings.TrimSpace(node.ReplicationAddress); r != "" {
				return r
			}
			return addr
		}
	}
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
			return node.Name
		}
	}
	return ""
}

// GetNode gets node information
func (nm *NodeManager) GetNode(ctx context.Context, address string) (*NodeInfo, error) {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	resolved := address
	if node := nm.nodes[resolved]; node != nil {
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
		return nil, fmt.Errorf("node not found: %s", address)
	}

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

	return nodes, nil
}

// addHostsEntry adds the new node's hostname and IP to /etc/hosts on all existing nodes
func (nm *NodeManager) addHostsEntry(ctx context.Context, ip, hostname string) {
	if hostname == "" || hostname == ip {
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
		return
	}

	// Add to each existing node's /etc/hosts
	hostsCmd := fmt.Sprintf(
		"grep -q '%s' /etc/hosts || echo '%s' | sudo tee -a /etc/hosts > /dev/null",
		hostsEntry, hostsEntry,
	)

	result, err := nm.controller.deployment.Exec(ctx, existingNodes, hostsCmd)
	if err != nil {
		nm.controller.logger.Warn("Failed to add hosts entry", zap.Error(err))
		return
	}

	for nodeHost, r := range result.Hosts {
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
	var offline []string
	for _, n := range nodes {
		addr := rm.controller.ResolveHost(n)
		rm.controller.nodes.mu.RLock()
		info := rm.controller.nodes.nodes[addr]
		rm.controller.nodes.mu.RUnlock()
		if info != nil && info.State == NodeStateOffline {
			offline = append(offline, n)
		}
	}
	if len(offline) > 0 {
		return fmt.Errorf("node(s) %s offline: the controller cannot reach them (see haify node list and the node.unreachable alert for why)",
			strings.Join(offline, ", "))
	}
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
		}
	}
	return rec
}
