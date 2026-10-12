package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// ReplicaState is one replica's live DRBD state.
//
// The optional fields are pointers because their zero values are meaningful
// readings in their own right: 0% synced and "quorum lost" are both real
// states, and neither may be invented for a replica that simply did not report.
type ReplicaState struct {
	Resource string
	Node     string
	// Role, DiskState and ReplicationState are exported verbatim as label
	// values; an empty one is skipped rather than exported as "".
	Role             string
	DiskState        string
	ReplicationState string
	// SyncPercent is resync completion in 0..100, or nil when the status source
	// could not report it. Steady state is 100, not 0: drbdsetup omits the
	// completion field entirely once a resync has finished, and treating that
	// absence as zero paints a fully in-sync cluster as one whose resync never
	// started.
	SyncPercent *float64
	// Quorum is nil unless DRBD actually reported quorum for this replica,
	// which in practice is only the node that was queried — a node can only
	// speak for its own quorum, and inferring a peer's would be a guess.
	Quorum *bool
	// OutOfSyncBytes is how much DRBD has marked as differing from the node
	// whose status was read; zero for that node itself.
	OutOfSyncBytes uint64
	// TLS is nil for the node whose status was read, which has no connection
	// to itself, and otherwise whether the connection to this peer is
	// encrypted.
	TLS *bool
	// WrittenBytes is what DRBD wrote to this node's backing disk since the
	// resource came up there, or nil when it was not reported — only the node
	// whose status was read reports its own.
	WrittenBytes *uint64
}

// ReplicationSnapshot is one complete reading of DRBD replication.
type ReplicationSnapshot struct {
	// Resources is every resource the controller knows about, whether or not
	// any of its replicas could be read. Each becomes an haify_drbd_resource_up
	// series, which is how "nothing answered for this resource" is expressed:
	// its replica series simply do not exist, and resource_up is 0.
	Resources []string
	// Replicas is every replica whose state was actually read. Replicas that
	// could not be reached are absent, not zeroed — a node that has fallen off
	// the network must not read as a node reporting perfect health, nor as one
	// reporting total failure.
	Replicas []ReplicaState
}

// SetReplication replaces the whole DRBD replication view.
//
// DRBD's role, disk state and replication state are enumerations, and there are
// two usual ways to export one. This exports only the value currently held, as
// a series carrying that value as a label with a constant 1 — so
// `haify_drbd_disk_state{state!="UpToDate"} == 1` finds every unhealthy replica,
// and `count by (state)` gives the cluster's distribution.
//
// The alternative — a 0/1 series for every value of the enum — allows
// `{state="UpToDate"} == 0` as well, but costs roughly 25 series per replica
// instead of 3, and requires this package to hold a copy of DRBD's enums. Those
// enums differ across DRBD versions, so the copy would go stale silently and
// start dropping states that the kernel reports and this code has never heard
// of. Exporting exactly what drbdsetup said cannot go stale.
//
// A state that stops being current has its series deleted rather than set to 0,
// which is the same rule as everywhere else here: absent means "not observed".
func (m *Metrics) SetReplication(snap ReplicationSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, s := range m.replicaSeries() {
		s.begin()
	}

	observed := make(map[string]bool, len(snap.Resources))
	for _, r := range snap.Replicas {
		observed[r.Resource] = true
		id := prometheus.Labels{"resource": r.Resource, "node": r.Node}

		if r.Role != "" {
			m.drbdRole.set(withLabel(id, "role", r.Role), 1)
		}
		if r.DiskState != "" {
			m.drbdDiskState.set(withLabel(id, "state", r.DiskState), 1)
		}
		if r.ReplicationState != "" {
			m.drbdReplicationState.set(withLabel(id, "state", r.ReplicationState), 1)
		}
		if r.SyncPercent != nil {
			m.drbdResync.set(id, clampRatio(*r.SyncPercent/100))
		}
		m.health.drbdOutOfSync.set(id, float64(r.OutOfSyncBytes))
		if r.TLS != nil {
			m.health.drbdTLS.set(id, boolValue(*r.TLS))
		}
		if r.WrittenBytes != nil {
			m.health.drbdWritten.set(id, float64(*r.WrittenBytes))
		}
		if r.Quorum != nil {
			quorum := 0.0
			if *r.Quorum {
				quorum = 1
			}
			m.drbdQuorum.set(id, quorum)
		}
	}

	for _, name := range snap.Resources {
		up := 0.0
		if observed[name] {
			up = 1
		}
		m.drbdResourceUp.set(prometheus.Labels{"resource": name}, up)
	}

	for _, s := range m.replicaSeries() {
		s.commit()
	}
	if ce := m.log().Check(zap.DebugLevel, "Metrics.SetReplication: replication view replaced"); ce != nil {
		var unread []string
		for _, name := range snap.Resources {
			if !observed[name] {
				unread = append(unread, name)
			}
		}
		ce.Write(zap.Int("resources", len(snap.Resources)),
			zap.Int("replicas", len(snap.Replicas)),
			zap.Strings("unread_resources", unread))
	}
}

func (m *Metrics) replicaSeries() []*seriesSet {
	return []*seriesSet{
		m.drbdRole, m.drbdDiskState, m.drbdReplicationState,
		m.drbdResync, m.drbdQuorum, m.drbdResourceUp,
		m.health.drbdOutOfSync, m.health.drbdTLS, m.health.drbdWritten,
	}
}

// withLabel copies a label set with one more label, so the shared
// resource/node identity is not mutated by the caller that adds a state.
func withLabel(base prometheus.Labels, name, value string) prometheus.Labels {
	out := make(prometheus.Labels, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out[name] = value
	return out
}

// clampRatio keeps a completion figure inside 0..1. DRBD has been seen to
// report slightly over 100 on a finishing resync, and a ratio above 1 on a
// panel labelled "percent complete" reads as a bug in the cluster rather than
// in the reporting.
func clampRatio(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}
