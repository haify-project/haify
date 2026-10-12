package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	pb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/deployment"
)

// Node diagnostics: the records the controller does not keep.
//
// ListControllerLogs serves the controller's own ring buffer, and almost
// nothing that breaks DRBD is in it. A promoter whose start list failed, a
// resource stuck Connecting on a peer slot nobody forgot, a kernel that
// refused the module, a thin pool with no metadata left — all of that is in
// the nodes' journals and dmesg, and until now reading it meant an operator
// with an ssh session and a guess about which node to look at.
//
// The collectors below are a fixed table. A request names collectors, never a
// command, so there is no path from an argument to something the shell runs:
// an unknown name is reported back and nothing executes. Everything in the
// table reads; nothing in it changes a node.

// diagCollector is one named, read-only reading of a node.
type diagCollector struct {
	// Name is what a caller asks for and what comes back in the response.
	Name string
	// What reports what this reads, in a sentence an assistant can put in front
	// of an operator without translating.
	What string
	// cmd renders the shell command. It takes the window in minutes and the
	// line cap so a collector can bound itself at the source rather than
	// shipping a megabyte of dmesg across the network to be cut here.
	//
	// Both arguments are integers, and the collector name is matched against
	// this table rather than interpolated, so nothing a caller sends reaches
	// the shell as text.
	cmd func(sinceMin, maxLines int) string
}

const (
	defaultDiagWindowMinutes = 60
	defaultDiagMaxLines      = 200
	maxDiagMaxLines          = 2000
	maxDiagWindowMinutes     = 7 * 24 * 60
	diagCollectorTimeout     = 20 * time.Second
)

// diagCollectors is the whole table. Adding to it is how this grows; there is
// deliberately no way for a caller to add one at runtime.
//
// Every command ends in `tail -n N+1`, one line more than the cap, which is
// how the caller learns it is looking at a tail: N+1 lines back means there
// was more, and the extra line is dropped before the response is built.
var diagCollectors = []diagCollector{
	{
		Name: "drbd_status",
		What: "every DRBD resource on this node with its role, disk and connection state",
		cmd: func(_, max int) string {
			return fmt.Sprintf("drbdsetup status --verbose --statistics 2>&1 | tail -n %d", max+1)
		},
	},
	{
		Name: "drbd_config_peers",
		What: "the hosts each DRBD resource's configuration names — a peer listed here is a replica, even when it is down",
		cmd: func(_, max int) string {
			return fmt.Sprintf("grep -HE '^[[:space:]]*on [^ ]+ *\\{' /etc/drbd.d/*.res 2>/dev/null | tail -n %d", max+1)
		},
	},
	{
		Name: "drbd_kernel",
		What: "the kernel's own DRBD messages — split brain, refused connections, IO errors",
		cmd: func(since, max int) string {
			// Windowed like every other log here. dmesg has no notion of
			// "since": it returned everything since boot, so an error a node
			// recovered from hours ago was reported as a current fault.
			return fmt.Sprintf(
				"journalctl -k --since '-%dmin' --no-pager -o short-iso 2>&1 | grep -iE 'drbd|split.?brain' | tail -n %d",
				since, max+1)
		},
	},
	{
		Name: "kernel_errors",
		What: "kernel messages at error level and above: IO errors, OOM kills, filesystem aborts",
		cmd: func(since, max int) string {
			return fmt.Sprintf(
				"journalctl -k -p err --since '-%dmin' --no-pager -o short-iso 2>&1 | tail -n %d",
				since, max+1)
		},
	},
	{
		Name: "reactor_journal",
		What: "drbd-reactor's log: what it promoted, demoted, or could not start",
		cmd: func(since, max int) string {
			return fmt.Sprintf(
				"journalctl -u drbd-reactor --since '-%dmin' --no-pager -o short-iso 2>&1 | tail -n %d",
				since, max+1)
		},
	},
	{
		Name: "promoter_journal",
		What: "the promoter units themselves — a target that failed to start names the step that failed",
		cmd: func(since, max int) string {
			return fmt.Sprintf(
				"journalctl -u 'drbd-services@*' -u 'drbd-promote@*' -u 'service-ip@*' "+
					"--since '-%dmin' --no-pager -o short-iso 2>&1 | tail -n %d",
				since, max+1)
		},
	},
	{
		Name: "failed_units",
		What: "systemd units in the failed state on this node",
		cmd: func(_, max int) string {
			return fmt.Sprintf(
				"systemctl list-units --state=failed --no-pager --no-legend --plain 2>&1 | tail -n %d", max+1)
		},
	},
	{
		Name: "haify_journal",
		What: "the Haify units' own journal — the controller and the Copilot as systemd saw them",
		cmd: func(since, max int) string {
			return fmt.Sprintf(
				"journalctl -u haify-controller -u haify-ai --since '-%dmin' --no-pager -o short-iso 2>&1 | tail -n %d",
				since, max+1)
		},
	},
	{
		Name: "storage",
		What: "volume groups and logical volumes with thin-pool data and metadata usage",
		cmd: func(_, max int) string {
			return fmt.Sprintf(
				"{ vgs --units g -o vg_name,vg_size,vg_free 2>&1; "+
					"lvs -o lv_name,vg_name,lv_size,data_percent,metadata_percent 2>&1; } | tail -n %d",
				max+1)
		},
	},
	{
		Name: "mounts",
		What: "what is mounted from a DRBD device and how full it is",
		cmd: func(_, max int) string {
			return fmt.Sprintf(
				"df -h 2>&1 | grep -E 'Filesystem|drbd' | tail -n %d", max+1)
		},
	},
}

// diagCollectorByName indexes the table once. A map lookup is also what makes
// an unknown name a reportable fact rather than an empty result.
var diagCollectorByName = func() map[string]diagCollector {
	m := make(map[string]diagCollector, len(diagCollectors))
	for _, c := range diagCollectors {
		m[c.Name] = c
	}
	return m
}()

// diagCollectorNames is the table's names in table order, which is roughly
// most- to least-often useful.
func diagCollectorNames() []string {
	out := make([]string, 0, len(diagCollectors))
	for _, c := range diagCollectors {
		out = append(out, c.Name)
	}
	return out
}

// CollectNodeDiagnostics reads the named collectors on the named nodes.
//
// It fans out per collector rather than per node: one Exec carries a command
// to every host at once, so the wall clock is the number of collectors and not
// their product with the number of nodes. A node that cannot be reached comes
// back as unreachable rather than as nine identical failures.
func (s *Server) CollectNodeDiagnostics(ctx context.Context, req *pb.CollectNodeDiagnosticsRequest) (*pb.CollectNodeDiagnosticsResponse, error) {
	log := s.ctrl.traceLogger()
	if s.ctrl == nil || s.ctrl.deployment == nil {
		log.Debug("Server.CollectNodeDiagnostics: no deployment client, cannot reach nodes")
		return &pb.CollectNodeDiagnosticsResponse{
			Success: false,
			Message: "this controller has no deployment client, so it cannot reach nodes",
		}, nil
	}

	chosen, unknown := resolveDiagCollectors(req.GetCollectors())
	log.Debug("Server.CollectNodeDiagnostics: collectors resolved", zap.Strings("requested", req.GetCollectors()),
		zap.Int("chosen", len(chosen)), zap.Strings("unknown", unknown))
	if len(chosen) == 0 {
		log.Debug("Server.CollectNodeDiagnostics: no known collector named")
		return &pb.CollectNodeDiagnosticsResponse{
			Success:             false,
			Message:             "no known collector was named",
			UnknownCollectors:   unknown,
			AvailableCollectors: diagCollectorNames(),
		}, nil
	}

	targets, err := s.diagTargets(ctx, req.GetNodes())
	if err != nil {
		log.Debug("Server.CollectNodeDiagnostics: resolving target nodes failed", zap.Strings("nodes", req.GetNodes()), zap.Error(err))
		return nil, err
	}
	if len(targets) == 0 {
		log.Debug("Server.CollectNodeDiagnostics: no registered node matched", zap.Strings("nodes", req.GetNodes()))
		return &pb.CollectNodeDiagnosticsResponse{
			Success:             false,
			Message:             "no registered node matched the request",
			UnknownCollectors:   unknown,
			AvailableCollectors: diagCollectorNames(),
		}, nil
	}

	since := clampInt(int(req.GetSinceMinutes()), defaultDiagWindowMinutes, 1, maxDiagWindowMinutes)
	maxLines := clampInt(int(req.GetMaxLines()), defaultDiagMaxLines, 1, maxDiagMaxLines)
	log.Debug("Server.CollectNodeDiagnostics: collecting", zap.Int("targets", len(targets)),
		zap.Int("since_minutes", since), zap.Int("max_lines", maxLines))

	hosts := make([]string, 0, len(targets))
	byHost := make(map[string]*pb.NodeDiagnostics, len(targets))
	out := make([]*pb.NodeDiagnostics, 0, len(targets))
	for _, t := range targets {
		nd := &pb.NodeDiagnostics{Node: t.name, Address: t.host, Reachable: true}
		byHost[t.host] = nd
		hosts = append(hosts, t.host)
		out = append(out, nd)
	}

	for _, c := range chosen {
		s.runDiagCollector(ctx, c, hosts, byHost, since, maxLines)
	}
	log.Debug("Server.CollectNodeDiagnostics: collected", zap.Int("nodes", len(out)), zap.Int("collectors", len(chosen)))

	return &pb.CollectNodeDiagnosticsResponse{
		Success:             true,
		Message:             "OK",
		Nodes:               out,
		UnknownCollectors:   unknown,
		AvailableCollectors: diagCollectorNames(),
	}, nil
}

// runDiagCollector executes one collector across every host and files each
// host's lines under its node.
//
// An Exec error is not a collector error: it means the whole fan-out failed
// before any host answered, and recording it on every node is the truth. A
// host missing from the result answered nothing, which is also worth saying —
// silence and success look identical otherwise.
func (s *Server) runDiagCollector(ctx context.Context, c diagCollector, hosts []string,
	byHost map[string]*pb.NodeDiagnostics, since, maxLines int) {

	log := s.ctrl.traceLogger()
	cmd := c.cmd(since, maxLines)
	log.Debug("Server.runDiagCollector: running collector", zap.String("collector", c.Name),
		zap.Strings("hosts", hosts), zap.Int("since_minutes", since), zap.Int("max_lines", maxLines))
	start := time.Now()
	res, err := s.ctrl.deployment.Exec(ctx, hosts, cmd,
		deployment.WithExecTimeout(diagCollectorTimeout))
	if err != nil {
		log.Debug("Server.runDiagCollector: exec failed for every host", zap.String("collector", c.Name),
			zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
		for _, nd := range byHost {
			nd.Collectors = append(nd.Collectors, &pb.NodeCollectorOutput{
				Collector: c.Name, Command: cmd, Ok: false,
				Error: fmt.Sprintf("could not run this collector: %v", err),
			})
		}
		return
	}

	if ce := log.Check(zap.DebugLevel, "Server.runDiagCollector: exec finished"); ce != nil {
		ce.Write(zap.String("collector", c.Name), zap.Strings("failed_hosts", res.FailedHosts()),
			zap.Duration("took", time.Since(start)))
	}
	answered := make(map[string]bool, len(hosts))
	// Keyed by host, and the key is what identifies it: HostResult.Host is set
	// by the current implementation and the map key is set by definition.
	for host, h := range res.Hosts {
		nd, ok := byHost[host]
		if !ok {
			log.Debug("Server.runDiagCollector: result for unrequested host ignored", zap.String("host", host))
			continue
		}
		answered[host] = true
		lines, truncated := diagLines(h.Output, maxLines)
		log.Debug("Server.runDiagCollector: host answered", zap.String("collector", c.Name), zap.String("host", host),
			zap.Bool("success", h.Success), zap.Int("lines", len(lines)), zap.Bool("truncated", truncated))
		co := &pb.NodeCollectorOutput{
			Collector: c.Name, Command: cmd,
			Lines: lines, Ok: h.Success, Truncated: truncated,
		}
		if h.Error != nil {
			co.Error = h.Error.Error()
		}
		// A node that answers nothing at all is not reachable for the purpose
		// of this call, whatever the registry last recorded about it.
		if !h.Success && len(lines) == 0 {
			log.Debug("Server.runDiagCollector: host unreachable for this call", zap.String("host", host))
			nd.Reachable = false
			if nd.Error == "" && co.Error != "" {
				nd.Error = co.Error
			}
		}
		nd.Collectors = append(nd.Collectors, co)
	}
	for h, nd := range byHost {
		if answered[h] {
			continue
		}
		log.Debug("Server.runDiagCollector: host did not answer", zap.String("collector", c.Name), zap.String("host", h))
		nd.Collectors = append(nd.Collectors, &pb.NodeCollectorOutput{
			Collector: c.Name, Command: cmd, Ok: false,
			Error: "the node did not answer this collector",
		})
	}
}

// diagTarget pairs the node's registered name with the address the controller
// reaches it on, because the response is read by a person who thinks in names
// and the fan-out is done by address.
type diagTarget struct {
	name string
	host string
}

// diagTargets resolves the requested node names against the registry.
//
// Only registered nodes are collected from. That is not merely tidy: it is
// what keeps a request from naming an arbitrary host for the controller to
// ssh into, and it means an unknown name comes back as "no registered node
// matched" rather than as a connection timeout forty seconds later.
func (s *Server) diagTargets(ctx context.Context, want []string) ([]diagTarget, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.diagTargets: resolving nodes", zap.Strings("want", want))
	nodes, err := s.ctrl.nodes.ListNodes(ctx)
	if err != nil {
		log.Debug("Server.diagTargets: listing nodes failed", zap.Error(err))
		return nil, err
	}
	keep := make(map[string]bool, len(want))
	for _, w := range want {
		if w = strings.TrimSpace(w); w != "" {
			keep[strings.ToLower(w)] = true
		}
	}
	out := make([]diagTarget, 0, len(nodes))
	seen := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if len(keep) > 0 &&
			!keep[strings.ToLower(n.Name)] &&
			!keep[strings.ToLower(n.Hostname)] &&
			!keep[strings.ToLower(n.Address)] {
			log.Debug("Server.diagTargets: node not requested", zap.String("node", n.Name))
			continue
		}
		host := n.Address
		if host == "" {
			host = n.Hostname
		}
		if host == "" {
			host = n.Name
		}
		// Two registry entries can resolve to one address; collecting twice
		// would double every finding built from this.
		if seen[host] {
			log.Debug("Server.diagTargets: address already targeted, skipped", zap.String("node", n.Name), zap.String("host", host))
			continue
		}
		seen[host] = true
		log.Debug("Server.diagTargets: targeting node", zap.String("node", n.Name), zap.String("host", host))
		out = append(out, diagTarget{name: n.Name, host: host})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	log.Debug("Server.diagTargets: resolved", zap.Int("count", len(out)))
	return out, nil
}

// resolveDiagCollectors turns the requested names into table entries, keeping
// table order so two callers asking for the same set get the same order.
func resolveDiagCollectors(want []string) (chosen []diagCollector, unknown []string) {
	if len(want) == 0 {
		zap.L().Debug("resolveDiagCollectors: none named, using all", zap.Int("count", len(diagCollectors)))
		return diagCollectors, nil
	}
	asked := make(map[string]bool, len(want))
	for _, w := range want {
		w = strings.ToLower(strings.TrimSpace(w))
		if w == "" {
			continue
		}
		if _, ok := diagCollectorByName[w]; ok {
			asked[w] = true
		} else {
			zap.L().Debug("resolveDiagCollectors: unknown collector", zap.String("collector", w))
			unknown = append(unknown, w)
		}
	}
	for _, c := range diagCollectors {
		if asked[c.Name] {
			chosen = append(chosen, c)
		}
	}
	return chosen, unknown
}

// diagLines splits a collector's output and applies the cap.
//
// The command asked for one line more than the cap, so len > max is exactly
// the signal that there was more: drop the extra oldest line and say so.
func diagLines(output string, max int) (lines []string, truncated bool) {
	output = strings.TrimRight(output, "\n")
	if output == "" {
		return nil, false
	}
	lines = strings.Split(output, "\n")
	if len(lines) > max {
		zap.L().Debug("diagLines: output over the cap, keeping the tail", zap.Int("lines", len(lines)), zap.Int("max_lines", max))
		lines = lines[len(lines)-max:]
		truncated = true
	}
	return lines, truncated
}

func clampInt(v, def, lo, hi int) int {
	if v <= 0 {
		v = def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
