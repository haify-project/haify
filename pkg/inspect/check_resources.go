package inspect

import (
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// checkResources judges every resource's replicas from every node's own view.
func checkResources(in *Input) []Check {
	log := zap.L()
	log.Debug("checkResources: start", zap.Int("resources", len(in.Resources)))
	var out []Check
	var unreadable []string
	for _, r := range sortedResources(in) {
		log.Debug("checkResources: resource", zap.String("resource", r.Name), zap.Strings("diskful", r.Diskful),
			zap.Strings("tiebreakers", r.Tiebreakers), zap.Strings("clients", r.Clients), zap.String("dr", r.DR),
			zap.String("served_by", r.ServedBy), zap.Bool("quorum_risk", r.QuorumRisk))
		if r.ServedBy != "" && r.ServedBy != "self-ha" {
			out = append(out, primaryCheck(in, r, AreaResources, "resource", "a `haify ha create` service", "")...)
			out = append(out, promoterChecks(in, r, AreaResources, "resource", "haify-ha-"+r.Name+".toml", "")...)
		} else if prim, _ := primaries(in, r); len(prim) > 1 {
			out = append(out, multiplePrimaries(r, AreaResources, "resource", prim))
		}
		for _, n := range participants(r) {
			p, ok := in.probe(n)
			if !ok {
				log.Debug("checkResources: node not probed", zap.String("resource", r.Name), zap.String("node", n))
				continue
			}
			if !p.DRBDOK {
				log.Debug("checkResources: DRBD status unreadable on node", zap.String("resource", r.Name), zap.String("node", n))
				unreadable = append(unreadable, in.nodeName(n))
				continue
			}
			out = append(out, replicaChecks(in, r, n, p)...)
		}
		out = append(out, disconnectedPeers(in, r)...)
		if r.QuorumRisk {
			out = append(out, Check{ID: "resource.quorum_risk", Area: AreaResources, Subject: r.Name, Status: StatusWarn,
				Message:  "two diskful replicas and no tiebreaker: losing either node suspends I/O",
				Evidence: []string{"diskful: " + strings.Join(r.Diskful, ", ")},
				Fix:      fmt.Sprintf("haify ha set-tiebreaker %s --node <third-node>", r.Name)})
		}
	}
	out = append(out, faultDomains(in)...)
	log.Debug("checkResources: done", zap.Int("findings", len(out)), zap.Strings("unreadable", unreadable))
	if len(unreadable) > 0 {
		out = append(out, Check{ID: "resource.status_unreadable", Area: AreaResources, Subject: strings.Join(dedupe(unreadable), ","),
			Status: StatusError, Message: "drbdsetup status --json gave no readable answer, so replica states there are unknown"})
	}
	if len(out) == 0 && len(in.Resources) == 0 {
		out = append(out, pass("resource.replicas", AreaResources, "no resources"))
	}
	if len(out) == 0 {
		out = append(out, pass("resource.replicas", AreaResources,
			"%s: every replica UpToDate and connected, one Primary wherever one is required", plural(len(in.Resources), "resource", "resources")))
	}
	return out
}

// replicaChecks judges one node's view of one resource: its own disks, and
// each peer it is (or should be) connected to.
func replicaChecks(in *Input, r Resource, n string, p *NodeProbe) []Check {
	name := in.nodeName(n)
	subject := r.Name + "@" + name
	addr := sshTarget(in, name)
	v := p.Resource(r.Name)
	if v == nil {
		zap.L().Debug("replicaChecks: resource not up on node", zap.String("resource", r.Name), zap.String("node", name))
		return []Check{{ID: "resource.not_up", Area: AreaResources, Subject: subject, Status: StatusFail,
			Message: fmt.Sprintf("%s is not up on %s; that replica is not protecting anything", r.Name, name),
			Fix:     fmt.Sprintf("ssh %s sudo drbdadm up %s", addr, r.Name)}}
	}
	log := zap.L()
	var out []Check
	expectDiskless := contains(r.Tiebreakers, name) || contains(r.Clients, name)
	log.Debug("replicaChecks: start", zap.String("resource", r.Name), zap.String("node", name), zap.String("role", v.Role),
		zap.Strings("disk_states", v.diskStates()), zap.Bool("expect_diskless", expectDiskless), zap.Int("connections", len(v.Connections)))
	ev := []string{fmt.Sprintf("%s: role %s, disk %s", name, v.Role, strings.Join(v.diskStates(), "/"))}
	for _, c := range v.Connections {
		ev = append(ev, fmt.Sprintf("-> %s: %s, peer-role %s, %s", c.Name, c.ConnectionState, orDash(c.PeerRole), peerStates(c)))
	}

	if bad := v.worstDisk(expectDiskless); bad != "" {
		syncing, done := anySyncing(v)
		log.Debug("replicaChecks: disk not UpToDate", zap.String("resource", r.Name), zap.String("node", name),
			zap.String("disk_state", bad), zap.Bool("syncing", syncing), zap.Float64("percent_done", done))
		if syncing && bad == "Inconsistent" {
			out = append(out, Check{ID: "resource.resyncing", Area: AreaResources, Subject: subject, Status: StatusWarn,
				Message:  fmt.Sprintf("resync in progress (%.1f%% done); the replica on %s is not a complete copy until it ends", done, name),
				Evidence: ev})
		} else {
			out = append(out, Check{ID: "resource.replica_state", Area: AreaResources, Subject: subject, Status: StatusFail,
				Message: diskMessage(bad, name), Evidence: ev, Fix: diskFix(bad, addr, r.Name, v)})
		}
	}
	if v.quorumLost() {
		out = append(out, Check{ID: "resource.quorum_lost", Area: AreaResources, Subject: subject, Status: StatusFail,
			Message:  fmt.Sprintf("%s has lost quorum for %s; I/O there is suspended or failing", name, r.Name),
			Evidence: ev, Fix: fmt.Sprintf("haify resource status %s", r.Name)})
	}
	for _, c := range v.Connections {
		peer := in.nodeName(c.Name)
		psubject := fmt.Sprintf("%s@%s->%s", r.Name, name, peer)
		log.Debug("replicaChecks: connection", zap.String("resource", r.Name), zap.String("node", name), zap.String("peer", peer),
			zap.String("connection_state", c.ConnectionState), zap.String("peer_role", c.PeerRole))
		switch {
		case strings.EqualFold(c.ConnectionState, "StandAlone"):
			out = append(out, Check{ID: "resource.standalone", Area: AreaResources, Subject: psubject, Status: StatusFail,
				Message: fmt.Sprintf("%s holds the connection to %s StandAlone: DRBD gave up on it, most often after refusing a split brain, and it stays that way until someone reconnects it",
					name, peer),
				Evidence: ev,
				Fix:      fmt.Sprintf("ssh %s sudo drbdsetup connect %s %d", addr, r.Name, c.PeerNodeID),
				Runbook:  "verify-and-repair"})
		case !c.Connected():
			// Judged once per unreachable peer by disconnectedPeers.
		default:
			peerDiskless := contains(r.Tiebreakers, peer) || contains(r.Clients, peer)
			if st := stuckState(c, expectDiskless || peerDiskless); st != "" {
				out = append(out, Check{ID: "resource.stuck_handshake", Area: AreaResources, Subject: psubject, Status: StatusFail,
					Message: fmt.Sprintf("connected to %s but replication is %s: the handshake never finished, so an out-of-date replica is never resynced",
						peer, st),
					Evidence: ev,
					Fix: fmt.Sprintf("ssh %s sudo drbdadm disconnect %s:%s && ssh %s sudo drbdadm connect %s:%s",
						addr, r.Name, c.Name, addr, r.Name, c.Name)})
			}
		}
	}
	return out
}

// primaryCheck requires exactly one Primary for a resource that serves.
//
// restart is the command that restarts its promoter; empty restarts
// drbd-reactor on a node with an UpToDate replica, which re-runs promotion.
func primaryCheck(in *Input, r Resource, area Area, prefix, why, restart string) []Check {
	prim, known := primaries(in, r)
	zap.L().Debug("primaryCheck: start", zap.String("resource", r.Name), zap.String("why", why), zap.Strings("primary", prim),
		zap.Bool("known", known))
	switch {
	case len(prim) == 1:
		return nil
	case len(prim) > 1:
		return []Check{multiplePrimaries(r, area, prefix, prim)}
	case !known:
		return []Check{{ID: prefix + ".no_primary", Area: area, Subject: r.Name, Status: StatusError,
			Message: "no node of " + r.Name + " answered, so whether it has a Primary is unknown"}}
	}
	ev, best := replicaEvidence(in, r)
	zap.L().Debug("primaryCheck: no Primary", zap.String("resource", r.Name), zap.String("best", best),
		zap.Bool("has_restart", restart != ""))
	c := Check{ID: prefix + ".no_primary", Area: area, Subject: r.Name, Status: StatusFail,
		Message:  fmt.Sprintf("%s should be serving (%s) but no node is Primary; nothing is serving its I/O", r.Name, why),
		Evidence: ev}
	switch {
	case best != "" && restart != "":
		c.Fix = restart
	case best != "":
		c.Fix = fmt.Sprintf("ssh %s sudo systemctl restart drbd-reactor", sshTarget(in, best))
	case len(r.Diskful) > 0:
		addr := sshTarget(in, r.Diskful[0])
		c.Message += "; no replica is UpToDate, so none can be promoted until one is forced — force the node that was Primary last, its data is the newest"
		c.Fix = fmt.Sprintf("ssh %s sudo drbdadm primary --force %s && ssh %s sudo drbdadm secondary %s", addr, r.Name, addr, r.Name)
	}
	return []Check{c}
}

func multiplePrimaries(r Resource, area Area, prefix string, prim []string) Check {
	return Check{ID: prefix + ".multiple_primaries", Area: area, Subject: r.Name, Status: StatusWarn,
		Message:  fmt.Sprintf("%d nodes are Primary at once; expected only during a live migration with dual-primary on", len(prim)),
		Evidence: []string{"Primary on: " + strings.Join(prim, ", ")},
		Fix:      fmt.Sprintf("haify resource dual-primary %s off", r.Name)}
}

// primaries collects the Primary nodes of a resource from every view: a
// node's own role and its connected peers' roles. known is false when no
// participant answered at all.
func primaries(in *Input, r Resource) ([]string, bool) {
	set := map[string]bool{}
	known := false
	for _, n := range participants(r) {
		p, ok := in.probe(n)
		if !ok || !p.DRBDOK {
			continue
		}
		known = true
		v := p.Resource(r.Name)
		if v == nil {
			continue
		}
		if v.Role == "Primary" {
			set[in.nodeName(n)] = true
		}
		for _, c := range v.Connections {
			if c.Connected() && c.PeerRole == "Primary" {
				set[in.nodeName(c.Name)] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	zap.L().Debug("primaries: collected", zap.String("resource", r.Name), zap.Strings("primary", out), zap.Bool("known", known))
	return out, known
}

// replicaEvidence lists every diskful replica's disk state, and names one
// that is UpToDate when there is one.
func replicaEvidence(in *Input, r Resource) ([]string, string) {
	var ev []string
	best := ""
	for _, n := range r.Diskful {
		p, ok := in.probe(n)
		if !ok {
			ev = append(ev, in.nodeName(n)+": no answer")
			continue
		}
		v := p.Resource(r.Name)
		if v == nil {
			ev = append(ev, in.nodeName(n)+": not up")
			continue
		}
		ev = append(ev, fmt.Sprintf("%s: role %s, disk %s", in.nodeName(n), v.Role, strings.Join(v.diskStates(), "/")))
		if best == "" && v.worstDisk(false) == "" {
			best = in.nodeName(n)
		}
	}
	zap.L().Debug("replicaEvidence: collected", zap.String("resource", r.Name), zap.Int("count", len(ev)), zap.String("best", best))
	return ev, best
}

func stuckState(c DRBDConnection, eitherDiskless bool) string {
	for _, pd := range c.PeerDevices {
		if stuckReplication[pd.ReplicationState] {
			zap.L().Debug("stuckState: handshake state on a connected link", zap.String("peer", c.Name), zap.Int("volume", pd.Volume),
				zap.String("replication_state", pd.ReplicationState))
			return pd.ReplicationState
		}
		if pd.ReplicationState == "Off" && !eitherDiskless {
			zap.L().Debug("stuckState: replication Off between diskful peers", zap.String("peer", c.Name), zap.Int("volume", pd.Volume))
			return "Off"
		}
	}
	return ""
}

func anySyncing(v *DRBDResource) (bool, float64) {
	for _, c := range v.Connections {
		if ok, done := c.syncing(); ok {
			return true, done
		}
	}
	return false, 0
}

func diskMessage(state, node string) string {
	switch state {
	case "Outdated":
		return fmt.Sprintf("the replica on %s is Outdated: it holds older data than its peers and is not catching up", node)
	case "Inconsistent":
		return fmt.Sprintf("the replica on %s is Inconsistent and no resync is running to complete it", node)
	case "Diskless":
		return fmt.Sprintf("%s should hold a copy but is Diskless: it lost or never attached its backing disk", node)
	default:
		return fmt.Sprintf("the replica on %s is %s", node, state)
	}
}

func diskFix(state, addr, res string, v *DRBDResource) string {
	switch state {
	case "Diskless", "Failed", "Detached":
		zap.L().Debug("diskFix: disk gone, attach", zap.String("resource", res), zap.String("disk_state", state))
		return fmt.Sprintf("ssh %s sudo drbdadm attach %s", addr, res)
	}
	for _, c := range v.Connections {
		if !c.Connected() {
			zap.L().Debug("diskFix: a peer is disconnected, adjust", zap.String("resource", res), zap.String("peer", c.Name))
			return fmt.Sprintf("ssh %s sudo drbdadm adjust %s", addr, res)
		}
	}
	zap.L().Debug("diskFix: all peers connected, reconnect", zap.String("resource", res), zap.String("disk_state", state))
	return fmt.Sprintf("ssh %s sudo drbdadm disconnect %s && ssh %s sudo drbdadm connect %s", addr, res, addr, res)
}

func peerStates(c DRBDConnection) string {
	var parts []string
	for _, pd := range c.PeerDevices {
		parts = append(parts, fmt.Sprintf("vol%d %s/%s", pd.Volume, orDash(pd.ReplicationState), orDash(pd.PeerDiskState)))
	}
	if len(parts) == 0 {
		return "no volumes"
	}
	return strings.Join(parts, ", ")
}

func participants(r Resource) []string {
	out := append([]string{}, r.Diskful...)
	out = append(out, r.Tiebreakers...)
	return append(out, r.Clients...)
}

func sortedResources(in *Input) []Resource {
	out := append([]Resource(nil), in.Resources...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// sshTarget is the address an operator reaches a node at.
func sshTarget(in *Input, name string) string {
	if n := in.node(in.nodeName(name)); n != nil && n.Address != "" {
		return n.Address
	}
	zap.L().Debug("sshTarget: no registered address, using the name", zap.String("node", name))
	return name
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func dedupe(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range list {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
