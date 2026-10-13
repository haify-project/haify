package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
)

// WAN replication metrics: what the primary site's haify-proxy publishes for
// each WAN resource, read on the health poll. A resource whose proxy could not
// be read has no series at all, and a figure an older proxy does not publish is
// left out rather than written as zero.

const wanSubsystem = "wan"

type wanSeries struct {
	backlog         *seriesSet
	bufferCap       *seriesSet
	bufferHighWater *seriesSet
	connected       *seriesSet
	compression     *seriesSet
	reconnects      *seriesSet
	ringFull        *seriesSet
	inputBytes      *seriesSet
	wireBytes       *seriesSet
	inputP99        *seriesSet
	inputPeak       *seriesSet
	linkRate        *seriesSet
	cpuSeconds      *seriesSet
}

func (m *Metrics) registerWAN(auto promauto.Factory) {
	m.log().Debug("Metrics.registerWAN: registering WAN proxy families")
	g := func(name, help string) *seriesSet { return gauge(auto, wanSubsystem, name, help, "resource") }
	m.wan = wanSeries{
		backlog: g("backlog_bytes",
			"Writes acknowledged on the primary site and not yet sent to the DR site: what a DR failover now would lose"),
		bufferCap: g("buffer_capacity_bytes", "Buffer capacity of the proxy's live connections"),
		bufferHighWater: g("buffer_high_water_bytes",
			"Largest backlog since the proxy started: how much buffer the workload has needed"),
		connected:   g("connected", "1 when the proxy's WAN connection is up"),
		compression: g("compression_ratio", "Payload bytes per byte put on the WAN since the proxy started"),
		reconnects:  g("reconnects", "WAN reconnects since the proxy started; take increase() of it"),
		ringFull: g("buffer_full_events",
			"Times the buffer filled and DRBD had to wait or go Ahead, since the proxy started; take increase() of it"),
		inputBytes: g("input_bytes", "Bytes DRBD handed to the proxy since it started; take rate() of it"),
		wireBytes:  g("wire_bytes", "Bytes the proxy put on the WAN since it started; take rate() of it"),
		inputP99: g("input_p99_bytes_per_second",
			"99th percentile of the per-second write rate into the proxy since it started"),
		inputPeak: g("input_peak_bytes_per_second", "Busiest second of writes into the proxy since it started"),
		linkRate: g("link_bytes_per_second",
			"Median WAN rate over the seconds a backlog waited for the link: its usable bandwidth"),
		cpuSeconds: g("cpu_seconds", "CPU time the primary site's proxy has used; take rate() of it"),
	}
}

func (w *wanSeries) all() []*seriesSet {
	return []*seriesSet{w.backlog, w.bufferCap, w.bufferHighWater, w.connected, w.compression, w.reconnects,
		w.ringFull, w.inputBytes, w.wireBytes, w.inputP99, w.inputPeak, w.linkRate, w.cpuSeconds}
}

// WANProxyReading is one WAN resource's proxy counters. The optional figures
// are nil when the proxy does not publish them.
type WANProxyReading struct {
	Resource         string
	BacklogBytes     uint64
	BufferCapBytes   uint64
	Connected        bool
	CompressionRatio float64
	Reconnects       uint64
	RingFullEvents   uint64
	WireBytes        uint64

	HighWaterBytes *uint64
	InputBytes     *uint64
	InputP99Bps    *float64
	InputPeakBps   *float64
	// LinkBps is nil unless the proxy has seen the link saturated long enough
	// to measure it.
	LinkBps    *float64
	CPUSeconds *float64
}

// SetWANProxies replaces the WAN proxy view.
func (m *Metrics) SetWANProxies(readings []WANProxyReading) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.wan.all() {
		s.begin()
	}
	for _, r := range readings {
		id := prometheus.Labels{"resource": r.Resource}
		m.wan.backlog.set(id, float64(r.BacklogBytes))
		m.wan.bufferCap.set(id, float64(r.BufferCapBytes))
		m.wan.connected.set(id, boolValue(r.Connected))
		m.wan.reconnects.set(id, float64(r.Reconnects))
		m.wan.ringFull.set(id, float64(r.RingFullEvents))
		m.wan.wireBytes.set(id, float64(r.WireBytes))
		// A ratio of 0 is "nothing sent yet", not a measurement.
		if r.CompressionRatio > 0 {
			m.wan.compression.set(id, r.CompressionRatio)
		}
		setOptional(m.wan.bufferHighWater, id, uintPtr(r.HighWaterBytes))
		setOptional(m.wan.inputBytes, id, uintPtr(r.InputBytes))
		setOptional(m.wan.inputP99, id, r.InputP99Bps)
		setOptional(m.wan.inputPeak, id, r.InputPeakBps)
		setOptional(m.wan.linkRate, id, r.LinkBps)
		setOptional(m.wan.cpuSeconds, id, r.CPUSeconds)
	}
	for _, s := range m.wan.all() {
		s.commit()
	}
	m.log().Debug("Metrics.SetWANProxies: WAN proxy view replaced", zap.Int("resources", len(readings)))
}

func setOptional(s *seriesSet, id prometheus.Labels, v *float64) {
	if v != nil {
		s.set(id, *v)
	}
}

func uintPtr(v *uint64) *float64 {
	if v == nil {
		return nil
	}
	f := float64(*v)
	return &f
}
