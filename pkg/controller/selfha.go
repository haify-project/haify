package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"go.uber.org/zap"
)

// Controller self-HA: the controller's own metadata database is placed on a
// small DRBD resource and the controller itself becomes a drbd-reactor
// managed service with a VIP. On node failure DRBD promotes elsewhere,
// reactor mounts the database, brings the VIP up, and starts the controller.
const (
	// SelfHaResource is the DRBD resource backing the controller database.
	SelfHaResource = "haify-meta"
	// selfHaMountPoint shadows the default database directory, so the
	// controller config needs no changes; the pre-HA database file remains
	// underneath the mount as a natural backup.
	selfHaMountPoint    = "/var/lib/haify"
	selfHaFsType        = "ext4"
	selfHaDefaultSizeGB = 1
	selfHaDefaultPort   = 7999
	selfHaControllerSvc = "haify-controller.service"

	selfHaReactorConfig = "/etc/drbd-reactor.d/haify-ha-" + SelfHaResource + ".toml"
	selfHaHandoffScript = "/opt/haify/bin/haify-selfha-handoff.sh"
	selfHaDisableScript = "/opt/haify/bin/haify-selfha-disable.sh"
	selfHaHandoffLog    = "/var/log/haify/selfha-handoff.log"
	selfHaDisableLog    = "/var/log/haify/selfha-disable.log"
)

// Local artifact paths replicated to the other nodes during self-HA
// enablement; package variables so tests can point them at fixtures.
var (
	controllerConfigPath = "/etc/haify/controller.toml"
	controllerUnitPath   = "/etc/systemd/system/haify-controller.service"
	// packagedControllerUnitPath is where the haify-controller package puts the
	// unit. Enable reads it when there is no /etc copy, and installs it on
	// the standbys as controllerUnitPath, which overrides a packaged one.
	packagedControllerUnitPath = "/lib/systemd/system/haify-controller.service"
	// dispatchConfigOverride replaces the configured dispatch config path;
	// tests point it at a fixture.
	dispatchConfigOverride = ""
)

// selfHaExtraServices returns the configured systemd units that should ride the
// controller's Self-HA promoter (config [self_ha] extra_services). Empty unless a
// deployment opts in — e.g. "haify-ai.service" to make the AI Copilot follow the
// controller across failover.
func (rm *ResourceManager) selfHaExtraServices() []string {
	if rm.controller == nil || rm.controller.config == nil {
		return nil
	}
	return rm.controller.config.SelfHA.ExtraServices
}

// EnableSelfHa makes the controller itself highly available. It provisions
// the metadata DRBD resource, distributes the controller artifacts and a
// DISABLED reactor promoter config to all nodes, then launches a detached
// handoff script (via systemd-run, so it survives the controller's own stop)
// that migrates the database onto the DRBD volume and enables reactor
// management everywhere. It returns the node-local handoff log path.
func (rm *ResourceManager) EnableSelfHa(ctx context.Context, vip, pool string, sizeGB, port uint32, nodes []string) (string, error) {
	if rm.deployment == nil {
		return "", fmt.Errorf("deployment client not set")
	}
	if rm.controller.db == nil {
		return "", fmt.Errorf("database not available")
	}
	if vip == "" || !strings.Contains(vip, "/") {
		return "", fmt.Errorf("vip must be in CIDR notation (e.g. 192.168.1.50/24), got %q", vip)
	}
	if pool == "" {
		return "", fmt.Errorf("pool is required")
	}
	if sizeGB == 0 {
		sizeGB = selfHaDefaultSizeGB
	}
	if port == 0 {
		port = selfHaDefaultPort
	}

	// Resolve target nodes (default: all registered nodes).
	nodeNames, nodeAddrs, err := rm.selfHaTargetNodes(ctx, nodes)
	if err != nil {
		return "", err
	}
	if len(nodeNames) < 2 {
		return "", fmt.Errorf("self-HA requires at least 2 nodes, got %d", len(nodeNames))
	}

	// The controller must run on one of the cluster nodes: the handoff
	// happens locally and the DRBD primary must be this node.
	selfAddr, err := rm.selfNodeAddress(nodeNames, nodeAddrs)
	if err != nil {
		return "", err
	}
	standbyAddrs := make([]string, 0, len(nodeAddrs)-1)
	for _, addr := range nodeAddrs {
		if addr != selfAddr {
			standbyAddrs = append(standbyAddrs, addr)
		}
	}

	// The metadata resource must not already exist.
	if _, err := rm.controller.db.GetResource(ctx, SelfHaResource); err == nil {
		// Disable keeps the metadata resource — it holds the database copy the
		// cluster ran on — and only the HA record says self-HA is on. Telling
		// an operator who just disabled it that it "may already be enabled"
		// left them no way forward.
		if cfg, _ := rm.controller.db.GetHaConfig(ctx, SelfHaResource); cfg == nil {
			return "", fmt.Errorf("self-HA is disabled, but its metadata resource %s from the previous time is still there; "+
				"the controller now runs on its local database, so remove it and enable again: haify resource delete %s",
				SelfHaResource, SelfHaResource)
		}
		return "", fmt.Errorf("self-HA is already enabled (resource %s); see haify ha self status", SelfHaResource)
	}

	// Where the binary goes and whether each standby can run it — checked
	// before anything is changed.
	binPlan, err := rm.planControllerBinary(ctx, standbyAddrs)
	if err != nil {
		return "", fmt.Errorf("preflight failed: %w", err)
	}

	if err := rm.selfHaPreflight(ctx, selfAddr, standbyAddrs); err != nil {
		return "", fmt.Errorf("preflight failed: %w", err)
	}

	rm.controller.logger.Info("Enabling controller self-HA",
		zap.String("vip", vip),
		zap.String("pool", pool),
		zap.Uint32("size_gb", sizeGB),
		zap.Strings("nodes", nodeNames),
		zap.String("self", selfAddr))

	// 1. Create the metadata DRBD resource (LVs + config + create-md + up).
	if err := rm.CreateResource(ctx, SelfHaResource, port, nodeNames, "C", sizeGB, pool, "lvm", nil); err != nil {
		return "", fmt.Errorf("failed to create metadata resource: %w", err)
	}

	// 2. Promote on this node and create the filesystem.
	if err := rm.SetPrimary(ctx, SelfHaResource, selfAddr, true); err != nil {
		return "", fmt.Errorf("failed to promote %s on %s: %w", SelfHaResource, selfAddr, err)
	}
	if err := rm.CreateFilesystemOnly(ctx, SelfHaResource, 0, selfHaFsType, selfAddr); err != nil {
		return "", fmt.Errorf("failed to create filesystem: %w", err)
	}

	// 3. Distribute controller artifacts so any node can run the controller.
	if err := rm.distributeControllerArtifacts(ctx, binPlan, standbyAddrs, nodeAddrs); err != nil {
		return "", err
	}

	// 4. Distribute the reactor promoter config DISABLED everywhere. It is
	// only activated by the handoff script after the database copy: enabling
	// it earlier could let a standby promote an empty filesystem and start a
	// second controller with an empty database.
	//
	// Extra services from config (e.g. "haify-ai.service") ride the same promoter,
	// so they start/stop with the controller on the active node — the AI Copilot
	// follows the controller's failover.
	services := append([]string{selfHaControllerSvc}, rm.selfHaExtraServices()...)
	promoterCfg := rm.generatePromoterConfig(SelfHaResource, services, selfHaMountPoint, vip, nil)
	if err := rm.distributeToAll(ctx, nodeAddrs, promoterCfg, selfHaReactorConfig+".disabled", ""); err != nil {
		return "", fmt.Errorf("failed to distribute reactor config: %w", err)
	}

	// 5. Persist the HA config so `ha status haify-meta` works after handoff.
	if err := rm.controller.db.SaveHaConfig(ctx, &database.HaConfig{
		Resource:   SelfHaResource,
		VIP:        vip,
		MountPoint: selfHaMountPoint,
		FsType:     selfHaFsType,
		Services:   services,
	}); err != nil {
		return "", fmt.Errorf("failed to persist HA config: %w", err)
	}

	// 6. Launch the detached handoff.
	script := generateSelfHaHandoffScript(standbyAddrs)
	if err := rm.runDetachedScript(ctx, selfAddr, selfHaHandoffScript, script, "haify-selfha-handoff"); err != nil {
		return "", err
	}

	return selfHaHandoffLog, nil
}

// DisableSelfHa reverts the controller to standalone operation on the given
// node. Reactor management is removed everywhere, the database is copied off
// the DRBD volume back to the node-local path, and the controller service is
// re-enabled as a normal systemd service. The metadata resource itself is
// kept and can be removed afterwards with `resource delete haify-meta`.
func (rm *ResourceManager) DisableSelfHa(ctx context.Context, node string) error {
	if rm.deployment == nil {
		return fmt.Errorf("deployment client not set")
	}
	if rm.controller.db == nil {
		return fmt.Errorf("database not available")
	}

	// Tolerate a missing HA config record: a previously failed disable run
	// deletes the record before the handoff, and disable must stay
	// retryable from that state. The metadata resource is the real witness.
	haCfg, _ := rm.controller.db.GetHaConfig(ctx, SelfHaResource)

	dbRes, err := rm.controller.db.GetResource(ctx, SelfHaResource)
	if err != nil {
		return fmt.Errorf("self-HA is not enabled (metadata resource %s not found): %w", SelfHaResource, err)
	}
	nodeNames := strings.Split(dbRes.Nodes, ",")
	nodeAddrs := make([]string, len(nodeNames))
	for i, n := range nodeNames {
		nodeAddrs[i] = rm.controller.ResolveHost(n)
	}

	targetAddr := rm.controller.ResolveHost(node)
	if !containsString(nodeAddrs, targetAddr) {
		return fmt.Errorf("node %s is not part of %s (nodes: %v)", node, SelfHaResource, nodeNames)
	}

	selfAddr, err := rm.selfNodeAddress(nodeNames, nodeAddrs)
	if err != nil {
		return err
	}
	otherAddrs := make([]string, 0, len(nodeAddrs)-1)
	for _, addr := range nodeAddrs {
		if addr != selfAddr {
			otherAddrs = append(otherAddrs, addr)
		}
	}

	// Remove the HA config record BEFORE the handoff: this write lands in
	// the DRBD-hosted database, which is exactly what gets copied back out,
	// so the standalone controller comes up without a stale self-HA record.
	// Best-effort: on a disable retry the record is already gone.
	if haCfg != nil {
		if err := rm.controller.db.DeleteHaConfig(ctx, SelfHaResource); err != nil {
			return fmt.Errorf("failed to delete HA config: %w", err)
		}
	}

	vip := ""
	if haCfg != nil {
		vip = haCfg.VIP
	}
	script := generateSelfHaDisableScript(otherAddrs, targetAddr, selfAddr, vip)
	if err := rm.runDetachedScript(ctx, selfAddr, selfHaDisableScript, script, "haify-selfha-disable"); err != nil {
		// Restore the record so state stays consistent if we failed to launch.
		if haCfg != nil {
			_ = rm.controller.db.SaveHaConfig(ctx, haCfg)
		}
		return err
	}
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

// GetSelfHaStatus reports whether self-HA is configured and where the
// controller is currently active (the DRBD primary of the metadata resource).
func (rm *ResourceManager) GetSelfHaStatus(ctx context.Context) (*SelfHaStatus, error) {
	if rm.controller.db == nil {
		return nil, fmt.Errorf("database not available")
	}
	haCfg, err := rm.controller.db.GetHaConfig(ctx, SelfHaResource)
	if err != nil || haCfg == nil {
		return &SelfHaStatus{Enabled: false}, nil
	}

	status := &SelfHaStatus{
		Enabled:  true,
		Resource: SelfHaResource,
		VIP:      haCfg.VIP,
	}
	if dbRes, err := rm.controller.db.GetResource(ctx, SelfHaResource); err == nil {
		status.Nodes = strings.Split(dbRes.Nodes, ",")
		hosts := make([]string, len(status.Nodes))
		for i, n := range status.Nodes {
			hosts[i] = rm.controller.ResolveHost(n)
		}
		if active, err := rm.findActiveNode(ctx, SelfHaResource, hosts); err == nil {
			status.ActiveNode = rm.controller.NodeName(active)
		}
	}
	return status, nil
}

// selfHaTargetNodes resolves the requested node list (or all registered
// nodes) to parallel name/address slices.
func (rm *ResourceManager) selfHaTargetNodes(ctx context.Context, requested []string) ([]string, []string, error) {
	if len(requested) == 0 {
		all, err := rm.controller.nodes.ListNodes(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to list nodes: %w", err)
		}
		for _, n := range all {
			requested = append(requested, n.Name)
		}
	}
	names := make([]string, 0, len(requested))
	addrs := make([]string, 0, len(requested))
	for _, name := range requested {
		addr := rm.controller.nodes.GetNodeAddressByName(name)
		if addr == "" {
			return nil, nil, fmt.Errorf("node %s is not registered", name)
		}
		names = append(names, name)
		addrs = append(addrs, addr)
	}
	return names, addrs, nil
}

// selfNodeAddress identifies which cluster node this controller process is
// running on by matching the local hostname against the registered nodes.
func (rm *ResourceManager) selfNodeAddress(nodeNames, nodeAddrs []string) (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("failed to determine local hostname: %w", err)
	}
	for i, name := range nodeNames {
		if name == hostname || nodeAddrs[i] == hostname {
			return nodeAddrs[i], nil
		}
	}
	if addr := rm.controller.nodes.GetNodeAddressByName(hostname); addr != "" && containsString(nodeAddrs, addr) {
		return addr, nil
	}
	return "", fmt.Errorf("controller host %q is not among the target nodes %v; self-HA must be enabled from a cluster node", hostname, nodeNames)
}

// selfHaPreflight validates every runtime requirement that the handoff
// script depends on, so failures happen while the controller is still alive.
func (rm *ResourceManager) selfHaPreflight(ctx context.Context, selfAddr string, standbyAddrs []string) error {
	all := append([]string{selfAddr}, standbyAddrs...)

	// A standby that takes over starts the controller with the dispatch
	// config enable copies to it, and reaches the nodes with the key that
	// config names. The key is not copied — it is each node's own — so it has
	// to be there already.
	dispatchContent, err := os.ReadFile(rm.dispatchConfigPath())
	if err != nil {
		return fmt.Errorf("the dispatch config %s is needed on every node and cannot be read here: %w",
			rm.dispatchConfigPath(), err)
	}
	if m := dispatchKeyPath.FindSubmatch(dispatchContent); m != nil {
		key := string(m[1])
		for _, addr := range standbyAddrs {
			if err := rm.execAllSuccess(ctx, []string{addr}, "sudo test -r "+key,
				fmt.Sprintf("%s has no SSH key at %s, which the dispatch config uses to reach the nodes", addr, key)); err != nil {
				return err
			}
		}
	}

	// drbd-reactor must be running everywhere.
	if err := rm.execAllSuccess(ctx, all, "systemctl is-active --quiet drbd-reactor",
		"drbd-reactor is not active"); err != nil {
		return err
	}
	// The VIP unit runs service-ip; install it where it is missing.
	if err := rm.ensureServiceIP(ctx, all); err != nil {
		return err
	}
	// The handoff and disable scripts run AS ROOT (via systemd-run) on
	// whichever node is the active controller at that moment, so root's
	// passwordless SSH must work between EVERY pair of nodes — hence the
	// sudo: deployment Exec may land as a non-root user, but it is root's
	// key/known_hosts that the scripts will use. accept-new also
	// pre-populates each node's root known_hosts as a side effect.
	for _, from := range all {
		for _, to := range all {
			if from == to {
				continue
			}
			cmd := fmt.Sprintf("sudo ssh %s %s true", selfHaSSHOpts, to)
			if err := rm.execAllSuccess(ctx, []string{from}, cmd,
				fmt.Sprintf("passwordless root SSH %s -> %s is required (any node may run the controller after failover)", from, to)); err != nil {
				return err
			}
		}
	}
	// The standby controllers must not be running (only one controller may
	// own the database).
	for _, addr := range standbyAddrs {
		result, err := rm.deployment.Exec(ctx, []string{addr}, "systemctl is-active --quiet haify-controller && echo RUNNING || echo STOPPED")
		if err != nil {
			return fmt.Errorf("failed to check haify-controller state on %s: %w", addr, err)
		}
		for _, hr := range result.Hosts {
			if strings.Contains(hr.Output, "RUNNING") {
				return fmt.Errorf("haify-controller is already running on %s; stop it before enabling self-HA", addr)
			}
		}
	}
	return nil
}

// distributeControllerArtifacts ships the running controller binary, its
// config, and the systemd unit to every standby, and disables autostart there
// (drbd-reactor decides who runs the controller from now on). The binary lands
// at the path the unit's ExecStart names; see controllerBinaryPlan.
func (rm *ResourceManager) distributeControllerArtifacts(ctx context.Context, binPlan *controllerBinaryPlan, standbyAddrs, allAddrs []string) error {
	if err := rm.installControllerBinary(ctx, binPlan); err != nil {
		return err
	}

	// Config: replicate this node's controller.toml verbatim.
	cfgContent, err := os.ReadFile(controllerConfigPath)
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", controllerConfigPath, err)
	}
	if err := rm.distributeToAll(ctx, standbyAddrs, string(cfgContent), controllerConfigPath, ""); err != nil {
		return fmt.Errorf("failed to distribute controller config: %w", err)
	}

	// Dispatch config: the controller cannot start without it — it is how the
	// controller reaches every node — and it lives outside controller.toml.
	// It used to be left behind, so enable reported success, and the first
	// failover brought the controller up on a standby that exited at once
	// ("dispatch config ... no such file"): the VIP moved and nothing answered
	// on it.
	dispatchPath := rm.dispatchConfigPath()
	dispatchContent, err := os.ReadFile(dispatchPath)
	if err != nil {
		return fmt.Errorf("failed to read the dispatch config %s: %w", dispatchPath, err)
	}
	if err := rm.distributeToAll(ctx, standbyAddrs, string(dispatchContent), dispatchPath, ""); err != nil {
		return fmt.Errorf("failed to distribute the dispatch config: %w", err)
	}

	// Systemd unit: replicate verbatim (the content the binary's target path
	// was read from) and daemon-reload.
	if err := rm.distributeToAll(ctx, standbyAddrs, binPlan.unit, controllerUnitPath,
		"sudo systemctl daemon-reload"); err != nil {
		return fmt.Errorf("failed to distribute controller unit: %w", err)
	}

	// Standbys must not autostart the controller.
	if len(standbyAddrs) > 0 {
		if err := rm.execAllSuccess(ctx, standbyAddrs, "sudo systemctl disable haify-controller 2>/dev/null; true",
			"failed to disable haify-controller autostart"); err != nil {
			return err
		}
	}

	// Mount unit for the database volume, on every node.
	mountUnit := rm.generateSystemdMountUnit(SelfHaResource, selfHaMountPoint, selfHaFsType)
	mountUnitPath := fmt.Sprintf("/etc/systemd/system/%s.mount",
		strings.ReplaceAll(strings.TrimPrefix(selfHaMountPoint, "/"), "/", "-"))
	if err := rm.distributeToAll(ctx, allAddrs, mountUnit, mountUnitPath,
		"sudo systemctl daemon-reload"); err != nil {
		return fmt.Errorf("failed to distribute mount unit: %w", err)
	}
	return nil
}

// distributeToAll writes content to remotePath on all hosts, optionally
// running postCmd afterwards, and fails if any host fails.
func (rm *ResourceManager) distributeToAll(ctx context.Context, hosts []string, content, remotePath, postCmd string) error {
	if len(hosts) == 0 {
		return nil
	}
	opts := []deployment.ConfigOption{}
	if postCmd != "" {
		opts = append(opts, deployment.WithPostCommand(postCmd))
	}
	result, err := rm.deployment.DistributeConfig(ctx, hosts, content, remotePath, opts...)
	if err != nil {
		return err
	}
	if !result.Success {
		var failed []string
		for host, hr := range result.Hosts {
			if !hr.Success {
				failed = append(failed, fmt.Sprintf("%s: %s", host, strings.TrimSpace(hr.Output)))
			}
		}
		return fmt.Errorf("distribution of %s failed: %v", remotePath, failed)
	}
	return nil
}

// execAllSuccess runs cmd on hosts and converts any failure into a single
// descriptive error.
func (rm *ResourceManager) execAllSuccess(ctx context.Context, hosts []string, cmd, failMsg string) error {
	result, err := rm.deployment.Exec(ctx, hosts, cmd)
	if err != nil {
		return fmt.Errorf("%s: %w", failMsg, err)
	}
	if !result.AllSuccess() {
		for host, hr := range result.Hosts {
			if !hr.Success {
				return fmt.Errorf("%s (node %s): %s", failMsg, host, strings.TrimSpace(hr.Output))
			}
		}
		return fmt.Errorf("%s on: %v", failMsg, result.FailedHosts())
	}
	return nil
}

// runDetachedScript writes a script to this node and launches it through
// systemd-run, so it keeps running after the controller stops itself.
func (rm *ResourceManager) runDetachedScript(ctx context.Context, selfAddr, scriptPath, content, unitName string) error {
	if err := rm.distributeToAll(ctx, []string{selfAddr}, content, scriptPath,
		fmt.Sprintf("sudo chmod 755 %s", scriptPath)); err != nil {
		return fmt.Errorf("failed to install script %s: %w", scriptPath, err)
	}
	launch := fmt.Sprintf("sudo systemd-run --unit=%s "+collectFlag+" /bin/bash %s", unitName, scriptPath)
	if err := rm.execAllSuccess(ctx, []string{selfAddr}, launch,
		fmt.Sprintf("failed to launch %s", unitName)); err != nil {
		return err
	}
	return nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// dispatchConfigPath is the dispatch config this controller runs with: the
// configured path, or dispatch's default under the controller's home.
func (rm *ResourceManager) dispatchConfigPath() string {
	if dispatchConfigOverride != "" {
		return dispatchConfigOverride
	}
	p := ""
	if rm.controller.config != nil {
		p = strings.TrimSpace(rm.controller.config.Dispatch.ConfigPath)
	}
	if p != "" {
		if strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				return filepath.Join(home, p[2:])
			}
		}
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/root"
	}
	return filepath.Join(home, ".dispatch", "config.toml")
}

// dispatchKeyPath finds the private key a dispatch config authenticates with.
var dispatchKeyPath = regexp.MustCompile(`(?m)^\s*key_path\s*=\s*"([^"]+)"`)
