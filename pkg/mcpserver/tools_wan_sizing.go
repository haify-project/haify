package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

type wanSizingIn struct {
	Resource          string `json:"resource" jsonschema:"resource name; a LAN resource is fine, whether to put it on the WAN is the first question"`
	Bandwidth         string `json:"bandwidth,omitempty" jsonschema:"usable link bandwidth such as 100Mbit, 1Gbit or 12MiB/s; empty uses the proxy's own measurement when it has one"`
	Days              uint32 `json:"days,omitempty" jsonschema:"days of controller write history to weigh; 0 is 7"`
	SampleCompression bool   `json:"sample_compression,omitempty" jsonschema:"read 64 MiB off the volume's backing device on one replica and compress it with zstd -3 for a ratio on the real data"`
}

type wanSizingOut struct {
	Outcome     string   `json:"outcome" jsonschema:"link_sufficient, buffer, compress, async_backup (ship snapshots instead) or undecided"`
	Summary     string   `json:"summary"`
	AvgMiBps    float64  `json:"avg_mib_per_sec"`
	AvgSource   string   `json:"avg_source"`
	PeakMiBps   float64  `json:"peak_mib_per_sec" jsonschema:"99th percentile burst rate"`
	PeakSource  string   `json:"peak_source"`
	Ratio       float64  `json:"compression_ratio"`
	RatioSource string   `json:"ratio_source"`
	LinkMiBps   float64  `json:"link_mib_per_sec,omitempty"`
	LinkSource  string   `json:"link_source,omitempty"`
	BufferRec   uint64   `json:"recommended_buffer_bytes,omitempty"`
	BufferBasis string   `json:"buffer_basis,omitempty"`
	BufferNow   uint64   `json:"current_buffer_bytes,omitempty"`
	Compression string   `json:"recommended_compression,omitempty"`
	CompNow     string   `json:"current_compression,omitempty"`
	CPUAtPeak   float64  `json:"cpu_cores_at_peak,omitempty"`
	OutageSecs  float64  `json:"outage_secs,omitempty" jsonschema:"how long the configured buffer holds writes at the average rate while the link is down"`
	DrainSecs   float64  `json:"drain_secs,omitempty" jsonschema:"how long a full buffer takes to reach the DR site; unmount and planned switchover wait for it"`
	Notes       []string `json:"notes,omitempty"`
}

type wanTuneIn struct {
	Resource    string `json:"resource" jsonschema:"WAN resource name"`
	Buffer      string `json:"buffer,omitempty" jsonschema:"RAM each DRBD connection may queue for the WAN, such as 2GiB, or default; empty keeps it"`
	Compression string `json:"compression,omitempty" jsonschema:"off, default (zstd level 3) or zstd:<1-19>; empty keeps it"`
}

const mibF = 1 << 20

// registerWANSizingTools adds the WAN sizing and tuning tools when the client
// supports them.
func (s *Server) registerWANSizingTools(srv *mcp.Server) {
	if c, supported := s.client.(interface {
		GetWanSizing(context.Context, *haifypb.GetWanSizingRequest) (*haifypb.GetWanSizingResponse, error)
	}); supported {
		addReadWithin(s, srv, readOnlyTool("haify_wan_sizing", "Size a WAN link and the proxy buffer for a resource",
			"Weigh a resource's measured write rates (days of controller history, and the WAN proxy's per-second "+
				"figures), its compressibility and the link bandwidth, and say which setup it needs: the link carries even "+
				"the bursts; the buffer absorbs bursts the link cannot; compression is needed to keep up; or the link "+
				"cannot keep up at all and snapshots should be shipped instead. Recommends a buffer size and says what "+
				"every figure rests on."), writeTimeout,
			func(ctx context.Context, _ *mcp.CallToolRequest, in wanSizingIn) (*mcp.CallToolResult, wanSizingOut, error) {
				s.logger.Debug("haify_wan_sizing: sizing", zap.String("resource", in.Resource), zap.String("bandwidth", in.Bandwidth),
					zap.Uint32("days", in.Days), zap.Bool("sample", in.SampleCompression))
				r, err := c.GetWanSizing(ctx, &haifypb.GetWanSizingRequest{Name: in.Resource, Bandwidth: in.Bandwidth,
					Days: in.Days, SampleCompression: in.SampleCompression})
				if err != nil {
					s.logger.Debug("haify_wan_sizing: failed", zap.String("resource", in.Resource), zap.Error(err))
					return nil, wanSizingOut{}, err
				}
				s.logger.Debug("haify_wan_sizing: done", zap.String("resource", in.Resource), zap.String("outcome", r.Outcome))
				return nil, wanSizingOut{Outcome: r.Outcome, Summary: r.Summary,
					AvgMiBps: r.AvgBps / mibF, AvgSource: r.AvgSource, PeakMiBps: r.PeakBps / mibF, PeakSource: r.PeakSource,
					Ratio: r.CompressionRatio, RatioSource: r.RatioSource, LinkMiBps: r.LinkBps / mibF, LinkSource: r.LinkSource,
					BufferRec: r.RecommendedBufferBytes, BufferBasis: r.BufferBasis, BufferNow: r.CurrentBufferBytes,
					Compression: r.RecommendedCompression, CompNow: r.CurrentCompression, CPUAtPeak: r.CpuCoresAtPeak,
					OutageSecs: r.OutageSecs, DrainSecs: r.DrainSecs, Notes: r.Notes}, nil
			})
	} else {
		s.logger.Debug("Server.registerWANSizingTools: client has no sizing call, skipping")
	}

	if c, supported := s.client.(interface {
		SetWanTuning(context.Context, *haifypb.SetWanTuningRequest) (*haifypb.SetWanTuningResponse, error)
	}); supported {
		addWrite(s, srv, writeTool("haify_wan_tune", "Change a WAN resource's proxy buffer and compression",
			"Set the RAM each DRBD connection of a WAN resource may queue for the link, and the compression, then restart "+
				"its tunnels on them. Writes in flight are not lost: DRBD resyncs what the restart dropped. Use "+
				"haify_wan_sizing to choose the values."),
			func(ctx context.Context, _ *mcp.CallToolRequest, in wanTuneIn) (*mcp.CallToolResult, opResult, error) {
				s.logger.Debug("haify_wan_tune: tuning", zap.String("resource", in.Resource), zap.String("buffer", in.Buffer),
					zap.String("compression", in.Compression))
				resp, err := c.SetWanTuning(ctx, &haifypb.SetWanTuningRequest{Name: in.Resource, Buffer: in.Buffer,
					Compression: in.Compression})
				if err != nil {
					s.logger.Debug("haify_wan_tune: failed", zap.String("resource", in.Resource), zap.Error(err))
					return nil, opResult{}, err
				}
				s.logger.Debug("haify_wan_tune: done", zap.String("resource", in.Resource))
				return nil, ok(resp.Message), nil
			})
	} else {
		s.logger.Debug("Server.registerWANSizingTools: client has no tuning call, skipping")
	}
}
