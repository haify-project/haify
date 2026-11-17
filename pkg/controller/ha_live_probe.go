package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/haify-project/haify/pkg/inspect"
	"go.uber.org/zap"
)

// What a live move of an HA guest needs to know of each replica before it
// changes anything, and again before it puts things back: where the guest
// runs, which node is Primary, whether the promoter is enabled, and whether
// the unit that watches the guest is still as drbd-reactor made it.

// liveNode is one replica as a live move sees it.
type liveNode struct {
	Host string
	Node string
	VM   *vmNodeFacts
	// Reactor, TargetActive and AgentActive are `systemctl is-active` of
	// drbd-reactor, the resource's services target and the guest's agent unit.
	Reactor      string
	TargetActive string
	AgentActive  string
	// AgentRestart is the agent unit's Restart= setting as systemd has it.
	AgentRestart string
	// Promoter is enabled, disabled or none: which promoter config is there.
	Promoter string
	// StopsOnExit is set when the promoter config asks drbd-reactor to stop
	// its services when the plugin goes away, which disabling it would do.
	StopsOnExit bool
	OcfAgent    bool
	// TwoPrimaries is set when DRBD allows two Primaries on a connection.
	TwoPrimaries bool
	// MoveUnit is `systemctl is-active` of the unit that runs `virsh migrate`.
	MoveUnit string
	DRBDUp   bool
	Role     string
	Disk     string
	Quorum   bool
	// Peers is each connection by the peer's DRBD name.
	Peers map[string]livePeer
}

// livePeer is one DRBD connection of a node.
type livePeer struct {
	Connection  string
	Role        string
	Disk        string
	Replication string
}

// reachable says whether the node answered the probe.
func (n *liveNode) reachable() bool { return n.VM != nil && n.VM.Reachable }

// liveProbeScript is appended to vmProbeScript for a live move.
func liveProbeScript(resource string, g *vmGuest, agentPath string) string {
	return fmt.Sprintf(`res=%s
unit=%s
target=%s
ptoml=%s
echo "REACTOR=$(systemctl is-active drbd-reactor 2>/dev/null)"
echo "TARGET_ACTIVE=$(systemctl is-active "$target" 2>/dev/null)"
echo "AGENT_ACTIVE=$(systemctl is-active "$unit" 2>/dev/null)"
echo "AGENT_RESTART=$(systemctl show -p Restart --value "$unit" 2>/dev/null)"
echo "MOVE_UNIT=$(systemctl is-active %s 2>/dev/null)"
if [ -f "$ptoml" ]; then echo PROMOTER=enabled; f=$ptoml
elif [ -f "$ptoml.disabled" ]; then echo PROMOTER=disabled; f=$ptoml.disabled
else echo PROMOTER=none; f=/dev/null; fi
if grep -Eq '^[[:space:]]*stop-services-on-exit[[:space:]]*=[[:space:]]*true' "$f"; then echo STOPS_ON_EXIT=yes; fi
if [ -x %s ]; then echo OCF_AGENT=yes; else echo OCF_AGENT=no; fi
echo "TWO_PRIMARIES=$(drbdsetup show "$res" 2>/dev/null | grep -c 'allow-two-primaries[[:space:]]*yes')"
echo "DRBD=$(drbdsetup status "$res" --json 2>/dev/null | base64 | tr -d '\n')"
`, shellSingleQuote(resource), shellSingleQuote(g.agentUnit(resource)), shellSingleQuote(reactorTargetUnit(resource)),
		shellSingleQuote(haTomlPath(resource)), shellSingleQuote(liveMoveUnit(resource)), shellSingleQuote(agentPath))
}

// liveNodeFrom builds a liveNode from one host's probe.
func liveNodeFrom(resource, host, node string, f *vmNodeFacts) *liveNode {
	n := &liveNode{Host: host, Node: node, VM: f, Peers: map[string]livePeer{}}
	if f == nil || !f.Reachable {
		return n
	}
	kv := f.Extra
	n.Reactor, n.TargetActive, n.AgentActive = kv["REACTOR"], kv["TARGET_ACTIVE"], kv["AGENT_ACTIVE"]
	n.AgentRestart, n.Promoter, n.MoveUnit = kv["AGENT_RESTART"], kv["PROMOTER"], kv["MOVE_UNIT"]
	n.StopsOnExit = kv["STOPS_ON_EXIT"] == "yes"
	n.OcfAgent = kv["OCF_AGENT"] == "yes"
	n.TwoPrimaries = kv["TWO_PRIMARIES"] != "" && kv["TWO_PRIMARIES"] != "0"
	raw, err := base64.StdEncoding.DecodeString(kv["DRBD"])
	// The status rides in Extra only to get here; it is of no use in a trace.
	delete(kv, "DRBD")
	if err != nil {
		return n
	}
	all, err := inspect.ParseDRBDStatus(string(raw))
	if err != nil {
		return n
	}
	for _, r := range all {
		if r.Name != resource {
			continue
		}
		n.DRBDUp, n.Role, n.Quorum = true, r.Role, true
		n.Disk = "UpToDate"
		for _, d := range r.Devices {
			if d.DiskState != "UpToDate" {
				n.Disk = d.DiskState
			}
			if d.Quorum != nil && !*d.Quorum {
				n.Quorum = false
			}
		}
		if len(r.Devices) == 0 {
			n.Disk = "none"
		}
		for _, c := range r.Connections {
			p := livePeer{Connection: c.ConnectionState, Role: c.PeerRole, Disk: "UpToDate", Replication: "Established"}
			for _, pd := range c.PeerDevices {
				if pd.PeerDiskState != "UpToDate" {
					p.Disk = pd.PeerDiskState
				}
				if pd.ReplicationState != "Established" {
					p.Replication = pd.ReplicationState
				}
			}
			n.Peers[c.Name] = p
		}
	}
	return n
}

// probeLiveNodes reads every replica's state for a live move, in host order.
func (rm *ResourceManager) probeLiveNodes(ctx context.Context, resource string, g *vmGuest, agentPath string, hosts []string) []*liveNode {
	log := rm.controller.traceLogger()
	facts := rm.probeVMNodes(ctx, g, hosts, liveProbeScript(resource, g, agentPath))
	out := make([]*liveNode, 0, len(hosts))
	for _, h := range hosts {
		n := liveNodeFrom(resource, h, rm.nodeLabel(h), facts[h])
		log.Debug("ResourceManager.probeLiveNodes: node", zap.String("resource", resource), zap.String("node", n.Node),
			zap.Bool("reachable", n.reachable()), zap.String("role", n.Role), zap.String("disk", n.Disk), zap.Bool("quorum", n.Quorum),
			zap.String("promoter", n.Promoter), zap.String("target_active", n.TargetActive), zap.String("agent_active", n.AgentActive),
			zap.String("agent_restart", n.AgentRestart), zap.Bool("two_primaries", n.TwoPrimaries), zap.String("move_unit", n.MoveUnit))
		out = append(out, n)
	}
	return out
}

// liveSource finds the node a live move starts from: the one node the guest
// runs on, which must also be the only Primary and have the guest under
// drbd-reactor. Anything else is a state a live move must not start from.
func liveSource(domain, resource string, nodes []*liveNode) (*liveNode, error) {
	var running, primary []*liveNode
	for _, n := range nodes {
		switch {
		case !n.reachable():
			// A node that cannot be asked cannot have its promoter paused,
			// and may be where the guest runs.
			return nil, fmt.Errorf("%s did not answer (%s); every replica must be reachable for a live move", n.Node, n.VM.Err)
		case !n.VM.Virsh:
			continue
		case !n.VM.StateKnown:
			return nil, fmt.Errorf("libvirt on %s did not say whether %s runs there (%s)", n.Node, domain, n.VM.State)
		}
		if n.VM.Running {
			running = append(running, n)
		}
	}
	for _, n := range nodes {
		if n.Role == "Primary" {
			primary = append(primary, n)
		}
		if n.TwoPrimaries {
			return nil, fmt.Errorf("%s already allows two Primaries on %s: another migration is under way, or one was left open; "+
				"`haify resource dual-primary %s off` closes it", resource, n.Node, resource)
		}
		switch n.Promoter {
		case "enabled":
		case "disabled":
			return nil, fmt.Errorf("the HA configuration of %s is disabled on %s; a live move needs it running", resource, n.Node)
		default:
			return nil, fmt.Errorf("%s has no HA configuration for %s", n.Node, resource)
		}
		if n.StopsOnExit {
			return nil, fmt.Errorf("the HA configuration of %s sets stop-services-on-exit, which stops the guest when "+
				"drbd-reactor lets go of it; remove that line first", resource)
		}
	}
	switch {
	case len(running) == 0:
		return nil, fmt.Errorf("%s is not running on any replica; there is nothing to move live", domain)
	case len(running) > 1:
		return nil, fmt.Errorf("%s runs on %s and %s at once: a migration is already under way", domain, running[0].Node, running[1].Node)
	}
	src := running[0]
	switch {
	case len(primary) != 1 || primary[0] != src:
		names := make([]string, 0, len(primary))
		for _, p := range primary {
			names = append(names, p.Node)
		}
		return nil, fmt.Errorf("%s runs on %s but %s is Primary on [%s]; a live move starts from one Primary, where the guest runs",
			domain, src.Node, resource, strings.Join(names, ", "))
	case src.TargetActive != "active" || src.AgentActive != "active":
		return nil, fmt.Errorf("%s runs on %s but not under drbd-reactor there (services %s, agent %s); "+
			"`haify ha status %s` shows what is wrong", domain, src.Node, orDash(src.TargetActive), orDash(src.AgentActive), resource)
	case src.Reactor != "active":
		return nil, fmt.Errorf("drbd-reactor is not running on %s", src.Node)
	}
	return src, nil
}

// orDash prints an empty state as a dash.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// liveTargetReasons says what stops tgt from receiving the guest from src;
// none means it can. srcDRBDName is the name DRBD on tgt knows src by.
func liveTargetReasons(src, tgt *liveNode, srcDRBDName string) []string {
	var r []string
	if !tgt.reachable() {
		return []string{"did not answer"}
	}
	if !tgt.VM.Virsh {
		return []string{"libvirt is not installed"}
	}
	if tgt.VM.Running {
		r = append(r, "the guest already runs there")
	}
	if tgt.Reactor != "active" {
		r = append(r, "drbd-reactor is not running")
	}
	if !tgt.OcfAgent {
		r = append(r, "the VirtualDomain agent is not installed")
	}
	switch {
	case !tgt.DRBDUp:
		r = append(r, "the resource is not up")
	case tgt.Disk != "UpToDate":
		r = append(r, "its copy is "+tgt.Disk+", not UpToDate")
	case !tgt.Quorum:
		r = append(r, "it has no quorum")
	case tgt.Role == "Primary":
		r = append(r, "it is already Primary")
	}
	if tgt.DRBDUp {
		p, ok := tgt.Peers[srcDRBDName]
		switch {
		case !ok:
			r = append(r, "it has no DRBD connection to "+src.Node)
		case p.Connection != "Connected":
			r = append(r, fmt.Sprintf("its connection to %s is %s", src.Node, p.Connection))
		case p.Replication != "Established" || p.Disk != "UpToDate":
			r = append(r, fmt.Sprintf("it is not in sync with %s (%s, %s)", src.Node, p.Replication, p.Disk))
		}
	}
	return r
}

// chooseLiveTarget picks where the guest goes: want when it is named,
// otherwise the first replica that can take it, the HA config's preferred
// nodes first. defState gives each node's definition state.
func chooseLiveTarget(src *liveNode, nodes []*liveNode, want string, preferred []string, defState map[string]VMDefinitionNode,
	drbdName func(node string) string) (*liveNode, error) {
	reasons := func(n *liveNode) []string {
		r := liveTargetReasons(src, n, drbdName(src.Node))
		if d, ok := defState[n.Node]; ok && d.State != vmDefOK {
			r = append(r, vmDefStateText(d))
		}
		return r
	}
	if want != "" {
		for _, n := range nodes {
			if n.Node != want && n.Host != want {
				continue
			}
			if n == src {
				return nil, fmt.Errorf("the guest already runs on %s", n.Node)
			}
			if r := reasons(n); len(r) > 0 {
				return nil, fmt.Errorf("%s cannot take the guest: %s", n.Node, strings.Join(r, "; "))
			}
			return n, nil
		}
		return nil, fmt.Errorf("%s holds no replica that could run the guest", want)
	}
	rank := map[string]int{}
	for i, p := range preferred {
		rank[p] = i + 1
	}
	var best *liveNode
	var refused []string
	for _, n := range nodes {
		if n == src {
			continue
		}
		if r := reasons(n); len(r) > 0 {
			refused = append(refused, fmt.Sprintf("%s: %s", n.Node, strings.Join(r, "; ")))
			continue
		}
		if best == nil || (rank[n.Node] != 0 && (rank[best.Node] == 0 || rank[n.Node] < rank[best.Node])) {
			best = n
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no replica can take the guest (%s)", strings.Join(refused, " | "))
	}
	return best, nil
}
