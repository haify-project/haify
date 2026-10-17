// Package metrics provides Prometheus metrics support for the Haify controller.
//
// Two families live here. The `haify_controller_*` family describes the
// controller itself and the inventory it manages; the `haify_drbd_*` family
// describes live replication, one series per replica.
//
// The rule the whole package is built around: a number that was never observed
// must not be exported as zero. A flat zero on a dashboard is indistinguishable
// from a healthy idle cluster, so every gauge fed from a poll is only written
// when that poll actually answered, and series whose subject vanished are
// deleted rather than zeroed. See seriesSet and RecordObservation.
package metrics

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

const (
	namespace = "haify"
	subsystem = "controller"
	// drbdSubsystem separates live replication state from controller
	// inventory. They answer different questions and are collected from
	// different places, and a single subsystem would have made
	// `haify_controller_disk_state` read as a property of the controller.
	drbdSubsystem = "drbd"
)

// Metrics holds all Prometheus metrics for the Haify controller
type Metrics struct {
	logger   *zap.Logger
	registry *prometheus.Registry

	// Operations counter tracks total operations with result status
	operationsTotal *prometheus.CounterVec

	// Operation duration histogram tracks operation latency
	operationDuration *prometheus.HistogramVec

	// Resources gauge tracks how many resources are healthy and how many are
	// degraded. Both label values are written on every successful observation,
	// so a zero here is a measured zero.
	resources *prometheus.GaugeVec

	// Storage capacity tracks pool storage in bytes
	storageCapacity *seriesSet

	// Nodes gauge tracks node counts by reachability
	nodes *prometheus.GaugeVec

	// Gateways gauge tracks gateway counts by type and state
	gateways *seriesSet

	// observedAt records when each data source last answered, so a dashboard
	// can tell a current reading from one frozen by a source that has been
	// failing for an hour. It is the companion to the "never write a zero we
	// did not measure" rule: keeping the last good value is only honest if the
	// staleness is visible.
	observedAt *prometheus.GaugeVec

	// gRPC requests counter
	grpcRequestsTotal *prometheus.CounterVec
	// apiRequests and apiErrors are the same count kept as plain totals, for
	// the controller's own history (GRPCTotals); errors are any status but OK.
	apiRequests, apiErrors atomic.Uint64

	// gRPC request duration histogram
	grpcRequestDuration *prometheus.HistogramVec

	// Live DRBD replication, one series per replica. See SetReplication for
	// why the enum states are exported as a present-value series rather than
	// as a numeric code or a full 0/1 matrix.
	drbdRole             *seriesSet
	drbdDiskState        *seriesSet
	drbdReplicationState *seriesSet
	drbdResync           *seriesSet
	drbdQuorum           *seriesSet
	drbdResourceUp       *seriesSet

	// Up gauge indicates the instance is available (always 1)
	up prometheus.Gauge

	health healthSeries
	wan    wanSeries

	mu sync.Mutex
}

// New creates and registers all Prometheus metrics.
//
// Everything is registered into a registry owned by this instance rather than
// into prometheus.DefaultRegisterer. The default registerer is process-global,
// so registering there made New callable exactly once per process: a second
// call — a test, or a controller restarted in-process — panicked on duplicate
// registration, and the metrics were never served from that registry anyway
// since Handler reads the private one.
func New(logger *zap.Logger) (*Metrics, error) {
	logger.Debug("New: registering Prometheus metrics", zap.String("namespace", namespace))
	registry := prometheus.NewRegistry()
	auto := promauto.With(registry)

	m := &Metrics{
		logger:   logger,
		registry: registry,
		operationsTotal: auto.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "operations_total",
				Help:      "Total number of operations performed by result",
			},
			[]string{"operation", "result"},
		),
		operationDuration: auto.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "operation_duration_seconds",
				Help:      "Operation duration in seconds",
				Buckets:   prometheus.DefBuckets,
			},
			[]string{"operation"},
		),
		resources: auto.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "resources",
				Help:      "Number of DRBD resources by health state (healthy, degraded)",
			},
			[]string{"state"},
		),
		nodes: auto.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "nodes",
				Help:      "Number of registered nodes by state (reachable, unreachable)",
			},
			[]string{"state"},
		),
		observedAt: auto.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "last_observation_timestamp_seconds",
				Help:      "Unix time of the last successful observation of each data source",
			},
			[]string{"source"},
		),
		grpcRequestsTotal: auto.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "grpc_requests_total",
				Help:      "Total number of gRPC requests by method and status",
			},
			[]string{"method", "status"},
		),
		grpcRequestDuration: auto.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "grpc_request_duration_seconds",
				Help:      "gRPC request duration in seconds",
				Buckets:   prometheus.DefBuckets,
			},
			[]string{"method"},
		),
		up: auto.NewGauge(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "up",
				Help:      "Indicates the Haify controller instance is available (always 1)",
			},
		),
	}

	// The pool label is not unique on its own: every node in a Haify cluster
	// tends to carry a volume group of the same name, so without the node the
	// second pool's capacity silently overwrites the first's.
	m.storageCapacity = newSeriesSet(auto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "storage_capacity_bytes",
			Help:      "Storage capacity in bytes by pool, node and state (total, used, free)",
		},
		[]string{"pool", "node", "state"},
	))
	m.gateways = newSeriesSet(auto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "gateways",
			Help:      "Number of gateways by type and state",
		},
		[]string{"type", "state"},
	))
	m.drbdRole = newSeriesSet(auto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: drbdSubsystem,
			Name:      "role",
			Help:      "1 for the DRBD role a replica currently holds; other roles are not exported",
		},
		[]string{"resource", "node", "role"},
	))
	m.drbdDiskState = newSeriesSet(auto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: drbdSubsystem,
			Name:      "disk_state",
			Help:      "1 for the DRBD disk state a replica currently reports; other states are not exported",
		},
		[]string{"resource", "node", "state"},
	))
	m.drbdReplicationState = newSeriesSet(auto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: drbdSubsystem,
			Name:      "replication_state",
			Help:      "1 for the DRBD replication state a peer connection currently reports; other states are not exported",
		},
		[]string{"resource", "node", "state"},
	))
	m.drbdResync = newSeriesSet(auto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: drbdSubsystem,
			Name:      "resync_completed_ratio",
			Help:      "Resync completion of a replica as a fraction of 1; 1 means fully in sync",
		},
		[]string{"resource", "node"},
	))
	m.drbdQuorum = newSeriesSet(auto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: drbdSubsystem,
			Name:      "quorum",
			Help:      "1 when a replica holds DRBD quorum, 0 when it has lost it; absent when unreported",
		},
		[]string{"resource", "node"},
	))
	m.drbdResourceUp = newSeriesSet(auto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: drbdSubsystem,
			Name:      "resource_up",
			Help:      "1 when live DRBD status was read for a resource, 0 when no node answered for it",
		},
		[]string{"resource"},
	))

	m.registerHealth(auto)
	m.registerWAN(auto)

	// Set up to 1
	m.up.Set(1)

	// Register Go and process collectors with the custom registry
	m.registry.MustRegister(collectors.NewGoCollector())
	m.registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	logger.Info("Prometheus metrics initialized")
	logger.Debug("New: metrics registered",
		zap.Int("replica_families", len(m.replicaSeries())))
	return m, nil
}

// log returns the instance's logger, or the process-wide one for a Metrics
// built without one.
func (m *Metrics) log() *zap.Logger {
	if m.logger == nil {
		return zap.L()
	}
	return m.logger
}

// Handler returns the HTTP handler for the /metrics endpoint. Each scrape is
// traced once at Debug, with who scraped and how long the gather took; the
// samples themselves are never logged.
func (m *Metrics) Handler() http.Handler {
	m.log().Debug("Metrics.Handler: building /metrics handler", zap.Bool("open_metrics", true))
	inner := promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		inner.ServeHTTP(w, r)
		m.log().Debug("Metrics.Handler: scrape served",
			zap.String("remote", r.RemoteAddr),
			zap.String("accept", r.Header.Get("Accept")),
			zap.Duration("took", time.Since(start)))
	})
}

// ==================== Recorders ====================

// RecordOperation records an operation with its result and duration.
func (m *Metrics) RecordOperation(operation, result string, duration float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.operationsTotal.WithLabelValues(operation, result).Inc()
	m.operationDuration.WithLabelValues(operation).Observe(duration)
	m.log().Debug("Metrics.RecordOperation: operation recorded",
		zap.String("operation", operation),
		zap.String("result", result),
		zap.Float64("duration_seconds", duration))
}

// RecordObservation records one attempt to read one cluster data source.
//
// The timestamp is only advanced on success. That is what lets a consumer tell
// "the cluster really has three reachable nodes" from "the last poll that could
// answer said three, forty minutes ago" — the gauges deliberately keep their
// last good value when a source fails, so the staleness has to be exported
// somewhere.
func (m *Metrics) RecordObservation(source string, ok bool, duration float64) {
	result := "error"
	if ok {
		result = "success"
	}
	m.RecordOperation("observe_"+source, result, duration)

	if !ok {
		m.log().Debug("Metrics.RecordObservation: source failed, keeping last observation time",
			zap.String("source", source), zap.Float64("duration_seconds", duration))
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().Unix()
	m.observedAt.WithLabelValues(source).Set(float64(now))
	m.log().Debug("Metrics.RecordObservation: source observed",
		zap.String("source", source), zap.Int64("observed_at", now),
		zap.Float64("duration_seconds", duration))
}

// RecordResourceCount sets how many resources are in one health state. Callers
// write every state they know about on each successful poll, so an unwritten
// state is one that has never been observed rather than one that is empty.
func (m *Metrics) RecordResourceCount(state string, count float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.resources.WithLabelValues(state).Set(count)
	m.log().Debug("Metrics.RecordResourceCount: resource count set",
		zap.String("state", state), zap.Float64("count", count))
}

// RecordNodeState records the count of nodes in a specific state.
func (m *Metrics) RecordNodeState(state string, count float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.nodes.WithLabelValues(state).Set(count)
	m.log().Debug("Metrics.RecordNodeState: node count set",
		zap.String("state", state), zap.Float64("count", count))
}

// PoolCapacity is one storage pool's capacity, in bytes.
//
// TotalBytes is what the pool can hold and UsedBytes how much of it is
// allocated; free is derived rather than passed so the three can never
// disagree.
type PoolCapacity struct {
	Pool  string
	Node  string
	Total uint64
	Used  uint64
}

// SetPoolCapacities replaces the whole storage capacity view.
//
// This replaced a per-pool setter. A pool that is deleted, or that lives on a
// node that stopped answering, has no later call to zero it, so the per-pool
// form exported a removed pool's last known capacity forever — and it took no
// node, so two nodes' identically named volume groups overwrote each other.
// Callers must only invoke this when the pool listing actually succeeded;
// passing an empty slice deletes every pool series.
func (m *Metrics) SetPoolCapacities(pools []PoolCapacity) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.storageCapacity.begin()
	unknownFree := 0
	for _, p := range pools {
		labels := func(state string) prometheus.Labels {
			return prometheus.Labels{"pool": p.Pool, "node": p.Node, "state": state}
		}
		m.storageCapacity.set(labels("total"), float64(p.Total))
		m.storageCapacity.set(labels("used"), float64(p.Used))
		// A pool whose total is unknown has no meaningful free figure; total
		// minus used would render an unknown capacity as a fully free one.
		if p.Total >= p.Used {
			m.storageCapacity.set(labels("free"), float64(p.Total-p.Used))
		} else {
			unknownFree++
		}
	}
	m.storageCapacity.commit()
	m.log().Debug("Metrics.SetPoolCapacities: pool capacity view replaced",
		zap.Int("count", len(pools)),
		zap.Int("free_unknown", unknownFree),
		zap.Int("series", len(m.storageCapacity.live)))
}

// GatewayCount is the number of gateways of one type in one state.
type GatewayCount struct {
	Type  string
	State string
	Count float64
}

// SetGatewayCounts replaces the whole gateway view, for the same reason
// SetPoolCapacities does: nothing else ever zeroes the state a gateway has left
// behind, so a gateway that moved from "started" to "stopped" would otherwise
// be counted in both forever.
func (m *Metrics) SetGatewayCounts(counts []GatewayCount) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.gateways.begin()
	for _, c := range counts {
		m.gateways.set(prometheus.Labels{"type": c.Type, "state": c.State}, c.Count)
	}
	m.gateways.commit()
	m.log().Debug("Metrics.SetGatewayCounts: gateway view replaced",
		zap.Int("count", len(counts)),
		zap.Int("series", len(m.gateways.live)))
}

// GRPCTotals is how many gRPC requests were served since start, and how many
// of them failed.
func (m *Metrics) GRPCTotals() (requests, errors uint64) {
	return m.apiRequests.Load(), m.apiErrors.Load()
}

// RecordGRPCRequest records a gRPC request with method, status, and duration
func (m *Metrics) RecordGRPCRequest(method, status string, duration float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.grpcRequestsTotal.WithLabelValues(method, status).Inc()
	m.grpcRequestDuration.WithLabelValues(method).Observe(duration)
	m.apiRequests.Add(1)
	if status != "OK" {
		m.apiErrors.Add(1)
	}
	m.log().Debug("Metrics.RecordGRPCRequest: request recorded",
		zap.String("method", method),
		zap.String("status", status),
		zap.Float64("duration_seconds", duration))
}

// ResetMetrics resets all metrics to zero (useful for testing)
func (m *Metrics) ResetMetrics() {
	m.log().Debug("Metrics.ResetMetrics: dropping every series")
	m.mu.Lock()
	defer m.mu.Unlock()

	m.operationsTotal.Reset()
	m.operationDuration.Reset()
	m.resources.Reset()
	m.nodes.Reset()
	m.observedAt.Reset()
	m.grpcRequestsTotal.Reset()
	m.grpcRequestDuration.Reset()

	m.storageCapacity.reset()
	m.gateways.reset()
	for _, s := range m.replicaSeries() {
		s.reset()
	}
	for _, s := range m.wan.all() {
		s.reset()
	}
}

// GetRegistry returns the Prometheus registry
func (m *Metrics) GetRegistry() *prometheus.Registry {
	return m.registry
}
