package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	pb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/wanproxy"
	"github.com/haify-project/haify/pkg/wansizing"
)

// defaultSizingDays is how much controller history sizing weighs by default.
const defaultSizingDays = 7

// GetWanSizing gathers what is known about a resource's write load and its
// link, and has wansizing.Decide say which setup the workload needs and how big
// the proxy buffer should be. Everything it reports says where it came from and
// over what window, because a verdict from ten minutes of data and one from a
// week of it read the same otherwise.
func (s *Server) GetWanSizing(ctx context.Context, req *pb.GetWanSizingRequest) (*pb.GetWanSizingResponse, error) {
	log := s.serverLog()
	name := strings.TrimSpace(req.GetName())
	log.Debug("GetWanSizing: start", zap.String("resource", name), zap.String("bandwidth", req.GetBandwidth()),
		zap.Uint32("days", req.GetDays()), zap.Bool("sample_compression", req.GetSampleCompression()))
	fail := func(format string, args ...any) (*pb.GetWanSizingResponse, error) {
		msg := fmt.Sprintf(format, args...)
		log.Debug("GetWanSizing: refused", zap.String("resource", name), zap.String("message", msg))
		return &pb.GetWanSizingResponse{Success: false, Message: msg}, nil
	}
	if name == "" {
		return fail("resource name is required")
	}
	if s.ctrl.db == nil {
		return fail("database not available")
	}
	dbRes, err := s.ctrl.db.GetResource(ctx, name)
	if err != nil || dbRes == nil {
		return fail("resource %q not found", name)
	}
	var in wansizing.Input
	if b := strings.TrimSpace(req.GetBandwidth()); b != "" {
		if in.LinkBps, err = wansizing.ParseBandwidth(b); err != nil {
			return fail("%v", err)
		}
	}
	days := int(req.GetDays())
	if days <= 0 {
		days = defaultSizingDays
	}

	resp := &pb.GetWanSizingResponse{Success: true, Wan: dbRes.WANMode}
	in.History = s.sizingHistory(name, days)
	if h := in.History; h != nil {
		resp.History = &pb.WanSizingHistory{WindowSecs: h.Seconds, Days: uint32(h.Days), Samples: uint32(h.Samples),
			ResolutionSecs: h.ResolutionSecs, AvgBps: h.AvgBps, P99Bps: h.P99Bps, PeakBps: h.PeakBps,
			FirstDay: h.First, LastDay: h.Last, Node: h.Node}
	}

	sampleHost := ""
	if dbRes.WANMode {
		tuning := wanTuningOf(dbRes)
		buffer, level := tuning.Effective()
		in.BufferBytes = float64(buffer)
		resp.CurrentBufferBytes = uint64(buffer)
		resp.CurrentCompression = wanproxy.CompressionString(tuning.ZstdLevel)
		if wan, werr := s.resources.WANStatus(ctx, name); werr == nil && wan != nil && wan.Metrics != nil {
			resp.Proxy = wanMetricsProto(wan.Metrics)
			in.Proxy = proxyFigures(wan.Metrics, level)
			sampleHost = wan.MetricsHost
		} else {
			log.Debug("GetWanSizing: no proxy figures", zap.String("resource", name), zap.Error(werr))
		}
	}
	if req.GetSampleCompression() {
		resp.Sample = s.sampleCompression(ctx, dbRes, sampleHost)
		if resp.Sample.GetError() == "" && resp.Sample.GetRatio() > 0 {
			in.Sample = &wansizing.SampleFigures{Ratio: resp.Sample.Ratio, SampledBytes: float64(resp.Sample.SampledBytes),
				Chunks: int(resp.Sample.Chunks), ZeroChunks: int(resp.Sample.ZeroChunks)}
		}
	}

	r := wansizing.Decide(in)
	resp.Outcome, resp.Summary = string(r.Outcome), r.Summary
	resp.AvgBps, resp.AvgSource, resp.PeakBps, resp.PeakSource = r.AvgBps, r.AvgSource, r.PeakBps, r.PeakSource
	resp.CompressionRatio, resp.RatioSource = r.Ratio, r.RatioSource
	resp.LinkBps, resp.LinkSource = r.LinkBps, r.LinkSource
	resp.RecommendedBufferBytes, resp.BufferBasis = uint64(r.RecommendedBufferBytes), r.BufferBasis
	resp.RecommendedCompression = r.Compression
	resp.CpuCoresAtPeak, resp.OutageSecs, resp.DrainSecs, resp.Notes = r.CPUCoresAtPeak, r.OutageSecs, r.DrainSecs, r.Notes
	if !dbRes.WANMode && r.Outcome != wansizing.Undecided {
		resp.Notes = append(resp.Notes, "this resource replicates on the LAN today; haify resource add-dr puts it on the WAN")
	}
	log.Debug("GetWanSizing: done", zap.String("resource", name), zap.String("outcome", resp.Outcome),
		zap.Bool("has_history", in.History != nil), zap.Bool("has_proxy", in.Proxy != nil), zap.Bool("has_sample", in.Sample != nil))
	return resp, nil
}

// sizingHistory is the resource's write history: from the live poll when it
// runs, from the database otherwise.
func (s *Server) sizingHistory(resource string, days int) *wansizing.Window {
	w := s.ctrl.writeHistory
	if w == nil {
		w = newWriteHistory(s.ctrl)
	}
	return w.Summary(resource, days)
}

// proxyFigures converts what a proxy published into sizing input; nil for a
// proxy too old to publish rates.
func proxyFigures(m *wanproxy.Metrics, zstdLevel int) *wansizing.ProxyFigures {
	if !m.HasRates() {
		zap.L().Debug("proxyFigures: proxy publishes no rates", zap.String("version", m.Version))
		return nil
	}
	u := func(p *uint64) float64 {
		if p == nil {
			return 0
		}
		return float64(*p)
	}
	f := func(p *float64) float64 {
		if p == nil {
			return 0
		}
		return *p
	}
	return &wansizing.ProxyFigures{
		UptimeSecs: u(m.UptimeSecs), RateSamplesSecs: u(m.RateSamplesSecs),
		InputAvgBps: f(m.InputAvgBps), InputP99Bps: f(m.InputP99Bps), InputPeakBps: f(m.InputPeakBps),
		HighWaterBytes: u(m.BufferHighWaterBytes), LongestBurstSecs: u(m.LongestBurstSecs),
		LinkP50Bps: f(m.LinkP50Bps), LinkSamplesSecs: u(m.LinkSamplesSecs),
		CompressionRatio: m.CompressionRat, SentBytes: float64(m.DRBDToWANBytes), InBytes: u(m.DRBDInBytes),
		CPUSeconds: f(m.CPUSeconds), ZstdLevel: zstdLevel,
	}
}

// wanMetricsProto renders proxy counters for the API. Figures an older proxy
// does not publish stay unset.
func wanMetricsProto(m *wanproxy.Metrics) *pb.WANMetrics {
	out := &pb.WANMetrics{
		BufferUsedBytes: m.BufferUsedBytes, BufferCapBytes: m.BufferCapBytes, BufferFillPercent: m.BufferFillPercent,
		DrbdToWanBytes: m.DRBDToWANBytes, WanWireBytes: m.WANWireBytes, WanToDrbdBytes: m.WANToDRBDBytes,
		FramesSent: m.FramesSent, CompressionRatio: m.CompressionRat, WanConnected: m.WANConnected,
		Reconnects: m.Reconnects, RingFullEvents: m.RingFullEvents,
		BufferHighWaterBytes: m.BufferHighWaterBytes, InputAvgBps: m.InputAvgBps, InputP99Bps: m.InputP99Bps,
		InputPeakBps: m.InputPeakBps, RateWindowSecs: m.RateSamplesSecs, LongestBurstSecs: m.LongestBurstSecs,
		CpuSeconds: m.CPUSeconds, ProxyVersion: m.Version,
	}
	if m.LinkSamplesSecs != nil && *m.LinkSamplesSecs >= wanLinkMinSamples {
		out.LinkBps = m.LinkP50Bps
	}
	return out
}

// Compressibility sampling.
//
// What the proxy achieves on live traffic is the best ratio there is, but a
// resource that is not on the WAN yet has none, and one whose proxy runs with
// compression off has only framing overhead. So this reads 16 chunks of 4 MiB
// spread over the volume's backing device on one replica and compresses each
// with zstd -3, the proxy's default level. It reads the backing device, not
// the DRBD device, so it works on a Secondary and never promotes anything.
// Chunks that are all zeros are never-written space on a thin volume: they
// compress to nothing and would flatter the ratio, so they are counted and
// left out.
const (
	sampleChunks     = 16
	sampleChunkBytes = 4 << 20
)

const sampleScript = `set -u
command -v zstd >/dev/null 2>&1 || { echo "zstd is not installed on this node"; exit 3; }
devs=$(drbdadm sh-ll-dev %s 2>/dev/null | grep '^/dev/')
[ -n "$devs" ] || { echo "no backing device for %s on this node"; exit 4; }
n=$(echo "$devs" | wc -l)
per=$(( (%d + n - 1) / n ))
t=$(mktemp)
trap 'rm -f "$t"' EXIT
for d in $devs; do
  chunks=$(( $(blockdev --getsize64 "$d") / %d ))
  [ "$chunks" -gt 1 ] || continue
  i=0
  while [ "$i" -lt "$per" ]; do
    dd if="$d" of="$t" bs=%d skip=$(( chunks * i / per )) count=1 iflag=direct status=none || exit 5
    raw=$(stat -c %%s "$t")
    z=0; cmp -s -n "$raw" "$t" /dev/zero && z=1
    echo "chunk $raw $(zstd -3 -q -c "$t" | wc -c) $z"
    i=$(( i + 1 ))
  done
done
`

func (s *Server) sampleCompression(ctx context.Context, dbRes *database.Resource, host string) *pb.WanSizingSample {
	log := s.serverLog()
	out := &pb.WanSizingSample{}
	if host == "" {
		host = s.sampleHostFor(dbRes)
	}
	out.Node = host
	if host == "" || s.resources.deployment == nil {
		out.Error = "no replica to sample from"
		return out
	}
	script := fmt.Sprintf(sampleScript, dbRes.Name, dbRes.Name, sampleChunks, sampleChunkBytes, sampleChunkBytes)
	start := time.Now()
	res, err := s.resources.deployment.Exec(ctx, []string{host}, "echo "+base64Std(script)+" | base64 -d | sudo /bin/bash")
	log.Debug("sampleCompression: ran", zap.String("resource", dbRes.Name), zap.String("host", host),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		out.Error = err.Error()
		return out
	}
	output, ok := "", false
	for _, h := range res.Hosts {
		output, ok = h.Output, h.Success
	}
	if !ok {
		out.Error = strings.TrimSpace(output)
		if out.Error == "" {
			out.Error = "sampling failed"
		}
		return out
	}
	var raw, comp float64
	for _, line := range strings.Split(output, "\n") {
		f := strings.Fields(line)
		if len(f) != 4 || f[0] != "chunk" {
			continue
		}
		r, _ := strconv.ParseFloat(f[1], 64)
		c, _ := strconv.ParseFloat(f[2], 64)
		out.Chunks++
		if f[3] == "1" {
			out.ZeroChunks++
			continue
		}
		raw += r
		comp += c
	}
	out.SampledBytes = uint64(raw)
	switch {
	case out.Chunks == 0:
		out.Error = "the volume is too small to sample"
	case comp == 0:
		out.Error = "every sampled chunk was never-written space; write some data first"
	default:
		out.Ratio = raw / comp
	}
	log.Debug("sampleCompression: parsed", zap.String("resource", dbRes.Name), zap.Uint32("chunks", out.Chunks),
		zap.Uint32("zero_chunks", out.ZeroChunks), zap.Float64("ratio", out.Ratio), zap.String("error", out.Error))
	return out
}

// sampleHostFor picks a diskful replica to read: the first listed node that is
// not a tiebreaker or diskless client.
func (s *Server) sampleHostFor(dbRes *database.Resource) string {
	diskless := disklessByDesign(s.ctrl, dbRes)
	for _, n := range splitCSV(dbRes.Nodes) {
		if n != "" && !diskless[n] {
			return s.ctrl.ResolveHost(n)
		}
	}
	return ""
}
