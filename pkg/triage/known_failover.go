package triage

import (
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// ── 6. the promoter could not start its target ──────────────────────────

func matchPromoterStartFailed(in Input) []Finding {
	ev, nodes := nodeLinesMatching(in, func(l string) bool {
		ll := strings.ToLower(l)
		if !strings.Contains(ll, "drbd-services@") && !strings.Contains(ll, "drbd-promote@") &&
			!strings.Contains(ll, "service-ip@") {
			return false
		}
		return strings.Contains(ll, "failed with result") ||
			strings.Contains(ll, "failed to start") ||
			strings.Contains(ll, "start request repeated too quickly")
	}, "promoter_journal", "reactor_journal", "failed_units")
	if len(ev) == 0 {
		zap.L().Debug("matchPromoterStartFailed: verdict, no promoter start failures")
		return nil
	}
	zap.L().Debug("matchPromoterStartFailed: verdict, promoter start failed", zap.Int("count", len(ev)), zap.Strings("nodes", nodes))
	return []Finding{{
		ID:       "promoter-start-failed",
		Title:    "a drbd-reactor promoter could not start its target",
		Severity: SeverityError,
		Count:    len(ev),
		Nodes:    nodes,
		Known:    true,
		Cause: "The promoter runs its start list in order — mount, then service IP, then the " +
			"services — and stops at the first step that fails, leaving the resource unpromoted. " +
			"The step that failed is named in the journal immediately before this line; the " +
			"failure people usually chase is the last line, which is only the target giving up.",
		Advice: []string{
			"Find the first failure, not the last: journalctl -u 'drbd-services@*' -u 'drbd-promote@*' -u 'service-ip@*' --since '-30min' -o short-iso | head -60",
			"Check the mount step's device exists and the filesystem is sound: lsblk /dev/drbd* && drbdsetup status",
			"Check the service IP is not already held elsewhere on the network: ip addr show | grep <vip>",
			"After fixing the failing step, let reactor try again: drbd-reactorctl status && systemctl reset-failed 'drbd-services@*'",
		},
		Evidence: ev,
	}}
}

// ── 7. a failover nobody asked for ──────────────────────────────────────

// matchUnplannedFailover pairs a failover event with the restart that caused it.
//
// Restarting a service the promoter manages is not a restart of that service:
// reactor sees the unit stop, demotes the resource and another node promotes
// it, so the whole target moves and the control plane is gone for seconds.
// That is the single most common self-inflicted outage on this cluster, and it
// is invisible in the events alone — a failover event looks the same whether a
// node died or somebody typed systemctl restart.
//
// The pairing is what makes it identifiable, so this only fires when both
// halves are present within the window. Without the journal line it stays a
// plain failover event, reported by the generic grouping.
func matchUnplannedFailover(in Input) []Finding {
	log := zap.L()
	var failovers []Event
	for _, e := range in.Events {
		if strings.EqualFold(e.Type, "resource.failover") {
			failovers = append(failovers, e)
		}
	}
	if len(failovers) == 0 {
		log.Debug("matchUnplannedFailover: verdict, no failover events")
		return nil
	}
	log.Debug("matchUnplannedFailover: pairing failovers with restarts", zap.Int("failovers", len(failovers)))

	const near = 3 * time.Minute
	var out []Finding
	for _, fo := range failovers {
		var ev []Evidence
		nodes := map[string]bool{}
		for _, n := range in.Nodes {
			for _, cname := range []string{"promoter_journal", "reactor_journal", "haify_journal"} {
				c, ok := n.Collector(cname)
				if !ok {
					continue
				}
				for _, line := range c.Lines {
					if !restartLine(line) {
						continue
					}
					if at, ok := journalTime(line); ok && !fo.At.IsZero() &&
						absDuration(at.Sub(fo.At)) > near {
						continue
					}
					nodes[n.Node] = true
					ev = append(ev, Evidence{Source: n.Node + "/" + cname, Node: n.Node, Line: strings.TrimSpace(line)})
				}
			}
		}
		if len(ev) == 0 {
			log.Debug("matchUnplannedFailover: no restart near this failover", zap.String("resource", fo.Resource),
				zap.String("node", fo.Node), zap.Time("at", fo.At))
			continue
		}
		log.Debug("matchUnplannedFailover: failover follows a managed-service restart", zap.String("resource", fo.Resource),
			zap.String("node", fo.Node), zap.Time("at", fo.At), zap.Int("restart_lines", len(ev)),
			zap.Strings("nodes", sortedKeys(nodes)))
		ev = append([]Evidence{{
			Source: "event", Node: fo.Node, At: stamp(fo.At),
			Line: fo.Type + ": " + fo.Message,
		}}, ev...)
		out = append(out, Finding{
			ID:       "unplanned-failover",
			Title:    fmt.Sprintf("%s failed over right after a managed service was restarted", orDefault(fo.Resource, "a resource")),
			Severity: SeverityError,
			Count:    len(ev),
			Nodes:    sortedKeys(nodes),
			Resource: fo.Resource,
			Known:    true,
			Cause: "Restarting a service that a drbd-reactor promoter manages is not a restart of " +
				"that service. Reactor sees the unit stop, demotes the resource, and another node " +
				"promotes it — the whole target moves and the control plane is unreachable for " +
				"several seconds. In the events this is indistinguishable from a node dying.",
			Advice: []string{
				"To replace a binary without moving the role: install it on every member node first, then move the role deliberately with haify ha evict " + orDefault(fo.Resource, "<resource>"),
				"To take a node out for maintenance: haify ha evict " + orDefault(fo.Resource, "<resource>") + "   (this is the supported way to move a role)",
				"Never systemctl restart a unit listed in the promoter's start list — check first with: drbd-reactorctl status",
			},
			Evidence: ev,
		})
	}
	log.Debug("matchUnplannedFailover: verdict", zap.Int("findings", len(out)))
	return out
}

func restartLine(line string) bool {
	ll := strings.ToLower(line)
	if !strings.Contains(ll, "stopping") && !strings.Contains(ll, "scheduled restart") &&
		!strings.Contains(ll, "deactivated successfully") && !strings.Contains(ll, "stopped") {
		return false
	}
	return strings.Contains(ll, "drbd-services@") || strings.Contains(ll, "haify-controller") ||
		strings.Contains(ll, "haify-ai") || strings.Contains(ll, "drbd-promote@")
}

// journalTime reads the leading timestamp of a `-o short-iso` journal line.
func journalTime(line string) (time.Time, bool) {
	f := strings.Fields(line)
	if len(f) == 0 {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02T15:04:05-0700", "2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, f[0]); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ── 8 & 9. what the events already say plainly ──────────────────────────

func matchNoPrimary(in Input) []Finding {
	var ev []Evidence
	nodes := map[string]bool{}
	resource := ""
	for _, e := range in.Events {
		if !strings.EqualFold(e.Type, "resource.no_primary") || strings.EqualFold(e.Status, "resolved") {
			continue
		}
		resource = orDefault(resource, e.Resource)
		if e.Node != "" {
			nodes[e.Node] = true
		}
		ev = append(ev, Evidence{Source: "event", Node: e.Node, At: stamp(e.At), Line: e.Type + ": " + e.Message})
	}
	if len(ev) == 0 {
		zap.L().Debug("matchNoPrimary: verdict, no open no-primary events")
		return nil
	}
	zap.L().Debug("matchNoPrimary: verdict, resource has no primary", zap.String("resource", resource),
		zap.Int("count", len(ev)), zap.Strings("nodes", sortedKeys(nodes)))
	return []Finding{{
		ID:       "no-primary",
		Title:    fmt.Sprintf("%s has no Primary — nothing can use it right now", orDefault(resource, "a resource")),
		Severity: SeverityCritical,
		Count:    len(ev),
		Nodes:    sortedKeys(nodes),
		Resource: resource,
		Known:    true,
		Cause: "No node holds the Primary role, so the volume is not mounted and whatever depends " +
			"on it is stopped. Either no node can reach quorum, or every candidate refused to " +
			"promote — the promoter's journal says which.",
		Advice: []string{
			"Check quorum before anything else; this report's phantom-peer finding, if present, is the answer: drbdsetup status " + orDefault(resource, "<resource>"),
			"Read why the promotion was refused: journalctl -u 'drbd-promote@*' --since '-30min' -o short-iso | tail -40",
			"If quorum is genuinely lost and you accept the risk, promote deliberately rather than forcing: haify ha evict " + orDefault(resource, "<resource>"),
		},
		Caution: "Do not use drbdadm primary --force to get out of this without first understanding " +
			"why quorum is missing. Forcing a Primary on the wrong side is how a quorum problem " +
			"becomes split brain.",
		Evidence: ev,
	}}
}

func matchNodeUnreachable(in Input) []Finding {
	nodes := map[string]bool{}
	var ev []Evidence
	for _, n := range in.Nodes {
		if n.Reachable {
			continue
		}
		zap.L().Debug("matchNodeUnreachable: node did not answer", zap.String("node", n.Node),
			zap.Bool("has_error", n.Error != ""))
		nodes[n.Node] = true
		line := n.Node + " did not answer the controller"
		if n.Error != "" {
			line += ": " + n.Error
		}
		ev = append(ev, Evidence{Source: "collector", Node: n.Node, Line: line})
	}
	for _, e := range in.Events {
		if strings.EqualFold(e.Type, "node.unreachable") && !strings.EqualFold(e.Status, "resolved") {
			if e.Node != "" {
				nodes[e.Node] = true
			}
			ev = append(ev, Evidence{Source: "event", Node: e.Node, At: stamp(e.At), Line: e.Type + ": " + e.Message})
		}
	}
	if len(ev) == 0 {
		zap.L().Debug("matchNodeUnreachable: verdict, every node answered")
		return nil
	}
	zap.L().Debug("matchNodeUnreachable: verdict, nodes unreachable", zap.Strings("nodes", sortedKeys(nodes)),
		zap.Int("count", len(ev)))
	return []Finding{{
		ID:       "node-unreachable",
		Title:    "the controller cannot reach " + strings.Join(sortedKeys(nodes), ", "),
		Severity: SeverityError,
		Count:    len(ev),
		Nodes:    sortedKeys(nodes),
		Known:    true,
		Cause: "The controller reaches nodes over SSH, so this is one of three things and they " +
			"look identical from here: the node is down, the network to it is down, or its SSH " +
			"key or address changed. Everything else in this report about that node is missing " +
			"rather than clean.",
		Advice: []string{
			"Separate down from unreachable: ping <address> and then ssh <address> true",
			"If SSH is the problem, check the address the controller has for it: haify node list",
			"Remember that a resource's replicas on an unreachable node still hold quorum votes; do not remove the node to 'clean up' while the cluster is degraded.",
		},
		Evidence: ev,
	}}
}
