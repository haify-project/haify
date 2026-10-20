package controller

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/deployment"
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

	log := rm.resourcesLog()
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		log.Debug("EvictHa: no hosts", zap.String("resource", resource), zap.Error(err))
		return err
	}

	rm.controller.logger.Info("Hosts configured",
		zap.Strings("hosts", hosts))

	// Find the active (Primary) node
	activeNode, err := rm.findActiveNode(ctx, resource, hosts)
	if err != nil {
		log.Debug("EvictHa: no active node", zap.String("resource", resource), zap.Error(err))
		return fmt.Errorf("failed to find active node: %w", err)
	}
	local := activeNode == localHostname()
	log.Debug("EvictHa: active node", zap.String("resource", resource), zap.String("node", activeNode), zap.Bool("local", local))

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
		log.Debug("EvictHa: self-HA resource; launching detached eviction", zap.String("resource", resource),
			zap.String("node", activeNode), zap.Bool("local", local))
		start := time.Now()
		if local {
			// Usually the case: the controller runs where haify-meta is Primary.
			// Its hostname need not be a name dispatch can reach.
			o, err := exec.Command("/bin/bash", "-c", evictCmd).CombinedOutput()
			log.Debug("EvictHa: ran local detached eviction", zap.String("resource", resource),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			if err != nil {
				return fmt.Errorf("failed to launch detached self-eviction: %s", strings.TrimSpace(string(o)))
			}
		} else if err := rm.execAllSuccess(ctx, []string{rm.controller.ResolveHost(activeNode)}, evictCmd,
			"failed to launch detached self-eviction"); err != nil {
			log.Debug("EvictHa: remote detached eviction failed", zap.String("resource", resource), zap.Error(err))
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
	start := time.Now()
	if local {
		o, err := exec.Command("/bin/bash", "-c", script).CombinedOutput()
		out = strings.TrimSpace(string(o))
		log.Debug("EvictHa: ran eviction script locally", zap.String("resource", resource),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return fmt.Errorf("failed to evict %s on %s: %s", resource, activeNode, out)
		}
	} else {
		host := rm.controller.ResolveHost(activeNode)
		result, err := rm.deployment.Exec(ctx, []string{host},
			"echo "+base64Std(script)+" | base64 -d | sudo /bin/bash", deployment.WithExecTimeout(evictTimeout))
		log.Debug("EvictHa: ran eviction script remotely", zap.String("resource", resource), zap.String("host", host),
			zap.Array("failed_hosts", resourcesFailedHosts{result}), zap.Duration("took", time.Since(start)), zap.Error(err))
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

// evictTimeout bounds the eviction script. drbd-reactorctl evict stops the
// services here, which a service may take its whole TimeoutStopSec over, then
// waits up to 20 seconds for a peer to take over. Cut off at the default 30
// seconds it was interrupted after the services had stopped and before any
// peer had promoted, and the resource came back where it was.
const evictTimeout = 3 * time.Minute

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

	log := rm.resourcesLog()
	if rm.deployment == nil {
		log.Debug("RemoveHa: no deployment client", zap.String("resource", resource))
		return fmt.Errorf("deployment client not set")
	}
	if rm.controller.db == nil {
		log.Debug("RemoveHa: no database", zap.String("resource", resource))
		return fmt.Errorf("database not available")
	}

	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		log.Debug("RemoveHa: no hosts", zap.String("resource", resource), zap.Error(err))
		return err
	}

	// Get HA config to know what to clean up
	haCfg, err := rm.controller.db.GetHaConfig(ctx, resource)
	log.Debug("RemoveHa: HA config lookup", zap.String("resource", resource), zap.Bool("found", haCfg != nil), zap.Error(err))
	if err != nil {
		return fmt.Errorf("failed to get HA config: %w", err)
	}

	// 1. Delete promoter config
	configPath := fmt.Sprintf("/etc/drbd-reactor.d/haify-ha-%s.toml", resource)
	start := time.Now()
	err = rm.deployment.DeleteConfig(ctx, hosts, configPath)
	log.Debug("RemoveHa: deleted promoter config", zap.String("resource", resource), zap.String("path", configPath),
		zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		rm.controller.logger.Warn("Failed to delete promoter config", zap.Error(err))
	}

	// 2. Delete mount unit if it exists
	if haCfg.MountPoint != "" {
		mountUnitName := strings.TrimPrefix(haCfg.MountPoint, "/")
		mountUnitName = strings.ReplaceAll(mountUnitName, "/", "-")
		mountUnitName = fmt.Sprintf("%s.mount", mountUnitName)
		mountPath := fmt.Sprintf("/etc/systemd/system/%s", mountUnitName)

		start := time.Now()
		err := rm.deployment.DeleteConfig(ctx, hosts, mountPath)
		log.Debug("RemoveHa: deleted mount unit", zap.String("resource", resource), zap.String("path", mountPath),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			rm.controller.logger.Warn("Failed to delete mount unit", zap.Error(err))
		}
	} else {
		log.Debug("RemoveHa: no mount point; no mount unit to delete", zap.String("resource", resource))
	}

	// 3. Reload daemons
	start = time.Now()
	res, err := rm.deployment.Exec(ctx, hosts, "systemctl daemon-reload && systemctl reload drbd-reactor")
	log.Debug("RemoveHa: reloaded systemd and drbd-reactor", zap.String("resource", resource),
		zap.Array("failed_hosts", resourcesFailedHosts{res}), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
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
		start := time.Now()
		result, err := rm.deployment.Exec(ctx, hosts, stopCmd)
		log.Debug("RemoveHa: stopped VIP unit", zap.String("resource", resource), zap.String("vip", haCfg.VIP),
			zap.String("instance", inst), zap.Array("failed_hosts", resourcesFailedHosts{result}),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			rm.controller.logger.Warn("Failed to stop VIP service-ip unit",
				zap.String("vip", haCfg.VIP), zap.Error(err))
		} else if result != nil && !result.AllSuccess() {
			rm.controller.logger.Warn("VIP service-ip unit may still be up on some nodes",
				zap.String("vip", haCfg.VIP), zap.Strings("failed_hosts", result.FailedHosts()))
		}
	}

	// 4. Remove from database
	err = rm.controller.db.DeleteHaConfig(ctx, resource)
	log.Debug("RemoveHa: deleted HA record", zap.String("resource", resource), zap.Error(err))
	if err != nil {
		return fmt.Errorf("failed to delete HA config from database: %w", err)
	}

	return nil
}
