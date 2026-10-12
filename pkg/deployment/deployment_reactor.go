package deployment

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"
)

// ============ Reactor Operations ============

// ReactorWriteConfig writes reactor plugin config
func (c *Client) ReactorWriteConfig(ctx context.Context, hosts []string, pluginID, content string) (*ConfigResult, error) {
	remotePath := fmt.Sprintf("/etc/drbd-reactor.d/%s.toml", pluginID)
	c.log().Debug("ReactorWriteConfig: writing plugin config",
		zap.Strings("hosts", hosts), zap.String("plugin", pluginID), zap.String("path", remotePath),
		zap.Int("size_bytes", len(content)))
	return c.DistributeConfig(ctx, hosts, content, remotePath,
		WithPostCommand("sudo systemctl reload drbd-reactor || sudo systemctl restart drbd-reactor"))
}

// ReactorEnablePlugin enables a promoter plugin
func (c *Client) ReactorEnablePlugin(ctx context.Context, hosts []string, pluginID string) (*ExecResult, error) {
	c.log().Debug("ReactorEnablePlugin: enabling promoter", zap.Strings("hosts", hosts), zap.String("plugin", pluginID))
	return c.Exec(ctx, hosts, fmt.Sprintf("sudo drbd-reactorctl prom enable %s", pluginID))
}

// ReactorDisablePlugin disables a promoter plugin
func (c *Client) ReactorDisablePlugin(ctx context.Context, hosts []string, pluginID string) (*ExecResult, error) {
	c.log().Debug("ReactorDisablePlugin: disabling promoter", zap.Strings("hosts", hosts), zap.String("plugin", pluginID))
	return c.Exec(ctx, hosts, fmt.Sprintf("sudo drbd-reactorctl prom disable %s", pluginID))
}

// ReactorEvict evicts a promoter from the current node
func (c *Client) ReactorEvict(ctx context.Context, config string) (*ExecResult, error) {
	c.log().Debug("ReactorEvict: evicting promoter from this node", zap.String("config", config))
	return c.Exec(ctx, []string{"localhost"}, fmt.Sprintf("sudo drbd-reactorctl evict %s", config))
}

// ReactorReload reloads drbd-reactor
func (c *Client) ReactorReload(ctx context.Context, hosts []string) (*ExecResult, error) {
	c.log().Debug("ReactorReload: reloading drbd-reactor", zap.Strings("hosts", hosts))
	return c.Exec(ctx, hosts, "sudo systemctl reload drbd-reactor || sudo systemctl restart drbd-reactor")
}

// ============ Reactor Status ============

// ReactorStatus represents the status output from drbd-reactorctl status --json
type ReactorStatus struct {
	Promoter   []ReactorPromoterStatus `json:"promoter"`
	Prometheus []ReactorPluginStatus   `json:"prometheus"`
	Debugger   []ReactorPluginStatus   `json:"debugger"`
	UMH        []ReactorPluginStatus   `json:"umh"`
	AgentX     []ReactorPluginStatus   `json:"agentx"`
}

// ReactorPromoterStatus represents status of a promoter plugin
type ReactorPromoterStatus struct {
	DRBDResource string                 `json:"drbd_resource"`
	Path         string                 `json:"path"`
	PrimaryOn    string                 `json:"primary_on"`
	Target       ReactorServiceStatus   `json:"target"`
	Dependencies []ReactorServiceStatus `json:"dependencies"`
	Status       string                 `json:"status"`
}

// ReactorServiceStatus represents status of a systemd service
type ReactorServiceStatus struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Freezer string `json:"freezer"`
}

// ReactorPluginStatus represents status of a generic reactor plugin
type ReactorPluginStatus struct {
	Path    string `json:"path"`
	Address string `json:"address,omitempty"`
	Status  string `json:"status"`
}

// ReactorStatusJSON gets the reactor status using JSON output
func (c *Client) ReactorStatusJSON(ctx context.Context, host string) (*ReactorStatus, error) {
	log := c.log()
	log.Debug("ReactorStatusJSON: reading reactor status", zap.String("host", host))
	result, err := c.Exec(ctx, []string{host}, "sudo drbd-reactorctl status --json")
	if err != nil {
		log.Debug("ReactorStatusJSON: exec errored", zap.String("host", host), zap.Error(err))
		return nil, fmt.Errorf("failed to get reactor status: %w", err)
	}

	for _, r := range result.Hosts {
		if !r.Success {
			log.Debug("ReactorStatusJSON: command failed", zap.String("host", host))
			return nil, fmt.Errorf("reactor status command failed: %s", r.Output)
		}

		var status ReactorStatus
		if err := jsonUnmarshal([]byte(r.Output), &status); err != nil {
			log.Debug("ReactorStatusJSON: cannot parse status", zap.String("host", host),
				zap.Int("output_len", len(r.Output)), zap.Error(err))
			return nil, fmt.Errorf("failed to parse reactor status JSON: %w", err)
		}
		log.Debug("ReactorStatusJSON: status read", zap.String("host", host), zap.Int("promoters", len(status.Promoter)))
		return &status, nil
	}

	log.Debug("ReactorStatusJSON: no result", zap.String("host", host))
	return nil, fmt.Errorf("no result from reactor status command")
}

// ReactorPromoterStatusByResource gets the promoter status for a specific DRBD resource
func (c *Client) ReactorPromoterStatusByResource(ctx context.Context, host, resource string) (*ReactorPromoterStatus, error) {
	log := c.log()
	log.Debug("ReactorPromoterStatusByResource: looking up promoter", zap.String("host", host), zap.String("resource", resource))
	status, err := c.ReactorStatusJSON(ctx, host)
	if err != nil {
		log.Debug("ReactorPromoterStatusByResource: status unavailable", zap.String("host", host), zap.Error(err))
		return nil, err
	}

	for _, promoter := range status.Promoter {
		if promoter.DRBDResource == resource {
			log.Debug("ReactorPromoterStatusByResource: found promoter",
				zap.String("resource", resource), zap.String("primary_on", promoter.PrimaryOn), zap.String("status", promoter.Status))
			return &promoter, nil
		}
	}

	log.Debug("ReactorPromoterStatusByResource: no promoter for resource",
		zap.String("resource", resource), zap.Int("promoters", len(status.Promoter)))
	return nil, fmt.Errorf("promoter status for resource %s not found", resource)
}

// jsonUnmarshal is a local wrapper for json.Unmarshal
func jsonUnmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
