package controller

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/gateway"
	"go.uber.org/zap"
)

// MakeHa creates a drbd-reactor promoter config for HA failover. Any ocfAgents
// are appended, in order, to the promoter start[] list after the built-in
// mount/vip/services entries. Passing nil keeps the historical behavior.
func (rm *ResourceManager) MakeHa(ctx context.Context, resource string, services []string, mountPoint, fsType, vip string, ocfAgents []OcfAgentSpec, startItems []HaStartItem) (string, error) {
	rm.controller.logger.Info("Making resource HA",
		zap.String("resource", resource),
		zap.Strings("services", services),
		zap.String("mount_point", mountPoint),
		zap.String("fstype", fsType),
		zap.String("vip", vip))

	log := rm.resourcesLog()
	log.Debug("MakeHa: start", zap.String("resource", resource), zap.Int("ocf_agents", len(ocfAgents)),
		zap.Int("start_items", len(startItems)))
	if rm.deployment == nil {
		log.Debug("MakeHa: no deployment client", zap.String("resource", resource))
		return "", fmt.Errorf("deployment client not set")
	}
	if rm.controller.db == nil {
		log.Debug("MakeHa: no database", zap.String("resource", resource))
		return "", fmt.Errorf("database not available")
	}
	if err := checkHaMountPoint(mountPoint); err != nil {
		log.Debug("MakeHa: mount point refused", zap.String("mount_point", mountPoint), zap.Error(err))
		return "", err
	}

	// Get resource info to find nodeAddresses
	dbResource, err := rm.controller.db.GetResource(ctx, resource)
	log.Debug("MakeHa: database lookup", zap.String("resource", resource), zap.Bool("found", dbResource != nil), zap.Error(err))
	if err != nil {
		return "", fmt.Errorf("failed to get resource from database: %w", err)
	}

	if dbResource == nil {
		return "", fmt.Errorf("resource not found: %s", resource)
	}
	if err := rm.controller.assertPromoterAllowed(ctx, resource, "ha create"); err != nil {
		log.Debug("MakeHa: promoter not allowed", zap.String("resource", resource), zap.Error(err))
		return "", err
	}

	// The DR node of a WAN resource gets no promoter: failing over to its
	// asynchronous copy is a manual decision (see promoter_placement.go).
	nodeNames := splitCSV(dbResource.Nodes)
	if dbResource.WANMode && dbResource.DRNode != "" {
		log.Debug("MakeHa: WAN resource; DR node gets no promoter", zap.String("resource", resource), zap.String("dr_node", dbResource.DRNode))
		nodeNames = without(nodeNames, dbResource.DRNode)
	}
	if len(nodeNames) == 0 {
		log.Debug("MakeHa: no nodes", zap.String("resource", resource))
		return "", fmt.Errorf("no nodes found for resource")
	}

	// Convert node names to addresses for deployment
	nodeAddresses := make([]string, len(nodeNames))
	for i, nodeName := range nodeNames {
		addr := rm.controller.nodes.GetNodeAddressByName(nodeName)
		if addr == "" {
			log.Debug("MakeHa: node has no address", zap.String("resource", resource), zap.String("node", nodeName))
			return "", fmt.Errorf("failed to resolve address for node: %s", nodeName)
		}
		nodeAddresses[i] = addr
	}
	hosts := nodeAddresses
	log.Debug("MakeHa: promoter hosts", zap.String("resource", resource), zap.Strings("nodes", nodeNames), zap.Strings("hosts", hosts))

	// A VIP is served by the service-ip@ systemd template, which runs the
	// service-ip helper. Without them the promoter target fails to start ("Unit
	// service-ip@... not found") and the HA config is silently broken, so they
	// are installed up front on every node that lacks them.
	if strings.TrimSpace(vip) != "" {
		log.Debug("MakeHa: VIP given; ensuring service-ip and nonlocal bind", zap.String("resource", resource), zap.String("vip", vip))
		if err := rm.ensureServiceIP(ctx, hosts); err != nil {
			log.Debug("MakeHa: service-ip install failed", zap.String("resource", resource), zap.Error(err))
			return "", err
		}
		if err := rm.ensureNonlocalBind(ctx, hosts); err != nil {
			log.Debug("MakeHa: nonlocal bind failed", zap.String("resource", resource), zap.Error(err))
			return "", err
		}
	}

	// Step 1: Check DRBD status and ensure resource is up
	rm.controller.logger.Info("Checking DRBD resource status",
		zap.String("resource", resource),
		zap.Strings("nodes", nodeAddresses))

	// First, ensure resource is up on all nodes
	rm.controller.logger.Info("Bringing up DRBD resource on all nodes")
	start := time.Now()
	upRes, err := rm.deployment.Exec(ctx, nodeAddresses, "sudo drbdadm up "+resource)
	log.Debug("MakeHa: drbdadm up", zap.String("resource", resource), zap.Array("failed_hosts", resourcesFailedHosts{upRes}),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		rm.controller.logger.Warn("Failed to bring up resource (continuing anyway)", zap.Error(err))
	}
	// Continue anyway - resource might already be up

	start = time.Now()
	statusResult, err := rm.deployment.Exec(ctx, nodeAddresses, "sudo drbdadm status "+resource)
	log.Debug("MakeHa: drbdadm status", zap.String("resource", resource), zap.Array("failed_hosts", resourcesFailedHosts{statusResult}),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return "", fmt.Errorf("failed to check DRBD status: %w", err)
	}

	// Check if any node is Primary, if not, set first node as Primary
	hasPrimary := false
	for _, r := range statusResult.Hosts {
		if r.Success && strings.Contains(string(r.Output), "role:Primary") {
			hasPrimary = true
			rm.controller.logger.Info("Found existing Primary node",
				zap.String("host", r.Host))
			break
		}
	}

	if !hasPrimary {
		rm.controller.logger.Info("No Primary node found, setting first node as Primary",
			zap.String("node", nodeNames[0]),
			zap.String("address", nodeAddresses[0]))
		if err := rm.SetPrimary(ctx, resource, nodeAddresses[0], true); err != nil {
			log.Debug("MakeHa: initial promote failed", zap.String("resource", resource), zap.Error(err))
			return "", fmt.Errorf("failed to set Primary: %w", err)
		}
		rm.controller.logger.Info("Primary set successfully",
			zap.String("node", nodeNames[0]))
	}

	// Step 2: Create filesystem if mount point and fs type are specified
	if mountPoint != "" && fsType != "" {
		rm.controller.logger.Info("Creating filesystem",
			zap.String("resource", resource),
			zap.String("fstype", fsType),
			zap.String("volume", "0"))

		// Check if filesystem already exists by checking if device can be read
		drbdDevice := fmt.Sprintf("/dev/drbd/by-res/%s/0", resource)
		checkFsCmd := fmt.Sprintf("sudo blkid -o value -s TYPE %s 2>/dev/null || echo 'none'", drbdDevice)
		start := time.Now()
		checkResult, err := rm.deployment.Exec(ctx, []string{nodeAddresses[0]}, checkFsCmd)
		log.Debug("MakeHa: probed filesystem", zap.String("resource", resource), zap.String("host", nodeAddresses[0]),
			zap.Duration("took", time.Since(start)), zap.Error(err))

		needsFs := true
		if err == nil {
			for _, r := range checkResult.Hosts {
				if r.Success {
					fsTypeFound := strings.TrimSpace(string(r.Output))
					if fsTypeFound != "none" && fsTypeFound != "" {
						rm.controller.logger.Info("Filesystem already exists",
							zap.String("existing_fstype", fsTypeFound))
						needsFs = false
						break
					}
				}
			}
		}

		log.Debug("MakeHa: filesystem decision", zap.String("resource", resource), zap.Bool("needs_fs", needsFs))
		if needsFs {
			if err := rm.CreateFilesystemOnly(ctx, resource, 0, fsType, nodeAddresses[0]); err != nil {
				log.Debug("MakeHa: mkfs failed", zap.String("resource", resource), zap.Error(err))
				return "", fmt.Errorf("failed to create filesystem: %w", err)
			}
			rm.controller.logger.Info("Filesystem created successfully")
		}
	}

	// Validate that all services exist on all nodes
	// This prevents failover failures when a service is missing on a standby node
	if len(services) > 0 {
		for _, svc := range services {
			// Check if service unit file exists on all nodes
			// Use systemctl show to check LoadState - "loaded" means unit file exists
			checkCmd := fmt.Sprintf("systemctl show %s -p LoadState 2>/dev/null || echo 'not-found'", svc)
			start := time.Now()
			result, err := rm.deployment.Exec(ctx, nodeAddresses, checkCmd)
			log.Debug("MakeHa: checked service unit", zap.String("resource", resource), zap.String("service", svc),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			if err != nil {
				return "", fmt.Errorf("failed to check service %s on nodes: %w", svc, err)
			}

			var missingNodes []string
			for node, hr := range result.Hosts {
				output := strings.TrimSpace(hr.Output)
				// Service exists if LoadState is "loaded"
				if !strings.Contains(output, "LoadState=loaded") {
					missingNodes = append(missingNodes, node)
				}
			}

			if len(missingNodes) > 0 {
				log.Debug("MakeHa: service missing on nodes", zap.String("service", svc), zap.Strings("hosts", missingNodes))
				return "", fmt.Errorf("service %s not found on nodes: %v. Please install the service on all nodes before configuring HA", svc, missingNodes)
			}

			rm.controller.logger.Info("Service validated on all nodes",
				zap.String("service", svc))
		}

		// Stop and disable services on all nodes before HA takeover
		rm.controller.logger.Info("Stopping and disabling services on all nodes for HA takeover",
			zap.Strings("services", services))

		for _, svc := range services {
			// Stop service
			stopCmd := fmt.Sprintf("systemctl stop %s", svc)
			start := time.Now()
			if _, err := rm.deployment.Exec(ctx, nodeAddresses, stopCmd); err != nil {
				rm.controller.logger.Warn("Failed to stop service", zap.String("service", svc), zap.Error(err))
			}

			// Disable service
			disableCmd := fmt.Sprintf("systemctl disable %s", svc)
			if _, err := rm.deployment.Exec(ctx, nodeAddresses, disableCmd); err != nil {
				rm.controller.logger.Warn("Failed to disable service", zap.String("service", svc), zap.Error(err))
			}
			log.Debug("MakeHa: stopped and disabled service", zap.String("service", svc), zap.Strings("hosts", nodeAddresses),
				zap.Duration("took", time.Since(start)))
		}

		// Migrate existing data to /tmp before HA takeover
		if mountPoint != "" {
			rm.controller.logger.Info("Backing up existing data before HA takeover",
				zap.String("mount_point", mountPoint))

			backupDir := fmt.Sprintf("/tmp/ha_backup_%s", strings.ReplaceAll(mountPoint, "/", "_"))

			// Use rsync or cp -a to backup all files including hidden ones and subdirectories
			backupCmd := fmt.Sprintf("if [ -d \"%s\" ]; then mkdir -p %s && rsync -a %s/ %s/ 2>/dev/null || cp -a %s/. %s/. 2>/dev/null; fi",
				mountPoint, backupDir, mountPoint, backupDir, mountPoint, backupDir)

			if _, err := rm.deployment.Exec(ctx, nodeAddresses, backupCmd); err != nil {
				rm.controller.logger.Warn("Failed to backup data (continuing anyway)",
					zap.String("mount_point", mountPoint),
					zap.Error(err))
			} else {
				rm.controller.logger.Info("Data backup completed",
					zap.String("backup_dir", backupDir))
			}
		}
	}

	// Handle mount unit creation
	if mountPoint != "" {
		mountUnitName := strings.TrimPrefix(mountPoint, "/")
		mountUnitName = strings.ReplaceAll(mountUnitName, "/", "-")
		mountUnitName = fmt.Sprintf("%s.mount", mountUnitName)

		mountContent := rm.generateSystemdMountUnit(resource, mountPoint, fsType)
		mountPath := fmt.Sprintf("/etc/systemd/system/%s", mountUnitName)

		rm.controller.logger.Info("Distributing mount unit", zap.String("path", mountPath))

		start := time.Now()
		_, err := rm.deployment.DistributeConfig(ctx, hosts, mountContent, mountPath)
		log.Debug("MakeHa: distributed mount unit", zap.String("path", mountPath), zap.Strings("hosts", hosts),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return "", fmt.Errorf("failed to distribute mount unit: %w", err)
		}

		// Reload systemd to pick up new unit
		if _, err := rm.deployment.Exec(ctx, hosts, "systemctl daemon-reload"); err != nil {
			rm.controller.logger.Warn("Failed to reload systemd", zap.Error(err))
		}
	} else {
		log.Debug("MakeHa: no mount point; no mount unit", zap.String("resource", resource))
	}

	// Generate drbd-reactor promoter config. When the caller supplied an explicit
	// ordered start[] list, honour it verbatim (systemd units and OCF agents are
	// peers in one sequence); otherwise fall back to the legacy bucketed order
	// (mount -> VIP -> services -> OCF agents) for older clients/CLI.
	configPath := fmt.Sprintf("/etc/drbd-reactor.d/haify-ha-%s.toml", resource)
	var configContent string
	if len(startItems) > 0 {
		configContent = rm.generatePromoterConfigOrdered(resource, startItems)
	} else {
		configContent = rm.generatePromoterConfig(resource, services, mountPoint, vip, ocfAgents)
	}

	rm.controller.logger.Debug("Generated promoter config",
		zap.String("config", configContent))

	// Distribute config to all hosts using DistributeConfig
	start = time.Now()
	_, err = rm.deployment.DistributeConfig(ctx, hosts, configContent, configPath)
	log.Debug("MakeHa: distributed promoter config", zap.String("path", configPath), zap.Bool("ordered", len(startItems) > 0),
		zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return "", fmt.Errorf("failed to distribute promoter config: %w", err)
	}
	// A DR node may still hold a copy written before DR nodes were left out.
	if dbResource.WANMode && dbResource.DRNode != "" {
		log.Debug("MakeHa: retiring stale promoter on DR node", zap.String("resource", resource), zap.String("dr_node", dbResource.DRNode))
		rm.retireHaPromoter(ctx, resource, []string{rm.controller.ResolveHost(dbResource.DRNode)})
	}

	// Reload drbd-reactor on all hosts
	start = time.Now()
	_, err = rm.deployment.ReactorReload(ctx, hosts)
	log.Debug("MakeHa: reloaded drbd-reactor", zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		rm.controller.logger.Warn("Failed to reload drbd-reactor", zap.Error(err))
	}

	// Restore backed up data after drbd-reactor takes over
	if mountPoint != "" {
		rm.controller.logger.Info("Restoring backed up data after HA takeover",
			zap.String("mount_point", mountPoint))

		backupDir := fmt.Sprintf("/tmp/ha_backup_%s", strings.ReplaceAll(mountPoint, "/", "_"))

		// Find the active (primary) node for restoration
		activeNode, err := rm.findActiveNode(ctx, resource, hosts)
		if err != nil {
			rm.controller.logger.Warn("Failed to find active node for data restore",
				zap.Error(err))
		} else {
			rm.controller.logger.Info("Restoring data on active node",
				zap.String("active_node", activeNode),
				zap.String("backup_dir", backupDir))

			// Restore data preserving original permissions with rsync/cp -a
			// Backup is kept at /tmp/ha_backup_* for manual recovery if needed
			restoreCmd := fmt.Sprintf(
				"if [ -d \"%s\" ] && [ \"$(ls -A %s 2>/dev/null)\" ]; then "+
					"mkdir -p %s && "+
					"rsync -a %s/ %s/ 2>/dev/null || cp -a %s/. %s/. && "+
					"echo 'Data restored successfully. Backup retained at %s for manual recovery if needed.'; "+
					"else echo 'No backup found or backup is empty'; fi",
				backupDir, backupDir, mountPoint, backupDir, mountPoint, mountPoint, backupDir, backupDir)

			start := time.Now()
			result, err := rm.deployment.Exec(ctx, []string{activeNode}, restoreCmd)
			log.Debug("MakeHa: ran data restore", zap.String("host", activeNode), zap.Duration("took", time.Since(start)), zap.Error(err))
			if err != nil {
				rm.controller.logger.Warn("Failed to restore data",
					zap.String("active_node", activeNode),
					zap.Error(err))
			} else {
				for host, hr := range result.Hosts {
					if hr.Output != "" {
						rm.controller.logger.Info("Restore result",
							zap.String("node", host),
							zap.String("output", hr.Output))
					}
				}
			}
		}
	}

	// Save HA config to database
	if rm.controller.db != nil {
		haCfg := &database.HaConfig{
			Resource:   resource,
			VIP:        vip,
			MountPoint: mountPoint,
			FsType:     fsType,
			Services:   services,
			OcfAgents:  ocfAgentRecords(ocfAgents),
			StartItems: startItemRecords(startItems),
		}
		err := rm.controller.db.SaveHaConfig(ctx, haCfg)
		log.Debug("MakeHa: saved HA record", zap.String("resource", resource), zap.Error(err))
		if err != nil {
			rm.controller.logger.Warn("Failed to save HA config to database", zap.Error(err))
		}
	}

	return configPath, nil
}

// ListHaConfigs lists all HA configurations from database
func (rm *ResourceManager) ListHaConfigs(ctx context.Context) ([]*database.HaConfig, error) {
	log := rm.resourcesLog()
	if rm.controller.db == nil {
		log.Debug("ListHaConfigs: no database")
		return nil, fmt.Errorf("database not available")
	}
	cfgs, err := rm.controller.db.ListHaConfigs(ctx)
	log.Debug("ListHaConfigs: listed", zap.Int("count", len(cfgs)), zap.Error(err))
	return cfgs, err
}

// GetHaConfig gets an HA configuration from database
func (rm *ResourceManager) GetHaConfig(ctx context.Context, resource string) (*database.HaConfig, error) {
	log := rm.resourcesLog()
	if rm.controller.db == nil {
		log.Debug("GetHaConfig: no database", zap.String("resource", resource))
		return nil, fmt.Errorf("database not available")
	}
	cfg, err := rm.controller.db.GetHaConfig(ctx, resource)
	log.Debug("GetHaConfig: lookup", zap.String("resource", resource), zap.Bool("found", cfg != nil), zap.Error(err))
	return cfg, err
}

// checkHaMountPoint refuses a mount point inside a directory another promoter
// mounts over: the controller's Self-HA database and the gateways' state. The
// promoters move independently, so whichever mounts second covers or is
// covered by the other, and a covered mount cannot be unmounted by path — its
// promoter then cannot stop, and the resource cannot be demoted.
func checkHaMountPoint(mountPoint string) error {
	if strings.TrimSpace(mountPoint) == "" {
		return nil
	}
	cleaned := filepath.Clean(mountPoint)
	for _, taken := range []string{selfHaMountPoint, gateway.DefaultClusterPrivateMountPath} {
		if cleaned == taken || strings.HasPrefix(cleaned, taken+"/") {
			zap.L().Debug("checkHaMountPoint: inside a Haify mount", zap.String("mount_point", cleaned), zap.String("taken", taken))
			return fmt.Errorf("mount point %s is inside %s, which Haify mounts itself; choose another directory", cleaned, taken)
		}
	}
	return nil
}

// vipServiceIPInstance derives the systemd template instance name for a VIP's
// service-ip@ unit from its (CIDR) string, matching how the promoter config is
// generated so teardown can target the exact same unit:
//
//	"192.168.1.50/24" -> "192.168.1.50-24"   (service-ip@192.168.1.50-24.service)
//	"192.168.1.50"     -> "192.168.1.50-32"   (bare IP defaults to /32)
//
// It returns "" for an empty vip so callers can skip VIP handling. This is the
// single source of truth for the instance name; generatePromoterConfig,
// RemoveHa's VIP-down step and the self-HA disable script all go through it.
func vipServiceIPInstance(vip string) string {
	vip = strings.TrimSpace(vip)
	if vip == "" {
		return ""
	}
	inst := strings.ReplaceAll(vip, "/", "-")
	if !strings.Contains(inst, "-") {
		inst = inst + "-32"
	}
	return inst
}

// generatePromoterConfig generates drbd-reactor promoter TOML config
// generatePromoterConfigOrdered renders the promoter config from an explicit,
// ordered start[] list where systemd/mount units and OCF agents are peers. It
// emits each item in the given order with no bucketing, so a correct stack
// (portblock -> Filesystem -> IPaddr2 -> service -> exportfs -> portunblock) is
// expressible. The TOML wrapper is byte-identical to generatePromoterConfig.
func (rm *ResourceManager) generatePromoterConfigOrdered(resource string, items []HaStartItem) string {
	startActions := make([]string, 0, len(items))
	for _, it := range items {
		entry := renderStartItem(it)
		if entry == "" {
			continue
		}
		startActions = append(startActions, fmt.Sprintf(`  "%s"`, entry))
	}
	rm.resourcesLog().Debug("generatePromoterConfigOrdered: rendered start list", zap.String("resource", resource),
		zap.Int("items", len(items)), zap.Int("actions", len(startActions)))

	return fmt.Sprintf(`# drbd-reactor promoter configuration for HA resource: %s
# Generated by haify-controller

[[promoter]]
[promoter.resources.%s]
runner = "systemd"
start = [
%s
]
on-drbd-demote-failure = "reboot"

`, resource, resource, strings.Join(startActions, ",\n"))
}

func (rm *ResourceManager) generatePromoterConfig(resource string, services []string, mountPoint, vip string, ocfAgents []OcfAgentSpec) string {
	var startActions []string

	// Add mount unit if mount point specified
	if mountPoint != "" {
		// Generate systemd mount unit name from path
		// e.g., /var/lib/haify -> var-lib-haify.mount
		mountUnit := strings.TrimPrefix(mountPoint, "/")
		mountUnit = strings.ReplaceAll(mountUnit, "/", "-")
		mountUnit = fmt.Sprintf("\"%s.mount\"", mountUnit)

		startActions = append(startActions, mountUnit)
	}

	// Add systemd services
	for _, svc := range services {
		startActions = append(startActions, fmt.Sprintf(`  "%s"`, svc))
	}

	// Append any extra OCF resource agents, in order, after the built-in
	// mount/services items.
	for _, agent := range ocfAgents {
		entry := renderOcfStartEntry(agent)
		if entry == "" {
			continue
		}
		startActions = append(startActions, fmt.Sprintf(`  "%s"`, entry))
	}

	// The VIP comes last, so it is the last thing up and the first thing
	// down: clients are only sent to a node whose service is ready, and stop
	// arriving before it goes away. It used to come before the services,
	// which during a failover handed clients a node still starting up, and on
	// the way out kept the address answering for a service that was already
	// stopping. Gateways have always ordered it this way. A service that binds
	// to the VIP itself still starts: MakeHa sets ip_nonlocal_bind.
	if inst := vipServiceIPInstance(vip); inst != "" {
		// Use service-ip systemd unit: service-ip@<IP>-<MASK>.service
		serviceIPUnit := fmt.Sprintf("\"service-ip@%s.service\"", inst)
		startActions = append(startActions, serviceIPUnit)
	}
	rm.resourcesLog().Debug("generatePromoterConfig: rendered start list", zap.String("resource", resource),
		zap.Bool("mount", mountPoint != ""), zap.Int("services", len(services)), zap.Int("ocf_agents", len(ocfAgents)),
		zap.Bool("vip", vip != ""), zap.Int("actions", len(startActions)))

	// Generate TOML config
	toml := fmt.Sprintf(`# drbd-reactor promoter configuration for HA resource: %s
# Generated by haify-controller

[[promoter]]
[promoter.resources.%s]
runner = "systemd"
start = [
%s
]
on-drbd-demote-failure = "reboot"

`, resource, resource, strings.Join(startActions, ",\n"))

	return toml
}
