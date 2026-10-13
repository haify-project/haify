package controller

import (
	"context"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/alert"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/metrics"
)

// metricsObserver turns one health poll into Prometheus series.
//
// It exists because the numbers a dashboard needs and the numbers the alert
// detector needs are the same numbers. Collecting them twice would double the
// SSH round trips to every storage node and let the alert and the panel
// disagree about the same instant, which is the disagreement an operator
// notices first and trusts least. So alert.Monitor publishes each poll through
// alert.Observer, and this is the adapter on the controller side — the same
// shape as the gateway package's ResourceManager/DeploymentClient adapters, and
// for the same reason: alert depends only on its own interfaces and never
// imports pkg/metrics.
//
// Everything here obeys one rule. A source that did not answer leaves its
// gauges exactly as they were rather than zeroing them, because a zero is
// indistinguishable from a healthy idle cluster and this whole change exists to
// stop /metrics from claiming things it does not know. Staleness is exported
// instead, as haify_controller_last_observation_timestamp_seconds.
type metricsObserver struct {
	metrics *metrics.Metrics
	// gateways is read straight from the local database rather than from the
	// gateway manager: gateway state is persisted on every transition, and the
	// manager's listing costs an SSH round trip per gateway that the poll has
	// no reason to pay.
	gateways gatewayRecordLister
	// backups and resources feed the backup and fault-domain series; both are
	// local reads.
	backups   backupLister
	resources resourceLister
	ctx       context.Context
	log       *zap.Logger
}

// gatewayRecordLister is the slice of the database the observer needs, kept as
// an interface so the adapter is testable without a bbolt file.
type gatewayRecordLister interface {
	ListGateways(ctx context.Context) ([]*database.Gateway, error)
}

// gatewayListTimeout bounds the local database read. It is generous by design:
// the point is only that a wedged read cannot stall the poll goroutine, which
// would delay the next health check.
const gatewayListTimeout = 5 * time.Second

func newMetricsObserver(c *Controller) *metricsObserver {
	obs := &metricsObserver{
		metrics: c.metrics,
		ctx:     c.ctx,
		log:     c.logger,
	}
	// A nil *database.DB in an interface is not a nil interface, so the check
	// has to happen here rather than at the call site.
	if c.db != nil {
		obs.gateways = c.db
		obs.backups = c.db
	}
	if c.resources != nil {
		obs.resources = c.resources
	}
	obs.debugLog().Debug("newMetricsObserver: created", zap.Bool("has_db", c.db != nil),
		zap.Bool("has_resources", c.resources != nil))
	return obs
}

// debugLog is the observer's logger, or zap's global one when it has none.
func (o *metricsObserver) debugLog() *zap.Logger {
	if o.log == nil {
		return zap.L()
	}
	return o.log
}

// Observed implements alert.Observer.
func (o *metricsObserver) Observed(obs alert.Observation) {
	log := o.debugLog()
	log.Debug("metricsObserver.Observed: exporting poll", zap.Bool("resources", obs.Resources.Enabled),
		zap.Bool("nodes", obs.Nodes.Enabled), zap.Bool("pools", obs.Pools.Enabled), zap.Int("firing", len(obs.Firing)))
	start := time.Now()
	o.observeResources(obs.Resources)
	o.observeNodes(obs.Nodes)
	o.observePools(obs.Pools)
	o.observeGateways()
	o.observeHealth(obs)
	log.Debug("metricsObserver.Observed: done", zap.Duration("took", time.Since(start)))
}

// observeResources records resource health and the live DRBD replication view.
func (o *metricsObserver) observeResources(res alert.ResourceObservation) {
	log := o.debugLog()
	if !res.Enabled {
		log.Debug("metricsObserver.observeResources: resource check disabled")
		return
	}
	o.metrics.RecordObservation("resources", res.OK, res.Duration.Seconds())
	if !res.OK {
		log.Debug("metricsObserver.observeResources: poll failed, keeping previous gauges",
			zap.Duration("took", res.Duration))
		return
	}

	var healthy, degraded float64
	snap := metrics.ReplicationSnapshot{
		Resources: make([]string, 0, len(res.Items)),
		Replicas:  make([]metrics.ReplicaState, 0, len(res.Items)),
	}

	for _, item := range res.Items {
		if item.Degraded() {
			degraded++
		} else {
			healthy++
		}
		snap.Resources = append(snap.Resources, item.Name)

		// A resource whose nodes could not be reached contributes no replica
		// series at all. That is deliberate: an unreachable replica has no disk
		// state, and inventing one — healthy or failed — is worse than the gap.
		// haify_drbd_resource_up goes to 0 for it, which is the signal that the
		// missing series are missing because nothing answered.
		for node, state := range item.NodeStates {
			snap.Replicas = append(snap.Replicas, metrics.ReplicaState{
				Resource:         item.Name,
				Node:             node,
				Role:             state.Role,
				DiskState:        state.DiskState,
				ReplicationState: state.ReplicationState,
				SyncPercent:      state.SyncPercent,
				Quorum:           state.Quorum,
				OutOfSyncBytes:   state.OutOfSyncKiB * 1024,
				TLS:              peerTLS(state),
				WrittenBytes:     kibToBytes(state.WrittenKiB),
			})
		}
	}

	// Both states are written every time, so a zero here is a measured zero:
	// "no resource is degraded" has to be expressible, or the panel that means
	// to say it stays blank instead.
	o.metrics.RecordResourceCount("healthy", healthy)
	o.metrics.RecordResourceCount("degraded", degraded)
	o.metrics.SetReplication(snap)
	o.metrics.SetWANProxies(wanProxyReadings(res.Items))
	log.Debug("metricsObserver.observeResources: exported", zap.Float64("healthy", healthy),
		zap.Float64("degraded", degraded), zap.Int("replicas", len(snap.Replicas)))
}

func (o *metricsObserver) observeNodes(nodes alert.NodeObservation) {
	log := o.debugLog()
	if !nodes.Enabled {
		log.Debug("metricsObserver.observeNodes: node check disabled")
		return
	}
	o.metrics.RecordObservation("nodes", nodes.OK, nodes.Duration.Seconds())
	if !nodes.OK {
		log.Debug("metricsObserver.observeNodes: poll failed, keeping previous gauges",
			zap.Duration("took", nodes.Duration))
		return
	}

	var reachable, unreachable float64
	for _, n := range nodes.Items {
		if n.Reachable {
			reachable++
		} else {
			unreachable++
		}
	}
	o.metrics.RecordNodeState("reachable", reachable)
	o.metrics.RecordNodeState("unreachable", unreachable)
	log.Debug("metricsObserver.observeNodes: exported", zap.Float64("reachable", reachable),
		zap.Float64("unreachable", unreachable))
}

func (o *metricsObserver) observePools(pools alert.PoolObservation) {
	log := o.debugLog()
	if !pools.Enabled {
		log.Debug("metricsObserver.observePools: pool check disabled")
		return
	}
	o.metrics.RecordObservation("pools", pools.OK, pools.Duration.Seconds())
	if !pools.OK {
		log.Debug("metricsObserver.observePools: poll failed, keeping previous gauges",
			zap.Duration("took", pools.Duration))
		return
	}

	capacities := make([]metrics.PoolCapacity, 0, len(pools.Items))
	skipped := 0
	for _, p := range pools.Items {
		total, used, ok := poolCapacityBytes(p)
		if !ok {
			skipped++
			continue
		}
		capacities = append(capacities, metrics.PoolCapacity{
			Pool: p.Name, Node: p.Node, Total: total, Used: used,
		})
	}
	o.metrics.SetPoolCapacities(capacities)
	log.Debug("metricsObserver.observePools: exported", zap.Int("count", len(capacities)),
		zap.Int("skipped_without_capacity", skipped))
}

// poolCapacityBytes picks the capacity figures that describe whether writes to
// a pool will succeed.
//
// For a thin pool that is the thin pool's own data capacity and utilisation,
// not the volume group's: Haify gives its thin pool 95% of the group's free
// extents at create (all of them on convert-thin), so vg_free is near zero from
// the moment the pool exists and a chart of it would show every pool in the
// cluster permanently full.
//
// A group with no thin pool has no such figure, and its group-level total and
// free are the real ones. A pool reporting neither is skipped rather than
// exported as zero-sized.
func poolCapacityBytes(p alert.PoolStatusInfo) (total, used uint64, ok bool) {
	if p.ThinPool != "" && p.ThinSizeBytes > 0 {
		return p.ThinSizeBytes, uint64(float64(p.ThinSizeBytes) * p.DataPercent / 100), true
	}
	if p.TotalBytes == 0 {
		return 0, 0, false
	}
	if p.FreeBytes > p.TotalBytes {
		return p.TotalBytes, 0, true
	}
	return p.TotalBytes, p.TotalBytes - p.FreeBytes, true
}

// observeGateways refreshes the gateway counts from the controller's own
// database. It rides the health poll for its clock rather than owning a ticker:
// one cadence is easier to reason about than two, and a gateway count that is
// up to one poll old is exactly as fresh as everything else on the page.
func (o *metricsObserver) observeGateways() {
	log := o.debugLog()
	if o.gateways == nil {
		log.Debug("metricsObserver.observeGateways: no database, skipping")
		return
	}

	ctx, cancel := context.WithTimeout(o.baseContext(), gatewayListTimeout)
	defer cancel()

	start := time.Now()
	records, err := o.gateways.ListGateways(ctx)
	o.metrics.RecordObservation("gateways", err == nil, time.Since(start).Seconds())
	log.Debug("metricsObserver.observeGateways: listed gateways", zap.Int("count", len(records)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		o.log.Warn("metrics observer: list gateways failed", zap.Error(err))
		return
	}

	counts := map[metrics.GatewayCount]float64{}
	for _, gw := range records {
		if gw == nil {
			continue
		}
		key := metrics.GatewayCount{
			Type:  strings.ToLower(string(gw.Type)),
			State: gatewayMetricState(gw.Status),
		}
		counts[key]++
	}

	out := make([]metrics.GatewayCount, 0, len(counts))
	for key, n := range counts {
		key.Count = n
		out = append(out, key)
	}
	o.metrics.SetGatewayCounts(out)
	log.Debug("metricsObserver.observeGateways: exported", zap.Int("series", len(out)))
}

// gatewayMetricState normalises a stored gateway status into a label value.
//
// An empty status is reported as "unknown" rather than dropped: a gateway that
// exists is part of the inventory whatever the controller last managed to learn
// about it, and omitting it would make the gateway total silently disagree with
// the gateway list.
func gatewayMetricState(status string) string {
	state := strings.ToLower(strings.TrimSpace(status))
	if state == "" {
		return "unknown"
	}
	return state
}

func (o *metricsObserver) baseContext() context.Context {
	if o.ctx != nil {
		return o.ctx
	}
	return context.Background()
}

func kibToBytes(kib *uint64) *uint64 {
	if kib == nil {
		return nil
	}
	b := *kib * 1024
	return &b
}

// wanLinkMinSamples is how many saturated seconds the proxy needs behind its
// link figure before it is exported; fewer is noise, not a measurement.
const wanLinkMinSamples = 10

// wanProxyReadings turns the proxy counters gathered for WAN resources into the
// /metrics view. Resources without a reading are left out.
func wanProxyReadings(items []alert.ResourceStatusInfo) []metrics.WANProxyReading {
	var out []metrics.WANProxyReading
	for _, item := range items {
		p := item.WANProxy
		if !item.WANEnabled || p == nil {
			continue
		}
		r := metrics.WANProxyReading{
			Resource:         item.Name,
			BacklogBytes:     p.BufferUsedBytes,
			BufferCapBytes:   p.BufferCapBytes,
			Connected:        p.WANConnected,
			CompressionRatio: p.CompressionRat,
			Reconnects:       p.Reconnects,
			RingFullEvents:   p.RingFullEvents,
			WireBytes:        p.WANWireBytes,
			HighWaterBytes:   p.BufferHighWaterBytes,
			InputBytes:       p.DRBDInBytes,
			InputP99Bps:      p.InputP99Bps,
			InputPeakBps:     p.InputPeakBps,
			CPUSeconds:       p.CPUSeconds,
		}
		if p.LinkSamplesSecs != nil && *p.LinkSamplesSecs >= wanLinkMinSamples {
			r.LinkBps = p.LinkP50Bps
		}
		out = append(out, r)
	}
	zap.L().Debug("wanProxyReadings: built", zap.Int("items", len(items)), zap.Int("readings", len(out)))
	return out
}
