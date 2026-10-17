package controller

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/alert"
	"github.com/haify-project/haify/pkg/event"
)

// clusterSummary counts the cluster for the dashboard. Health comes from the
// last health poll (cluster_view.go), everything else from the controller's
// own records and event history; nothing here talks to a node.
func (c *Controller) clusterSummary(ctx context.Context) *haifypb.GetClusterSummaryResponse {
	log := c.traceLogger()
	now := time.Now()
	out := &haifypb.GetClusterSummaryResponse{Success: true, ControlPlane: c.controlPlaneNode(ctx)}
	var snap viewSnapshot
	if c.view != nil {
		snap = c.view.snapshot()
		if !snap.At.IsZero() {
			out.ObservedUnix = snap.At.Unix()
		}
	}
	out.Resources, out.Replication = c.summaryResources(ctx, snap)
	out.Nodes = c.summaryNodes(ctx)
	out.Capacity, out.PoolFullness = summaryCapacity(snap)
	out.Gateways = c.summaryGateways(ctx, snap)
	out.Protection = c.summaryProtection(ctx, now)
	out.Today, out.EventsByHour = c.summaryEvents(now)
	out.Attention = attentionItems(snap.Firing)
	c.summaryInspection(ctx, out.Today)
	log.Debug("Controller.clusterSummary: built", zap.Int32("resources", out.Resources.Total),
		zap.Int32("nodes", out.Nodes.Total), zap.Int32("pools", out.Capacity.Pools), zap.Int("attention", len(out.Attention)),
		zap.Duration("took", time.Since(now)))
	return out
}

func (c *Controller) summaryResources(ctx context.Context, snap viewSnapshot) (*haifypb.ResourceCounts, *haifypb.ReplicationCounts) {
	rc, rep := &haifypb.ResourceCounts{}, &haifypb.ReplicationCounts{}
	if c.db == nil {
		return rc, rep
	}
	records, err := c.db.ListResources(ctx)
	if err != nil {
		c.traceLogger().Debug("Controller.summaryResources: listing failed", zap.Error(err))
		return rc, rep
	}
	for _, r := range records {
		rc.Total++
		if vols, err := c.db.ListVolumes(ctx, r.Name); err == nil {
			rc.Volumes += int32(len(vols))
		}
		tiebreakers := splitCSV(r.DisklessNodes)
		clients := splitCSV(r.DisklessClients)
		rep.Tiebreakers += int32(len(tiebreakers))
		rep.DisklessClients += int32(len(clients))
		rep.Replicas += int32(max(len(splitCSV(r.Nodes))-len(tiebreakers)-len(clients), 0))
		if r.WANMode {
			rep.Offsite++
		}
		rv, ok := snap.Resources[r.Name]
		if !ok {
			rc.Unknown++
			continue
		}
		rep.EncryptedLinks += int32(rv.TLSLinks)
		rep.OutOfSyncBytes += rv.OutOfSyncBytes
		switch rv.State {
		case stateHealthy:
			rc.Healthy++
		case stateResyncing:
			rc.Resyncing++
		case stateDegraded:
			rc.Degraded++
		case stateNoQuorum:
			rc.NoQuorum++
		default:
			rc.Unknown++
		}
	}
	if snap.WriteBps >= 0 && !snap.At.IsZero() {
		w, r := snap.WriteBps, snap.ReadBps
		rep.WriteBytesPerSec, rep.ReadBytesPerSec = &w, &r
	}
	return rc, rep
}

func (c *Controller) summaryNodes(ctx context.Context) *haifypb.NodeCounts {
	nc := &haifypb.NodeCounts{}
	if c.nodes == nil {
		return nc
	}
	nodes, _ := c.nodes.ListNodes(ctx)
	for _, n := range nodes {
		if n.State == NodeStateEvicted {
			continue
		}
		nc.Total++
		switch n.State {
		case NodeStateOffline:
			nc.Offline++
		case NodeStateMaintenance:
			nc.Draining++
			nc.Online++
		default:
			nc.Online++
		}
	}
	return nc
}

// summaryCapacity adds up the pools and sorts them into ten buckets of how
// full they are.
func summaryCapacity(snap viewSnapshot) (*haifypb.CapacityCounts, []int32) {
	cc := &haifypb.CapacityCounts{}
	buckets := make([]int32, 10)
	for _, p := range snap.Pools {
		used, total := poolUsage(p)
		cc.UsedBytes += used
		cc.TotalBytes += total
		cc.Pools++
		pct := 0.0
		if total > 0 {
			pct = float64(used) / float64(total) * 100
		}
		switch {
		case pct >= 90:
			cc.PoolsAbove_90++
		case pct >= 80:
			cc.Pools_80To_90++
		}
		buckets[min(int(pct/10), 9)]++
	}
	return cc, buckets
}

// summaryGateways counts gateways, and as serving those whose resource has a
// Primary in the last poll.
func (c *Controller) summaryGateways(ctx context.Context, snap viewSnapshot) *haifypb.GatewayCounts {
	gc := &haifypb.GatewayCounts{}
	if c.db == nil {
		return gc
	}
	gws, _ := c.db.ListGateways(ctx)
	for _, g := range gws {
		gc.Total++
		for _, rep := range snap.Resources[g.Resource].Replicas {
			if rep.Role == "Primary" {
				gc.Serving++
				break
			}
		}
	}
	return gc
}

func (c *Controller) summaryProtection(ctx context.Context, now time.Time) *haifypb.ProtectionCounts {
	pc := &haifypb.ProtectionCounts{}
	if c.db == nil {
		return pc
	}
	day0 := startOfDay(now)
	covered := map[string]bool{}
	if scheds, err := c.db.ListSnapshotSchedules(ctx); err == nil {
		for _, s := range scheds {
			pc.SnapshotSchedules++
			covered[s.Resource] = true
			if !s.LastRun.Before(day0) {
				pc.SchedulesRunToday++
			}
		}
	}
	if scheds, err := c.db.ListBackupSchedules(ctx); err == nil {
		for _, s := range scheds {
			covered[s.Resource] = true
			if !s.LastRun.Before(day0) {
				pc.SchedulesRunToday++
			}
		}
	}
	pc.Covered = int32(len(covered))
	if backups, err := c.db.ListBackups(ctx, "", ""); err == nil {
		for _, b := range backups {
			if b.StartedAt.Before(day0) {
				continue
			}
			pc.BackupsToday++
			if strings.EqualFold(b.State, "failed") {
				pc.BackupsFailedToday++
			}
		}
	}
	return pc
}

// summaryEvents counts today's events by hour and by type.
func (c *Controller) summaryEvents(now time.Time) (*haifypb.TodayCounts, []*haifypb.HourEvents) {
	today := &haifypb.TodayCounts{}
	hours := make([]*haifypb.HourEvents, 24)
	for i := range hours {
		hours[i] = &haifypb.HourEvents{}
	}
	if c.events == nil {
		return today, hours
	}
	day0 := startOfDay(now)
	lost := map[string]bool{}
	for _, e := range c.events.Recent(event.Filter{}, 0, 0) {
		if e.Timestamp.Before(day0) {
			continue
		}
		today.Events++
		h := hours[e.Timestamp.In(now.Location()).Hour()]
		switch e.Severity {
		case event.SeverityCritical:
			today.Critical++
			h.Critical++
		case event.SeverityWarning:
			today.Warning++
			h.Warning++
		default:
			h.Info++
		}
		if e.Status == event.StatusResolved {
			continue
		}
		switch e.Type {
		case event.TypeResourceFailover:
			today.Failovers++
		case event.TypeNodeUnreachable:
			// A node that drops out three times is one node lost.
			lost[e.Node] = true
		case event.TypeReplicaMoved, event.TypeVolumeMoved:
			today.ReplicasMoved++
		case event.TypePoolExtended:
			today.PoolsExtended++
		}
	}
	today.NodesLost = int32(len(lost))
	return today, hours
}

// attentionItems groups what the health monitor has raised and not yet
// resolved by type, worst first. The monitor's own list is the source, not
// the event history: an event raised before a controller restart is never
// resolved in that history if its condition ended while no controller ran.
func attentionItems(firing []alert.FiringCondition) []*haifypb.AttentionItem {
	byType := map[string]*haifypb.AttentionItem{}
	for _, e := range firing {
		if e.Severity == string(event.SeverityInfo) {
			continue
		}
		it, ok := byType[e.Type]
		if !ok {
			it = &haifypb.AttentionItem{Type: e.Type, Severity: e.Severity}
			byType[e.Type] = it
		}
		if e.Severity == string(event.SeverityCritical) {
			it.Severity = e.Severity
		}
		it.Count++
		subject := e.Resource
		if subject == "" {
			subject = e.Node
		}
		if subject != "" && len(it.Subjects) < 3 {
			it.Subjects = append(it.Subjects, subject)
		}
	}
	out := make([]*haifypb.AttentionItem, 0, len(byType))
	for _, it := range byType {
		sort.Strings(it.Subjects)
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Severity == "critical") != (out[j].Severity == "critical") {
			return out[i].Severity == "critical"
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// summaryInspection fills in the newest inspection's result.
func (c *Controller) summaryInspection(ctx context.Context, today *haifypb.TodayCounts) {
	if c.inspections == nil {
		return
	}
	r, err := c.inspections.Get(ctx, "latest")
	if err != nil || r == nil {
		return
	}
	today.InspectionPassed = int32(r.Summary.Pass)
	today.InspectionTotal = int32(r.Summary.Pass + r.Summary.Warn + r.Summary.Fail + r.Summary.Error)
}

// controlPlaneNode is the registered name of the node this controller runs
// on, or its hostname when no node matches.
func (c *Controller) controlPlaneNode(ctx context.Context) string {
	host, _ := os.Hostname()
	if c.nodes == nil {
		return host
	}
	nodes, _ := c.nodes.ListNodes(ctx)
	short := strings.SplitN(host, ".", 2)[0]
	for _, n := range nodes {
		if n.Name == host || n.Hostname == host || n.Name == short || strings.SplitN(n.Hostname, ".", 2)[0] == short {
			return n.Name
		}
	}
	return host
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
