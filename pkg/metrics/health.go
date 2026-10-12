package metrics

import (
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
)

// Health metrics: the conditions the alerting acts on, as numbers a dashboard
// or an outside Prometheus can act on too. Every family follows the package
// rule: a series that is not observed is deleted, never written as zero.

type healthSeries struct {
	nodeReachable   *seriesSet
	poolThinPercent *seriesSet
	alertsFiring    *seriesSet
	backupLast      *seriesSet
	backupBytes     *seriesSet
	faultDomainRisk *seriesSet
	drbdOutOfSync   *seriesSet
	drbdTLS         *seriesSet
	drbdWritten     *seriesSet
}

func gauge(auto promauto.Factory, subsystem, name, help string, labels ...string) *seriesSet {
	return newSeriesSet(auto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystem, Name: name, Help: help,
	}, labels))
}

func (m *Metrics) registerHealth(auto promauto.Factory) {
	m.log().Debug("Metrics.registerHealth: registering health families")
	m.health = healthSeries{
		nodeReachable: gauge(auto, subsystem, "node_reachable",
			"1 when the controller reached the node on its last check, 0 when it did not", "node"),
		poolThinPercent: gauge(auto, subsystem, "pool_thin_used_percent",
			"Thin pool utilisation in percent, data and metadata; a full thin pool fails writes", "pool", "node", "kind"),
		alertsFiring: gauge(auto, subsystem, "alerts_firing",
			"Number of conditions currently raised, by event type and severity", "type", "severity"),
		backupLast: gauge(auto, subsystem, "backup_last_success_timestamp_seconds",
			"When the newest completed backup of a resource to a target started", "resource", "target"),
		backupBytes: gauge(auto, subsystem, "backup_last_shipped_bytes",
			"What the newest completed backup carried: the full image, or the changed ranges of an incremental", "resource", "target", "kind"),
		faultDomainRisk: gauge(auto, subsystem, "resource_fault_domain_risk",
			"1 when losing one labelled fault domain would take every copy of the resource or its quorum", "resource", "domain"),
		drbdOutOfSync: gauge(auto, drbdSubsystem, "out_of_sync_bytes",
			"Data DRBD has marked as differing between this replica and the node whose status was read", "resource", "node"),
		drbdTLS: gauge(auto, drbdSubsystem, "connection_tls",
			"1 when the connection to this peer runs over TLS, 0 when it does not", "resource", "node"),
		drbdWritten: gauge(auto, drbdSubsystem, "written_bytes",
			"Bytes DRBD wrote to this node's backing disk since the resource came up there; take rate() of it", "resource", "node"),
	}
}

// NodeReach is one node's reachability.
type NodeReach struct {
	Node      string
	Reachable bool
}

// SetNodeReachability replaces the per-node reachability series.
func (m *Metrics) SetNodeReachability(nodes []NodeReach) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.health.nodeReachable
	s.begin()
	unreachable := 0
	for _, n := range nodes {
		if !n.Reachable {
			unreachable++
		}
		s.set(prometheus.Labels{"node": n.Node}, boolValue(n.Reachable))
	}
	s.commit()
	m.log().Debug("Metrics.SetNodeReachability: reachability view replaced",
		zap.Int("count", len(nodes)), zap.Int("unreachable", unreachable))
}

// ThinUsage is one thin pool's utilisation.
type ThinUsage struct {
	Pool, Node               string
	DataPercent, MetaPercent float64
}

// SetThinPoolUsage replaces the thin pool utilisation series.
func (m *Metrics) SetThinPoolUsage(pools []ThinUsage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.health.poolThinPercent
	s.begin()
	for _, p := range pools {
		s.set(prometheus.Labels{"pool": p.Pool, "node": p.Node, "kind": "data"}, p.DataPercent)
		s.set(prometheus.Labels{"pool": p.Pool, "node": p.Node, "kind": "metadata"}, p.MetaPercent)
	}
	s.commit()
	m.log().Debug("Metrics.SetThinPoolUsage: thin pool view replaced",
		zap.Int("count", len(pools)), zap.Int("series", len(s.live)))
}

// SetFiringAlerts replaces the count of raised conditions, given one entry
// per condition as "type/severity".
func (m *Metrics) SetFiringAlerts(conditions []string) {
	counts := map[string]float64{}
	for _, c := range conditions {
		counts[c]++
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.health.alertsFiring
	s.begin()
	for c, n := range counts {
		typ, sev, _ := strings.Cut(c, "/")
		s.set(prometheus.Labels{"type": typ, "severity": sev}, n)
	}
	s.commit()
	m.log().Debug("Metrics.SetFiringAlerts: firing alert view replaced",
		zap.Int("count", len(conditions)), zap.Int("series", len(counts)))
}

// BackupState is the newest completed backup of one resource to one target.
type BackupState struct {
	Resource, Target, Kind string
	StartedUnix            float64
	ShippedBytes           uint64
}

// SetBackups replaces the backup series.
func (m *Metrics) SetBackups(backups []BackupState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	last, size := m.health.backupLast, m.health.backupBytes
	last.begin()
	size.begin()
	for _, b := range backups {
		id := prometheus.Labels{"resource": b.Resource, "target": b.Target}
		last.set(id, b.StartedUnix)
		size.set(withLabel(id, "kind", b.Kind), float64(b.ShippedBytes))
	}
	last.commit()
	size.commit()
	m.log().Debug("Metrics.SetBackups: backup view replaced",
		zap.Int("count", len(backups)), zap.Int("series", len(last.live)))
}

// SetFaultDomainRisks replaces the fault-domain risk series, given each
// resource's comma-separated risky domains.
func (m *Metrics) SetFaultDomainRisks(risks map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.health.faultDomainRisk
	s.begin()
	names := make([]string, 0, len(risks))
	for r := range risks {
		names = append(names, r)
	}
	sort.Strings(names)
	for _, r := range names {
		for _, d := range strings.Split(risks[r], ",") {
			if d != "" {
				s.set(prometheus.Labels{"resource": r, "domain": d}, 1)
			}
		}
	}
	s.commit()
	m.log().Debug("Metrics.SetFaultDomainRisks: fault-domain risk view replaced",
		zap.Int("resources", len(risks)), zap.Int("series", len(s.live)))
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
