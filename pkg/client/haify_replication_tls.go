package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// SetupReplicationTLS prepares nodes (all registered ones when empty) for
// encrypted DRBD replication and reports each node's readiness.
func (c *HaifyClient) SetupReplicationTLS(ctx context.Context, nodes []string) ([]*haifypb.NodeTLSInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.SetupReplicationTLS: sending request", zap.Strings("nodes", nodes))
	resp, err := c.client.SetupReplicationTLS(ctx, &haifypb.SetupReplicationTLSRequest{Nodes: nodes})
	if err != nil {
		log.Debug("HaifyClient.SetupReplicationTLS: SetupReplicationTLS failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.SetupReplicationTLS: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.SetupReplicationTLS: done", zap.Int("count", len(resp.Nodes)))
	return resp.Nodes, nil
}

// ReplicationTLSStatus reports whether each node can carry an encrypted
// connection, and what is missing when it cannot.
func (c *HaifyClient) ReplicationTLSStatus(ctx context.Context, nodes []string) ([]*haifypb.NodeTLSInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ReplicationTLSStatus: sending request", zap.Strings("nodes", nodes))
	resp, err := c.client.GetReplicationTLSStatus(ctx, &haifypb.GetReplicationTLSStatusRequest{Nodes: nodes})
	if err != nil {
		log.Debug("HaifyClient.ReplicationTLSStatus: GetReplicationTLSStatus failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ReplicationTLSStatus: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ReplicationTLSStatus: done", zap.Int("count", len(resp.Nodes)))
	return resp.Nodes, nil
}

// SetResourceTLS switches a resource's connections to or from TLS.
func (c *HaifyClient) SetResourceTLS(ctx context.Context, resource string, enabled bool) (string, error) {
	log := zap.L()
	log.Debug("HaifyClient.SetResourceTLS: sending request", zap.String("resource", resource), zap.Bool("enabled", enabled))
	resp, err := c.client.SetResourceTLS(ctx, &haifypb.SetResourceTLSRequest{Resource: resource, Enabled: enabled})
	if err != nil {
		log.Debug("HaifyClient.SetResourceTLS: SetResourceTLS failed", zap.Error(err))
		return "", err
	}
	if !resp.Success {
		log.Debug("HaifyClient.SetResourceTLS: controller reported failure", zap.String("message", resp.Message))
		return "", fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.SetResourceTLS: done")
	return resp.Message, nil
}
