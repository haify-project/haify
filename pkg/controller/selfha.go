package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
)

// Controller self-HA: the controller's own metadata database is placed on a
// small DRBD resource and the controller itself becomes a drbd-reactor
// managed service with a VIP. On node failure DRBD promotes elsewhere,
// reactor mounts the database, brings the VIP up, and starts the controller.
//
// This file holds enable, disable and status; the checks run before enable
// are in selfha_preflight.go, and the copying of files and scripts to the
// nodes in selfha_distribute.go.
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
		zap.L().Debug("selfHaExtraServices: no controller config, no extra services")
		return nil
	}
	extra := rm.controller.config.SelfHA.ExtraServices
	rm.controller.traceLogger().Debug("selfHaExtraServices: configured extra services", zap.Strings("services", extra))
	return extra
}

// EnableSelfHa makes the controller itself highly available. It provisions
// the metadata DRBD resource, distributes the controller artifacts and a
// DISABLED reactor promoter config to all nodes, then launches a detached
// handoff script (via systemd-run, so it survives the controller's own stop)
// that migrates the database onto the DRBD volume and enables reactor
// management everywhere. It returns the node-local handoff log path.
func (rm *ResourceManager) EnableSelfHa(ctx context.Context, vip, pool string, sizeGB, port uint32, nodes []string) (string, error) {
	log := rm.controller.traceLogger()
	log.Debug("EnableSelfHa: enabling", zap.String("vip", vip), zap.String("pool", pool), zap.Uint32("size_gb", sizeGB),
		zap.Uint32("port", port), zap.Strings("nodes", nodes))
	if rm.deployment == nil {
		log.Debug("EnableSelfHa: no deployment client")
		return "", fmt.Errorf("deployment client not set")
	}
	if rm.controller.db == nil {
		log.Debug("EnableSelfHa: no database")
		return "", fmt.Errorf("database not available")
	}
	if vip == "" || !strings.Contains(vip, "/") {
		log.Debug("EnableSelfHa: VIP not in CIDR notation", zap.String("vip", vip))
		return "", fmt.Errorf("vip must be in CIDR notation (e.g. 192.168.1.50/24), got %q", vip)
	}
	if pool == "" {
		log.Debug("EnableSelfHa: no pool given")
		return "", fmt.Errorf("pool is required")
	}
	if sizeGB == 0 {
		log.Debug("EnableSelfHa: no size, using the default", zap.Int("size_gb", selfHaDefaultSizeGB))
		sizeGB = selfHaDefaultSizeGB
	}
	if port == 0 {
		log.Debug("EnableSelfHa: no port, using the default", zap.Int("port", selfHaDefaultPort))
		port = selfHaDefaultPort
	}

	// Resolve target nodes (default: all registered nodes).
	nodeNames, nodeAddrs, err := rm.selfHaTargetNodes(ctx, nodes)
	if err != nil {
		log.Debug("EnableSelfHa: target nodes not resolved", zap.Error(err))
		return "", err
	}
	if len(nodeNames) < 2 {
		log.Debug("EnableSelfHa: too few nodes", zap.Strings("nodes", nodeNames))
		return "", fmt.Errorf("self-HA requires at least 2 nodes, got %d", len(nodeNames))
	}

	// The controller must run on one of the cluster nodes: the handoff
	// happens locally and the DRBD primary must be this node.
	selfAddr, err := rm.selfNodeAddress(nodeNames, nodeAddrs)
	if err != nil {
		log.Debug("EnableSelfHa: this controller is not on a target node", zap.Error(err))
		return "", err
	}
	standbyAddrs := make([]string, 0, len(nodeAddrs)-1)
	for _, addr := range nodeAddrs {
		if addr != selfAddr {
			standbyAddrs = append(standbyAddrs, addr)
		}
	}
	log.Debug("EnableSelfHa: nodes resolved", zap.Strings("nodes", nodeNames), zap.String("self", selfAddr),
		zap.Strings("standbys", standbyAddrs))

	// The metadata resource must not already exist.
	if _, err := rm.controller.db.GetResource(ctx, SelfHaResource); err == nil {
		// Disable keeps the metadata resource — it holds the database copy the
		// cluster ran on — and only the HA record says self-HA is on. Telling
		// an operator who just disabled it that it "may already be enabled"
		// left them no way forward.
		if cfg, _ := rm.controller.db.GetHaConfig(ctx, SelfHaResource); cfg == nil {
			log.Debug("EnableSelfHa: metadata resource left from a previous disable", zap.String("resource", SelfHaResource))
			return "", fmt.Errorf("self-HA is disabled, but its metadata resource %s from the previous time is still there; "+
				"the controller now runs on its local database, so remove it and enable again: haify resource delete %s",
				SelfHaResource, SelfHaResource)
		}
		log.Debug("EnableSelfHa: already enabled", zap.String("resource", SelfHaResource))
		return "", fmt.Errorf("self-HA is already enabled (resource %s); see haify ha self status", SelfHaResource)
	}

	// Where the binary goes and whether each standby can run it — checked
	// before anything is changed.
	binPlan, err := rm.planControllerBinary(ctx, standbyAddrs)
	if err != nil {
		log.Debug("EnableSelfHa: binary plan failed", zap.Error(err))
		return "", fmt.Errorf("preflight failed: %w", err)
	}

	if err := rm.selfHaPreflight(ctx, selfAddr, standbyAddrs); err != nil {
		log.Debug("EnableSelfHa: preflight failed", zap.Error(err))
		return "", fmt.Errorf("preflight failed: %w", err)
	}

	rm.controller.logger.Info("Enabling controller self-HA",
		zap.String("vip", vip),
		zap.String("pool", pool),
		zap.Uint32("size_gb", sizeGB),
		zap.Strings("nodes", nodeNames),
		zap.String("self", selfAddr))

	// 1. Create the metadata DRBD resource (LVs + config + create-md + up).
	log.Debug("EnableSelfHa: step 1, creating metadata resource", zap.String("resource", SelfHaResource), zap.Uint32("port", port))
	if err := rm.CreateResource(ctx, SelfHaResource, port, nodeNames, "C", sizeGB, pool, "lvm", nil); err != nil {
		log.Debug("EnableSelfHa: metadata resource creation failed", zap.Error(err))
		return "", fmt.Errorf("failed to create metadata resource: %w", err)
	}

	// 2. Promote on this node and create the filesystem.
	log.Debug("EnableSelfHa: step 2, promoting and making the filesystem", zap.String("node", selfAddr))
	if err := rm.SetPrimary(ctx, SelfHaResource, selfAddr, true); err != nil {
		log.Debug("EnableSelfHa: promote failed", zap.String("node", selfAddr), zap.Error(err))
		return "", fmt.Errorf("failed to promote %s on %s: %w", SelfHaResource, selfAddr, err)
	}
	if err := rm.CreateFilesystemOnly(ctx, SelfHaResource, 0, selfHaFsType, selfAddr); err != nil {
		log.Debug("EnableSelfHa: mkfs failed", zap.String("node", selfAddr), zap.Error(err))
		return "", fmt.Errorf("failed to create filesystem: %w", err)
	}

	// 3. Distribute controller artifacts so any node can run the controller.
	log.Debug("EnableSelfHa: step 3, distributing controller artifacts", zap.Strings("standbys", standbyAddrs))
	if err := rm.distributeControllerArtifacts(ctx, binPlan, standbyAddrs, nodeAddrs); err != nil {
		log.Debug("EnableSelfHa: artifact distribution failed", zap.Error(err))
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
	log.Debug("EnableSelfHa: step 4, distributing disabled promoter config", zap.Strings("services", services),
		zap.String("path", selfHaReactorConfig+".disabled"))
	promoterCfg := rm.generatePromoterConfig(SelfHaResource, services, selfHaMountPoint, vip, nil)
	if err := rm.distributeToAll(ctx, nodeAddrs, promoterCfg, selfHaReactorConfig+".disabled", ""); err != nil {
		log.Debug("EnableSelfHa: promoter config distribution failed", zap.Error(err))
		return "", fmt.Errorf("failed to distribute reactor config: %w", err)
	}

	// 5. Persist the HA config so `ha status haify-meta` works after handoff.
	log.Debug("EnableSelfHa: step 5, saving HA record", zap.String("resource", SelfHaResource))
	if err := rm.controller.db.SaveHaConfig(ctx, &database.HaConfig{
		Resource:   SelfHaResource,
		VIP:        vip,
		MountPoint: selfHaMountPoint,
		FsType:     selfHaFsType,
		Services:   services,
	}); err != nil {
		log.Debug("EnableSelfHa: HA record save failed", zap.Error(err))
		return "", fmt.Errorf("failed to persist HA config: %w", err)
	}

	// 6. Launch the detached handoff.
	log.Debug("EnableSelfHa: step 6, launching handoff", zap.String("node", selfAddr), zap.String("script", selfHaHandoffScript))
	script := generateSelfHaHandoffScript(standbyAddrs)
	if err := rm.runDetachedScript(ctx, selfAddr, selfHaHandoffScript, script, "haify-selfha-handoff"); err != nil {
		log.Debug("EnableSelfHa: handoff launch failed", zap.Error(err))
		return "", err
	}

	log.Debug("EnableSelfHa: handoff running", zap.String("log", selfHaHandoffLog))
	return selfHaHandoffLog, nil
}

// DisableSelfHa reverts the controller to standalone operation on the given
// node. Reactor management is removed everywhere, the database is copied off
// the DRBD volume back to the node-local path, and the controller service is
// re-enabled as a normal systemd service. The metadata resource itself is
// kept and can be removed afterwards with `resource delete haify-meta`.
func (rm *ResourceManager) DisableSelfHa(ctx context.Context, node string) error {
	log := rm.controller.traceLogger()
	log.Debug("DisableSelfHa: disabling", zap.String("node", node))
	if rm.deployment == nil {
		log.Debug("DisableSelfHa: no deployment client")
		return fmt.Errorf("deployment client not set")
	}
	if rm.controller.db == nil {
		log.Debug("DisableSelfHa: no database")
		return fmt.Errorf("database not available")
	}

	// Tolerate a missing HA config record: a previously failed disable run
	// deletes the record before the handoff, and disable must stay
	// retryable from that state. The metadata resource is the real witness.
	haCfg, _ := rm.controller.db.GetHaConfig(ctx, SelfHaResource)
	log.Debug("DisableSelfHa: HA record", zap.Bool("found", haCfg != nil))

	dbRes, err := rm.controller.db.GetResource(ctx, SelfHaResource)
	if err != nil {
		log.Debug("DisableSelfHa: metadata resource not found", zap.Error(err))
		return fmt.Errorf("self-HA is not enabled (metadata resource %s not found): %w", SelfHaResource, err)
	}
	nodeNames := strings.Split(dbRes.Nodes, ",")
	nodeAddrs := make([]string, len(nodeNames))
	for i, n := range nodeNames {
		nodeAddrs[i] = rm.controller.ResolveHost(n)
		log.Debug("DisableSelfHa: metadata node", zap.String("node", n), zap.String("host", nodeAddrs[i]))
	}

	targetAddr := rm.controller.ResolveHost(node)
	if !containsString(nodeAddrs, targetAddr) {
		log.Debug("DisableSelfHa: target node is not a metadata node", zap.String("node", node), zap.String("host", targetAddr))
		return fmt.Errorf("node %s is not part of %s (nodes: %v)", node, SelfHaResource, nodeNames)
	}

	selfAddr, err := rm.selfNodeAddress(nodeNames, nodeAddrs)
	if err != nil {
		log.Debug("DisableSelfHa: this controller is not on a metadata node", zap.Error(err))
		return err
	}
	otherAddrs := make([]string, 0, len(nodeAddrs)-1)
	for _, addr := range nodeAddrs {
		if addr != selfAddr {
			otherAddrs = append(otherAddrs, addr)
		}
	}
	log.Debug("DisableSelfHa: nodes resolved", zap.String("self", selfAddr), zap.String("target", targetAddr),
		zap.Strings("others", otherAddrs))

	// Remove the HA config record BEFORE the handoff: this write lands in
	// the DRBD-hosted database, which is exactly what gets copied back out,
	// so the standalone controller comes up without a stale self-HA record.
	// Best-effort: on a disable retry the record is already gone.
	if haCfg != nil {
		if err := rm.controller.db.DeleteHaConfig(ctx, SelfHaResource); err != nil {
			log.Debug("DisableSelfHa: HA record delete failed", zap.Error(err))
			return fmt.Errorf("failed to delete HA config: %w", err)
		}
		log.Debug("DisableSelfHa: HA record deleted")
	}

	vip := ""
	if haCfg != nil {
		vip = haCfg.VIP
	}
	log.Debug("DisableSelfHa: launching disable script", zap.String("node", selfAddr), zap.String("vip", vip))
	script := generateSelfHaDisableScript(otherAddrs, targetAddr, selfAddr, vip)
	if err := rm.runDetachedScript(ctx, selfAddr, selfHaDisableScript, script, "haify-selfha-disable"); err != nil {
		// Restore the record so state stays consistent if we failed to launch.
		if haCfg != nil {
			restoreErr := rm.controller.db.SaveHaConfig(ctx, haCfg)
			log.Debug("DisableSelfHa: launch failed, HA record restored", zap.NamedError("restore_error", restoreErr))
		}
		log.Debug("DisableSelfHa: disable script launch failed", zap.Error(err))
		return err
	}
	log.Debug("DisableSelfHa: disable running", zap.String("log", selfHaDisableLog))
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
	log := rm.controller.traceLogger()
	log.Debug("GetSelfHaStatus: reading")
	if rm.controller.db == nil {
		log.Debug("GetSelfHaStatus: no database")
		return nil, fmt.Errorf("database not available")
	}
	haCfg, err := rm.controller.db.GetHaConfig(ctx, SelfHaResource)
	if err != nil || haCfg == nil {
		log.Debug("GetSelfHaStatus: no HA record, self-HA off", zap.Error(err))
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
		} else {
			log.Debug("GetSelfHaStatus: active node not found", zap.Strings("hosts", hosts), zap.Error(err))
		}
	} else {
		log.Debug("GetSelfHaStatus: metadata resource not read", zap.Error(err))
	}
	log.Debug("GetSelfHaStatus: enabled", zap.String("vip", status.VIP), zap.Strings("nodes", status.Nodes),
		zap.String("active_node", status.ActiveNode))
	return status, nil
}
