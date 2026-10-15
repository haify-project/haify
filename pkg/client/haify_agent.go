package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// RegisterLocalAgent registers the node this process runs on as a local-agent
// node: one the controller does not reach over SSH, whose DRBD configuration
// this process applies itself. A node already registered for SSH stays so; the
// returned node says which it is (LocalAgent).
func (c *HaifyClient) RegisterLocalAgent(ctx context.Context, name, address, hostname string) (*haifypb.NodeInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.RegisterLocalAgent: sending request", zap.String("name", name), zap.String("address", address),
		zap.String("hostname", hostname))
	resp, err := c.client.RegisterNode(ctx, &haifypb.RegisterNodeRequest{
		Name:       name,
		Address:    address,
		LocalAgent: true,
		Hostname:   hostname,
	})
	if err != nil {
		log.Debug("HaifyClient.RegisterLocalAgent: RegisterNode failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.RegisterLocalAgent: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.RegisterLocalAgent: done", zap.Bool("local_agent", resp.Node.GetLocalAgent()))
	return resp.Node, nil
}

// GetNodeAgentState returns the resources the local-agent node must have up,
// with their DRBD configuration. It is also the agent's heartbeat.
func (c *HaifyClient) GetNodeAgentState(ctx context.Context, node string) ([]*haifypb.AgentResource, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetNodeAgentState: sending request", zap.String("node", node))
	resp, err := c.client.GetNodeAgentState(ctx, &haifypb.GetNodeAgentStateRequest{Node: node})
	if err != nil {
		log.Debug("HaifyClient.GetNodeAgentState: call failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.GetNodeAgentState: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.GetNodeAgentState: done", zap.Int("resources", len(resp.Resources)))
	return resp.Resources, nil
}
