package wanproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// NodeProxyState is one node's view of its haify-proxy instance.
type NodeProxyState struct {
	Host   string
	Active bool // systemd reports the per-resource unit as active
}

// Metrics is the counter snapshot haify-proxy publishes. Field tags mirror the
// JSON that `haify_proxy::metrics::Snapshot` serializes — renaming one on either
// side breaks the contract.
type Metrics struct {
	// BufferUsedBytes is the un-replicated backlog: writes the local DRBD has
	// already acknowledged upstream that have NOT reached the DR site. Under
	// protocol A this IS the data-loss window if the primary is lost now, which
	// is the whole reason these metrics exist.
	BufferUsedBytes   uint64  `json:"buffer_used_bytes"`
	BufferCapBytes    uint64  `json:"buffer_cap_bytes"`
	BufferFillPercent float64 `json:"buffer_fill_percent"`

	DRBDToWANBytes uint64  `json:"drbd_to_wan_bytes"`
	WANWireBytes   uint64  `json:"wan_wire_bytes"`
	WANToDRBDBytes uint64  `json:"wan_to_drbd_bytes"`
	FramesSent     uint64  `json:"frames_sent"`
	CompressionRat float64 `json:"compression_ratio"`

	WANConnected   bool   `json:"wan_connected"`
	Reconnects     uint64 `json:"reconnects"`
	RingFullEvents uint64 `json:"ring_full_events"`

	// The fields below are what sizing a link and the buffer needs, published
	// by haify-proxy 0.2 and later. They are pointers because an older proxy
	// does not publish them, and an absent figure must read as unknown, never
	// as a measured zero.

	// Version is the proxy's version; empty for a proxy older than 0.2.
	Version    string   `json:"version,omitempty"`
	UptimeSecs *uint64  `json:"uptime_secs,omitempty"`
	CPUSeconds *float64 `json:"cpu_seconds,omitempty"`
	// BufferHighWaterBytes is the largest backlog since the proxy started.
	BufferHighWaterBytes *uint64 `json:"buffer_high_water_bytes,omitempty"`
	// DRBDInBytes counts what the local DRBD handed over, on enqueue, so unlike
	// DRBDToWANBytes it is not capped by the link.
	DRBDInBytes *uint64 `json:"drbd_in_bytes,omitempty"`
	// RateSamplesSecs is how many one-second samples the rates below rest on;
	// they all cover the time since the proxy started.
	RateSamplesSecs *uint64  `json:"rate_samples_secs,omitempty"`
	InputAvgBps     *float64 `json:"input_avg_bps,omitempty"`
	InputP99Bps     *float64 `json:"input_p99_bps,omitempty"`
	InputPeakBps    *float64 `json:"input_peak_bps,omitempty"`
	// LinkP50Bps is the median wire rate over the LinkSamplesSecs seconds in
	// which a backlog waited for the link: its usable bandwidth, measured
	// without sending anything extra. No samples means the link has never been
	// the bottleneck, and then it says nothing about the link.
	LinkSamplesSecs *uint64  `json:"link_samples_secs,omitempty"`
	LinkP50Bps      *float64 `json:"link_p50_bps,omitempty"`
	// LongestBurstSecs is the longest run of seconds in which the backlog grew
	// with the link up: how long a burst outran the link, not counting the
	// drain after it.
	LongestBurstSecs *uint64 `json:"longest_burst_secs,omitempty"`
}

// HasRates reports whether the proxy published the per-second rate figures
// (haify-proxy 0.2 and later).
func (m *Metrics) HasRates() bool {
	return m != nil && m.RateSamplesSecs != nil && m.InputP99Bps != nil && m.InputPeakBps != nil
}

// ProxyStatus is the health of one WAN resource's proxy pair, suitable for
// surfacing in `resource status`, the alert monitor and the UI.
type ProxyStatus struct {
	Resource     string
	Primary      NodeProxyState
	DR           NodeProxyState
	WANReachable bool // the primary can currently reach the DR WAN endpoint

	// PrimaryMetrics is the primary side's published counters, or nil when the
	// snapshot could not be read (an older proxy build, a proxy that has not
	// published its first tick yet, or an unreachable node). Callers must treat
	// nil as "unknown", never as zero — reporting a zero backlog when the truth
	// is unknown is exactly the wrong way to be wrong here.
	PrimaryMetrics *Metrics
}

// Healthy reports whether both proxy instances are active and the WAN leg is
// reachable — the condition for the WAN resource to actually replicate.
func (s *ProxyStatus) Healthy() bool {
	return s != nil && s.Primary.Active && s.DR.Active && s.WANReachable
}

// Status reports the live health of a WAN resource's proxy pair: whether the
// haify-proxy@<resource> unit is active on each node, and whether the primary can
// currently reach the DR WAN endpoint. It is read-only (no sudo) and does a
// single reachability attempt so a status query never blocks on retries.
func Status(ctx context.Context, deploy DeploymentClient, spec ProxySpec) (*ProxyStatus, error) {
	log := zap.L()
	log.Debug("Status: start", zap.String("resource", spec.Resource), zap.String("primary", spec.PrimaryNodeAddr),
		zap.String("dr", spec.DRNodeAddr))
	if deploy == nil {
		log.Debug("Status: no deployment client", zap.String("resource", spec.Resource))
		return nil, fmt.Errorf("wanproxy: deployment client is nil")
	}
	if err := spec.Validate(); err != nil {
		log.Debug("Status: spec invalid", zap.String("resource", spec.Resource), zap.Error(err))
		return nil, err
	}

	instance := UnitInstance(spec.Resource)
	st := &ProxyStatus{
		Resource: spec.Resource,
		Primary:  NodeProxyState{Host: spec.PrimaryNodeAddr},
		DR:       NodeProxyState{Host: spec.DRNodeAddr},
	}

	start := time.Now()
	res, err := deploy.Exec(ctx, []string{spec.PrimaryNodeAddr, spec.DRNodeAddr},
		fmt.Sprintf("systemctl is-active %s", instance))
	log.Debug("Status: queried unit state", zap.String("unit", instance), zap.Error(err), zap.Duration("took", time.Since(start)))
	if err != nil {
		return nil, fmt.Errorf("wanproxy: query proxy status: %w", err)
	}
	if res != nil {
		if h := res.Hosts[spec.PrimaryNodeAddr]; h != nil {
			st.Primary.Active = h.Success
		}
		if h := res.Hosts[spec.DRNodeAddr]; h != nil {
			st.DR.Active = h.Success
		}
	}

	// Single-shot reachability: a status query must not block on the retry loop.
	st.WANReachable = Reachable(ctx, deploy, spec)

	// Counters from the primary — the side that holds the backlog. A failure here
	// leaves PrimaryMetrics nil ("unknown") rather than failing the status call:
	// losing observability must not make the resource look broken.
	st.PrimaryMetrics = readMetrics(ctx, deploy, spec.PrimaryNodeAddr, spec.Resource)

	log.Debug("Status: done", zap.String("resource", spec.Resource), zap.Bool("primary_active", st.Primary.Active),
		zap.Bool("dr_active", st.DR.Active), zap.Bool("wan_reachable", st.WANReachable),
		zap.Bool("has_metrics", st.PrimaryMetrics != nil))
	return st, nil
}

// ReadNodeMetrics fetches and parses one node's published snapshot for a leg.
// Returns nil when it cannot be read, which callers must treat as "unknown".
func ReadNodeMetrics(ctx context.Context, deploy DeploymentClient, host, legID string) *Metrics {
	return readMetrics(ctx, deploy, host, legID)
}

// readMetrics fetches and parses a node's published snapshot. Returns nil when
// it cannot be read or parsed — an older proxy, a first tick that has not landed
// yet, or an unreachable node all look the same from here and all mean "unknown".
func readMetrics(ctx context.Context, deploy DeploymentClient, host, resource string) *Metrics {
	log := zap.L()
	if deploy == nil || host == "" {
		log.Debug("readMetrics: no client or host", zap.String("resource", resource), zap.String("host", host))
		return nil
	}
	start := time.Now()
	res, err := deploy.Exec(ctx, []string{host}, fmt.Sprintf("cat %s", NodeMetricsPath(resource)))
	log.Debug("readMetrics: read snapshot", zap.String("host", host), zap.String("resource", resource),
		zap.Error(err), zap.Duration("took", time.Since(start)))
	if err != nil || res == nil {
		return nil
	}
	hres := res.Hosts[host]
	if hres == nil || !hres.Success {
		log.Debug("readMetrics: snapshot not readable on host", zap.String("host", host), zap.String("resource", resource))
		return nil
	}
	return ParseMetrics(hres.Output)
}

// ParseMetrics decodes a published snapshot. Returns nil for anything that is not
// a usable document, so a truncated or garbage read is reported as unknown rather
// than as a resource with no backlog.
func ParseMetrics(s string) *Metrics {
	s = strings.TrimSpace(s)
	if s == "" {
		zap.L().Debug("ParseMetrics: empty snapshot")
		return nil
	}
	var m Metrics
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		zap.L().Debug("ParseMetrics: snapshot does not parse", zap.Int("bytes", len(s)), zap.Error(err))
		return nil
	}
	zap.L().Debug("ParseMetrics: parsed", zap.Uint64("buffer_used_bytes", m.BufferUsedBytes),
		zap.Bool("wan_connected", m.WANConnected), zap.Uint64("reconnects", m.Reconnects))
	return &m
}
