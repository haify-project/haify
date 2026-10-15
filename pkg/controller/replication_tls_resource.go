package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Turning TLS on or off for a resource. DRBD refuses to change a connection's
// transport while it is up (net_conf_change: -EINVAL), so every connection has
// to go down and come back with the new options. Taking them all down at once
// would drop the Primary below quorum; instead each link is cycled on its own —
// disconnect both ends, load the new net options, connect — while the others
// keep the votes. A link whose handshake fails is left StandAlone by DRBD and
// never retried, so the switch stops there and names it.

// tlsLinkTimeout is how long one link may take to come back.
const tlsLinkTimeout = 60 * time.Second

// SetResourceTLS switches encrypted replication on or off for every connection
// of resource, and returns how many links it changed.
func (rm *ResourceManager) SetResourceTLS(ctx context.Context, resource string, on bool) (int, error) {
	log := rm.resourcesLog()
	log.Debug("SetResourceTLS: start", zap.String("resource", resource), zap.Bool("tls", on))
	info, err := rm.GetResource(ctx, resource)
	if err != nil {
		log.Debug("SetResourceTLS: resource lookup failed", zap.String("resource", resource), zap.Error(err))
		return 0, err
	}
	if info.WANMode {
		log.Debug("SetResourceTLS: WAN resource; refusing", zap.String("resource", resource))
		return 0, fmt.Errorf("%s replicates off-site through haify-proxy, which already runs mutual TLS; DRBD TLS is for the local links", resource)
	}
	members := tlsMembers(info)
	log.Debug("SetResourceTLS: mesh members", zap.String("resource", resource), zap.Strings("nodes", members))
	if on {
		if err := rm.assertTLSReady(ctx, members); err != nil {
			log.Debug("SetResourceTLS: members not ready for TLS", zap.String("resource", resource), zap.Error(err))
			return 0, err
		}
	}
	value := "no"
	if on {
		value = "yes"
	}
	if _, _, err := rm.stageResourceConfig(ctx, resource, func(current string) (string, error) {
		return applyDrbdOptions(current, map[string]string{"net/tls": value})
	}); err != nil {
		log.Debug("SetResourceTLS: staging config failed", zap.String("resource", resource), zap.Error(err))
		return 0, err
	}
	log.Debug("SetResourceTLS: config staged", zap.String("resource", resource), zap.String("tls", value))

	changed := 0
	for i, a := range members {
		for _, b := range members[i+1:] {
			if rm.linkTLS(ctx, resource, a, b) == on {
				log.Debug("SetResourceTLS: link already as wanted", zap.String("resource", resource),
					zap.String("node", a), zap.String("peer", b))
				continue
			}
			log.Debug("SetResourceTLS: cycling link", zap.String("resource", resource), zap.String("node", a), zap.String("peer", b))
			if err := rm.cycleLink(ctx, resource, a, b, on); err != nil {
				log.Debug("SetResourceTLS: link cycle failed", zap.String("resource", resource), zap.String("node", a),
					zap.String("peer", b), zap.Int("changed", changed), zap.Error(err))
				return changed, err
			}
			changed++
		}
	}
	rm.controller.logger.Info("Switched replication TLS",
		zap.String("resource", resource), zap.Bool("tls", on), zap.Int("links", changed))
	return changed, nil
}

// tlsMembers is every node in the resource's connection mesh.
func tlsMembers(info *ResourceInfo) []string {
	var out []string
	seen := map[string]bool{}
	for _, list := range [][]string{info.Nodes, info.DisklessNodes, info.DisklessClients} {
		for _, n := range list {
			if n != "" && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// assertTLSReady refuses when any of nodes cannot complete a handshake: DRBD
// would leave that link StandAlone.
func (rm *ResourceManager) assertTLSReady(ctx context.Context, nodes []string) error {
	log := rm.resourcesLog()
	log.Debug("assertTLSReady: checking nodes", zap.Strings("nodes", nodes))
	states, err := rm.ReplicationTLSStatus(ctx, nodes)
	if err != nil {
		log.Debug("assertTLSReady: status failed", zap.Error(err))
		return err
	}
	var bad []string
	for _, s := range states {
		if !s.Ready {
			bad = append(bad, fmt.Sprintf("%s (%s)", s.Node, s.Problem))
		}
	}
	if len(bad) > 0 {
		log.Debug("assertTLSReady: nodes not ready", zap.Strings("not_ready", bad))
		return fmt.Errorf("not every node can carry an encrypted connection: %s", strings.Join(bad, "; "))
	}
	log.Debug("assertTLSReady: all nodes ready", zap.Int("count", len(states)))
	return nil
}

// cycleLink takes the a–b connection down at both ends and brings it back with
// the options now in the config.
func (rm *ResourceManager) cycleLink(ctx context.Context, resource, a, b string, on bool) error {
	log := rm.resourcesLog()
	log.Debug("cycleLink: start", zap.String("resource", resource), zap.String("node", a), zap.String("peer", b), zap.Bool("tls", on))
	steps := []struct{ node, peer, cmd string }{
		{a, b, "disconnect"}, {b, a, "disconnect"},
		{a, b, "net-options"}, {b, a, "net-options"},
		{a, b, "connect"}, {b, a, "connect"},
	}
	for _, s := range steps {
		cmd := fmt.Sprintf("sudo drbdadm %s %s:%s", s.cmd, resource, s.peer)
		if s.cmd == "disconnect" {
			cmd += " 2>/dev/null || true"
		}
		start := time.Now()
		err := rm.execAllSuccess(ctx, []string{rm.controller.ResolveHost(s.node)}, cmd,
			fmt.Sprintf("%s %s on %s", s.cmd, resource, s.node))
		log.Debug("cycleLink: step", zap.String("resource", resource), zap.String("step", s.cmd), zap.String("node", s.node),
			zap.String("peer", s.peer), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return err
		}
	}
	deadline := time.Now().Add(tlsLinkTimeout)
	for {
		state, tls := rm.linkState(ctx, resource, a, b)
		log.Debug("cycleLink: waiting for link", zap.String("resource", resource), zap.String("node", a), zap.String("peer", b),
			zap.String("state", state), zap.Bool("tls", tls))
		if state == "Connected" && tls == on {
			log.Debug("cycleLink: link back", zap.String("resource", resource), zap.String("node", a), zap.String("peer", b))
			return nil
		}
		if state == "StandAlone" || time.Now().After(deadline) {
			log.Debug("cycleLink: link did not come back", zap.String("resource", resource), zap.String("node", a),
				zap.String("peer", b), zap.String("state", state), zap.Bool("timed_out", time.Now().After(deadline)))
			return fmt.Errorf("the %s link of %s did not come back with tls=%v (it is %s); see `journalctl -u tlshd` on both nodes, then `haify resource repair %s`",
				a+"–"+b, resource, on, orUnknown(state), resource)
		}
		select {
		case <-ctx.Done():
			log.Debug("cycleLink: context done while waiting", zap.String("resource", resource), zap.Error(ctx.Err()))
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// linkTLS reports whether the a–b link is currently up with TLS.
func (rm *ResourceManager) linkTLS(ctx context.Context, resource, a, b string) bool {
	state, tls := rm.linkState(ctx, resource, a, b)
	return state == "Connected" && tls
}

// linkState reads the a–b connection as a sees it.
func (rm *ResourceManager) linkState(ctx context.Context, resource, a, b string) (string, bool) {
	log := rm.resourcesLog()
	host := rm.controller.ResolveHost(a)
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, []string{host}, "sudo drbdsetup status "+resource+" --json")
	log.Debug("linkState: read status", zap.String("resource", resource), zap.String("host", host),
		zap.Array("failed_hosts", resourcesFailedHosts{res}), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || res == nil || !res.AllSuccess() {
		return "", false
	}
	var status []struct {
		Connections []struct {
			Name  string `json:"name"`
			State string `json:"connection-state"`
			TLS   bool   `json:"tls"`
		} `json:"connections"`
	}
	if err := json.Unmarshal([]byte(hostOutput(res, host)), &status); err != nil {
		log.Debug("linkState: status JSON unreadable", zap.String("resource", resource), zap.String("host", host), zap.Error(err))
		return "", false
	}
	for _, r := range status {
		for _, c := range r.Connections {
			if c.Name == b {
				log.Debug("linkState: link", zap.String("resource", resource), zap.String("node", a), zap.String("peer", b),
					zap.String("state", c.State), zap.Bool("tls", c.TLS))
				return c.State, c.TLS
			}
		}
	}
	log.Debug("linkState: no connection to peer", zap.String("resource", resource), zap.String("node", a), zap.String("peer", b))
	return "", false
}

// assertNewMemberTLS refuses to add node to a resource whose connections are
// encrypted when node cannot complete the handshake.
func (rm *ResourceManager) assertNewMemberTLS(ctx context.Context, resource, node string) error {
	log := rm.resourcesLog()
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil || len(hosts) == 0 {
		log.Debug("assertNewMemberTLS: no hosts; skipping check", zap.String("resource", resource), zap.Error(err))
		return nil
	}
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, hosts[:1],
		fmt.Sprintf("grep -Eq '^[[:space:]]*tls[[:space:]]+yes;' /etc/drbd.d/%s.res && echo HAIFY_TLS=yes; true", resource))
	log.Debug("assertNewMemberTLS: checked config for tls", zap.String("resource", resource), zap.String("host", hosts[0]),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || res == nil {
		return nil
	}
	if _, ok := tlsField(hostOutput(res, hosts[0]), "HAIFY_TLS"); !ok {
		log.Debug("assertNewMemberTLS: resource does not use TLS", zap.String("resource", resource))
		return nil
	}
	// The node's certificate is written by the controller, which cannot
	// reach a local-agent node.
	if rm.controller.nodes != nil && rm.controller.nodes.isLocalAgentHost(node) {
		log.Debug("assertNewMemberTLS: refusing, local-agent node", zap.String("node", node))
		return fmt.Errorf("%s replicates over TLS, which a local-agent node cannot join: its certificate cannot be installed there", resource)
	}
	if err := rm.assertTLSReady(ctx, []string{node}); err != nil {
		log.Debug("assertNewMemberTLS: new member not ready", zap.String("resource", resource), zap.String("node", node), zap.Error(err))
		return fmt.Errorf("%s replicates over TLS: %w", resource, err)
	}
	log.Debug("assertNewMemberTLS: new member ready", zap.String("resource", resource), zap.String("node", node))
	return nil
}
