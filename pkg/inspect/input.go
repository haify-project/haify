package inspect

import (
	"crypto/x509"
	"time"

	"go.uber.org/zap"
)

// Input is everything the checks read. The controller fills it from its
// database and one probe per node; tests fill it by hand.
type Input struct {
	Now time.Time

	// Nodes are the registered nodes.
	Nodes []Node
	// Probes holds each node's probe, keyed by node name. A node that did not
	// answer has an entry in ProbeErrors instead.
	Probes      map[string]*NodeProbe
	ProbeErrors map[string]string
	// ProbeStart and ProbeEnd bracket the probe round on the controller's
	// clock. A node's reported time is compared against the whole window, so
	// SSH latency is never mistaken for clock skew.
	ProbeStart, ProbeEnd time.Time

	Resources []Resource
	Gateways  []Gateway

	// Pool thresholds in percent, from [alert].
	PoolNearFull, PoolFull float64
	// Previous is the last stored report, for growth trends; may be nil.
	Previous *Report

	Alerts  AlertInput
	Backups BackupInput
	SelfHA  *SelfHAInput
	TLS     TLSInput

	// Disks are the disks under every pool and their health (disk_health.go);
	// DisksProbed is false when they could not be read.
	Disks       []Disk
	DisksProbed bool
}

// Node is one registered node.
type Node struct {
	Name               string
	Address            string
	ReplicationAddress string
	Hostname           string
}

// Resource is one DRBD resource as the database records it.
type Resource struct {
	Name string
	// Diskful, Tiebreakers and Clients are node names.
	Diskful     []string
	Tiebreakers []string
	Clients     []string
	// DR is the node name of a WAN resource's off-site DR replica, which is
	// also in Diskful; empty for a LAN resource.
	DR string
	// ServedBy says why the resource must have a Primary ("ha", "self-ha");
	// empty when nothing requires one. Gateways are carried separately.
	ServedBy string
	// QuorumRisk and FaultDomainRisk are the controller's own derivations.
	QuorumRisk      bool
	FaultDomainRisk string
}

// Gateway is one storage gateway record.
type Gateway struct {
	Resource string
	Type     string // nfs, iscsi, nvmeof
	Status   string
}

// Serving reports whether the gateway is meant to be exporting right now.
func (g Gateway) Serving() bool { return g.Status != "stopped" }

// AlertInput is the delivery side of alerting.
type AlertInput struct {
	Enabled bool
	// Targets are every place events are sent: database channels and
	// controller.toml receivers.
	Targets []DeliveryTarget
	// Events are the firing warning/critical events in the window.
	Events []AlertEvent
	// Since is the start of the window.
	Since time.Time
}

// DeliveryTarget is one channel and what is known about its deliveries.
type DeliveryTarget struct {
	Name    string
	Enabled bool
	// Accepts reports whether the channel's filter takes an event.
	Accepts func(AlertEvent) bool `json:"-"`
	// Since is when the channel came to exist; an event raised before it was
	// never offered to it. Zero means it was there all along.
	Since time.Time
	// Known is false when nothing has been recorded for the channel yet.
	Known               bool
	LastSuccess         time.Time
	LastFailure         time.Time
	LastError           string
	ConsecutiveFailures int
	// Failed holds the IDs of recent events it gave up on.
	Failed map[uint64]bool
}

// AlertEvent is one firing event.
type AlertEvent struct {
	ID       uint64
	Type     string
	Severity string
	Resource string
	Node     string
	Message  string
	At       time.Time
}

// BackupInput is what the database knows about scheduled protection.
type BackupInput struct {
	SchedulerEnabled bool
	Schedules        []BackupSchedule
	SnapSchedules    []SnapSchedule
	Targets          map[string]bool
	// BaseSnapshots are the `_bk_` snapshots backup records still need,
	// as "node/vg/lv" and "vg/lv" for records with no node.
	BaseSnapshots map[string]bool
	// Running is set while a backup is in flight; its snapshot may not be
	// recorded yet, so leftovers are not judged then.
	Running bool
}

// BackupSchedule is one cron-driven backup.
type BackupSchedule struct {
	Name, Resource, Target, Cron string
	Enabled                      bool
	CreatedAt                    time.Time
	LastRun                      time.Time
	LastError                    string
	// LastSuccess is when the newest completed backup it made finished.
	LastSuccess time.Time
}

// SnapSchedule is one cron-driven snapshot schedule.
type SnapSchedule struct {
	Name, Resource, Cron string
	Enabled              bool
	CreatedAt, LastRun   time.Time
}

// SelfHAInput describes the controller's own HA.
type SelfHAInput struct {
	Resource string
	Nodes    []string
}

// TLSInput carries certificates read on the controller.
type TLSInput struct {
	// APICert is the gRPC server certificate when [tls] is on.
	APICert     *x509.Certificate
	APICertPath string
	APICertErr  string
	// ReplicationCA is the DRBD TLS CA when one has been created.
	ReplicationCA     *x509.Certificate
	ReplicationCAPath string
}

// nodeName maps a name, hostname or address to the registered node name,
// returning ref itself when no node matches. DRBD names peers by hostname,
// the database by node name; checks speak node names.
func (in *Input) nodeName(ref string) string {
	for _, n := range in.Nodes {
		if ref == n.Name || ref == n.Hostname || ref == n.Address {
			return n.Name
		}
	}
	zap.L().Debug("Input.nodeName: no registered node matches, keeping the reference", zap.String("ref", ref))
	return ref
}

// node returns the registered node named name, or nil.
func (in *Input) node(name string) *Node {
	for i := range in.Nodes {
		if in.Nodes[i].Name == name {
			return &in.Nodes[i]
		}
	}
	return nil
}

// probe returns a node's probe and whether it answered.
func (in *Input) probe(name string) (*NodeProbe, bool) {
	p, ok := in.Probes[in.nodeName(name)]
	return p, ok && p != nil
}

// unanswered lists the nodes in names that have no probe.
func (in *Input) unanswered(names []string) []string {
	var out []string
	for _, n := range names {
		if _, ok := in.probe(n); !ok {
			out = append(out, in.nodeName(n))
		}
	}
	if len(out) > 0 {
		zap.L().Debug("Input.unanswered: nodes without a probe", zap.Strings("nodes", out), zap.Int("asked", len(names)))
	}
	return out
}
