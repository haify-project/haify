package controller

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/alert"
)

// The cluster as the last health poll saw it.
//
// The dashboard and the resource list both need every resource's health, and
// asking the nodes for it on each page view would cost an SSH round trip per
// resource per viewer. The poll already gathers it every few seconds for the
// alerts, so this keeps the latest reading and the per-resource figures
// derived from it — state, where the problem is, write rate — for the API to
// serve from memory.

// Resource states, as the API names them.
const (
	stateHealthy   = "healthy"
	stateResyncing = "resyncing"
	stateDegraded  = "degraded"
	stateNoQuorum  = "no_quorum"
	stateUnknown   = "unknown"
)

// resyncStates are the replication states of a replica being caught up.
var resyncStates = map[string]bool{
	"SyncSource": true, "SyncTarget": true, "PausedSyncS": true, "PausedSyncT": true,
	"StartingSyncS": true, "StartingSyncT": true, "WFBitMapS": true, "WFBitMapT": true, "WFSyncUUID": true,
}

// replicaView is one member of a resource as the view keeps it.
type replicaView struct {
	Node  string
	State string // "ok", "resyncing", "bad" or "tiebreaker"
	Role  string
}

// resourceView is one resource's health from the last poll.
type resourceView struct {
	State          string
	Summary        string
	Replicas       []replicaView
	OutOfSyncBytes uint64
	// WriteBps is the write rate over the last interval, -1 when unmeasured.
	WriteBps float64
	ReadBps  float64
	// Links and TLSLinks count the answering node's connections to its peers.
	Links, TLSLinks int
}

// counterReading is a DRBD counter as one poll read it.
type counterReading struct {
	node             string
	writtenKiB, read uint64
	at               time.Time
}

type clusterView struct {
	log *zap.Logger
	now func() time.Time

	mu        sync.RWMutex
	at        time.Time
	resources map[string]resourceView
	nodes     []alert.NodeStatusInfo
	nodesOK   bool
	pools     []alert.PoolStatusInfo
	poolsOK   bool
	firing    []alert.FiringCondition
	counters  map[string]counterReading
	// writeBps and readBps are the cluster's totals, -1 before two polls.
	writeBps, readBps float64
}

func newClusterView(log *zap.Logger) *clusterView {
	if log == nil {
		log = zap.L()
	}
	return &clusterView{log: log, now: time.Now, resources: map[string]resourceView{},
		counters: map[string]counterReading{}, writeBps: -1, readBps: -1}
}

// Observed implements alert.Observer.
func (v *clusterView) Observed(obs alert.Observation) {
	now := v.now()
	v.mu.Lock()
	defer v.mu.Unlock()
	v.firing = obs.Firing
	if obs.Nodes.Enabled && obs.Nodes.OK {
		v.nodes, v.nodesOK = obs.Nodes.Items, true
	}
	if obs.Pools.Enabled && obs.Pools.OK {
		v.pools, v.poolsOK = obs.Pools.Items, true
	}
	if !obs.Resources.Enabled || !obs.Resources.OK {
		v.log.Debug("clusterView.Observed: no resource reading this poll", zap.Bool("enabled", obs.Resources.Enabled),
			zap.Bool("ok", obs.Resources.OK))
		return
	}
	resources := make(map[string]resourceView, len(obs.Resources.Items))
	counters := make(map[string]counterReading, len(obs.Resources.Items))
	var writeTotal, readTotal float64
	measured := 0
	for _, item := range obs.Resources.Items {
		rv := classifyResource(item)
		rv.WriteBps, rv.ReadBps = -1, -1
		if cur, ok := localCounters(item, now); ok {
			counters[item.Name] = cur
			if prev, had := v.counters[item.Name]; had && prev.node == cur.node &&
				cur.writtenKiB >= prev.writtenKiB && cur.read >= prev.read {
				if dt := cur.at.Sub(prev.at).Seconds(); dt >= 1 {
					rv.WriteBps = float64(cur.writtenKiB-prev.writtenKiB) * 1024 / dt
					rv.ReadBps = float64(cur.read-prev.read) * 1024 / dt
					writeTotal += rv.WriteBps
					readTotal += rv.ReadBps
					measured++
				}
			}
		}
		resources[item.Name] = rv
	}
	v.resources, v.counters, v.at = resources, counters, now
	if measured > 0 {
		v.writeBps, v.readBps = writeTotal, readTotal
	}
	v.log.Debug("clusterView.Observed: updated", zap.Int("resources", len(resources)), zap.Int("rates_measured", measured),
		zap.Float64("write_bps", v.writeBps), zap.Float64("read_bps", v.readBps))
}

// localCounters is the answering node's write and read counters.
func localCounters(item alert.ResourceStatusInfo, now time.Time) (counterReading, bool) {
	for node, st := range item.NodeStates {
		if st.WrittenKiB != nil {
			r := counterReading{node: node, writtenKiB: *st.WrittenKiB, at: now}
			if st.ReadKiB != nil {
				r.read = *st.ReadKiB
			}
			return r, true
		}
	}
	return counterReading{}, false
}

// classifyResource sorts one resource into the worst state any of its
// replicas is in, and says where: no quorum over degraded over resyncing.
func classifyResource(item alert.ResourceStatusInfo) resourceView {
	rv := resourceView{State: stateUnknown}
	if len(item.NodeStates) == 0 {
		rv.Summary = "no status"
		return rv
	}
	nodes := make([]string, 0, len(item.NodeStates))
	for n := range item.NodeStates {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	var noQuorum, degraded, resyncing bool
	var notes []string
	primary := ""
	for _, n := range nodes {
		st := item.NodeStates[n]
		rep := replicaView{Node: n, State: "ok", Role: st.Role}
		if st.Role == "Primary" {
			primary = n
		}
		// Every peer reports what differs from the answering node; the most any
		// one replica is behind is what is at risk.
		rv.OutOfSyncBytes = max(rv.OutOfSyncBytes, st.OutOfSyncKiB*1024)
		if st.Connection != "" {
			rv.Links++
			if st.TLS {
				rv.TLSLinks++
			}
		}
		switch {
		case resyncStates[st.ReplicationState]:
			rep.State = "resyncing"
			resyncing = true
			if st.SyncPercent != nil {
				notes = append(notes, n+" "+formatPercent(*st.SyncPercent))
			} else {
				notes = append(notes, n+" resyncing")
			}
		case st.ExpectedDiskless && st.DiskState == "Diskless" && st.Connection != "StandAlone":
			rep.State = "tiebreaker"
		default:
			if bad := replicaProblem(st); bad != "" {
				rep.State = "bad"
				degraded = true
				notes = append(notes, n+" "+bad)
			}
		}
		if st.Quorum != nil && !*st.Quorum {
			noQuorum = true
		}
		rv.Replicas = append(rv.Replicas, rep)
	}
	switch {
	case noQuorum:
		rv.State = stateNoQuorum
	case degraded:
		rv.State = stateDegraded
	case resyncing:
		rv.State = stateResyncing
	default:
		rv.State = stateHealthy
	}
	switch {
	case len(notes) > 0:
		rv.Summary = strings.Join(notes, ", ")
	case primary != "":
		rv.Summary = "primary " + primary
	default:
		rv.Summary = "no primary"
	}
	return rv
}

// replicaProblem names what is wrong with a replica in a word or two, or ""
// when nothing is.
func replicaProblem(st alert.NodeStateInfo) string {
	if st.Connection != "" && !strings.EqualFold(st.Connection, "Connected") {
		return st.Connection
	}
	switch st.DiskState {
	case "Failed", "Detached", "Inconsistent", "Outdated":
		return st.DiskState
	case "Diskless":
		if !st.ExpectedDiskless {
			return "Diskless"
		}
	}
	switch st.ReplicationState {
	case "StandAlone", "Disconnecting", "Unconnected":
		return st.ReplicationState
	}
	return ""
}

func formatPercent(p float64) string {
	return fmt.Sprintf("%.0f%%", p)
}

// resource returns one resource's view and whether the last poll had it.
func (v *clusterView) resource(name string) (resourceView, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	rv, ok := v.resources[name]
	return rv, ok
}

// snapshot is a consistent copy of everything the view holds.
type viewSnapshot struct {
	At                time.Time
	Resources         map[string]resourceView
	Nodes             []alert.NodeStatusInfo
	NodesOK           bool
	Pools             []alert.PoolStatusInfo
	PoolsOK           bool
	Firing            []alert.FiringCondition
	WriteBps, ReadBps float64
}

func (v *clusterView) snapshot() viewSnapshot {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return viewSnapshot{At: v.at, Resources: v.resources, Nodes: v.nodes, NodesOK: v.nodesOK, Pools: v.pools,
		PoolsOK: v.poolsOK, Firing: v.firing, WriteBps: v.writeBps, ReadBps: v.readBps}
}
