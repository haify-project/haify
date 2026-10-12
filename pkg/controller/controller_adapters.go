package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/deployment"
	"github.com/haify-project/haify/pkg/gateway"
	"github.com/haify-project/haify/pkg/wanproxy"
)

// GatewayResourceManager adapts ResourceManager to gateway.ResourceManager interface
type GatewayResourceManager struct {
	rm                *ResourceManager
	autoStateVolume   bool
	stateVolumeSizeGB uint32
}

// NewGatewayResourceManager creates a new gateway resource manager adapter.
// autoStateVolume/stateVolumeSizeGB control whether a missing cluster-private
// state volume is provisioned automatically during gateway creation.
func NewGatewayResourceManager(rm *ResourceManager, autoStateVolume bool, stateVolumeSizeGB uint32) gateway.ResourceManager {
	if stateVolumeSizeGB == 0 {
		zap.L().Debug("NewGatewayResourceManager: state volume size unset, using 1 GiB")
		stateVolumeSizeGB = 1
	}
	zap.L().Debug("NewGatewayResourceManager: built", zap.Bool("auto_state_volume", autoStateVolume),
		zap.Uint32("state_volume_size_gb", stateVolumeSizeGB))
	return &GatewayResourceManager{
		rm:                rm,
		autoStateVolume:   autoStateVolume,
		stateVolumeSizeGB: stateVolumeSizeGB,
	}
}

// EnsureGatewayVolumes provisions the small cluster-private state volume(s) a
// gateway needs, so a single-volume resource can be exported directly. It is a
// no-op when the resource already has enough volumes or when auto-provisioning
// is disabled (the gateway's own check then surfaces a clear error).
func (a *GatewayResourceManager) EnsureGatewayVolumes(ctx context.Context, resource string, minVolumes int) error {
	log := a.rm.controller.traceLogger()
	log.Debug("GatewayResourceManager.EnsureGatewayVolumes: start", zap.String("resource", resource),
		zap.Int("min_volumes", minVolumes), zap.Bool("auto_state_volume", a.autoStateVolume))
	if !a.autoStateVolume {
		log.Debug("GatewayResourceManager.EnsureGatewayVolumes: auto-provisioning off, nothing to do")
		return nil
	}
	info, err := a.rm.GetResource(ctx, resource)
	if err != nil {
		log.Debug("GatewayResourceManager.EnsureGatewayVolumes: reading resource failed", zap.Error(err))
		return err
	}
	if len(info.Volumes) >= minVolumes {
		log.Debug("GatewayResourceManager.EnsureGatewayVolumes: enough volumes already", zap.Int("volumes", len(info.Volumes)))
		return nil
	}
	if len(info.Volumes) == 0 {
		log.Debug("GatewayResourceManager.EnsureGatewayVolumes: resource has no volumes")
		return fmt.Errorf("resource %q has no volumes to derive a pool from", resource)
	}
	pool := info.Volumes[0].Pool
	if pool == "" {
		log.Debug("GatewayResourceManager.EnsureGatewayVolumes: first volume has no pool")
		return fmt.Errorf("cannot determine storage pool for resource %q", resource)
	}
	for n := len(info.Volumes); n < minVolumes; n++ {
		volName := fmt.Sprintf("%s_state%d", resource, n)
		a.rm.controller.logger.Info("Auto-provisioning gateway state volume",
			zap.String("resource", resource),
			zap.String("volume", volName),
			zap.String("pool", pool),
			zap.Uint32("size_gb", a.stateVolumeSizeGB))
		if err := a.rm.AddVolume(ctx, resource, volName, pool, a.stateVolumeSizeGB); err != nil {
			log.Debug("GatewayResourceManager.EnsureGatewayVolumes: adding state volume failed",
				zap.String("volume", volName), zap.Error(err))
			return fmt.Errorf("auto-provision state volume %q: %w", volName, err)
		}
		log.Debug("GatewayResourceManager.EnsureGatewayVolumes: state volume added", zap.String("volume", volName))
	}
	return nil
}

func (a *GatewayResourceManager) GetResource(ctx context.Context, name string) (*gateway.ResourceInfo, error) {
	log := a.rm.controller.traceLogger()
	info, err := a.rm.GetResource(ctx, name)
	if err != nil {
		log.Debug("GatewayResourceManager.GetResource: reading resource failed", zap.String("resource", name), zap.Error(err))
		return nil, err
	}

	// Convert controller.ResourceInfo to gateway.ResourceInfo
	gwVolumes := make([]*gateway.ResourceVolumeInfo, len(info.Volumes))
	for i, v := range info.Volumes {
		gwVolumes[i] = &gateway.ResourceVolumeInfo{
			VolumeID: v.VolumeID,
			Device:   v.Device,
			SizeGB:   v.SizeGB,
			// The gateway tells its own auto-provisioned "<res>_state<N>"
			// volume from the operator's "<res>_data" by this name. Without
			// it, it can only go by position — which is what used to make it
			// export the wrong one.
			BackingVolume: v.BackingVolume,
		}
	}

	gwNodeStates := make(map[string]*gateway.ResourceNodeState)
	for k, v := range info.NodeStates {
		gwNodeStates[k] = &gateway.ResourceNodeState{
			Role:        v.Role,
			DiskState:   v.DiskState,
			Replication: v.Replication,
		}
	}

	// The nodes a gateway may run on: the diskful replicas, never a tiebreaker
	// or diskless client, and never the DR node, which failover to is manual.
	hosts, herr := a.rm.failoverHosts(ctx, name)
	log.Debug("GatewayResourceManager.GetResource: converted", zap.String("resource", name),
		zap.Int("volumes", len(gwVolumes)), zap.Strings("nodes", info.Nodes), zap.Strings("hosts", hosts),
		zap.NamedError("hosts_error", herr))

	return &gateway.ResourceInfo{
		Name:       info.Name,
		Port:       info.Port,
		Protocol:   info.Protocol,
		Nodes:      info.Nodes,
		Hosts:      hosts,
		Role:       info.Role,
		Volumes:    gwVolumes,
		NodeStates: gwNodeStates,
	}, nil
}

func (a *GatewayResourceManager) SetPrimary(ctx context.Context, resource, node string, force bool) error {
	err := a.rm.SetPrimary(ctx, resource, node, force)
	a.rm.controller.traceLogger().Debug("GatewayResourceManager.SetPrimary: done", zap.String("resource", resource),
		zap.String("node", node), zap.Bool("force", force), zap.Error(err))
	return err
}

// GatewayDeploymentClient adapts deployment.Client to gateway.DeploymentClient interface
type GatewayDeploymentClient struct {
	dc *deployment.Client
}

// NewGatewayDeploymentClient creates a new gateway deployment client adapter
func NewGatewayDeploymentClient(dc *deployment.Client) gateway.DeploymentClient {
	return &GatewayDeploymentClient{dc: dc}
}

func (a *GatewayDeploymentClient) DistributeConfig(ctx context.Context, hosts []string, content, remotePath string) error {
	// Gateway configs can hold CHAP or SMB secrets: the path and size only.
	start := time.Now()
	_, err := a.dc.DistributeConfig(ctx, hosts, content, remotePath)
	zap.L().Debug("GatewayDeploymentClient.DistributeConfig: distributed", zap.Strings("hosts", hosts),
		zap.String("path", remotePath), zap.Int("bytes", len(content)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

func (a *GatewayDeploymentClient) Exec(ctx context.Context, hosts []string, cmd string) error {
	// Gateway commands can carry CHAP or SMB secrets: only the length is traced.
	start := time.Now()
	result, err := a.dc.Exec(ctx, hosts, cmd)
	if err != nil {
		zap.L().Debug("GatewayDeploymentClient.Exec: exec failed", zap.Strings("hosts", hosts), zap.Int("cmd_len", len(cmd)),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return err
	}
	// Per-host command failures must surface: swallowing them let gateway
	// setup steps (e.g. formatting the cluster-private volume) fail
	// silently while the gateway reported success.
	for host, hr := range result.Hosts {
		if !hr.Success {
			zap.L().Debug("GatewayDeploymentClient.Exec: command failed on host", zap.String("host", host),
				zap.Int("cmd_len", len(cmd)), zap.Duration("took", time.Since(start)))
			return fmt.Errorf("command failed on %s: %s", host, strings.TrimSpace(hr.Output))
		}
	}
	zap.L().Debug("GatewayDeploymentClient.Exec: succeeded", zap.Strings("hosts", hosts), zap.Int("cmd_len", len(cmd)),
		zap.Duration("took", time.Since(start)))
	return nil
}

// ExecOutput implements gateway.HostOutputReader: it returns what cmd printed
// on each host where it succeeded, so the gateway manager can read state that
// exists only on the nodes.
func (a *GatewayDeploymentClient) ExecOutput(ctx context.Context, hosts []string, cmd string) (map[string]string, error) {
	start := time.Now()
	result, err := a.dc.Exec(ctx, hosts, cmd)
	if err != nil {
		zap.L().Debug("GatewayDeploymentClient.ExecOutput: exec failed", zap.Strings("hosts", hosts), zap.Int("cmd_len", len(cmd)),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, err
	}
	out := make(map[string]string, len(result.Hosts))
	for host, hr := range result.Hosts {
		if hr != nil && hr.Success {
			out[host] = hr.Output
		}
	}
	zap.L().Debug("GatewayDeploymentClient.ExecOutput: finished", zap.Strings("hosts", hosts), zap.Int("cmd_len", len(cmd)),
		zap.Int("succeeded", len(out)), zap.Duration("took", time.Since(start)))
	return out, nil
}

// WanproxyDeploymentClient adapts the controller's deploymentClient to the
// wanproxy.DeploymentClient interface, converting deployment result types into
// wanproxy.Result. It mirrors GatewayDeploymentClient (the gateway adapter) and
// wraps the same interface, so the WAN provisioner runs over the exact SSH
// transport the rest of the controller uses (and is trivially fakeable in tests).
type WanproxyDeploymentClient struct {
	dc deploymentClient
}

// NewWanproxyDeploymentClient creates a wanproxy deployment client adapter over
// the controller's deployment client.
func NewWanproxyDeploymentClient(dc deploymentClient) wanproxy.DeploymentClient {
	return &WanproxyDeploymentClient{dc: dc}
}

func (a *WanproxyDeploymentClient) DistributeConfig(ctx context.Context, hosts []string, content, remotePath string) (*wanproxy.Result, error) {
	// The content can be a private key: the path and size only.
	start := time.Now()
	res, err := a.dc.DistributeConfig(ctx, hosts, content, remotePath)
	zap.L().Debug("WanproxyDeploymentClient.DistributeConfig: distributed", zap.Strings("hosts", hosts),
		zap.String("path", remotePath), zap.Int("bytes", len(content)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	return configResultToWanproxy(res), nil
}

func (a *WanproxyDeploymentClient) Exec(ctx context.Context, hosts []string, cmd string) (*wanproxy.Result, error) {
	start := time.Now()
	res, err := a.dc.Exec(ctx, hosts, cmd)
	if err != nil {
		zap.L().Debug("WanproxyDeploymentClient.Exec: exec failed", zap.Strings("hosts", hosts), zap.Int("cmd_len", len(cmd)),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, err
	}
	if ce := zap.L().Check(zap.DebugLevel, "WanproxyDeploymentClient.Exec: finished"); ce != nil {
		ce.Write(zap.Strings("hosts", hosts), zap.Int("cmd_len", len(cmd)),
			zap.Strings("failed_hosts", tracedFailedHosts(res)), zap.Duration("took", time.Since(start)))
	}
	return execResultToWanproxy(res), nil
}

// execResultToWanproxy converts a deployment.ExecResult into a wanproxy.Result.
func execResultToWanproxy(res *deployment.ExecResult) *wanproxy.Result {
	out := &wanproxy.Result{Hosts: make(map[string]*wanproxy.HostResult)}
	if res == nil {
		return out
	}
	for host, hr := range res.Hosts {
		out.Hosts[host] = &wanproxy.HostResult{Host: hr.Host, Output: hr.Output, Success: hr.Success, Err: hr.Error}
	}
	return out
}

// configResultToWanproxy converts a deployment.ConfigResult into a
// wanproxy.Result. ConfigResult carries a top-level Success flag that may be set
// with an empty per-host map (a fully-successful distribute), so when the map is
// empty we reflect the aggregate flag to keep Result.AllSuccess() accurate.
func configResultToWanproxy(res *deployment.ConfigResult) *wanproxy.Result {
	out := &wanproxy.Result{Hosts: make(map[string]*wanproxy.HostResult)}
	if res == nil {
		return out
	}
	for host, hr := range res.Hosts {
		out.Hosts[host] = &wanproxy.HostResult{Host: hr.Host, Output: hr.Output, Success: hr.Success, Err: hr.Error}
	}
	if len(out.Hosts) == 0 {
		zap.L().Debug("configResultToWanproxy: no per-host results, using aggregate", zap.Bool("success", res.Success))
		out.Hosts["_"] = &wanproxy.HostResult{Host: "_", Success: res.Success}
	}
	return out
}
