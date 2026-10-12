package inspect

import (
	"fmt"
	"math"
	"net"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Node thresholds.
const (
	clockWarn   = 2.0  // seconds
	clockFail   = 30.0 // seconds
	rootFSWarn  = 85   // percent
	rootFSFail  = 95   // percent
	lowDiskTool = "du -xh -d2 / 2>/dev/null | sort -h | tail -20"
)

// checkNodes judges each node from its probe: reachability, clock, root
// filesystem, address, DRBD stack, and that every node agrees with the
// others on versions, controller binary and /etc/hosts.
func checkNodes(in *Input) []Check {
	log := zap.L()
	log.Debug("checkNodes: start", zap.Int("nodes", len(in.Nodes)), zap.Int("probes", len(in.Probes)),
		zap.Int("probe_errors", len(in.ProbeErrors)))
	var out []Check
	for _, name := range sortedKeys(in.ProbeErrors) {
		addr := sshTarget(in, name)
		log.Debug("checkNodes: node did not answer", zap.String("node", name), zap.String("addr", addr),
			zap.String("error", in.ProbeErrors[name]))
		out = append(out, Check{ID: "nodes.ssh", Area: AreaNodes, Subject: name, Status: StatusFail,
			Message: fmt.Sprintf("%s did not answer the probe over SSH at %s; every check on it is unknown. If it is up at another address (DHCP), register that address",
				name, addr),
			Evidence: []string{in.ProbeErrors[name]},
			Fix:      fmt.Sprintf("haify node set-address %s <address-it-answers-on>", name),
			Runbook:  "renumber-nodes"})
	}
	for _, n := range in.Nodes {
		p, ok := in.Probes[n.Name]
		if !ok || p == nil {
			log.Debug("checkNodes: no probe for node", zap.String("node", n.Name))
			continue
		}
		out = append(out, nodeChecks(in, n, p)...)
	}
	out = append(out, consistencyChecks(in)...)
	out = append(out, hostsChecks(in)...)
	log.Debug("checkNodes: done", zap.Int("findings", len(out)), zap.String("worst", string(Worst(out))))
	switch {
	case len(in.Nodes) == 0:
		out = append(out, pass("nodes.health", AreaNodes, "no registered nodes"))
	case Worst(out) == StatusPass:
		out = append(out, pass("nodes.health", AreaNodes,
			"%s answered: clocks within %.0fs, root filesystems below %d%%, addresses in place, DRBD stack loaded and consistent",
			plural(len(in.Probes), "node", "nodes"), clockWarn, rootFSWarn))
	}
	return out
}

func nodeChecks(in *Input, n Node, p *NodeProbe) []Check {
	log := zap.L()
	var out []Check
	addr := sshTarget(in, n.Name)
	log.Debug("nodeChecks: start", zap.String("node", n.Name), zap.String("addr", addr), zap.Bool("complete", p.Complete),
		zap.String("hostname", p.Hostname), zap.String("ntp", p.NTPSynced), zap.Int("root_use", p.RootUse),
		zap.String("drbd_kmod", p.DRBDKmod), zap.String("reactor", p.Reactor), zap.String("reactor_active", p.ReactorUp))
	if !p.Complete {
		out = append(out, Check{ID: "nodes.probe_incomplete", Area: AreaNodes, Subject: n.Name, Status: StatusError,
			Message: "the probe output stopped before its end; checks on this node may be missing", Evidence: p.Problems})
	}
	if p.Now > 0 && !in.ProbeStart.IsZero() {
		if c, ok := clockCheck(in, n, p); ok {
			out = append(out, c)
		}
	} else {
		log.Debug("nodeChecks: no node time or probe window, clock not judged", zap.String("node", n.Name))
	}
	if p.NTPSynced == "no" {
		out = append(out, Check{ID: "nodes.ntp", Area: AreaNodes, Subject: n.Name, Status: StatusWarn,
			Message: "the clock is not synchronised to NTP; it will drift", Evidence: []string{"timedatectl NTPSynchronized=no"},
			Fix: fmt.Sprintf("ssh %s sudo timedatectl set-ntp true", addr)})
	}
	switch {
	case p.RootUse >= rootFSFail:
		out = append(out, rootCheck(n, addr, p, StatusFail))
	case p.RootUse >= rootFSWarn:
		out = append(out, rootCheck(n, addr, p, StatusWarn))
	}
	if p.Hostname != "" && n.Hostname != "" && p.Hostname != n.Hostname && p.Hostname != n.Name {
		out = append(out, Check{ID: "nodes.identity", Area: AreaNodes, Subject: n.Name, Status: StatusFail,
			Message: fmt.Sprintf("%s answers as host %q, not %q: the address now belongs to another machine", n.Address, p.Hostname, n.Hostname),
			Fix:     fmt.Sprintf("haify node set-address %s <its-current-address>", n.Name), Runbook: "renumber-nodes"})
	}
	for _, a := range []string{n.Address, n.ReplicationAddress} {
		ip := net.ParseIP(a)
		if ip == nil || p.HasAddr(a) || len(p.Addrs) == 0 {
			continue
		}
		log.Debug("nodeChecks: registered address not on an interface", zap.String("node", n.Name), zap.String("addr", a),
			zap.Bool("private", privateAddr(ip)), zap.Bool("identity_holds", identityHolds(n, p)), zap.Strings("interfaces", p.Addrs))
		// The node answered SSH at a public address that none of its
		// interfaces carries, as itself: a cloud VM behind NAT. Re-registering
		// it at the interface address would cut the controller off from it.
		if !privateAddr(ip) && identityHolds(n, p) {
			out = append(out, Check{ID: "nodes.address", Area: AreaNodes, Subject: n.Name, Status: StatusPass,
				Message: fmt.Sprintf("registered at public address %s, which none of its interfaces carries, and it answers there as itself: reached through NAT",
					a),
				Evidence: []string{"interfaces: " + strings.Join(p.Addrs, ", ")}})
			continue
		}
		out = append(out, Check{ID: "nodes.address", Area: AreaNodes, Subject: n.Name, Status: StatusWarn,
			Message:  fmt.Sprintf("registered address %s is on none of %s's interfaces (DHCP renumbered it, or SSH goes through NAT)", a, n.Name),
			Evidence: []string{"interfaces: " + strings.Join(p.Addrs, ", ")},
			Fix:      fmt.Sprintf("haify node set-address %s %s", n.Name, firstNonEmpty(candidateAddr(a, p.Addrs), "<new-address>")),
			Runbook:  "renumber-nodes"})
	}
	if p.DRBDKmod == "" {
		out = append(out, Check{ID: "nodes.drbd_module", Area: AreaNodes, Subject: n.Name, Status: StatusFail,
			Message: "the drbd kernel module is not loaded; no resource can come up here",
			Fix:     fmt.Sprintf("ssh %s sudo modprobe drbd", addr)})
	}
	if p.Reactor != "" && p.ReactorUp != "active" {
		out = append(out, Check{ID: "nodes.reactor", Area: AreaNodes, Subject: n.Name, Status: StatusFail,
			Message:  "drbd-reactor is not running, so nothing fails over to or away from this node",
			Evidence: []string{"systemctl is-active drbd-reactor: " + orDash(p.ReactorUp)},
			Fix:      fmt.Sprintf("ssh %s sudo systemctl enable --now drbd-reactor", addr)})
	}
	return out
}

// clockCheck compares a node's clock with the probe window. The node's time
// was read somewhere between ProbeStart and ProbeEnd, so only the part of the
// difference outside that window is certain skew; latency never counts.
func clockCheck(in *Input, n Node, p *NodeProbe) (Check, bool) {
	start := float64(in.ProbeStart.UnixNano()) / 1e9
	end := float64(in.ProbeEnd.UnixNano()) / 1e9
	skew := 0.0
	switch {
	case p.Now > end:
		skew = p.Now - end
	case p.Now < start:
		skew = p.Now - start
	}
	abs := math.Abs(skew)
	zap.L().Debug("clockCheck: skew", zap.String("node", n.Name), zap.Float64("node_time", p.Now), zap.Float64("window_start", start),
		zap.Float64("window_end", end), zap.Float64("skew_seconds", skew))
	if abs <= clockWarn {
		return Check{}, false
	}
	st := StatusWarn
	if abs > clockFail {
		st = StatusFail
	}
	dir := "ahead of"
	if skew < 0 {
		dir = "behind"
	}
	return Check{ID: "nodes.clock", Area: AreaNodes, Subject: n.Name, Status: st,
		Message: fmt.Sprintf("clock is at least %.1fs %s the controller's", abs, dir),
		Evidence: []string{
			"node time " + time.Unix(0, int64(p.Now*1e9)).UTC().Format(time.RFC3339Nano),
			fmt.Sprintf("probe window %s .. %s", in.ProbeStart.UTC().Format(time.RFC3339Nano), in.ProbeEnd.UTC().Format(time.RFC3339Nano)),
			"NTP synchronised: " + orDash(p.NTPSynced)},
		Fix: fmt.Sprintf("ssh %s sudo chronyc makestep", sshTarget(in, n.Name))}, true
}

func rootCheck(n Node, addr string, p *NodeProbe, st Status) Check {
	return Check{ID: "nodes.root_fs", Area: AreaNodes, Subject: n.Name, Status: st,
		Message: fmt.Sprintf("root filesystem is %d%% full; at 100%% the kubelet evicts pods (the CSI node plugin among them) and journald stops", p.RootUse),
		Fix:     fmt.Sprintf("ssh %s 'sudo %s'", addr, lowDiskTool)}
}

// privateAddr reports RFC 1918, CGNAT (100.64.0.0/10), ULA, loopback and
// link-local addresses: ones DHCP hands out and NAT hides behind.
func privateAddr(ip net.IP) bool {
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return true
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return cgnat.Contains(ip)
}

// identityHolds reports that the node answered as the host it was registered
// as, or that no hostname was recorded to compare with.
func identityHolds(n Node, p *NodeProbe) bool {
	return n.Hostname == "" || p.Hostname == "" || p.Hostname == n.Hostname || p.Hostname == n.Name
}

// candidateAddr picks the interface address most likely to be the new
// registered one: same family, not loopback or link-local.
func candidateAddr(old string, addrs []string) string {
	o := net.ParseIP(old)
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || (ip.To4() == nil) != (o.To4() == nil) {
			continue
		}
		zap.L().Debug("candidateAddr: chose interface address", zap.String("old", old), zap.String("addr", a))
		return a
	}
	zap.L().Debug("candidateAddr: no candidate", zap.String("old", old), zap.Strings("addrs", addrs))
	return ""
}

// consistencyChecks compares versions and the controller binary across nodes.
func consistencyChecks(in *Input) []Check {
	var out []Check
	for _, comp := range []struct {
		id, what string
		get      func(*NodeProbe) string
	}{
		{"nodes.drbd_version", "DRBD kernel module", func(p *NodeProbe) string { return p.DRBDKmod }},
		{"nodes.drbd_utils_version", "drbd-utils", func(p *NodeProbe) string { return p.DRBDUtils }},
		{"nodes.reactor_version", "drbd-reactor", func(p *NodeProbe) string { return p.Reactor }},
	} {
		groups := map[string][]string{}
		for _, name := range sortedKeys(in.Probes) {
			if v := comp.get(in.Probes[name]); v != "" {
				groups[v] = append(groups[v], name)
			}
		}
		zap.L().Debug("consistencyChecks: versions", zap.String("component", comp.what), zap.Int("distinct", len(groups)))
		if len(groups) > 1 {
			out = append(out, Check{ID: comp.id, Area: AreaNodes, Status: StatusWarn,
				Message:  fmt.Sprintf("%s differs between nodes; mixed versions are fine during an upgrade and a trap after one", comp.what),
				Evidence: groupEvidence(groups)})
		}
	}
	out = append(out, binaryArchChecks(in)...)
	// Binaries are compared only between nodes of one architecture: an
	// aarch64 and an x86_64 build of the same version never hash alike.
	byArch := map[string]map[string][]string{}
	for _, name := range sortedKeys(in.Probes) {
		p := in.Probes[name]
		if p.CtlSHA == "" {
			continue
		}
		if byArch[p.Arch] == nil {
			byArch[p.Arch] = map[string][]string{}
		}
		byArch[p.Arch][p.CtlSHA] = append(byArch[p.Arch][p.CtlSHA], name)
	}
	for _, arch := range sortedKeys(byArch) {
		zap.L().Debug("consistencyChecks: controller binaries", zap.String("arch", arch), zap.Int("distinct", len(byArch[arch])))
		if c, ok := binaryCheck(in, arch, byArch[arch]); ok {
			out = append(out, c)
		}
	}
	return out
}

// binaryCheck compares the controller binaries of one architecture.
func binaryCheck(in *Input, arch string, bins map[string][]string) (Check, bool) {
	if len(bins) < 2 {
		return Check{}, false
	}
	which := "nodes"
	if arch != "" {
		which = arch + " nodes"
	}
	st := StatusWarn
	msg := "the installed haify-controller binaries differ between " + which
	zap.L().Debug("binaryCheck: binaries differ", zap.String("arch", arch), zap.Int("distinct", len(bins)),
		zap.Bool("self_ha", in.SelfHA != nil))
	if in.SelfHA != nil {
		st = StatusFail
		msg += "; a Self-HA failover starts whichever version that node has, against the same database"
	}
	var nodes []string
	for _, ns := range bins {
		nodes = append(nodes, ns...)
	}
	sort.Strings(nodes)
	return Check{ID: "nodes.controller_binary", Area: AreaNodes, Subject: arch, Status: st, Message: msg,
		Evidence: groupEvidence(bins),
		Fix:      "./scripts/deploy-all.sh " + strings.Join(nodes, ",")}, true
}

func groupEvidence(groups map[string][]string) []string {
	var ev []string
	for _, v := range sortedKeys(groups) {
		ev = append(ev, fmt.Sprintf("%s: %s", v, strings.Join(groups[v], ", ")))
	}
	return ev
}

// hostsChecks flags /etc/hosts lines that map a registered node's name to an
// address other than its registered one. The controller writes these entries
// itself, and one left behind by a renumbering sends name lookups to a
// machine that is no longer the node.
func hostsChecks(in *Input) []Check {
	var out []Check
	seen := map[string]bool{}
	for _, holder := range sortedKeys(in.Probes) {
		for _, e := range in.Probes[holder].Hosts {
			ip := net.ParseIP(e.IP)
			if ip == nil || ip.IsLoopback() {
				continue
			}
			for _, n := range in.Nodes {
				if !namesNode(e.Names, n) || e.IP == n.Address || e.IP == n.ReplicationAddress {
					continue
				}
				// /etc/hosts may carry the same line twice; say it once.
				key := holder + "|" + n.Name + "|" + e.IP
				if seen[key] {
					continue
				}
				seen[key] = true
				zap.L().Debug("hostsChecks: stale hosts entry", zap.String("host", holder), zap.String("node", n.Name),
					zap.String("ip", e.IP), zap.String("registered", n.Address))
				out = append(out, Check{ID: "nodes.hosts_entry", Area: AreaNodes, Subject: holder + ":" + n.Name, Status: StatusWarn,
					Message: fmt.Sprintf("/etc/hosts on %s maps %s to %s, but %s is registered at %s", holder,
						strings.Join(e.Names, " "), e.IP, n.Name, n.Address),
					Fix: fmt.Sprintf(`ssh %s "sudo sed -i 's/^%s[[:space:]]/%s /' /etc/hosts"`, sshTarget(in, holder),
						strings.ReplaceAll(e.IP, ".", `\.`), n.Address)})
			}
		}
	}
	return out
}

func namesNode(names []string, n Node) bool {
	for _, name := range names {
		if name == n.Name || (n.Hostname != "" && name == n.Hostname) {
			return true
		}
	}
	return false
}

// elfMachineArch maps an ELF e_machine (CtlMachine) to the uname -m it runs on.
var elfMachineArch = map[string]string{"3e00": "x86_64", "b700": "aarch64", "f300": "riscv64", "1500": "ppc64le", "1600": "s390x"}

// binaryArchChecks fails a node whose controller binary is built for another
// architecture: the hash comparison above cannot see it, since it only
// compares nodes of one architecture, and under Self-HA that node is where a
// failover would try, and fail, to start the controller.
func binaryArchChecks(in *Input) []Check {
	var out []Check
	for _, name := range sortedKeys(in.Probes) {
		p := in.Probes[name]
		want, known := elfMachineArch[p.CtlMachine]
		if p.CtlMachine == "" || p.Arch == "" || !known || want == p.Arch {
			continue
		}
		zap.L().Debug("binaryArchChecks: binary built for another architecture", zap.String("node", name),
			zap.String("binary_arch", want), zap.String("arch", p.Arch))
		out = append(out, Check{ID: "nodes.controller_binary_arch", Area: AreaNodes, Subject: name, Status: StatusFail,
			Message: fmt.Sprintf("the haify-controller binary on %s is built for %s, but the node is %s; it cannot run there",
				name, want, p.Arch),
			Evidence: []string{p.CtlBin},
			Fix:      "./scripts/deploy-all.sh " + name + " (it builds for each node's architecture)"})
	}
	return out
}
