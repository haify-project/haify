package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// MakeHa creates a drbd-reactor promoter config for HA failover. Optional
// ocfAgents are appended, in order, to the promoter start[] list; pass nil to
// keep the historical behavior.
func (c *HaifyClient) MakeHa(ctx context.Context, resource string, services []string, mountPoint, fsType, vip string, ocfAgents []*haifypb.OcfAgent, startItems []*haifypb.HaStartItem) (string, error) {
	log := zap.L()
	log.Debug("HaifyClient.MakeHa: sending request", zap.String("resource", resource), zap.Strings("services", services), zap.String("mount_point", mountPoint), zap.String("fs_type", fsType), zap.String("vip", vip), zap.Int("ocf_agents_count", len(ocfAgents)), zap.Int("start_items_count", len(startItems)))
	req := &haifypb.MakeHaRequest{
		Resource:   resource,
		Services:   services,
		MountPoint: mountPoint,
		Fstype:     fsType,
		Vip:        vip,
		OcfAgents:  ocfAgents,
		StartItems: startItems,
	}

	resp, err := c.client.MakeHa(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.MakeHa: MakeHa failed", zap.Error(err))
		return "", err
	}

	if !resp.Success {
		log.Debug("HaifyClient.MakeHa: controller reported failure", zap.String("message", resp.Message))
		return "", fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.MakeHa: done")
	return resp.ConfigPath, nil
}

// EnableSelfHa makes the controller itself highly available. It returns the
// metadata resource name and the node-local handoff log path.
func (c *HaifyClient) EnableSelfHa(ctx context.Context, vip, pool string, sizeGB, port uint32, nodes []string) (string, string, error) {
	log := zap.L()
	log.Debug("HaifyClient.EnableSelfHa: sending request", zap.String("vip", vip), zap.String("pool", pool), zap.Uint32("size_gb", sizeGB), zap.Uint32("port", port), zap.Strings("nodes", nodes))
	resp, err := c.client.EnableSelfHa(ctx, &haifypb.EnableSelfHaRequest{
		Vip:    vip,
		Pool:   pool,
		SizeGb: sizeGB,
		Port:   port,
		Nodes:  nodes,
	})
	if err != nil {
		log.Debug("HaifyClient.EnableSelfHa: EnableSelfHa failed", zap.Error(err))
		return "", "", err
	}
	if !resp.Success {
		log.Debug("HaifyClient.EnableSelfHa: controller reported failure", zap.String("message", resp.Message))
		return "", "", fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.EnableSelfHa: done")
	return resp.Resource, resp.HandoffLog, nil
}

// DisableSelfHa reverts the controller to standalone operation on node.
func (c *HaifyClient) DisableSelfHa(ctx context.Context, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.DisableSelfHa: sending request", zap.String("node", node))
	resp, err := c.client.DisableSelfHa(ctx, &haifypb.DisableSelfHaRequest{Node: node})
	if err != nil {
		log.Debug("HaifyClient.DisableSelfHa: DisableSelfHa failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.DisableSelfHa: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.DisableSelfHa: done")
	return nil
}

// SelfHaStatus describes the controller self-HA state.
type SelfHaStatus struct {
	Enabled    bool
	Resource   string
	VIP        string
	Nodes      []string
	ActiveNode string
}

// GetSelfHaStatus reports the controller self-HA state.
func (c *HaifyClient) GetSelfHaStatus(ctx context.Context) (*SelfHaStatus, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetSelfHaStatus: sending request")
	resp, err := c.client.GetSelfHaStatus(ctx, &haifypb.GetSelfHaStatusRequest{})
	if err != nil {
		log.Debug("HaifyClient.GetSelfHaStatus: GetSelfHaStatus failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.GetSelfHaStatus: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.GetSelfHaStatus: done")
	return &SelfHaStatus{
		Enabled:    resp.Enabled,
		Resource:   resp.Resource,
		VIP:        resp.Vip,
		Nodes:      resp.Nodes,
		ActiveNode: resp.ActiveNode,
	}, nil
}

func (c *HaifyClient) EvictHa(ctx context.Context, resource string) error {
	log := zap.L()
	log.Debug("HaifyClient.EvictHa: sending request", zap.String("resource", resource))
	req := &haifypb.EvictHaRequest{
		Resource: resource,
	}

	resp, err := c.client.EvictHa(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.EvictHa: EvictHa failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.EvictHa: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.EvictHa: done")
	return nil
}

// DeleteHa deletes an HA configuration
func (c *HaifyClient) DeleteHa(ctx context.Context, resource string) error {
	log := zap.L()
	log.Debug("HaifyClient.DeleteHa: sending request", zap.String("resource", resource))
	req := &haifypb.DeleteHaRequest{
		Resource: resource,
	}

	resp, err := c.client.DeleteHa(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.DeleteHa: DeleteHa failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.DeleteHa: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.DeleteHa: done")
	return nil
}

// GetHa gets an HA configuration
func (c *HaifyClient) GetHa(ctx context.Context, resource string) (*haifypb.HaConfigInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetHa: sending request", zap.String("resource", resource))
	req := &haifypb.GetHaRequest{
		Resource: resource,
	}

	resp, err := c.client.GetHa(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.GetHa: GetHa failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.GetHa: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.GetHa: done")
	return resp.Config, nil
}

// ListHa lists all HA configurations
func (c *HaifyClient) ListHa(ctx context.Context) ([]*haifypb.HaConfigInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListHa: sending request")
	req := &haifypb.ListHaRequest{}

	resp, err := c.client.ListHa(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ListHa: ListHa failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.ListHa: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.ListHa: done", zap.Int("count", len(resp.Configs)))
	return resp.Configs, nil
}

// GetHaStatus retrieves drbd-reactor promoter status for one or all HA
// resources by querying the primary node via the controller's SSH dispatch.
func (c *HaifyClient) GetHaStatus(ctx context.Context, resource string) ([]*haifypb.HaPromoterStatus, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetHaStatus: sending request", zap.String("resource", resource))
	resp, err := c.client.GetHaStatus(ctx, &haifypb.GetHaStatusRequest{Resource: resource})
	if err != nil {
		log.Debug("HaifyClient.GetHaStatus: GetHaStatus failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.GetHaStatus: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.GetHaStatus: done", zap.Int("count", len(resp.Promoters)))
	return resp.Promoters, nil
}

// ListResourceAgents lists the OCF resource agents available on the nodes.
func (c *HaifyClient) ListResourceAgents(ctx context.Context) ([]*haifypb.ResourceAgentInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListResourceAgents: sending request")
	resp, err := c.client.ListResourceAgents(ctx, &haifypb.ListResourceAgentsRequest{})
	if err != nil {
		log.Debug("HaifyClient.ListResourceAgents: ListResourceAgents failed", zap.Error(err))
		return nil, err
	}
	log.Debug("HaifyClient.ListResourceAgents: done", zap.Int("count", len(resp.Agents)))
	return resp.Agents, nil
}

// GetResourceAgentMetadata returns an OCF agent's parsed meta-data schema.
func (c *HaifyClient) GetResourceAgentMetadata(ctx context.Context, provider, name string) (*haifypb.GetResourceAgentMetadataResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetResourceAgentMetadata: sending request", zap.String("provider", provider), zap.String("name", name))
	resp, err := c.client.GetResourceAgentMetadata(ctx, &haifypb.GetResourceAgentMetadataRequest{
		Provider: provider,
		Name:     name,
	})
	if err != nil {
		log.Debug("HaifyClient.GetResourceAgentMetadata: GetResourceAgentMetadata failed", zap.Error(err))
		return nil, err
	}
	log.Debug("HaifyClient.GetResourceAgentMetadata: done")
	return resp, nil
}

// GetHaToml reads a resource's drbd-reactor promoter TOML.
func (c *HaifyClient) GetHaToml(ctx context.Context, resource string) (*haifypb.GetHaTomlResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetHaToml: sending request", zap.String("resource", resource))
	resp, err := c.client.GetHaToml(ctx, &haifypb.GetHaTomlRequest{Resource: resource})
	if err != nil {
		log.Debug("HaifyClient.GetHaToml: GetHaToml failed", zap.Error(err))
		return nil, err
	}
	log.Debug("HaifyClient.GetHaToml: done")
	return resp, nil
}

// SyncHaToml writes an edited promoter TOML to all resource nodes and reloads
// drbd-reactor.
func (c *HaifyClient) SyncHaToml(ctx context.Context, resource, content string) (string, error) {
	log := zap.L()
	log.Debug("HaifyClient.SyncHaToml: sending request", zap.String("resource", resource), zap.Int("content_bytes", len(content)))
	resp, err := c.client.SyncHaToml(ctx, &haifypb.SyncHaTomlRequest{
		Resource: resource,
		Content:  content,
	})
	if err != nil {
		log.Debug("HaifyClient.SyncHaToml: SyncHaToml failed", zap.Error(err))
		return "", err
	}
	if !resp.Success {
		log.Debug("HaifyClient.SyncHaToml: controller reported failure", zap.String("message", resp.Message))
		return "", fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.SyncHaToml: done")
	return resp.Message, nil
}
