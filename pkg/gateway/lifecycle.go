package gateway

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Taking a gateway in and out of service is not "start the systemd target" and
// "stop the systemd target", which is why it lives in its own file rather than
// next to the per-protocol create paths.
//
// drbd-reactor owns the service chain. As long as a promoter config sits in
// /etc/drbd-reactor.d, reactor will re-promote the resource and restart the
// whole chain within seconds of anything stopping it — so a stop that only
// touches systemd looks like it worked and is undone before the caller's next
// command. Every operation here therefore manipulates the *config file* first
// (mirroring `drbd-reactorctl disable`) and only then touches the running
// services.
//
// The two other things that are easy to get wrong, and that every function
// below depends on, are the shell quoting (see runScript) and the portblock
// residue (see flushPortblockRules). Both are invisible when they fail: the
// first silently empties shell variables, the second silently firewalls a
// gateway that reports healthy on every other check. Keeping them beside the
// callers that need them is what stops a future start/stop path from being
// written without them.

// DeleteGateway deletes a gateway configuration
func (m *Manager) DeleteGateway(ctx context.Context, id string) error {
	log := m.log()
	log.Debug("DeleteGateway: start", zap.String("id", id), zap.Strings("hosts", m.hosts))
	m.logger.Info("Deleting gateway", zap.String("id", id))

	// Take the gateway out of reactor management FIRST (same disable
	// semantics as StopGateway). Stopping the target while the config is
	// live races reactor restarting the chain — which is exactly how stale
	// portblock rules survived deletion.
	if err := m.StopGateway(ctx, id); err != nil {
		m.logger.Warn("Failed to stop gateway before deletion", zap.String("id", id), zap.Error(err))
	}

	// Remove configs on all nodes
	for _, host := range m.hosts {
		m.logger.Info("Removing gateway config on node",
			zap.String("node", host),
			zap.String("gateway", id))

		// Delete the reactor config of every gateway type.
		for _, t := range GatewayTypes {
			configPath := filepath.Join(DrbdReactorConfigDir, fmt.Sprintf("haify-%s-%s.toml", t, id))
			// A stopped gateway keeps its config as .toml.disabled; the node
			// running an edited gateway may hold a .toml.pending.
			rmCmd := fmt.Sprintf("sudo rm -f %[1]s %[1]s.disabled %[1]s.pending", configPath)
			start := time.Now()
			err := m.deployment.Exec(ctx, []string{host}, rmCmd)
			log.Debug("DeleteGateway: removed config files",
				zap.String("host", host), zap.String("type", t), zap.String("file", configPath),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			if err != nil {
				m.logger.Warn("Failed to remove config file", zap.String("host", host), zap.String("file", configPath), zap.Error(err))
			}
		}

		// Reload drbd-reactor to pick up changes
		start := time.Now()
		err := m.deployment.Exec(ctx, []string{host}, reloadReactorCmd)
		log.Debug("DeleteGateway: reloaded drbd-reactor",
			zap.String("host", host), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			m.logger.Warn("Failed to reload drbd-reactor", zap.String("host", host), zap.Error(err))
		}
	}

	m.logger.Info("Gateway deleted successfully", zap.String("id", id))
	return nil
}

// writeReactorConfig installs a gateway's promoter config on the resource's
// diskful nodes and retires it everywhere else.
//
// The config used to go to every host the controller manages. A promoter on a
// node that is not a replica can still act: a tiebreaker is connected to the
// resource, and DRBD 9 lets a diskless node become Primary with I/O served over
// the network. Once one was connected, its promoter took the resource the first
// time the diskful nodes could not, and ran the whole gateway — filesystems,
// NFS state, the service IP — on a machine with no copy of the data.
func (m *Manager) writeReactorConfig(ctx context.Context, resource, pluginID, config string) error {
	log := m.log()
	remotePath := filepath.Join(DrbdReactorConfigDir, fmt.Sprintf("%s.toml", pluginID))
	// The config is never traced: an iSCSI one carries the CHAP password.
	log.Debug("writeReactorConfig: start",
		zap.String("resource", resource), zap.String("plugin", pluginID), zap.Int("size_bytes", len(config)))
	run, rest := m.promoterHosts(ctx, resource)

	m.logger.Debug("Writing reactor config",
		zap.Strings("hosts", run),
		zap.Strings("retired_on", rest),
		zap.String("path", remotePath))

	if strings.HasPrefix(pluginID, "haify-nfs-") {
		log.Debug("writeReactorConfig: NFS gateway, preparing nodes", zap.Strings("hosts", run))
		m.prepareNFSNode(ctx, run, "")
	}
	start := time.Now()
	if err := m.deployment.DistributeConfig(ctx, run, config, remotePath); err != nil {
		log.Debug("writeReactorConfig: distribute failed",
			zap.Strings("hosts", run), zap.String("path", remotePath), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to write config: %w", err)
	}
	log.Debug("writeReactorConfig: config distributed",
		zap.Strings("hosts", run), zap.String("path", remotePath), zap.Duration("took", time.Since(start)))

	reloadCmd := reloadReactorCmd
	start = time.Now()
	err := m.deployment.Exec(ctx, run, reloadCmd)
	log.Debug("writeReactorConfig: reloaded drbd-reactor",
		zap.Strings("hosts", run), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		m.logger.Warn("Failed to reload drbd-reactor", zap.Error(err))
	}
	m.retirePromoter(ctx, rest, resource)
	return nil
}

// promoterHosts splits the managed hosts into those that may run resource's
// gateway — its diskful replicas — and the rest. When the resource cannot be
// looked up, every host is returned as a runner and none as the rest: retiring
// a promoter on a guess could take down the only node serving the gateway.
func (m *Manager) promoterHosts(ctx context.Context, resource string) (run, rest []string) {
	log := m.log()
	all := append([]string(nil), m.hosts...)
	if m.resources == nil {
		log.Debug("promoterHosts: no resource manager, every host runs it",
			zap.String("resource", resource), zap.Strings("hosts", all))
		return all, nil
	}
	start := time.Now()
	res, err := m.resources.GetResource(ctx, resource)
	if err != nil || res == nil {
		log.Debug("promoterHosts: resource lookup failed, every host runs it",
			zap.String("resource", resource), zap.Strings("hosts", all),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return all, nil
	}
	run = res.Hosts
	if len(run) == 0 {
		log.Debug("promoterHosts: resource has no diskful hosts recorded, using its nodes",
			zap.String("resource", resource), zap.Strings("nodes", res.Nodes))
		run = res.Nodes
	}
	if len(run) == 0 {
		log.Debug("promoterHosts: resource has no nodes, every host runs it",
			zap.String("resource", resource), zap.Strings("hosts", all))
		return all, nil
	}
	in := make(map[string]bool, len(run))
	for _, h := range run {
		in[h] = true
	}
	for _, h := range m.hosts {
		if !in[h] {
			rest = append(rest, h)
		}
	}
	log.Debug("promoterHosts: split",
		zap.String("resource", resource), zap.Strings("run", run), zap.Strings("rest", rest),
		zap.Duration("took", time.Since(start)))
	return append([]string(nil), run...), rest
}

// retirePromoter removes resource's gateway promoter from hosts that must not
// run it, and stops the gateway there if it is running. Stopping the target
// demotes the resource, which lets a diskful node's promoter take it.
func (m *Manager) retirePromoter(ctx context.Context, hosts []string, resource string) {
	log := m.log()
	if len(hosts) == 0 {
		log.Debug("retirePromoter: no host to retire it on", zap.String("resource", resource))
		return
	}
	log.Debug("retirePromoter: start", zap.String("resource", resource), zap.Strings("hosts", hosts))
	script := fmt.Sprintf(`found=
for b in %[1]s; do
  for f in "$b" "$b.disabled" "$b.pending"; do
    [ -e "$f" ] && rm -f "$f" && found=1
  done
done
[ -n "$found" ] || exit 0
systemctl reload drbd-reactor || systemctl restart drbd-reactor
systemctl stop '%[2]s' 2>/dev/null
true`, promoterConfigPaths(resource, ""), fmt.Sprintf("drbd-services@%s.target", strings.ReplaceAll(resource, "-", "\\x2d")))
	start := time.Now()
	err := m.runScript(ctx, hosts, script)
	log.Debug("retirePromoter: done",
		zap.String("resource", resource), zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		m.logger.Warn("Failed to retire gateway promoter on non-replica nodes",
			zap.String("resource", resource), zap.Strings("hosts", hosts), zap.Error(err))
	}
}

// StartGateway starts a stopped gateway by re-enabling its reactor config
// (.toml.disabled -> .toml) and reloading drbd-reactor, which then promotes
// and starts the service chain on the best node.
func (m *Manager) StartGateway(ctx context.Context, id string) error {
	log := m.log()
	begin := time.Now()
	log.Debug("StartGateway: start", zap.String("id", id))
	run, rest := m.promoterHosts(ctx, id)
	// Flush stale portblock DROP rules first (config still readable as
	// .disabled). The OCF portunblock agent's stop action deliberately
	// re-blocks the port, so every failover leaves one rule behind on the
	// old node — and a later failback finds the port silently firewalled.
	m.flushPortblockRules(ctx, run, id)
	// Gateways created before promoters were confined to the replicas still
	// have one on every host; re-enabling it there would hand the resource
	// back to a node without a copy of the data.
	m.retirePromoter(ctx, rest, id)
	m.prepareNFSNode(ctx, run, id)
	if err := m.moveClusterPrivatePath(ctx, run, id); err != nil {
		log.Debug("StartGateway: moving the state mount failed", zap.String("id", id), zap.Error(err))
		return err
	}
	start := time.Now()
	if err := m.runScript(ctx, run, serviceIPLastScript(id)); err != nil {
		log.Debug("StartGateway: reordering the service IP failed",
			zap.String("id", id), zap.Strings("hosts", run), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("move the service IP to the end of the chain: %w", err)
	}
	log.Debug("StartGateway: service IP is last in the chain",
		zap.String("id", id), zap.Strings("hosts", run), zap.Duration("took", time.Since(start)))

	enableScript := fmt.Sprintf(`for f in %s; do
  [ -f "$f.disabled" ] && mv "$f.disabled" "$f"
done
true`, promoterConfigPaths(id, ""))
	start = time.Now()
	if err := m.runScript(ctx, run, enableScript); err != nil {
		log.Debug("StartGateway: re-enabling the config failed",
			zap.String("id", id), zap.Strings("hosts", run), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to re-enable gateway config: %w", err)
	}
	log.Debug("StartGateway: config re-enabled",
		zap.String("id", id), zap.Strings("hosts", run), zap.Duration("took", time.Since(start)))
	start = time.Now()
	err := m.deployment.Exec(ctx, run, reloadReactorCmd)
	log.Debug("StartGateway: reloaded drbd-reactor",
		zap.String("id", id), zap.Strings("hosts", run), zap.Duration("took", time.Since(start)),
		zap.Duration("total", time.Since(begin)), zap.Error(err))
	return err
}

// moveClusterPrivatePath moves a stopped gateway's state mount out from under
// the controller's Self-HA mount point (see DefaultClusterPrivateMountPath). It
// rewrites only a disabled config: a running gateway's promoter would restart
// its whole chain on the change, and a chain that cannot stop is what this is
// here to prevent. `gateway stop` then `gateway start` moves a running one.
//
// A node may still hold the old mount. Visible, it is unmounted here — the
// gateway is stopped, so nothing uses it. Covered by the controller's own
// mount it cannot be reached by path at all, and would keep the device open
// through the next demote; that node is named and the start refused.
func (m *Manager) moveClusterPrivatePath(ctx context.Context, hosts []string, id string) error {
	oldDir := filepath.Join(legacyClusterPrivateMountPath, id)
	newDir := filepath.Join(DefaultClusterPrivateMountPath, id)
	m.log().Debug("moveClusterPrivatePath: start",
		zap.String("id", id), zap.Strings("hosts", hosts), zap.String("old", oldDir), zap.String("new", newDir))
	script := fmt.Sprintf(`old=%[1]s new=%[2]s
if awk -v p="$old" '$5 == p {f=1} END {exit !f}' /proc/self/mountinfo; then
  if mountpoint -q "$old"; then
    umount "$old" || { echo "$(hostname): cannot unmount the gateway's old state mount $old" >&2; exit 3; }
  else
    echo "$(hostname): the gateway's old state mount $old is hidden under the controller database mount %[3]s; move the controller off this node (haify ha evict haify-meta), then start the gateway again" >&2
    exit 3
  fi
fi
for f in %[4]s; do
  [ -f "$f" ] || continue
  sed "s#=$old\([/ \"]\)#=$new\1#g" "$f" >"$f.new" && mv "$f.new" "$f"
done
true`, oldDir, newDir, legacyClusterPrivateMountPath, promoterConfigPaths(id, ".disabled"))
	start := time.Now()
	if err := m.runScript(ctx, hosts, script); err != nil {
		m.log().Debug("moveClusterPrivatePath: failed",
			zap.String("id", id), zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("move gateway state mount out of %s: %w", legacyClusterPrivateMountPath, err)
	}
	m.log().Debug("moveClusterPrivatePath: done", zap.String("id", id), zap.Duration("took", time.Since(start)))
	return nil
}

// serviceIPLastScript moves the service IP to the end of a stopped iSCSI or
// NFS gateway's chain, where the templates now put it (see them for why). Like
// moveClusterPrivatePath it touches only a disabled config.
func serviceIPLastScript(id string) string {
	return fmt.Sprintf(`for f in /etc/drbd-reactor.d/haify-iscsi-%[1]s.toml.disabled /etc/drbd-reactor.d/haify-nfs-%[1]s.toml.disabled; do
  [ -f "$f" ] || continue
  awk '/"ocf:heartbeat:IPaddr2 / {ip = $0; next} /^[ \t]*\][ \t]*$/ && ip != "" {print ip; ip = ""} {print}' "$f" >"$f.new" && mv "$f.new" "$f"
done
`, id)
}

// StopGateway stops a gateway. Simply stopping the systemd target is not
// enough: drbd-reactor sees the resource may promote again and restarts the
// whole chain within seconds. Mirroring `drbd-reactorctl disable`, the
// reactor config is renamed to .toml.disabled first so reactor drops the
// resource, then the target is stopped for real.
func (m *Manager) StopGateway(ctx context.Context, id string) error {
	log := m.log()
	begin := time.Now()
	log.Debug("StopGateway: start", zap.String("id", id), zap.Strings("hosts", m.hosts))
	// A .toml.pending is the edited config the running node has not loaded
	// (see live_edit.go): it, not the .toml, is what the gateway starts from.
	disableScript := fmt.Sprintf(`for f in %s; do
  [ -f "$f" ] && mv "$f" "$f.disabled"
  [ -f "$f.pending" ] && mv -f "$f.pending" "$f.disabled"
done
true`, promoterConfigPaths(id, ""))
	start := time.Now()
	if err := m.runScript(ctx, m.hosts, disableScript); err != nil {
		log.Debug("StopGateway: disabling the config failed",
			zap.String("id", id), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to disable gateway config: %w", err)
	}
	log.Debug("StopGateway: config disabled", zap.String("id", id), zap.Duration("took", time.Since(start)))
	if err := m.reloadDrbdReactor(ctx); err != nil {
		log.Debug("StopGateway: reload failed", zap.String("id", id), zap.Error(err))
		return err
	}

	escapedID := strings.ReplaceAll(id, "-", "\\x2d")
	stopCmd := fmt.Sprintf("systemctl stop drbd-services@%s.target 2>/dev/null || true", escapedID)
	start = time.Now()
	if err := m.deployment.Exec(ctx, m.hosts, stopCmd); err != nil {
		log.Debug("StopGateway: stopping the target failed",
			zap.String("id", id), zap.Duration("took", time.Since(start)), zap.Error(err))
		return err
	}
	log.Debug("StopGateway: target stopped", zap.String("id", id), zap.Duration("took", time.Since(start)))
	// With reactor management gone and the target down, any DROP rule left
	// for the gateway's VIP/port is stale residue from reload/failure
	// loops; flush it so a later start isn't silently firewalled.
	m.flushPortblockRules(ctx, m.hosts, id)
	log.Debug("StopGateway: done", zap.String("id", id), zap.Duration("took", time.Since(begin)))
	return nil
}

// runScript executes a shell script on hosts. Dispatch wraps commands in
// sh -c "..." (double quotes), so $variables are expanded by the OUTER
// shell — i.e. silently emptied — before the script runs. Base64-encoding
// the script makes it immune to that quoting chain.
func (m *Manager) runScript(ctx context.Context, hosts []string, script string) error {
	// The script itself is not traced: some carry credentials (SMB
	// passwords, CHAP), and the deployment trace shows only that it ran.
	m.log().Debug("runScript: running script", zap.Strings("hosts", hosts), zap.Int("size_bytes", len(script)))
	start := time.Now()
	err := m.deployment.Exec(ctx, hosts, scriptCmd(script))
	m.log().Debug("runScript: done", zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// flushPortblockRules removes accumulated portblock DROP rules for the
// gateway's VIP/port on the given hosts. The OCF portblock/portunblock pair
// is not symmetric across drbd-reactor reloads and failed start loops, so
// rules pile up and leave a port firewalled while every service shows
// healthy. The VIP/port pair is extracted from the gateway's reactor config,
// which must therefore still exist when this is called. Best-effort: a flush
// failure is logged, never fatal.
func (m *Manager) flushPortblockRules(ctx context.Context, hosts []string, id string) {
	m.log().Debug("flushPortblockRules: start", zap.String("id", id), zap.Strings("hosts", hosts))
	script := fmt.Sprintf(`for b in %s; do
  f=$b; [ -f "$f" ] || f=$b.disabled; [ -f "$f" ] || continue
  ip=$(grep -oE 'ip=[0-9.]+' "$f" | head -1 | cut -d= -f2)
  port=$(grep -oE 'portno=[0-9]+' "$f" | head -1 | cut -d= -f2)
  [ -n "$ip" ] && [ -n "$port" ] || continue
  while iptables -D INPUT -d "$ip" -p tcp -m multiport --dports "$port" -j DROP 2>/dev/null; do :; done
done
true`, promoterConfigPaths(id, ""))
	if err := m.runScript(ctx, hosts, script); err != nil {
		m.logger.Warn("Failed to flush leftover portblock rules",
			zap.String("gateway", id), zap.Error(err))
	}
}

// reloadReactorCmd reloads drbd-reactor where it is installed. A registered
// node need not run it — a hypervisor host kept for its disks, say — and
// "unit not found" there used to fail stopping a gateway that never ran on it.
const reloadReactorCmd = "if systemctl cat drbd-reactor.service >/dev/null 2>&1; then " +
	"sudo systemctl reload drbd-reactor || sudo systemctl restart drbd-reactor; fi"

// reloadDrbdReactor reloads drbd-reactor configuration
func (m *Manager) reloadDrbdReactor(ctx context.Context) error {
	reloadCmd := reloadReactorCmd
	start := time.Now()
	err := m.deployment.Exec(ctx, m.hosts, reloadCmd)
	m.log().Debug("reloadDrbdReactor: done", zap.Strings("hosts", m.hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}
