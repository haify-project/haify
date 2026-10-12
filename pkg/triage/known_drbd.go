package triage

import (
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// ── 1. a peer slot nobody forgot ────────────────────────────────────────

// matchPhantomPeer finds a DRBD connection to a peer that is not a registered
// node.
//
// This is the quorum failure that reads as "the resource will not promote and
// every node looks fine". Removing a replica without `drbdadm forget-peer`
// leaves its slot in the metadata; the slot still counts toward the majority,
// so a two-node cluster needs three votes and can never have them. Nothing
// reports it as an error — the phantom simply sits in `Connecting` forever,
// which is also what a peer that is merely down looks like.
//
// The registry alone cannot tell a phantom from a replica on a host Haify never
// registered — a Proxmox node, say — that is merely powered off: both are
// unregistered and both sit in Connecting. Reading the first as the second
// told an operator to forget-peer a real, recoverable replica. The resource's
// own configuration settles it: a peer it names is a replica that is down; a
// peer it does not name is a leftover slot. When no node reported its
// configuration, the finding says it could not tell rather than guessing.
func matchPhantomPeer(in Input) []Finding {
	log := zap.L()
	log.Debug("matchPhantomPeer: looking for peer slots of unregistered nodes", zap.Int("nodes", len(in.Nodes)))
	known := map[string]bool{}
	for _, n := range in.Nodes {
		known[strings.ToLower(n.Node)] = true
		if n.Address != "" {
			known[strings.ToLower(n.Address)] = true
		}
	}
	if len(known) == 0 {
		log.Debug("matchPhantomPeer: no registered nodes, nothing to compare against")
		return nil // without a registry there is nothing to be a phantom against
	}

	type phantom struct {
		peer     string
		resource string
		nodes    map[string]bool
		lines    []Evidence
	}
	seen := map[string]*phantom{}

	for _, n := range in.Nodes {
		out, ok := n.Collector("drbd_status")
		if !ok || !out.Ok {
			log.Debug("matchPhantomPeer: no drbd status from node, skipping", zap.String("node", n.Node), zap.Bool("ran", ok))
			continue
		}
		log.Debug("matchPhantomPeer: reading drbd status", zap.String("node", n.Node), zap.Int("lines", len(out.Lines)))
		resource := ""
		for _, line := range out.Lines {
			if r, isRes := drbdResourceLine(line); isRes {
				resource = r
				continue
			}
			peer, state, isPeer := drbdPeerLine(line)
			if !isPeer || known[strings.ToLower(peer)] {
				continue
			}
			// Connected to something unregistered is a registry gap, not a
			// quorum fault; only a peer that never connects holds the vote.
			if !strings.EqualFold(state, "Connecting") {
				continue
			}
			key := resource + "/" + peer
			p := seen[key]
			if p == nil {
				log.Debug("matchPhantomPeer: unregistered peer stuck connecting", zap.String("node", n.Node),
					zap.String("resource", resource), zap.String("peer", peer))
				p = &phantom{peer: peer, resource: resource, nodes: map[string]bool{}}
				seen[key] = p
			}
			p.nodes[n.Node] = true
			p.lines = append(p.lines, Evidence{
				Source: n.Node + "/drbd_status", Node: n.Node, Line: strings.TrimSpace(line),
			})
		}
	}

	configured, haveConfig := configuredPeers(in)

	out := make([]Finding, 0, len(seen))
	for _, p := range seen {
		res := p.resource
		if res == "" {
			res = "<resource>"
		}
		if configured[p.resource+"/"+strings.ToLower(p.peer)] {
			log.Debug("matchPhantomPeer: peer is in the resource configuration, a replica that is down",
				zap.String("resource", p.resource), zap.String("peer", p.peer), zap.Int("count", len(p.lines)))
			out = append(out, Finding{
				ID:       "peer-down",
				Title:    fmt.Sprintf("%s cannot reach its replica on %q", res, p.peer),
				Severity: SeverityError,
				Count:    len(p.lines),
				Nodes:    sortedKeys(p.nodes),
				Resource: p.resource,
				Known:    true,
				Cause: fmt.Sprintf("%q is a replica in %s's configuration, on a host that is not a "+
					"registered Haify node. It has not connected: the host is down, unreachable, or not "+
					"running DRBD. The resource runs on fewer copies until it returns.", p.peer, res),
				Advice: []string{
					fmt.Sprintf("Bring %s back (power it on, or fix its network) and it resyncs on its own", p.peer),
					fmt.Sprintf("Watch it reconnect: drbdsetup status %s", res),
					fmt.Sprintf("Only if %s is gone for good: remove the replica from the resource first, then drbdadm forget-peer %s:%s", p.peer, res, p.peer),
				},
				Caution:  "Do not forget-peer a replica that is only powered off: it would need a full resync to rejoin.",
				Evidence: p.lines,
			})
			continue
		}
		advice := []string{
			fmt.Sprintf("Confirm the slot is a leftover and not a node you meant to keep: drbdsetup status %s --verbose", res),
			fmt.Sprintf("On every surviving node, clear it: drbdadm forget-peer %s:%s", res, p.peer),
			fmt.Sprintf("Check the majority is now reachable: drbdsetup status %s", res),
		}
		log.Debug("matchPhantomPeer: peer is not configured, a leftover slot", zap.String("resource", p.resource),
			zap.String("peer", p.peer), zap.Bool("have_config", haveConfig), zap.Int("count", len(p.lines)))
		if !haveConfig {
			advice = append([]string{fmt.Sprintf(
				"No node reported its DRBD configuration, so this could also be a replica that is only down: check /etc/drbd.d/%s.res for an `on %s` stanza before touching it", res, p.peer)}, advice...)
		}
		out = append(out, Finding{
			ID:       "phantom-peer",
			Title:    fmt.Sprintf("%s has a peer slot for %q, which is not a registered node", res, p.peer),
			Severity: SeverityCritical,
			Count:    len(p.lines),
			Nodes:    sortedKeys(p.nodes),
			Resource: p.resource,
			Known:    true,
			Cause: "A replica was removed without clearing its slot from the DRBD metadata. " +
				"The slot still counts toward quorum, so the surviving nodes can never form a " +
				"majority and the resource will not promote — while every node that is actually " +
				"present looks healthy.",
			Advice: advice,
			Caution: "forget-peer is not reversible. A node that is merely powered off will need a " +
				"full resync to rejoin after this, so be sure the peer is gone for good.",
			Evidence: p.lines,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	log.Debug("matchPhantomPeer: verdict", zap.Int("findings", len(out)))
	return out
}

// configuredPeers reads the drbd_config_peers collector into a set of
// "<resource>/<host>" for every host a resource's configuration names. The
// second result is false when no node reported its configuration.
func configuredPeers(in Input) (map[string]bool, bool) {
	log := zap.L()
	set := map[string]bool{}
	have := false
	for _, n := range in.Nodes {
		out, ok := n.Collector("drbd_config_peers")
		if !ok || !out.Ok {
			log.Debug("configuredPeers: no drbd configuration from node", zap.String("node", n.Node), zap.Bool("ran", ok))
			continue
		}
		log.Debug("configuredPeers: reading drbd configuration", zap.String("node", n.Node), zap.Int("lines", len(out.Lines)))
		have = true
		for _, line := range out.Lines {
			// /etc/drbd.d/<resource>.res:    on <host> {
			file, rest, found := strings.Cut(line, ":")
			if !found {
				continue
			}
			f := strings.Fields(rest)
			if len(f) < 2 || f[0] != "on" {
				continue
			}
			base := file[strings.LastIndex(file, "/")+1:]
			res := strings.TrimSuffix(base, ".res")
			set[res+"/"+strings.ToLower(strings.TrimSuffix(f[1], "{"))] = true
		}
	}
	log.Debug("configuredPeers: done", zap.Int("count", len(set)), zap.Bool("have_config", have))
	return set, have
}

// drbdResourceLine recognises the un-indented line that names a resource in
// `drbdsetup status` output.
func drbdResourceLine(line string) (string, bool) {
	if line == "" || line[0] == ' ' || line[0] == '\t' {
		return "", false
	}
	f := strings.Fields(line)
	if len(f) < 2 || !strings.HasPrefix(f[1], "node-id:") {
		return "", false
	}
	return f[0], true
}

// drbdPeerLine recognises an indented peer line and returns its name and
// connection state.
func drbdPeerLine(line string) (peer, state string, ok bool) {
	if line == "" || (line[0] != ' ' && line[0] != '\t') {
		return "", "", false
	}
	f := strings.Fields(line)
	if len(f) < 2 {
		return "", "", false
	}
	for _, tok := range f[1:] {
		if v, found := strings.CutPrefix(tok, "connection:"); found {
			return f[0], v, true
		}
	}
	return "", "", false
}

// ── 2. split brain ──────────────────────────────────────────────────────

// isSplitBrainReport recognises DRBD saying a split brain happened, and only
// that. Matching the words alone caught drbd-reactor's startup line
// "Detected split-brain avoidance policy: 'quorum'" — a statement that the
// cluster is protected — and turned every failover into a critical split-brain
// finding telling the operator to discard one side's data.
//
// A split brain that was resolved is not one either: DRBD logs "detected,
// manually solved" (or "automatically solved") and runs the initial-split-brain
// handler on every detection, resolved or not. Only the split-brain handler
// and "detected but unresolved" mean it is still there. Counting the resolved
// ones sent the operator through discarding data again after a DR failback
// that had already done it.
func isSplitBrainReport(l string) bool {
	ll := strings.ToLower(l)
	if !strings.Contains(ll, "split-brain") && !strings.Contains(ll, "split brain") {
		return false
	}
	if strings.Contains(ll, "avoidance policy") ||
		strings.Contains(ll, "automatically solved") || strings.Contains(ll, "manually solved") ||
		strings.Contains(ll, "initial-split-brain") {
		return false
	}
	// "Split-Brain detected but unresolved, dropping connection!" from the
	// kernel, and the split-brain handler being invoked.
	return strings.Contains(ll, "detected") || strings.Contains(ll, "helper command")
}

func matchSplitBrain(in Input) []Finding {
	ev, nodes := nodeLinesMatching(in, isSplitBrainReport, "drbd_kernel", "kernel_errors", "reactor_journal")
	if len(ev) == 0 {
		zap.L().Debug("matchSplitBrain: verdict, no split brain reported")
		return nil
	}
	zap.L().Debug("matchSplitBrain: verdict, split brain reported", zap.Int("count", len(ev)), zap.Strings("nodes", nodes))
	return []Finding{{
		ID:       "split-brain",
		Title:    "DRBD reported split brain",
		Severity: SeverityCritical,
		Count:    len(ev),
		Nodes:    nodes,
		Known:    true,
		Cause: "Two nodes both wrote to the same resource while they could not see each other. " +
			"DRBD has kept both versions and refuses to connect them, because merging them " +
			"automatically would silently destroy whichever side it did not pick.",
		Advice: []string{
			"Decide which side has the writes you need — check the application, not DRBD; both sides are internally consistent.",
			"On the side you are discarding: drbdadm disconnect <resource>",
			"On the side you are discarding: drbdadm secondary <resource>",
			"On the side you are discarding: drbdadm connect --discard-my-data <resource>",
			"On the side you are keeping: drbdadm connect <resource>",
			"Watch it resync: drbdsetup status <resource>",
		},
		Caution: "--discard-my-data destroys every write made on that side during the split. " +
			"Take a snapshot or a backup of the discarded side first if there is any doubt " +
			"about which one is right.",
		Evidence: ev,
	}}
}

// ── 5. the module is not there ──────────────────────────────────────────

func matchDRBDModuleMissing(in Input) []Finding {
	ev, nodes := nodeLinesMatching(in, func(l string) bool {
		ll := strings.ToLower(l)
		return strings.Contains(ll, "module drbd not found") ||
			(strings.Contains(ll, "modprobe") && strings.Contains(ll, "drbd") && strings.Contains(ll, "fatal"))
	}, "drbd_kernel", "kernel_errors", "reactor_journal", "promoter_journal", "haify_journal")
	if len(ev) == 0 {
		zap.L().Debug("matchDRBDModuleMissing: verdict, no node reported the module missing")
		return nil
	}
	zap.L().Debug("matchDRBDModuleMissing: verdict, module missing", zap.Int("count", len(ev)), zap.Strings("nodes", nodes))
	return []Finding{{
		ID:       "drbd-module-missing",
		Title:    "the DRBD kernel module is not loadable on this node",
		Severity: SeverityCritical,
		Count:    len(ev),
		Nodes:    nodes,
		Known:    true,
		Cause: "The drbd-utils package installs cleanly on its own, so a node can look configured " +
			"and pass a casual check while having no kernel module at all. Such a node cannot " +
			"carry a resource: every drbdadm up on it fails.",
		Advice: []string{
			"Confirm what is actually there: drbdadm --version   (DRBD_KERNEL_VERSION=0 means no module)",
			"Install the module for the running kernel — the package name follows the distribution: drbd-dkms, kmod-drbd9x, or a LINBIT build matching uname -r.",
			"Load it and make that persist: modprobe drbd && echo drbd > /etc/modules-load.d/drbd.conf",
			"Re-check the node: haify node health-check <node>",
		},
		Evidence: ev,
	}}
}
