package main

import (
	"context"
	"fmt"
	"io"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"google.golang.org/protobuf/encoding/protojson"
)

func wanSizingCommand() *cobra.Command {
	var req haifypb.GetWanSizingRequest
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "sizing <resource> [--bandwidth 100Mbit] [--days 7] [--sample-compression]",
		Short: "Say whether a link carries a resource's writes, and how big the proxy buffer should be",
		Long: "Weighs what has been measured of the resource's write load against a link bandwidth:\n\n" +
			"  link_sufficient  the link carries even the bursts; the buffer only rides out outages\n" +
			"  buffer           bursts outrun the link, the average does not; the buffer absorbs them\n" +
			"  compress         only compression brings the average under the link\n" +
			"  async_backup     even compressed the link cannot keep up; ship snapshots on a schedule\n\n" +
			"The write load comes from the controller's history of the resource's DRBD write counter\n" +
			"(recorded on every health poll, for any resource, so a LAN resource can be sized before\n" +
			"it goes on the WAN) and, for a WAN resource, from its proxy's per-second figures. The\n" +
			"compression ratio comes from the proxy, or from --sample-compression, which reads 64 MiB\n" +
			"off the backing device on one replica. Without --bandwidth the proxy's own measurement of\n" +
			"the link is used when it has one; iperf3 between the sites gives a better figure.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			req.Name = args[0]
			log.Debug("wanSizingCommand: running", zap.String("resource", req.Name), zap.String("bandwidth", req.Bandwidth),
				zap.Uint32("days", req.Days), zap.Bool("sample", req.SampleCompression), zap.Bool("json", asJSON))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(c)
			resp, err := c.GetWanSizing(ctx, &req)
			if err != nil {
				log.Debug("wanSizingCommand: GetWanSizing failed", zap.Error(err))
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				data, err := protojson.MarshalOptions{Multiline: true, EmitUnpopulated: false}.Marshal(resp)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintln(out, string(data))
				return nil
			}
			printWANSizing(out, resp)
			return nil
		},
	}
	cmd.Flags().StringVar(&req.Bandwidth, "bandwidth", "", "Usable link bandwidth, such as 100Mbit, 1Gbit or 12MiB/s")
	cmd.Flags().Uint32Var(&req.Days, "days", 0, "Days of controller history to weigh (default 7)")
	cmd.Flags().BoolVar(&req.SampleCompression, "sample-compression", false,
		"Read 64 MiB off the backing device on one replica and compress it with zstd -3")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print the full answer as JSON")
	return cmd
}

// printWANSizing renders the verdict first, then what it rests on.
func printWANSizing(out io.Writer, r *haifypb.GetWanSizingResponse) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format, args...) }
	rate := func(bps float64) string {
		return fmt.Sprintf("%.1f MiB/s (%.0f Mbit/s)", bps/(1<<20), bps*8/1e6)
	}
	p("Outcome: %s\n  %s\n\n", r.GetOutcome(), r.GetSummary())
	if r.GetAvgSource() != "" {
		p("Average write:  %s — %s\n", rate(r.GetAvgBps()), r.GetAvgSource())
	}
	if r.GetPeakSource() != "" {
		p("Burst write:    %s — %s\n", rate(r.GetPeakBps()), r.GetPeakSource())
	}
	p("Compression:    %.2f:1 — %s\n", r.GetCompressionRatio(), r.GetRatioSource())
	if r.GetLinkBps() > 0 {
		p("Link:           %s — %s\n", rate(r.GetLinkBps()), r.GetLinkSource())
	}
	if b := r.GetRecommendedBufferBytes(); b > 0 {
		p("\nBuffer:         %s per connection recommended (%s)\n", humanBytes(b), r.GetBufferBasis())
		if cur := r.GetCurrentBufferBytes(); cur > 0 {
			p("                %s configured", humanBytes(cur))
			if s := r.GetOutageSecs(); s > 0 {
				p("; holds about %s of writes at the average rate while the link is down", (time.Duration(s) * time.Second).String())
			}
			p("\n")
			if s := r.GetDrainSecs(); s > 0 {
				p("                a full buffer takes about %s to reach the DR site; unmounting or switching over waits for it\n",
					(time.Duration(s) * time.Second).String())
			}
		}
	}
	if c := r.GetRecommendedCompression(); c != "" {
		p("Compression:    %s recommended", c)
		if cur := r.GetCurrentCompression(); cur != "" {
			p(" (now %s)", cur)
		}
		p("\n")
	}
	if c := r.GetCpuCoresAtPeak(); c > 0 {
		p("Proxy CPU:      about %.2f cores at the burst rate, from what it has used so far\n", c)
	}
	if s := r.GetSample(); s != nil {
		if s.GetError() != "" {
			p("Sample:         not taken on %s: %s\n", s.GetNode(), s.GetError())
		} else if s.GetRatio() > 0 {
			p("Sample:         %.2f:1 on %s read from %s\n", s.GetRatio(), humanBytes(s.GetSampledBytes()), s.GetNode())
		}
	}
	if len(r.GetNotes()) > 0 {
		p("\nNotes:\n")
		for _, n := range r.GetNotes() {
			p("  - %s\n", n)
		}
	}
	if r.GetWan() && r.GetRecommendedBufferBytes() > 0 {
		p("\nApply with: haify wan tune <resource> --buffer <size> --compression <off|zstd:N>\n")
	}
}

func wanTuneCommand() *cobra.Command {
	var req haifypb.SetWanTuningRequest
	cmd := &cobra.Command{
		Use:   "tune <resource> [--buffer 2GiB|default] [--compression off|default|zstd:N]",
		Short: "Change a WAN resource's proxy buffer and compression",
		Long: "--buffer is the RAM each DRBD connection of a leg may queue for the link (default\n" +
			"4 GiB). When it fills, DRBD goes Ahead and catches up with a resync of the blocks it\n" +
			"marked. --compression is off, default (zstd level 3) or zstd:<1-19>.\n\n" +
			"The tunnels restart on the new settings. Nothing is lost: DRBD resyncs what was in\n" +
			"flight. haify wan sizing suggests the values.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			req.Name = args[0]
			log.Debug("wanTuneCommand: running", zap.String("resource", req.Name), zap.String("buffer", req.Buffer),
				zap.String("compression", req.Compression))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(c)
			resp, err := c.SetWanTuning(ctx, &req)
			if err != nil {
				log.Debug("wanTuneCommand: SetWanTuning failed", zap.Error(err))
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), resp.Message)
			return nil
		},
	}
	cmd.Flags().StringVar(&req.Buffer, "buffer", "", "Buffer per connection, such as 2GiB, or default")
	cmd.Flags().StringVar(&req.Compression, "compression", "", "off, default or zstd:<1-19>")
	return cmd
}
