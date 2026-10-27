package triage

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// ── 11. a service failing on every node ─────────────────────────────────

// The two alerts that name this are read here, and only while they are still
// raised. An alert is raised and later resolved; reporting every firing event
// in the window would show an operator a loop that ended an hour ago as if it
// were the problem in front of them.

// outstanding returns, for each condition of the given event type, the latest
// event on it when that is still a firing one. A condition is a resource and
// the node field the event carries: both of these alerts are about one subject,
// and one subject recovering says nothing about another.
func outstanding(events []Event, eventType string) []Event {
	latest := map[string]Event{}
	for _, e := range events {
		if !strings.EqualFold(e.Type, eventType) {
			continue
		}
		key := e.Resource + "|" + e.Node
		cur, seen := latest[key]
		if !seen || e.At.After(cur.At) || (e.At.Equal(cur.At) && e.ID > cur.ID) {
			latest[key] = e
		}
	}
	var out []Event
	for _, e := range latest {
		if strings.EqualFold(e.Status, "firing") {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Resource != out[j].Resource {
			return out[i].Resource < out[j].Resource
		}
		return out[i].Node < out[j].Node
	})
	zap.L().Debug("outstanding: conditions still raised", zap.String("type", eventType), zap.Int("count", len(out)))
	return out
}

// matchFailoverLoop reads resource.failover_loop: the Primary of one resource
// has moved several times in a short while.
//
// Each move is the cluster doing its job, which is why none of them is a
// finding. What they add up to is a service that fails wherever it runs, and
// the one thing that does not help is moving the role again.
func matchFailoverLoop(in Input) []Finding {
	var out []Finding
	for _, e := range outstanding(in.Events, "resource.failover_loop") {
		var nodes []string
		for n := range strings.SplitSeq(e.Details["nodes"], ",") {
			if n != "" {
				nodes = append(nodes, n)
			}
		}
		count, _ := strconv.Atoi(e.Details["failovers"])
		count = max(count, 1)
		zap.L().Debug("matchFailoverLoop: verdict, failover loop raised",
			zap.String("resource", e.Resource), zap.Int("failovers", count), zap.Strings("nodes", nodes))
		res := orDefault(e.Resource, "<resource>")
		out = append(out, Finding{
			ID:       "failover-loop",
			Title:    fmt.Sprintf("%s's Primary keeps moving between nodes — the service fails wherever it runs", orDefault(e.Resource, "a resource")),
			Severity: SeverityCritical,
			Count:    count,
			Nodes:    nodes,
			Resource: e.Resource,
			Known:    true,
			Cause: "The Primary moved several times in a few minutes. A node that fails now and then does " +
				"not do that; a service that fails on every node does: it fails where the chain runs, the " +
				"cluster moves the chain, it fails on the next node for the same reason, and the chain " +
				"moves back. The reason is almost always something every node shares, such as the data " +
				"on the volume, since all of them read the same copy.",
			Advice: []string{
				"See which unit of the chain is failing and how often it restarted: haify ha status " + res,
				"Read why it fails, on the node that is Primary now (the unit is named above): journalctl -u <unit> --since '-30min' -o short-iso | tail -60",
				"Check the filesystem under the volume for errors; a damaged one corrupts what the service reads: journalctl -k --since '-1day' | grep -E 'EXT4-fs (error|warning)'",
				"If the data is what is damaged, the last good copy is a backup, not another node: haify backup list",
			},
			Caution: "Do not move the role again to get out of this (haify ha evict): every node reads the same " +
				"volume, so the next one fails the same way, and each move stops and starts everything the " +
				"chain runs. Copy the damaged files somewhere safe before repairing anything in place.",
			Evidence: []Evidence{{Source: "event", Node: e.Node, At: stamp(e.At), Line: e.Type + ": " + e.Message}},
		})
	}
	return out
}

// matchServiceFailing reads ha.service_failing: a service of an HA chain is not
// running, or keeps restarting, on the node that holds the chain.
//
// It is the half of the failure DRBD cannot show. The replicas are UpToDate, a
// Primary exists, the mount and the service IP are up, and nothing is served.
func matchServiceFailing(in Input) []Finding {
	var out []Finding
	for _, e := range outstanding(in.Events, "ha.service_failing") {
		unit := orDefault(e.Details["unit"], e.Node)
		host := e.Details["node"]
		var nodes []string
		if host != "" {
			nodes = []string{host}
		}
		zap.L().Debug("matchServiceFailing: verdict, service failing",
			zap.String("resource", e.Resource), zap.String("unit", unit), zap.String("node", host))
		out = append(out, Finding{
			ID:       "ha-service-failing",
			Title:    fmt.Sprintf("%s of %s is failing on %s — nothing is being served", unit, orDefault(e.Resource, "a resource"), orDefault(host, "its node")),
			Severity: SeverityCritical,
			Count:    1,
			Nodes:    nodes,
			Resource: e.Resource,
			Known:    true,
			Cause: "A unit of the HA chain is stopped, or crashes on every start. DRBD and the resource checks " +
				"show nothing wrong because the replicas stay UpToDate and a Primary exists; only the unit's own " +
				"state shows it. systemd restarts a crashing unit within seconds, so it can look running for as " +
				"long as it is looked at once.",
			Advice: []string{
				"Read why the unit stops, on " + orDefault(host, "the node that holds the chain") + ": journalctl -u " + unit + " --since '-30min' -o short-iso | tail -60",
				"See its state and how often systemd restarted it: systemctl status " + unit,
				"See where the chain runs and what else is in it: haify ha status " + orDefault(e.Resource, "<resource>"),
			},
			Caution: "If the unit fails because of the data it reads, moving the Primary does not help: the volume " +
				"is the same on every node, and the next one fails the same way.",
			Evidence: []Evidence{{Source: "event", Node: host, At: stamp(e.At), Line: e.Type + ": " + e.Message}},
		})
	}
	return out
}
