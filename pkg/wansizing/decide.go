package wansizing

import (
	"fmt"
	"math"
)

// Outcome is which of the four WAN replication setups a workload needs.
type Outcome string

const (
	// LinkSufficient: the link carries even the bursts. The proxy buffer only
	// rides out outages, and compression is optional.
	LinkSufficient Outcome = "link_sufficient"
	// Buffer: bursts outrun the link but the average does not, so the buffer
	// absorbs them and drains between bursts.
	Buffer Outcome = "buffer"
	// Compress: only compression brings the average under the link; the buffer
	// absorbs what is left of the bursts.
	Compress Outcome = "compress"
	// AsyncBackup: even compressed, the average outruns the link. The DR copy
	// would fall further behind for good; ship snapshots on a schedule instead.
	AsyncBackup Outcome = "async_backup"
	// Undecided: no link bandwidth to compare against, or no write rates.
	Undecided Outcome = "undecided"
)

// ProxyFigures is what a running haify-proxy measured since it started.
type ProxyFigures struct {
	UptimeSecs       float64
	RateSamplesSecs  float64
	InputAvgBps      float64
	InputP99Bps      float64
	InputPeakBps     float64
	HighWaterBytes   float64
	LongestBurstSecs float64
	LinkP50Bps       float64
	LinkSamplesSecs  float64
	// CompressionRatio is payload over wire bytes; SentBytes the payload behind
	// it. With compression off it only reflects framing overhead.
	CompressionRatio float64
	SentBytes        float64
	InBytes          float64
	CPUSeconds       float64
	// ZstdLevel is the proxy's effective level, 0 when compression is off.
	ZstdLevel int
}

// SampleFigures is a compressibility sample read off the volume itself.
type SampleFigures struct {
	Ratio        float64
	SampledBytes float64
	Chunks       int
	// ZeroChunks were all zeros, never-written space on a thin volume, and are
	// left out of Ratio: they would compress to nothing and flatter the result.
	ZeroChunks int
}

// Input is everything Decide weighs. Any part may be missing.
type Input struct {
	History *Window
	Proxy   *ProxyFigures
	Sample  *SampleFigures
	// LinkBps is the link bandwidth the operator states, bytes per second;
	// 0 when not given.
	LinkBps float64
	// BufferBytes is the per-connection buffer the proxy is configured with
	// (0 when the resource has no proxy).
	BufferBytes float64
}

// Result is the verdict and what it rests on.
type Result struct {
	Outcome Outcome
	Summary string

	AvgBps, PeakBps       float64
	AvgSource, PeakSource string
	Ratio                 float64
	RatioSource           string
	LinkBps               float64
	LinkSource            string

	// RecommendedBufferBytes is 0 when the buffer is irrelevant to the outcome.
	RecommendedBufferBytes float64
	BufferBasis            string
	// Compression is the recommended setting: "off", "zstd:3" or "" when it
	// does not matter.
	Compression string
	// CPUCoresAtPeak is the proxy's measured CPU per byte times PeakBps; 0 when
	// the proxy has not moved enough data to say.
	CPUCoresAtPeak float64
	// OutageSecs is how long the configured buffer holds writes at the average
	// rate while the link is down; 0 when unknown.
	OutageSecs float64
	// DrainSecs is how long a full configured buffer takes to reach the DR
	// site. Closing the device (unmount, VM shutdown, a planned switchover)
	// waits for the backlog, so this bounds how long that can take.
	DrainSecs float64

	Notes []string
}

const (
	// minRatioBytes is how much a ratio needs behind it to be trusted.
	minRatioBytes = 64 << 20
	// minLinkSamples is how many saturated seconds make a link measurement.
	minLinkSamples = 10
	// bufferFloor is the smallest buffer worth recommending.
	bufferFloor = 256 << 20
	// bufferCeiling is the most DRBD can use: it goes Ahead once
	// congestion-fill is in flight and takes at most 10 GiB for it.
	bufferCeiling = 10 << 30
	// dayHistorySecs: less history than this misses the daily cycle.
	dayHistorySecs = 24 * 3600
)

// Decide applies the four rules (peak within the link; average within it;
// compressed average within it; otherwise ship snapshots) to the best figures
// the input has, and sizes the buffer.
func Decide(in Input) Result {
	var r Result
	pickRates(&r, in)
	pickRatio(&r, in)
	pickLink(&r, in)

	if r.AvgBps <= 0 && r.PeakBps <= 0 {
		r.Outcome = Undecided
		r.Summary = "no write rates measured yet: the controller records them on every health poll, " +
			"and a WAN resource's proxy publishes them from the moment it starts"
		return r
	}
	ratio := math.Max(r.Ratio, 1)
	if r.LinkBps <= 0 {
		r.Outcome = Undecided
		r.Summary = fmt.Sprintf("no link bandwidth to compare against: the link needs %s for the bursts, %s for the "+
			"average, or %s with compression; give --bandwidth", Rate(r.PeakBps), Rate(r.AvgBps), Rate(r.AvgBps/ratio))
		return r
	}

	switch {
	case r.PeakBps <= r.LinkBps:
		r.Outcome = LinkSufficient
		r.Summary = fmt.Sprintf("the link (%s) carries even the bursts (%s): the buffer only has to ride out outages, "+
			"and compression is optional", Rate(r.LinkBps), Rate(r.PeakBps))
		r.Compression = "off"
	case r.AvgBps <= r.LinkBps:
		r.Outcome = Buffer
		r.Summary = fmt.Sprintf("bursts (%s) outrun the link (%s) but the average (%s) does not: the buffer absorbs "+
			"them and drains in between", Rate(r.PeakBps), Rate(r.LinkBps), Rate(r.AvgBps))
		r.Compression = "off"
	case r.AvgBps/ratio <= r.LinkBps:
		r.Outcome = Compress
		r.Summary = fmt.Sprintf("the average (%s) outruns the link (%s); compressed %.1f:1 it is %s, which fits, "+
			"and the buffer absorbs the bursts", Rate(r.AvgBps), Rate(r.LinkBps), ratio, Rate(r.AvgBps/ratio))
		r.Compression = "zstd:3"
	default:
		r.Outcome = AsyncBackup
		r.Summary = fmt.Sprintf("even compressed (%.1f:1) the average (%s) outruns the link (%s): the DR copy would "+
			"fall further behind for good. Ship snapshots on a schedule (haify backup schedule) instead, or get a "+
			"faster link", ratio, Rate(r.AvgBps/ratio), Rate(r.LinkBps))
		return r
	}
	if r.Ratio > 1.2 && r.Compression == "off" {
		r.Notes = append(r.Notes, fmt.Sprintf("compression is not needed to keep up, but at %.1f:1 it shortens the "+
			"backlog after a burst or an outage by as much; keep it if the CPU is there", r.Ratio))
	}
	sizeBuffer(&r, in, ratio)
	cpuAtPeak(&r, in)
	return r
}

// pickRates takes the average from whichever source covers more time, and the
// burst rate as the larger 99th percentile: the controller's history covers
// days but averages each poll interval, the proxy sees every second but only
// since it started.
func pickRates(r *Result, in Input) {
	h, p := in.History, in.Proxy
	if h != nil && h.Samples > 0 {
		r.AvgBps, r.AvgSource = h.AvgBps, describeHistory(h)
		r.PeakBps, r.PeakSource = math.Min(h.P99Bps, h.PeakBps), "99th percentile of "+describeHistory(h)
		if h.Seconds < dayHistorySecs {
			r.Notes = append(r.Notes, fmt.Sprintf("the controller has %s of write history for this resource; "+
				"measure for days, including the busiest ones, before relying on this", Duration(h.Seconds)))
		}
	}
	if p != nil && p.RateSamplesSecs > 0 {
		if r.AvgSource == "" || p.UptimeSecs > h.Seconds {
			r.AvgBps, r.AvgSource = p.InputAvgBps, fmt.Sprintf("the proxy, since it started %s ago", Duration(p.UptimeSecs))
		}
		p99 := math.Min(p.InputP99Bps, p.InputPeakBps)
		if p99 >= r.PeakBps {
			r.PeakBps = p99
			r.PeakSource = fmt.Sprintf("99th percentile of the proxy's per-second samples over %s", Duration(p.RateSamplesSecs))
		}
		if p.InputPeakBps > r.PeakBps {
			r.Notes = append(r.Notes, fmt.Sprintf("the busiest single second was %s", Rate(p.InputPeakBps)))
		}
	}
	if h != nil && h.Samples > 0 && (p == nil || p.RateSamplesSecs == 0) {
		r.Notes = append(r.Notes, fmt.Sprintf("the burst rate is a %s average; bursts shorter than that are "+
			"averaged away, so the real peak is higher", Duration(h.ResolutionSecs)))
	}
}

func describeHistory(h *Window) string {
	return fmt.Sprintf("%s of controller history (%d polls, one per %s)", Duration(h.Seconds), h.Samples,
		Duration(h.ResolutionSecs))
}

// pickRatio prefers what the proxy achieved on the live stream, then a sample
// of the volume, then assumes nothing.
func pickRatio(r *Result, in Input) {
	if p := in.Proxy; p != nil && p.ZstdLevel > 0 && p.SentBytes >= minRatioBytes && p.CompressionRatio > 0 {
		r.Ratio = p.CompressionRatio
		r.RatioSource = fmt.Sprintf("achieved by the proxy on %s of replication traffic", Bytes(p.SentBytes))
		return
	}
	if s := in.Sample; s != nil && s.Ratio > 0 {
		r.Ratio = s.Ratio
		r.RatioSource = fmt.Sprintf("zstd -3 on %s sampled from %d places on the volume", Bytes(s.SampledBytes), s.Chunks-s.ZeroChunks)
		switch {
		case s.ZeroChunks == 1:
			r.RatioSource += " (1 never-written chunk of zeros left out)"
		case s.ZeroChunks > 1:
			r.RatioSource += fmt.Sprintf(" (%d never-written chunks of zeros left out)", s.ZeroChunks)
		}
		return
	}
	r.Ratio = 1
	r.RatioSource = "unknown, taken as 1:1"
	r.Notes = append(r.Notes, "the compression ratio is not known yet: run with --sample-compression, "+
		"or let the proxy move some data with compression on")
}

// pickLink prefers the operator's figure, which can come from a proper
// measurement such as iperf3, over the proxy's passive one, and says when they
// disagree.
func pickLink(r *Result, in Input) {
	measured := 0.0
	if p := in.Proxy; p != nil && p.LinkSamplesSecs >= minLinkSamples {
		measured = p.LinkP50Bps
	}
	switch {
	case in.LinkBps > 0:
		r.LinkBps, r.LinkSource = in.LinkBps, "given"
		if measured > 0 && math.Abs(measured-in.LinkBps)/in.LinkBps > 0.3 {
			r.Notes = append(r.Notes, fmt.Sprintf("the proxy measured the link at %s while it was saturated, "+
				"against the %s given", Rate(measured), Rate(in.LinkBps)))
		}
	case measured > 0:
		r.LinkBps = measured
		r.LinkSource = fmt.Sprintf("measured by the proxy over %s in which a backlog waited for the link",
			Duration(in.Proxy.LinkSamplesSecs))
	}
}

// sizeBuffer recommends a per-connection buffer: twice what a burst leaves
// queued, where a burst lasts as long as the proxy has seen one last, at the
// rate it exceeds what the link drains. Twice the backlog the proxy has
// actually held is a floor, since that one is a measurement and not a model.
func sizeBuffer(r *Result, in Input, ratio float64) {
	drain := r.LinkBps
	if r.Compression != "off" {
		drain *= ratio
	}
	burstSecs, burstBasis := 60.0, "an assumed 60 s burst"
	if p := in.Proxy; p != nil && p.LongestBurstSecs > 0 {
		burstSecs = p.LongestBurstSecs
		burstBasis = fmt.Sprintf("the longest burst the proxy saw (%s)", Duration(burstSecs))
	} else if h := in.History; h != nil && h.ResolutionSecs > 0 && (p == nil || p.RateSamplesSecs == 0) {
		burstSecs = h.ResolutionSecs
		burstBasis = fmt.Sprintf("a burst as long as one poll interval (%s)", Duration(burstSecs))
	}
	model := 2 * math.Max(0, r.PeakBps-drain) * burstSecs
	basis := fmt.Sprintf("2 x (%s burst - %s drained) x %s", Rate(r.PeakBps), Rate(drain), burstBasis)
	if p := in.Proxy; p != nil && 2*p.HighWaterBytes > model {
		model = 2 * p.HighWaterBytes
		basis = fmt.Sprintf("2 x the largest backlog the proxy has held (%s)", Bytes(p.HighWaterBytes))
	}
	rec := math.Max(model, bufferFloor)
	r.RecommendedBufferBytes = math.Ceil(rec/bufferFloor) * bufferFloor
	r.BufferBasis = basis
	if r.RecommendedBufferBytes > bufferCeiling {
		r.RecommendedBufferBytes = bufferCeiling
		r.Notes = append(r.Notes, fmt.Sprintf("the bursts need %s of buffer, more than DRBD can keep in flight "+
			"(10 GiB): the longest of them will push DRBD into Ahead and a resync, or need a faster link", Bytes(rec)))
	}
	if in.BufferBytes > 0 {
		if in.BufferBytes < r.RecommendedBufferBytes {
			r.Notes = append(r.Notes, fmt.Sprintf("the configured buffer (%s) is smaller: raise it with haify wan tune "+
				"--buffer, or bursts will push DRBD into Ahead and a resync", Bytes(in.BufferBytes)))
		}
		if r.AvgBps > 0 {
			r.OutageSecs = in.BufferBytes / r.AvgBps
		}
		if drain > 0 {
			r.DrainSecs = in.BufferBytes / drain
		}
	}
}

// cpuAtPeak scales the proxy's measured CPU per byte up to the burst rate. It
// covers everything the proxy does, TLS included, not compression alone.
func cpuAtPeak(r *Result, in Input) {
	p := in.Proxy
	if p == nil || p.InBytes < minRatioBytes || p.CPUSeconds <= 0 {
		return
	}
	r.CPUCoresAtPeak = p.CPUSeconds / p.InBytes * r.PeakBps
}

// Rate renders bytes per second as MiB/s and Mbit/s, the two units link and
// disk figures are quoted in.
func Rate(bps float64) string {
	return fmt.Sprintf("%.1f MiB/s (%.0f Mbit/s)", bps/(1<<20), bps*8/1e6)
}

// Bytes renders a byte count in binary units.
func Bytes(b float64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MiB", b/(1<<20))
	default:
		return fmt.Sprintf("%.0f KiB", b/(1<<10))
	}
}

// Duration renders seconds the way an operator reads a window.
func Duration(secs float64) string {
	switch {
	case secs >= 2*86400:
		return fmt.Sprintf("%.1f days", secs/86400)
	case secs >= 2*3600:
		return fmt.Sprintf("%.1f h", secs/3600)
	case secs >= 120:
		return fmt.Sprintf("%.0f min", secs/60)
	default:
		return fmt.Sprintf("%.0f s", secs)
	}
}
