package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Editing a gateway that is running.
//
// The promoter config is the gateway's durable definition, and every edit
// rewrites it on the resource's diskful nodes. What it must not do is make
// drbd-reactor reload it on the node that runs the gateway. drbd-reactor
// (src/plugin.rs start_from_config) keeps a plugin across a reload only when
// its config is unchanged; a changed one is stopped and started anew, and with
// stop-services-on-exit = true the stop takes the whole service chain down and
// demotes the resource. Every node then races to promote it again — an edit
// became an unannounced failover, and on sdt one ended with no node serving
// the gateway at all. drbd-reactor does not watch its directory either (its
// snippet monitor only logs that a reload is due), so an unloaded config
// changes nothing by itself.
//
// So an edit goes three ways:
//
//   - The other diskful nodes get the new .toml and reload drbd-reactor. The
//     gateway is not running there, and a new promoter that sees the resource
//     Primary elsewhere does not try to start it (try_initial_target_start), so
//     the reload only regenerates their units: a failover lands on the edited
//     chain.
//   - The node running the gateway keeps the .toml its drbd-reactor has
//     loaded — any later reload there, for whatever reason, then leaves the
//     gateway alone — and gets the new one as .toml.pending, which
//     drbd-reactor ignores. A drop-in on drbd-reactor.service renames it into
//     place before drbd-reactor next starts; the next edit made while another
//     node runs the gateway replaces it with the .toml.
//   - On that node the edit is applied to what is running: the unit drop-ins
//     drbd-reactor would generate from the new config are written (so systemd
//     restarting a unit, or the gateway failing back here, uses the edited
//     chain), units the edit removed are stopped, parameter changes are set on
//     the live target (see live_protocols.go), and units it added are started.
//
// A gateway that runs nowhere has nothing to interrupt: its nodes get the .toml
// and reload as they always did.

// pendingSuffix marks a config written for the node that runs the gateway,
// waiting for that node's drbd-reactor to restart.
const pendingSuffix = ".pending"

// targetStateMarker starts the line the target-state probe prints.
const targetStateMarker = "haify-gateway-target"

// pendingHookScript installs the drop-in that renames a pending gateway config
// into place before drbd-reactor starts (after a reboot or a restart, when the
// gateway's units are written from the .toml anyway). A stopped gateway's
// pending copy becomes its .toml.disabled.
const pendingHookScript = `d=/etc/systemd/system/drbd-reactor.service.d
f=$d/50-haify-pending-gateway-config.conf
want=$(cat <<'EOF'
[Service]
ExecStartPre=-/bin/sh -c 'for p in /etc/drbd-reactor.d/haify-*.toml.pending; do [ -e "$$p" ] || continue; f="$${p%%.pending}"; if [ -e "$$f.disabled" ] && [ ! -e "$$f" ]; then mv -f "$$p" "$$f.disabled"; else mv -f "$$p" "$$f"; fi; done'
EOF
)
[ "$(cat "$f" 2>/dev/null)" = "$want" ] && exit 0
mkdir -p "$d" && printf '%s\n' "$want" > "$f" && systemctl daemon-reload
`

// liveEdit is what applying an edit to the node running the gateway takes,
// worked out before anything is touched.
type liveEdit struct {
	newUnits *promoterUnits
	diff     unitDiff
	params   []string
}

func planLiveEdit(oldContent, newContent string) (*liveEdit, error) {
	log := zap.L()
	log.Debug("planLiveEdit: start", zap.Int("old_size_bytes", len(oldContent)), zap.Int("new_size_bytes", len(newContent)))
	// Parse errors are not traced here: they can quote a start entry, and an
	// iSCSI target entry holds the CHAP password.
	oldU, err := parsePromoterUnits(oldContent)
	if err != nil {
		log.Debug("planLiveEdit: current config unparsable")
		return nil, fmt.Errorf("current config: %w", err)
	}
	newU, err := parsePromoterUnits(newContent)
	if err != nil {
		log.Debug("planLiveEdit: edited config unparsable")
		return nil, fmt.Errorf("edited config: %w", err)
	}
	if oldU.Resource != newU.Resource || oldU.TargetAs != newU.TargetAs || oldU.DependenciesAs != newU.DependenciesAs {
		log.Debug("planLiveEdit: edit changes the promoter itself",
			zap.String("old_resource", oldU.Resource), zap.String("new_resource", newU.Resource),
			zap.String("old_target_as", oldU.TargetAs), zap.String("new_target_as", newU.TargetAs),
			zap.String("old_dependencies_as", oldU.DependenciesAs), zap.String("new_dependencies_as", newU.DependenciesAs))
		return nil, fmt.Errorf("the edit changes the promoter itself, which cannot be applied to a running gateway")
	}
	e := &liveEdit{newUnits: newU, diff: diffUnits(oldU, newU)}
	for _, pair := range e.diff.Changed {
		log.Debug("planLiveEdit: unit parameters changed", zap.String("unit", pair[1].Name), zap.String("agent", pair[1].Agent))
		s, err := liveParamScript(pair[0], pair[1])
		if err != nil {
			log.Debug("planLiveEdit: change cannot be applied live", zap.String("unit", pair[1].Name), zap.Error(err))
			return nil, err
		}
		e.params = append(e.params, s)
	}
	log.Debug("planLiveEdit: planned",
		zap.String("resource", newU.Resource), zap.Int("units_added", len(e.diff.Added)),
		zap.Int("units_removed", len(e.diff.Removed)), zap.Int("units_changed", len(e.diff.Changed)))
	return e, nil
}

// script is what runs on the node that runs the gateway.
func (e *liveEdit) script() string {
	p := e.newUnits
	target := servicesTarget(p.Resource)
	zap.L().Debug("liveEdit.script: rendering", zap.String("resource", p.Resource), zap.String("target", target),
		zap.Int("units", len(p.Units)), zap.Int("param_scripts", len(e.params)))
	var b strings.Builder
	fmt.Fprintf(&b, `systemctl is-active --quiet %s || { echo "$(hostname): gateway %s is no longer running here" >&2; exit 3; }
put() {
  mkdir -p "$1" || exit 1
  printf '%%s' "$2" | base64 -d > "$1/reactor.conf.haify-new" || exit 1
  # drbd-reactor's snippet monitor reads a target drop-in newer than its last
  # reload as "reload required" - advice that would restart this gateway.
  if [ -n "$3" ] && [ -e "$1/reactor.conf" ]; then touch -r "$1/reactor.conf" "$1/reactor.conf.haify-new"; fi
  mv -f "$1/reactor.conf.haify-new" "$1/reactor.conf" || exit 1
}
`, shq(target), p.Resource)
	for i, u := range p.Units {
		fmt.Fprintf(&b, "put %s %s\n", shq(path.Join(systemdRunDir, u.Name+".d")),
			base64.StdEncoding.EncodeToString([]byte(p.unitDropIn(i))))
	}
	fmt.Fprintf(&b, "put %s %s keep-mtime\n", shq(path.Join(systemdRunDir, target+".d")),
		base64.StdEncoding.EncodeToString([]byte(p.targetDropIn())))
	b.WriteString("systemctl daemon-reload || exit 1\n")
	// Nothing requires a removed unit any more, so stopping it stops it alone.
	for i := len(e.diff.Removed) - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "systemctl stop %s || exit 1\n", shq(e.diff.Removed[i].Name))
	}
	for _, s := range e.params {
		fmt.Fprintf(&b, "(\n%s) || exit 1\n", s)
	}
	for _, u := range e.diff.Added {
		fmt.Fprintf(&b, "systemctl start %s || exit 1\n", shq(u.Name))
	}
	return b.String()
}

// gatewayTargetStates reports, per host, systemctl is-active of the gateway's
// services target. A host that did not answer is missing.
func (m *Manager) gatewayTargetStates(ctx context.Context, hosts []string, resource string) (map[string]string, error) {
	log := m.log()
	log.Debug("gatewayTargetStates: start", zap.String("resource", resource), zap.Strings("hosts", hosts))
	reader, ok := m.deployment.(HostOutputReader)
	if !ok {
		log.Debug("gatewayTargetStates: deployment client cannot return output")
		return nil, fmt.Errorf("deployment client cannot read node output")
	}
	script := fmt.Sprintf("printf '%%s %%s\\n' %s \"$(systemctl is-active %s 2>/dev/null)\"\n",
		targetStateMarker, shq(servicesTarget(resource)))
	start := time.Now()
	outputs, err := reader.ExecOutput(ctx, hosts, scriptCmd(script))
	if err != nil {
		log.Debug("gatewayTargetStates: probe failed",
			zap.String("resource", resource), zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, fmt.Errorf("find where gateway %s runs: %w", resource, err)
	}
	states := map[string]string{}
	for host, out := range outputs {
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) >= 1 && f[0] == targetStateMarker {
				states[host] = "unknown"
				if len(f) > 1 {
					states[host] = f[1]
				}
			}
		}
		log.Debug("gatewayTargetStates: host state", zap.String("host", host), zap.String("state", states[host]))
	}
	log.Debug("gatewayTargetStates: done",
		zap.String("resource", resource), zap.Int("count", len(states)), zap.Duration("took", time.Since(start)))
	return states, nil
}

// runningGatewayHost returns the one host whose services target is active, ""
// when none is, and an error when that cannot be told.
func runningGatewayHost(hosts []string, states map[string]string, resource string) (string, error) {
	var active, unsure []string
	for _, h := range hosts {
		switch s, ok := states[h]; {
		case !ok:
			unsure = append(unsure, h+" (no answer)")
		case s == "active":
			active = append(active, h)
		case s == "inactive" || s == "failed":
		default:
			unsure = append(unsure, h+" ("+s+")")
		}
	}
	sort.Strings(unsure)
	log := zap.L()
	if len(unsure) > 0 {
		log.Debug("runningGatewayHost: cannot tell", zap.String("resource", resource), zap.Strings("unsure", unsure))
		return "", fmt.Errorf("gateway %s: cannot tell whether it is running on %s; nothing was changed, retry once it has settled",
			resource, strings.Join(unsure, ", "))
	}
	if len(active) > 1 {
		log.Debug("runningGatewayHost: running on several nodes", zap.String("resource", resource), zap.Strings("active", active))
		return "", fmt.Errorf("gateway %s is running on more than one node (%s); nothing was changed",
			resource, strings.Join(active, ", "))
	}
	if len(active) == 1 {
		log.Debug("runningGatewayHost: running", zap.String("resource", resource), zap.String("host", active[0]))
		return active[0], nil
	}
	log.Debug("runningGatewayHost: running nowhere", zap.String("resource", resource), zap.Strings("hosts", hosts))
	return "", nil
}

// editRunningGateway writes an edited config of a gateway that is not stopped
// and applies it where the gateway runs, as described at the top of the file.
func (m *Manager) editRunningGateway(ctx context.Context, resource, pluginID, oldContent, newContent string) error {
	log := m.log()
	begin := time.Now()
	log.Debug("editRunningGateway: start", zap.String("resource", resource), zap.String("plugin", pluginID))
	edit, err := planLiveEdit(oldContent, newContent)
	if err != nil {
		log.Debug("editRunningGateway: edit cannot be planned", zap.String("resource", resource))
		return fmt.Errorf("gateway %s: %w; nothing was changed", resource, err)
	}
	run, rest := m.promoterHosts(ctx, resource)
	states, err := m.gatewayTargetStates(ctx, run, resource)
	if err != nil {
		log.Debug("editRunningGateway: target states unknown", zap.String("resource", resource), zap.Error(err))
		return err
	}
	primary, err := runningGatewayHost(run, states, resource)
	if err != nil {
		log.Debug("editRunningGateway: serving node unclear", zap.String("resource", resource), zap.Error(err))
		return err
	}
	livePath := gatewayConfigPath(pluginID)
	clearPending := fmt.Sprintf("rm -f %s\n", shq(livePath+pendingSuffix))
	if primary == "" {
		m.logger.Info("Gateway runs nowhere; writing its edited config and reloading drbd-reactor",
			zap.String("gateway", resource), zap.Strings("hosts", run))
		if err := m.runScript(ctx, run, clearPending); err != nil {
			log.Debug("editRunningGateway: clearing pending copies failed", zap.Strings("hosts", run), zap.Error(err))
			return fmt.Errorf("clear pending gateway config: %w", err)
		}
		return m.writeReactorConfig(ctx, resource, pluginID, newContent)
	}

	var others []string
	for _, h := range run {
		if h != primary {
			others = append(others, h)
		}
	}
	m.logger.Info("Editing running gateway",
		zap.String("gateway", resource), zap.String("running_on", primary), zap.Strings("other_nodes", others),
		zap.Int("units_added", len(edit.diff.Added)), zap.Int("units_removed", len(edit.diff.Removed)),
		zap.Int("units_changed", len(edit.diff.Changed)))

	if strings.HasPrefix(pluginID, "haify-nfs-") {
		log.Debug("editRunningGateway: NFS gateway, preparing nodes", zap.Strings("hosts", run))
		m.prepareNFSNode(ctx, run, "")
	}
	if err := m.runScript(ctx, []string{primary}, pendingHookScript); err != nil {
		log.Debug("editRunningGateway: pending hook install failed", zap.String("host", primary), zap.Error(err))
		return fmt.Errorf("gateway %s: install the pending-config hook on %s: %w; nothing was changed", resource, primary, err)
	}
	start := time.Now()
	if err := m.deployment.DistributeConfig(ctx, []string{primary}, newContent, livePath+pendingSuffix); err != nil {
		log.Debug("editRunningGateway: writing the pending copy failed",
			zap.String("host", primary), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("gateway %s: write the edited config on %s: %w", resource, primary, err)
	}
	log.Debug("editRunningGateway: pending copy written on the serving node",
		zap.String("host", primary), zap.String("path", livePath+pendingSuffix), zap.Duration("took", time.Since(start)))
	if len(others) > 0 {
		start = time.Now()
		if err := m.deployment.DistributeConfig(ctx, others, newContent, livePath); err != nil {
			log.Debug("editRunningGateway: writing the live copy on other nodes failed",
				zap.Strings("hosts", others), zap.Duration("took", time.Since(start)), zap.Error(err))
			return fmt.Errorf("gateway %s: write the edited config on %s: %w", resource, strings.Join(others, ","), err)
		}
		log.Debug("editRunningGateway: live copy written on other nodes",
			zap.Strings("hosts", others), zap.Duration("took", time.Since(start)))
		if err := m.runScript(ctx, others, clearPending); err != nil {
			m.logger.Warn("Failed to clear a pending gateway config", zap.Strings("hosts", others), zap.Error(err))
		}
		start = time.Now()
		err := m.deployment.Exec(ctx, others, reloadReactorCmd)
		log.Debug("editRunningGateway: reloaded drbd-reactor on other nodes",
			zap.Strings("hosts", others), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			m.logger.Warn("Failed to reload drbd-reactor", zap.Strings("hosts", others), zap.Error(err))
		}
	} else {
		log.Debug("editRunningGateway: no other diskful node", zap.String("resource", resource))
	}
	m.retirePromoter(ctx, rest, resource)

	if err := m.runScript(ctx, []string{primary}, edit.script()); err != nil {
		log.Debug("editRunningGateway: applying the edit live failed", zap.String("host", primary), zap.Error(err))
		return fmt.Errorf("gateway %s: the edited config is saved on every node, but applying it to the gateway running on %s failed: %w. "+
			"The running gateway may still have some of its previous settings; a failover uses the edited config. "+
			"To apply it now, restart the gateway (haify gateway stop %s, then haify gateway start %s), which interrupts its clients",
			resource, primary, err, resource, resource)
	}
	log.Debug("editRunningGateway: done",
		zap.String("resource", resource), zap.String("host", primary), zap.Duration("took", time.Since(begin)))
	return nil
}
