package alert

import (
	"context"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/wanproxy"
)

// NodeStateInfo is one replica's DRBD state as the monitor needs it.
type NodeStateInfo struct {
	DiskState        string
	ReplicationState string
	// Role is "Primary", "Secondary", or "Unknown". Failover detection is built
	// on this.
	Role string
	// ExpectedDiskless marks a node that is supposed to have no local copy: a
	// quorum tiebreaker, or a diskless client mounting the volume over the DRBD
	// network. For these, "Diskless" is the healthy steady state, not a fault.
	//
	// Without this the monitor fires a warning for every tiebreaker — and
	// resource.auto_tiebreaker defaults to on, so every two-replica resource
	// has one — producing a permanent alert that can never clear. A wall of
	// un-clearable alerts is worse than no alerting: it trains people to
	// ignore the ones that matter.
	ExpectedDiskless bool
	// SyncPercent is resync completion for this replica in 0..100, or nil when
	// the status source carried no completion figure at all.
	//
	// Nil is not zero. Only the structured drbdsetup JSON reports completion;
	// the plain-text fallback parse has no such field, and charting its zero
	// value would paint a fully in-sync replica as one whose resync never
	// started. Nothing here alerts on it — it is carried for Observer.
	SyncPercent *float64
	// Quorum reports whether this replica currently holds DRBD quorum, or nil
	// when DRBD did not say. Nil is the normal case for peers: a node's status
	// only carries its own quorum, and a peer's would be a guess. Carried for
	// Observer; the quorum alerts are the operator's to write from the metric.
	Quorum *bool
	// Connection is the peer's DRBD connection state ("Connected",
	// "Connecting", "StandAlone", ...) as the node that answered the status
	// query sees it. Empty for that answering node itself, which has no
	// connection to describe, and for status sources that do not report one.
	//
	// A peer that is not Connected tells us nothing else about itself: DRBD
	// prints its name and its connection state and stops. Role, DiskState and
	// ReplicationState are all empty for such a peer, and reading those empties
	// as facts is what this field exists to prevent.
	Connection string
	// TLS is whether the connection to this peer is encrypted. Carried for
	// Observer.
	TLS bool
	// OutOfSyncKiB is how much of this replica DRBD knows differs from the node
	// whose status was read. On a connected, Established replica it is non-zero
	// only after an online verify found blocks that disagree.
	OutOfSyncKiB uint64
	// WrittenKiB is what DRBD wrote to this node's backing disks since the
	// resource came up there, or nil when not reported (peers, text parse).
	// Carried for Observer: it is what the write-anomaly detector reads.
	WrittenKiB *uint64
}

// connected reports whether this replica's link is usable, which is the
// precondition for believing anything else the view says about it.
//
// An empty Connection is the answering node itself (or a status source with no
// connection field) and counts as connected: it is the one node whose state was
// read locally rather than across a link.
func (st NodeStateInfo) connected() bool {
	return st.Connection == "" || strings.EqualFold(st.Connection, "Connected")
}

// ResourceStatusInfo is one resource's health across its replicas.
type ResourceStatusInfo struct {
	Name       string
	NodeStates map[string]NodeStateInfo
	// WAN replication health (only meaningful when WANEnabled). WANHealthy is
	// true when both haify-proxy instances are active and the DR WAN endpoint is
	// reachable; WANMessage describes the fault otherwise.
	WANEnabled bool
	WANHealthy bool
	WANMessage string
	// WANProxy is the counters the primary site's haify-proxy published, or
	// nil when none could be read. Carried for Observer (Prometheus export).
	WANProxy *wanproxy.Metrics
	// IdleWithoutPrimary marks a resource whose Primary another system
	// decides (Proxmox VE, Kubernetes, OpenStack): it is promoted while a guest
	// or pod uses it and demoted when that goes, which is not a fault, and it
	// moves when that system moves its user. Losing its Primary is not raised
	// as a critical and a move is not a failover; degrade, quorum and sync
	// alerts still apply.
	IdleWithoutPrimary bool
	// Owner is what the resource is to the system that created it, such as
	// "PVC db/data-postgres-0"; empty when nothing names it.
	Owner string
}

// Title is how messages name the resource: with its owner when it has one,
// because a generated name like pvc-<uid> tells an operator nothing.
func (r ResourceStatusInfo) Title() string {
	if r.Owner == "" {
		return r.Name
	}
	return r.Name + " (" + r.Owner + ")"
}

// Degraded reports whether any replica of the resource is in a faulty state.
//
// It is the same predicate the degrade alert fires on, exported rather than
// reimplemented so a "degraded resources" gauge and a resource.degraded alert
// can never disagree about the same instant — the disagreement an operator
// notices is the one between the alert that paged them and the page they open
// next.
func (r ResourceStatusInfo) Degraded() bool {
	for _, state := range r.NodeStates {
		if bad, _ := isDegraded(state); bad {
			return true
		}
	}
	return false
}

// ResourceLister is satisfied by *controller.ResourceManager or a fake in tests.
type ResourceLister interface {
	GetResourceStatusList(ctx context.Context) ([]ResourceStatusInfo, error)
}

// NodeStatusInfo is one registered node's reachability.
type NodeStatusInfo struct {
	Name      string
	Reachable bool
	Message   string
}

// NodeLister reports whether each registered node can still be reached. It is
// optional: a Monitor built without one simply never raises node events.
type NodeLister interface {
	GetNodeStatusList(ctx context.Context) ([]NodeStatusInfo, error)
}

// PoolStatusInfo is one storage pool's capacity as the monitor needs it.
//
// Only thin pool utilisation is here, and deliberately not the volume group's
// free space: Haify creates a thin pool from nearly all of its group, so vg_free
// is near zero from the moment the pool exists and alerting on it would fire
// permanently for every pool in the cluster.
type PoolStatusInfo struct {
	Name string
	Node string
	// ThinPool is the thin pool LV inside the group, empty when the group holds
	// none. Empty means the percentages below carry no information — a thick
	// group is not a pool at 0% — and no capacity condition is evaluated.
	ThinPool    string
	DataPercent float64
	MetaPercent float64
	// OutOfSpace is LVM's own out-of-data-space flag, which is a fact rather
	// than a threshold and is alerted on regardless of the percentages.
	OutOfSpace bool
	// TotalBytes and FreeBytes are the volume group's own capacity, and
	// ThinSizeBytes the data capacity of the thin pool inside it — the figure
	// DataPercent is a percentage of.
	//
	// No condition here reads them, for the reason in the type comment: Haify
	// drives vg_free to near zero on purpose, so alerting on it would fire forever.
	// They are carried for Observer, which charts capacity rather than judging
	// it, and for which a thick group's group-level figures are the only ones
	// that exist.
	TotalBytes    uint64
	FreeBytes     uint64
	ThinSizeBytes uint64
	// VDO is true for a VDO-backed thin pool, and VDOPhysicalKnown when its
	// physical usage, VDOPhysicalPercent, was read this poll. A thin pool on
	// VDO can be half empty logically and out of physical space, so that
	// figure is judged on its own.
	VDO                bool
	VDOPhysicalKnown   bool
	VDOPhysicalPercent float64
}

// PoolLister reports the capacity of every managed pool. It is optional: a
// Monitor built without one never raises pool events.
type PoolLister interface {
	GetPoolStatusList(ctx context.Context) ([]PoolStatusInfo, error)
}

// SourceStatus is how one of the monitor's data sources answered on one poll.
//
// The three states it distinguishes are the whole point of it. A source that is
// switched off, a source that failed, and a source that answered with nothing
// look identical once flattened to a slice of results, and a consumer that
// cannot tell them apart will publish "zero nodes are reachable" for a database
// error — which is precisely the reading an operator cannot distinguish from a
// dead cluster.
type SourceStatus struct {
	// Enabled is false when the source is not configured at all. Nothing about
	// it was attempted, so nothing about it should be reported either way.
	Enabled bool
	// OK is true when the listing succeeded. False with Enabled set means the
	// listing was attempted and failed; Items is then empty and means nothing.
	OK bool
	// Err is the listing failure, nil when OK.
	Err error
	// Duration is how long the listing took, successful or not.
	Duration time.Duration
}

// ResourceObservation is the resource listing of one poll.
type ResourceObservation struct {
	SourceStatus
	Items []ResourceStatusInfo
}

// NodeObservation is the node listing of one poll.
type NodeObservation struct {
	SourceStatus
	Items []NodeStatusInfo
}

// PoolObservation is the pool listing of one poll.
type PoolObservation struct {
	SourceStatus
	Items []PoolStatusInfo
}

// Observation is everything one poll cycle saw, exactly as the checks saw it.
type Observation struct {
	Resources ResourceObservation
	Nodes     NodeObservation
	Pools     PoolObservation
	// Firing is every condition raised at the end of the poll.
	Firing []FiringCondition
}

// FiringCondition is one raised alert, as a metric counts it.
type FiringCondition struct {
	Type     string
	Severity string
}

// Observer is handed each poll's Observation after the events for it have been
// published.
//
// The monitor's job is to turn cluster state into events, but the state it
// gathers is also exactly what a metrics exporter needs, and gathering it twice
// would double the SSH traffic and let the two views disagree about the same
// instant. So one poll is published to whoever asked for it.
//
// This is an interface rather than a direct call into a metrics package for the
// same reason the three listers above are interfaces: alert depends on
// abstractions in both directions, has no opinion about what a consumer does
// with a snapshot, and stays testable with nothing but fakes. The adapter that
// turns an Observation into Prometheus series lives on the controller side,
// alongside the adapters that satisfy the listers.
//
// Observed is called from the poll goroutine and must not block: a slow
// observer delays the next health check, which is the one thing the monitor
// cannot afford.
type Observer interface {
	Observed(Observation)
}

// source names which lister owns a condition, so a condition can be cleared
// when its subject vanishes without being cleared merely because a *different*
// lister failed.
//
// This used to be inferred from the shape of the event key — a condition with
// an empty Resource was a node condition, anything else a resource condition.
// Pool conditions have both a name and a node and fit neither, and guessing
// would have let a failed pool listing resolve every node alert, or a healthy
// node poll silently clear a pool alert whose source never answered.
type source int

const (
	sourceResources source = iota
	sourceNodes
	sourcePools
)
