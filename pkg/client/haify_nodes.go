package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// RegisterNode registers a new node
func (c *HaifyClient) RegisterNode(ctx context.Context, name, address string) (*haifypb.NodeInfo, error) {
	return c.RegisterNodeWithReplicationAddress(ctx, name, address, "")
}

// RegisterNodeWithReplicationAddress registers a node whose DRBD traffic should
// use a dedicated address (NIC/subnet) instead of the management address the
// controller SSHes to. An empty replicationAddress means they are the same.
func (c *HaifyClient) RegisterNodeWithReplicationAddress(ctx context.Context, name, address, replicationAddress string) (*haifypb.NodeInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.RegisterNodeWithReplicationAddress: sending request", zap.String("name", name), zap.String("address", address), zap.String("replication_address", replicationAddress))
	req := &haifypb.RegisterNodeRequest{
		Name:               name,
		Address:            address,
		ReplicationAddress: replicationAddress,
	}

	resp, err := c.client.RegisterNode(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.RegisterNodeWithReplicationAddress: RegisterNode failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.RegisterNodeWithReplicationAddress: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.RegisterNodeWithReplicationAddress: done")
	return resp.Node, nil
}

// SetNodeLabels sets or merges labels on a node (by name or address). With
// replace=true the label set is replaced wholesale; otherwise labels are merged
// (an empty value deletes that key).
func (c *HaifyClient) SetNodeLabels(ctx context.Context, node string, labels map[string]string, replace bool) (*haifypb.NodeInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.SetNodeLabels: sending request", zap.String("node", node), zap.Int("labels_count", len(labels)), zap.Bool("replace", replace))
	resp, err := c.client.SetNodeLabels(ctx, &haifypb.SetNodeLabelsRequest{
		Node:    node,
		Labels:  labels,
		Replace: replace,
	})
	if err != nil {
		log.Debug("HaifyClient.SetNodeLabels: SetNodeLabels failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.SetNodeLabels: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.SetNodeLabels: done")
	return resp.Node, nil
}

// ListNodes lists all nodes
func (c *HaifyClient) ListNodes(ctx context.Context) ([]*haifypb.NodeInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListNodes: sending request")
	req := &haifypb.ListNodesRequest{}

	resp, err := c.client.ListNodes(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ListNodes: ListNodes failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.ListNodes: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.ListNodes: done", zap.Int("count", len(resp.Nodes)))
	return resp.Nodes, nil
}

// GetNode retrieves a node by address, hostname, or registered name.
func (c *HaifyClient) GetNode(ctx context.Context, address string) (*haifypb.NodeInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetNode: sending request", zap.String("address", address))
	req := &haifypb.GetNodeRequest{
		Address: address,
	}

	resp, err := c.client.GetNode(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.GetNode: GetNode failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.GetNode: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.GetNode: done")
	return resp.Node, nil
}

// UnregisterNode unregisters a node
func (c *HaifyClient) UnregisterNode(ctx context.Context, address string) error {
	log := zap.L()
	log.Debug("HaifyClient.UnregisterNode: sending request", zap.String("address", address))
	req := &haifypb.UnregisterNodeRequest{
		Address: address,
	}

	resp, err := c.client.UnregisterNode(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.UnregisterNode: UnregisterNode failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.UnregisterNode: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.UnregisterNode: done")
	return nil
}

// DrainNode moves all Primary DRBD resources off the node and marks it maintenance.
func (c *HaifyClient) DrainNode(ctx context.Context, name string) ([]string, error) {
	log := zap.L()
	log.Debug("HaifyClient.DrainNode: sending request", zap.String("name", name))
	resp, err := c.client.DrainNode(ctx, &haifypb.DrainNodeRequest{Name: name})
	if err != nil {
		log.Debug("HaifyClient.DrainNode: DrainNode failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.DrainNode: controller reported failure", zap.String("message", resp.Message))
		return resp.ResourcesMoved, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.DrainNode: done", zap.Int("count", len(resp.ResourcesMoved)))
	return resp.ResourcesMoved, nil
}

// UndrainNode returns a drained node to active service.
func (c *HaifyClient) UndrainNode(ctx context.Context, name string) error {
	log := zap.L()
	log.Debug("HaifyClient.UndrainNode: sending request", zap.String("name", name))
	resp, err := c.client.UndrainNode(ctx, &haifypb.UndrainNodeRequest{Name: name})
	if err != nil {
		log.Debug("HaifyClient.UndrainNode: UndrainNode failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.UndrainNode: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.UndrainNode: done")
	return nil
}

// HealthCheck performs a health check on a node
func (c *HaifyClient) HealthCheck(ctx context.Context, node string) (*NodeHealthInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.HealthCheck: sending request", zap.String("node", node))
	req := &haifypb.HealthCheckRequest{
		Node: node,
	}

	resp, err := c.client.HealthCheck(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.HealthCheck: HealthCheck failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.HealthCheck: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.HealthCheck: done")
	return &NodeHealthInfo{
		DrbdInstalled:           resp.Health.DrbdInstalled,
		DrbdVersion:             resp.Health.DrbdVersion,
		DrbdReactorInstalled:    resp.Health.DrbdReactorInstalled,
		DrbdReactorVersion:      resp.Health.DrbdReactorVersion,
		DrbdReactorRunning:      resp.Health.DrbdReactorRunning,
		ResourceAgentsInstalled: resp.Health.ResourceAgentsInstalled,
		AvailableAgents:         resp.Health.AvailableAgents,
	}, nil
}

// NodeHealthInfo represents the health status of a node
type NodeHealthInfo struct {
	DrbdInstalled           bool     `json:"drbd_installed"`
	DrbdVersion             string   `json:"drbd_version"`
	DrbdReactorInstalled    bool     `json:"drbd_reactor_installed"`
	DrbdReactorVersion      string   `json:"drbd_reactor_version"`
	DrbdReactorRunning      bool     `json:"drbd_reactor_running"`
	ResourceAgentsInstalled bool     `json:"resource_agents_installed"`
	AvailableAgents         []string `json:"available_agents"`
}

// ListControllerLogs returns recent lines from the active controller's own log.
// CollectNodeDiagnostics reads the named read-only collectors on the named
// nodes. Unlike the three List* calls above, a partial answer is the normal
// answer: a node that is down is the finding, so the response is returned
// whole and each node carries its own reachability.
func (c *HaifyClient) CollectNodeDiagnostics(ctx context.Context, req *haifypb.CollectNodeDiagnosticsRequest) (*haifypb.CollectNodeDiagnosticsResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.CollectNodeDiagnostics: sending request", zap.Strings("nodes", req.GetNodes()), zap.Strings("collectors", req.GetCollectors()),
		zap.Int32("since_minutes", req.GetSinceMinutes()), zap.Int32("max_lines", req.GetMaxLines()))
	resp, err := c.client.CollectNodeDiagnostics(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.CollectNodeDiagnostics: CollectNodeDiagnostics failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.CollectNodeDiagnostics: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.CollectNodeDiagnostics: done")
	return resp, nil
}

// SetNodeAddress renumbers a registered node. The response lists the
// resources whose DRBD config was rewritten and any that failed; Success is
// false when one did, so read both.
func (c *HaifyClient) SetNodeAddress(ctx context.Context, node, address, replicationAddress string) (*haifypb.SetNodeAddressResponse, error) {
	zap.L().Debug("HaifyClient.SetNodeAddress: sending request", zap.String("node", node), zap.String("address", address), zap.String("replication_address", replicationAddress))
	return c.client.SetNodeAddress(ctx, &haifypb.SetNodeAddressRequest{
		Node: node, Address: address, ReplicationAddress: replicationAddress,
	})
}

// SetNodeAddresses renumbers several nodes together — what every node getting
// a new DHCP lease at once needs.
func (c *HaifyClient) SetNodeAddresses(ctx context.Context, moves []*haifypb.NodeAddressMove) (*haifypb.SetNodeAddressResponse, error) {
	zap.L().Debug("HaifyClient.SetNodeAddresses: sending request", zap.Int("moves_count", len(moves)))
	return c.client.SetNodeAddress(ctx, &haifypb.SetNodeAddressRequest{Moves: moves})
}

// SetNodeReplicationPaths sets a node's extra replication addresses (empty
// clears them) and rewrites its resources.
func (c *HaifyClient) SetNodeReplicationPaths(ctx context.Context, node string, addresses []string) (*haifypb.SetNodeAddressResponse, error) {
	zap.L().Debug("HaifyClient.SetNodeReplicationPaths: sending request", zap.String("node", node), zap.Strings("addresses", addresses))
	return c.client.SetNodeReplicationPaths(ctx, &haifypb.SetNodeReplicationPathsRequest{Node: node, Addresses: addresses})
}
