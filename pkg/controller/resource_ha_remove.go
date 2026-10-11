package controller

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"go.uber.org/zap"
)

// EvictHa evicts the HA resource from the active node
// drbd-reactor will handle the complete failover process:
// 1. Mask the target on active node
// 2. Stop all services (mount, VIP, etc.)
// 3. Demote DRBD to Secondary
// 4. Wait for another node to promote to Primary
func (rm *ResourceManager) EvictHa(ctx context.Context, resource string) error {
	rm.controller.logger.Info("Evicting HA resource",
		zap.String("resource", resource))

	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		return err
	}

	rm.controller.logger.Info("Hosts configured",
		zap.Strings("hosts", hosts))

	// Find the active (Primary) node
	activeNode, err := rm.findActiveNode(ctx, resource, hosts)
	if err != nil {
		return fmt.Errorf("failed to find active node: %w", err)
	}

	rm.controller.logger.Info("Found active node for eviction",
		zap.String("resource", resource),
		zap.String("active_node", activeNode))

	// The config name for drbd-reactorctl (without .toml extension)
	configName := fmt.Sprintf("haify-ha-%s", resource)

	// Evicting the controller's own metadata resource stops this very
	// process mid-eviction: a synchronous drbd-reactorctl child (local or
	// SSH session) dies with us and aborts the eviction half-way. Launch it
	// detached through systemd-run on the active node and return.
	if resource == SelfHaResource {
		evictCmd := fmt.Sprintf(
			"sudo systemd-run --unit=haify-selfha-evict "+collectFlag+" drbd-reactorctl evict %s", configName)
		if activeNode == localHostname() {
			// Usually the case: the controller runs where haify-meta is Primary.
			// Its hostname need not be a name dispatch can reach.
			if o, err := exec.Command("/bin/bash", "-c", evictCmd).CombinedOutput(); err != nil {
				return fmt.Errorf("failed to launch detached self-eviction: %s", strings.TrimSpace(string(o)))
			}
		} else if err := rm.execAllSuccess(ctx, []string{rm.controller.ResolveHost(activeNode)}, evictCmd,
			"failed to launch detached self-eviction"); err != nil {
			return err
		}
		rm.controller.logger.Info("Detached self-eviction launched",
			zap.String("resource", resource),
			zap.String("active_node", activeNode))
		return nil
	}

	// The promoter that owns the resource is an HA config or a gateway, and
	// only the node can say which: its config file is what exists there.
	// drbd-reactorctl itself is no help — handed a config name that does not
	// exist it prints "ignoring" and exits 0, so evicting a gateway under the
	// HA config's name reported success and moved nothing.
	script := evictScript(resource)
	var out string
	if activeNode == localHostname() {
		o, err := exec.Command("/bin/bash", "-c", script).CombinedOutput()
		out = strings.TrimSpace(string(o))
		if err != nil {
			return fmt.Errorf("failed to evict %s on %s: %s", resource, activeNode, out)
		}
	} else {
		result, err := rm.deployment.Exec(ctx, []string{rm.controller.ResolveHost(activeNode)},
			"echo "+base64Std(script)+" | base64 -d | sudo /bin/bash")
		if err != nil {
			return fmt.Errorf("failed to evict HA resource: %w", err)
		}
		if !result.AllSuccess() {
			return fmt.Errorf("evict failed: %s", result.FailureDetails())
		}
		for _, hr := range result.Hosts {
			out = strings.TrimSpace(hr.Output)
		}
	}
	rm.controller.logger.Info("Evict output", zap.String("node", activeNode), zap.String("output", out))

	rm.controller.logger.Info("HA resource evicted successfully",
		zap.String("resource", resource))

	return nil
}

// evictPromoterConfigs are the promoter configs Haify writes for a resource, in
// the order evictScript tries them.
var evictPromoterConfigs = []string{"haify-ha-%s", "haify-nfs-%s", "haify-iscsi-%s", "haify-nvmeof-%s", "haify-smb-%s"}

// evictScript evicts the resource from the node it runs on through whichever
// Haify promoter config for it exists there, and fails when none does — or when
// no other node took the resource over. drbd-reactorctl exits 0 either way:
// when the local services do not stop in time it re-enables the resource
// where it was and says so only in its output.
//
// An app's config is named after the app, not the resource
// (haify-app-<name>.toml), so it is found by the resource it promotes.
func evictScript(resource string) string {
	names := make([]string, len(evictPromoterConfigs))
	for i, f := range evictPromoterConfigs {
		names[i] = fmt.Sprintf(f, resource)
	}
	apps := fmt.Sprintf(`$(grep -l '^\[promoter\.resources\.%s\]' /etc/drbd-reactor.d/haify-app-*.toml 2>/dev/null | sed 's#.*/##; s#\.toml$##')`, resource)
	names = append(names, apps)
	return fmt.Sprintf(`for n in %[1]s; do
  [ -f /etc/drbd-reactor.d/$n.toml ] || continue
  out=$(drbd-reactorctl evict "$n" 2>&1); rc=$?
  printf '%%s\n' "$out"
  [ $rc -eq 0 ] || exit $rc
  printf '%%s\n' "$out" | grep -qE "Node '[^']+' took over" && exit 0
  echo "no other node took over %[2]s; it is still running here" >&2
  exit 4
done
echo "no drbd-reactor promoter manages %[2]s on this node (no HA config, gateway or app)" >&2
exit 3
`, strings.Join(names, " "), resource)
}

// RemoveHa removes HA configuration for a resource
func (rm *ResourceManager) RemoveHa(ctx context.Context, resource string) error {
	rm.controller.logger.Info("Removing HA configuration", zap.String("resource", resource))

	if rm.deployment == nil {
		return fmt.Errorf("deployment client not set")
	}
	if rm.controller.db == nil {
		return fmt.Errorf("database not available")
	}

	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		return err
	}

	// Get HA config to know what to clean up
	haCfg, err := rm.controller.db.GetHaConfig(ctx, resource)
	if err != nil {
		return fmt.Errorf("failed to get HA config: %w", err)
	}

	// 1. Delete promoter config
	configPath := fmt.Sprintf("/etc/drbd-reactor.d/haify-ha-%s.toml", resource)
	if err := rm.deployment.DeleteConfig(ctx, hosts, configPath); err != nil {
		rm.controller.logger.Warn("Failed to delete promoter config", zap.Error(err))
	}

	// 2. Delete mount unit if it exists
	if haCfg.MountPoint != "" {
		mountUnitName := strings.TrimPrefix(haCfg.MountPoint, "/")
		mountUnitName = strings.ReplaceAll(mountUnitName, "/", "-")
		mountUnitName = fmt.Sprintf("%s.mount", mountUnitName)
		mountPath := fmt.Sprintf("/etc/systemd/system/%s", mountUnitName)

		if err := rm.deployment.DeleteConfig(ctx, hosts, mountPath); err != nil {
			rm.controller.logger.Warn("Failed to delete mount unit", zap.Error(err))
		}
	}

	// 3. Reload daemons
	if _, err := rm.deployment.Exec(ctx, hosts, "systemctl daemon-reload && systemctl reload drbd-reactor"); err != nil {
		rm.controller.logger.Warn("Failed to reload daemons", zap.Error(err))
	}

	// 3b. Explicitly bring the VIP down. Removing the promoter config and
	// reloading drbd-reactor does NOT reliably stop the units reactor already
	// started, and the VIP's service-ip@ unit is Type=oneshot with
	// RemainAfterExit=yes, so without an explicit stop the floating IP lingers
	// on whichever node was Primary. Run it on every resource node (we do not
	// know which one held the VIP) after the config is gone so reactor cannot
	// restart it. Idempotent: stopping an inactive/absent template instance is
	// a no-op, and any failure is only a warning so teardown still completes.
	if inst := vipServiceIPInstance(haCfg.VIP); inst != "" {
		stopCmd := fmt.Sprintf("systemctl stop service-ip@%s.service", inst)
		result, err := rm.deployment.Exec(ctx, hosts, stopCmd)
		if err != nil {
			rm.controller.logger.Warn("Failed to stop VIP service-ip unit",
				zap.String("vip", haCfg.VIP), zap.Error(err))
		} else if result != nil && !result.AllSuccess() {
			rm.controller.logger.Warn("VIP service-ip unit may still be up on some nodes",
				zap.String("vip", haCfg.VIP), zap.Strings("failed_hosts", result.FailedHosts()))
		}
	}

	// 4. Remove from database
	if err := rm.controller.db.DeleteHaConfig(ctx, resource); err != nil {
		return fmt.Errorf("failed to delete HA config from database: %w", err)
	}

	return nil
}
