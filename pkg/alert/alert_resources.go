package alert

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/event"
)

func (m *Monitor) checkResources(ctx context.Context, sc *pollScope, obs *Observation) {
	if m.resources == nil {
		m.log.Debug("Monitor.checkResources: no resource lister, resource checks skipped")
		return
	}
	obs.Resources.Enabled = true

	start := time.Now()
	resources, err := m.resources.GetResourceStatusList(ctx)
	obs.Resources.Duration = time.Since(start)
	m.log.Debug("Monitor.checkResources: listed resources",
		zap.Int("count", len(resources)), zap.Duration("took", obs.Resources.Duration), zap.Error(err))
	if err != nil {
		obs.Resources.Err = err
		m.log.Warn("alert monitor: list resources failed", zap.Error(err))
		return
	}
	obs.Resources.OK = true
	obs.Resources.Items = resources
	sc.resourcesOK = true

	for _, res := range resources {
		sc.live[res.Name] = true
		m.log.Debug("Monitor.checkResources: checking resource",
			zap.String("resource", res.Name), zap.Int("replicas", len(res.NodeStates)),
			zap.Bool("wan_enabled", res.WANEnabled), zap.Bool("idle_without_primary", res.IdleWithoutPrimary))
		m.checkReplicas(res, sc)
		m.checkPrimary(res, sc)
		m.checkWAN(res, sc)
		m.checkOutOfSync(res, sc)
	}

	m.mu.Lock()
	for name := range m.primaries {
		if !sc.live[name] {
			m.log.Debug("Monitor.checkResources: resource gone, forgetting its Primary", zap.String("resource", name))
			delete(m.primaries, name)
			delete(m.lastPrimary, name)
		}
	}
	m.mu.Unlock()
}

// checkReplicas raises one condition per replica whose disk or replication
// state has left the healthy set.
func (m *Monitor) checkReplicas(res ResourceStatusInfo, sc *pollScope) {
	stranded := strandedOutdated(res)
	for node, state := range res.NodeStates {
		degraded, reason := isDegraded(state)
		severity := event.SeverityWarning
		m.log.Debug("Monitor.checkReplicas: checking replica",
			zap.String("resource", res.Name), zap.String("node", node),
			zap.String("disk_state", state.DiskState), zap.String("repl_state", state.ReplicationState),
			zap.String("connection", state.Connection), zap.String("role", state.Role),
			zap.Bool("degraded", degraded), zap.String("reason", reason), zap.Bool("stranded", stranded))
		if stranded && state.DiskState == "Outdated" {
			m.log.Debug("Monitor.checkReplicas: every replica Outdated, raising as critical",
				zap.String("resource", res.Name), zap.String("node", node))
			// Outdated is only "older than its peers" while a peer holds
			// current data. When every replica is Outdated none can be
			// promoted, so nothing serves the resource: say so, and say what
			// decides the way out.
			severity = event.SeverityCritical
			reason = "disk is Outdated and no replica holds current data, so nothing can be promoted and the resource is not served; " +
				"compare the replicas with `drbdadm get-gi " + res.Name + "` (equal current UUIDs mean equal data) and promote one with " +
				"`haify resource primary " + res.Name + " <node> --force`"
		}
		m.level(event.Event{
			Type:     event.TypeResourceDegraded,
			Severity: severity,
			Resource: res.Name,
			Node:     node,
			Details: map[string]string{
				"disk_state": state.DiskState,
				"repl_state": state.ReplicationState,
			},
		}, sc, sourceResources, degraded,
			fmt.Sprintf("resource %s on %s degraded: %s", res.Title(), node, reason),
			fmt.Sprintf("resource %s on %s recovered to normal state", res.Title(), node))
	}
}

// strandedOutdated reports whether the resource has replicas that are all
// Outdated: none reports UpToDate, so no promotion can succeed.
func strandedOutdated(res ResourceStatusInfo) bool {
	outdated := false
	for _, st := range res.NodeStates {
		switch st.DiskState {
		case "Outdated":
			outdated = true
		case "UpToDate", "Consistent", "Inconsistent":
			return false
		}
	}
	return outdated
}

// checkOutOfSync raises one condition per replica that is connected and
// replicating yet holds data that differs from its peer. While a replica is
// disconnected or resyncing the same counter only measures how far it has to
// catch up, which checkReplicas already reports.
func (m *Monitor) checkOutOfSync(res ResourceStatusInfo, sc *pollScope) {
	for node, state := range res.NodeStates {
		active := state.connected() && state.ReplicationState == "Established" && state.OutOfSyncKiB > 0
		m.log.Debug("Monitor.checkOutOfSync: checking replica",
			zap.String("resource", res.Name), zap.String("node", node),
			zap.Uint64("out_of_sync_kib", state.OutOfSyncKiB), zap.Bool("out_of_sync", active))
		m.level(event.Event{
			Type:     event.TypeResourceOutOfSync,
			Severity: event.SeverityWarning,
			Resource: res.Name,
			Node:     node,
			Details:  map[string]string{"out_of_sync_kib": fmt.Sprint(state.OutOfSyncKiB)},
		}, sc, sourceResources, active,
			fmt.Sprintf("resource %s: %d KiB on %s are marked out of sync with its peer — the copies may differ, or the marks may be left over from a verify or an interrupted resync; resource verify %s --resync makes them identical and clears the marks", res.Title(), state.OutOfSyncKiB, node, res.Name),
			fmt.Sprintf("resource %s on %s holds the same data as its peer again", res.Title(), node))
	}
}

// checkPrimary detects the Primary role moving, disappearing, or appearing.
func (m *Monitor) checkPrimary(res ResourceStatusInfo, sc *pollScope) {
	cur := primarySet(res)

	m.mu.Lock()
	prev, known := m.primaries[res.Name]
	m.primaries[res.Name] = cur
	if cur != "" {
		m.lastPrimary[res.Name] = cur
	}
	demoted, expectPrimary := m.lastPrimary[res.Name]
	m.mu.Unlock()

	// Losing the Primary is a LEVEL condition and has to resolve under its own
	// type. It used to be published directly as a one-shot firing event, with a
	// separate resource.promoted event standing in for the clear — but a
	// receiver pairs firing with resolved by type, so nothing ever closed the
	// critical. It stayed outstanding for the life of the process, and survived
	// even deleting the resource, since resolveVanished can only clear
	// conditions that went through here and were recorded in m.firing.
	//
	// The node is deliberately not part of the key: it would change from the
	// demoted node to "" on the very next poll and split one condition into two.
	noPrimary := event.Event{
		Type:     event.TypeResourceNoPrimary,
		Severity: event.SeverityCritical,
		Resource: res.Name,
		Details:  map[string]string{"from": demoted},
	}

	m.mu.Lock()
	recovering := m.firing[noPrimary.Key()]
	m.mu.Unlock()

	// "Nobody reports Primary" only means there is no Primary when every replica
	// was actually asked. The controller reads a resource from whichever node
	// answers first, and that node describes its unreachable peers as one line
	// — "<peer> connection:Connecting" — carrying no role. So a node rebooting
	// back into the cluster produces, for the few seconds its links take to
	// establish, a view in which no node anywhere is Primary.
	//
	// That is measured, not hypothetical: on 2026-09-21 a node rejoining raised
	// this CRITICAL against a resource whose Primary had been serving
	// uninterrupted for two days — same second as the node.unreachable clear,
	// with no role change in the Primary's kernel log and no service restart.
	// A false CRITICAL on "the control plane lost its Primary" is worse than a
	// late true one: it is the alert people stop believing.
	partialView := !fullyConnected(res)
	m.log.Debug("Monitor.checkPrimary: Primary observed",
		zap.String("resource", res.Name), zap.String("primary", cur), zap.String("previous", prev),
		zap.Bool("known", known), zap.String("last_primary", demoted), zap.Bool("expect_primary", expectPrimary),
		zap.Bool("partial_view", partialView), zap.Bool("recovering", recovering),
		zap.Bool("idle_without_primary", res.IdleWithoutPrimary))

	m.level(noPrimary, sc, sourceResources, expectPrimary && cur == "" && !partialView && !res.IdleWithoutPrimary,
		fmt.Sprintf("resource %s has no Primary: %s was demoted and nothing took over", res.Title(), demoted),
		fmt.Sprintf("resource %s has a Primary again on %s", res.Title(), cur))

	// First sighting: record where the Primary is without claiming it just moved
	// there. See the package comment.
	if !known || prev == cur {
		m.log.Debug("Monitor.checkPrimary: no Primary transition to report",
			zap.String("resource", res.Name), zap.Bool("first_sighting", !known))
		return
	}

	switch {
	case cur == "":
		// Reported by the level condition above.
		m.log.Debug("Monitor.checkPrimary: Primary lost, left to the no-Primary condition",
			zap.String("resource", res.Name), zap.String("previous", prev))
	case prev == "":
		// A resource taking a Primary for the first time is worth saying. The
		// same transition arriving as the end of an outage is not: the
		// no_primary condition just resolved and named the same node, and two
		// events for one change is how a feed stops being read.
		if recovering {
			m.log.Debug("Monitor.checkPrimary: promotion ends a no-Primary outage, already reported",
				zap.String("resource", res.Name), zap.String("primary", cur))
			return
		}
		m.log.Debug("Monitor.checkPrimary: resource promoted", zap.String("resource", res.Name), zap.String("primary", cur))
		m.publish(event.Event{
			Type:     event.TypeResourcePromoted,
			Severity: event.SeverityInfo,
			Status:   event.StatusResolved,
			Resource: res.Name,
			Node:     cur,
			Message:  fmt.Sprintf("resource %s promoted on %s", res.Title(), cur),
			Details:  map[string]string{"to": cur},
		})
	case res.IdleWithoutPrimary:
		// Proxmox VE, Kubernetes or OpenStack decides where this resource is
		// Primary, and moving it is their everyday business: a live migration,
		// a pod rescheduled, an instance started elsewhere. Calling that a
		// failover, at warning, would put one for every migration in the feed.
		// A node failure behind a move is reported by its own alerts.
		m.log.Debug("Monitor.checkPrimary: externally managed resource moved, reported as info",
			zap.String("resource", res.Name), zap.String("from", prev), zap.String("to", cur))
		m.publish(event.Event{
			Type:     event.TypeResourcePromoted,
			Severity: event.SeverityInfo,
			Status:   event.StatusResolved,
			Resource: res.Name,
			Node:     cur,
			Message:  fmt.Sprintf("resource %s moved: Primary on %s, was %s", res.Title(), cur, prev),
			Details:  map[string]string{"from": prev, "to": cur},
		})
	default:
		// One-shot, not a level condition: a failover has already happened and
		// there is no later state in which it "clears". Marking it firing would
		// leave it outstanding forever in any receiver that pairs firing with
		// resolved — the wall of un-clearable alerts that makes people stop
		// reading them. no_primary is different and does resolve, via promoted.
		m.log.Debug("Monitor.checkPrimary: Primary moved, reporting failover",
			zap.String("resource", res.Name), zap.String("from", prev), zap.String("to", cur))
		m.publish(event.Event{
			Type:     event.TypeResourceFailover,
			Severity: event.SeverityWarning,
			Status:   event.StatusInfo,
			Resource: res.Name,
			Node:     cur,
			Message:  fmt.Sprintf("resource %s failed over: Primary moved from %s to %s", res.Title(), prev, cur),
			Details:  map[string]string{"from": prev, "to": cur},
		})
	}
}

// checkWAN tracks the cross-site link as its own condition, keyed to the
// resource rather than a node, so a broken link is surfaced even when every
// local replica looks healthy.
func (m *Monitor) checkWAN(res ResourceStatusInfo, sc *pollScope) {
	if !res.WANEnabled {
		m.log.Debug("Monitor.checkWAN: no WAN replication, skipped", zap.String("resource", res.Name))
		return
	}
	m.log.Debug("Monitor.checkWAN: checking WAN link",
		zap.String("resource", res.Name), zap.Bool("wan_healthy", res.WANHealthy), zap.String("message", res.WANMessage))
	m.level(event.Event{
		Type:     event.TypeWANDegraded,
		Severity: event.SeverityCritical,
		Resource: res.Name,
		Node:     "wan",
	}, sc, sourceResources, !res.WANHealthy,
		fmt.Sprintf("resource %s WAN replication degraded: %s", res.Title(), res.WANMessage),
		fmt.Sprintf("resource %s WAN replication recovered", res.Title()))
}

// fullyConnected reports whether every replica in this view was actually
// reachable, which is what makes an absence in the view evidence of anything.
func fullyConnected(res ResourceStatusInfo) bool {
	for _, st := range res.NodeStates {
		if !st.connected() {
			return false
		}
	}
	return true
}

// primarySet renders the nodes currently holding the Primary role as a stable,
// comparable string. Sorting matters: map iteration order would otherwise make
// a dual-primary resource look like it flapped between every pair of polls.
func primarySet(res ResourceStatusInfo) string {
	var primaries []string
	for node, st := range res.NodeStates {
		if strings.EqualFold(st.Role, "Primary") {
			primaries = append(primaries, node)
		}
	}
	sort.Strings(primaries)
	return strings.Join(primaries, ",")
}

// isDegraded reports whether a replica's DRBD state indicates a fault.
func isDegraded(st NodeStateInfo) (bool, string) {
	// A replica whose link is down is a replica that is not protecting
	// anything, and the connection state is the only thing DRBD says about it —
	// there is no disk state to fall through to. StandAlone in particular is
	// not a transient: DRBD sets it deliberately, most often after refusing to
	// resolve a split brain, and it stays until someone intervenes.
	//
	// This is measured. A replica once sat StandAlone after a split-brain
	// disconnect, and nothing fired for a day: the peer carried no entry in the view at all, and a replica that is
	// absent is a replica nobody checks.
	if !st.connected() {
		if strings.EqualFold(st.Connection, "StandAlone") {
			return true, "connection is StandAlone (DRBD disconnected it; a split brain leaves it here until resolved)"
		}
		return true, fmt.Sprintf("connection is %s", st.Connection)
	}
	switch st.DiskState {
	case "Diskless":
		// Expected for tiebreakers and diskless clients; a fault anywhere else,
		// where it means the node lost its local copy.
		if !st.ExpectedDiskless {
			return true, "disk state is Diskless"
		}
	case "Failed", "Detached":
		// Not exempted by ExpectedDiskless: a node with no disk by design still
		// should not be reporting a failed one.
		return true, fmt.Sprintf("disk state is %s", st.DiskState)
	case "Inconsistent":
		// Connected is not recovered. A replica catching up after an outage
		// holds a partial copy until the resync ends, and a second failure in
		// that window leaves one copy of the data. Reporting it recovered the
		// moment the link came back told an operator the redundancy was there
		// while it was still being rebuilt.
		return true, "disk is Inconsistent (resyncing; not yet a complete copy)"
	case "Outdated":
		return true, "disk is Outdated (holds data older than its peers)"
	}
	switch st.ReplicationState {
	case "StandAlone", "Disconnecting", "Unconnected":
		return true, fmt.Sprintf("replication state is %s", st.ReplicationState)
	}
	return false, ""
}
