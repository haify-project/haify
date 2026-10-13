package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	pb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/haify-project/haify/pkg/wanproxy"
	"github.com/haify-project/haify/pkg/wansizing"
)

// renderedLegConfig is the last config written to the primary for wan1.
func renderedLegConfig(t *testing.T, srv *Server) string {
	t.Helper()
	dep := srv.resources.deployment.(*fakeDeploymentClient)
	for i := len(dep.distributedConfigs) - 1; i >= 0; i-- {
		c := dep.distributedConfigs[i]
		if c.remotePath == wanproxy.NodeConfigPath("wan1") && strings.Contains(c.content, `role = "dialer"`) {
			return c.content
		}
	}
	t.Fatal("no dialer config was written")
	return ""
}

func TestSetWanTuningRewritesTheLegsAndRepairKeepsIt(t *testing.T) {
	srv, ctrl := wanEndpointFixture(t, "203.0.113.7")
	withLiveWANConfig(srv, oldTwoEndpointWAN)
	resp, err := srv.SetWanTuning(context.Background(), &pb.SetWanTuningRequest{Name: "wan1", Buffer: "1GiB", Compression: "off"})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.Message)
	assert.Equal(t, uint64(1<<30), resp.BufferBytes)
	assert.Equal(t, "off", resp.Compression)

	r, _ := ctrl.db.GetResource(context.Background(), "wan1")
	assert.Equal(t, int64(1<<30), r.WANBufferBytes)
	assert.Equal(t, wanproxy.CompressionOff, r.WANZstdLevel)
	cfg := renderedLegConfig(t, srv)
	assert.Contains(t, cfg, "bulk_cap_bytes = 1073741824")
	assert.Contains(t, cfg, "zstd_level = 0")

	// A repair rebuilds the legs from the record, and must not quietly put the
	// defaults back.
	rep, err := srv.RepairWanProxy(context.Background(), &pb.RepairWanProxyRequest{Name: "wan1"})
	require.NoError(t, err)
	require.True(t, rep.Success, rep.Message)
	assert.Contains(t, renderedLegConfig(t, srv), "bulk_cap_bytes = 1073741824")

	// Back to the defaults.
	resp, err = srv.SetWanTuning(context.Background(), &pb.SetWanTuningRequest{Name: "wan1", Buffer: "default", Compression: "default"})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.Message)
	cfg = renderedLegConfig(t, srv)
	assert.Contains(t, cfg, "bulk_cap_bytes = 4294967296")
	assert.Contains(t, cfg, "zstd_level = 3")
}

func TestSetWanTuningRestoresTheOldTuningWhenTheRebuildFails(t *testing.T) {
	srv, ctrl := wanEndpointFixture(t) // the DR answers on nothing
	withLiveWANConfig(srv, oldTwoEndpointWAN)
	resp, err := srv.SetWanTuning(context.Background(), &pb.SetWanTuningRequest{Name: "wan1", Buffer: "2GiB"})
	require.NoError(t, err)
	assert.False(t, resp.Success)
	assert.Contains(t, resp.Message, "not changed")
	r, _ := ctrl.db.GetResource(context.Background(), "wan1")
	assert.Zero(t, r.WANBufferBytes)
}

func TestSetWanTuningRefusesBadRequests(t *testing.T) {
	srv, ctrl := wanEndpointFixture(t, "203.0.113.7")
	require.NoError(t, ctrl.db.SaveResource(context.Background(), &database.Resource{Name: "lan1", Nodes: "p1,d1", Port: 7151}))
	for _, req := range []*pb.SetWanTuningRequest{
		{Name: "wan1"},
		{Name: "wan1", Buffer: "5MiB"},
		{Name: "wan1", Buffer: "lots"},
		{Name: "wan1", Compression: "zstd:30"},
		{Name: "wan1", Compression: "lz4"},
		{Name: "lan1", Buffer: "1GiB"},
		{Name: "nope", Buffer: "1GiB"},
	} {
		resp, err := srv.SetWanTuning(context.Background(), req)
		require.NoError(t, err)
		assert.False(t, resp.Success, "%+v", req)
	}
}

// sizingFixture is wan1 with a running proxy that publishes metrics, plus a
// day of controller history.
func sizingFixture(t *testing.T, metrics string) (*Server, *Controller) {
	t.Helper()
	srv, ctrl := wanEndpointFixture(t, "203.0.113.7")
	dep := srv.resources.deployment.(*fakeDeploymentClient)
	dep.execFunc = func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		switch {
		case strings.Contains(cmd, "list-units"):
			return successExecResult(hosts, "haify-proxy@wan1.service loaded active running Haify WAN replication proxy"), nil
		case strings.Contains(cmd, "is-active"):
			return successExecResult(hosts, "active"), nil
		case strings.Contains(cmd, "cat /run/haify-proxy/wan1.json"):
			return successExecResult(hosts, metrics), nil
		case strings.Contains(cmd, "| base64 -d | sudo /bin/bash"):
			return successExecResult(hosts, "chunk 4194304 1048576 0\nchunk 4194304 1048576 0\nchunk 4194304 120 1\n"), nil
		}
		return successExecResult(hosts, ""), nil
	}
	var h wansizing.History
	now := time.Now()
	for i := 0; i < 24; i++ {
		h.Add(now.Add(time.Duration(i-24)*time.Hour), 3600, 3600*2*(1<<20), "p1") // 2 MiB/s all day
	}
	data, err := json.Marshal(h)
	require.NoError(t, err)
	require.NoError(t, ctrl.db.SaveWriteHistory(context.Background(), "wan1", data))
	return srv, ctrl
}

const proxyMetrics = `{"version":"0.2.0","uptime_secs":7200,"cpu_seconds":30,"buffer_used_bytes":0,
"buffer_cap_bytes":8589934592,"buffer_fill_percent":0,"buffer_high_water_bytes":104857600,
"drbd_in_bytes":21474836480,"drbd_to_wan_bytes":21474836480,"wan_wire_bytes":10737418240,
"wan_to_drbd_bytes":0,"frames_sent":1,"compression_ratio":2.0,"wan_connected":true,"reconnects":0,
"ring_full_events":0,"rate_samples_secs":7200,"input_avg_bps":3145728,"input_p99_bps":31457280,
"input_peak_bps":41943040,"link_samples_secs":120,"link_p50_bps":10485760,"longest_burst_secs":15}`

func TestGetWanSizingWeighsHistoryProxyAndLink(t *testing.T) {
	srv, _ := sizingFixture(t, proxyMetrics)
	resp, err := srv.GetWanSizing(context.Background(), &pb.GetWanSizingRequest{Name: "wan1"})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.Message)

	// avg 3 MiB/s (proxy, 2 h) beats the 1-day history only on coverage: the
	// history covers a day, so it is the average used.
	assert.Contains(t, resp.AvgSource, "controller history")
	assert.InDelta(t, 2*(1<<20), resp.AvgBps, 1)
	// The burst is the proxy's per-second p99, 30 MiB/s; the link its own
	// measurement, 10 MiB/s.
	assert.Contains(t, resp.PeakSource, "per-second")
	assert.Contains(t, resp.LinkSource, "measured by the proxy")
	assert.Equal(t, string(wansizing.Buffer), resp.Outcome)
	assert.Equal(t, uint64(4<<30), resp.CurrentBufferBytes)
	assert.Equal(t, "zstd:3", resp.CurrentCompression)
	assert.Equal(t, 2.0, resp.CompressionRatio, "the live stream's ratio")
	require.NotNil(t, resp.History)
	require.NotNil(t, resp.Proxy)
	require.NotNil(t, resp.Proxy.LinkBps)
	assert.Greater(t, resp.RecommendedBufferBytes, uint64(0))
	assert.Greater(t, resp.CpuCoresAtPeak, 0.0)

	// A given bandwidth beats the measured one.
	resp, err = srv.GetWanSizing(context.Background(), &pb.GetWanSizingRequest{Name: "wan1", Bandwidth: "1Gbit"})
	require.NoError(t, err)
	assert.Equal(t, string(wansizing.LinkSufficient), resp.Outcome)
	assert.Equal(t, "given", resp.LinkSource)

	_, err = srv.GetWanSizing(context.Background(), &pb.GetWanSizingRequest{Name: "wan1", Bandwidth: "fast"})
	require.NoError(t, err)
}

func TestGetWanSizingOnAnOldProxyAndALANResource(t *testing.T) {
	// An older proxy publishes no rates: they must read as unknown.
	srv, ctrl := sizingFixture(t, `{"buffer_used_bytes":0,"compression_ratio":1.5,"wan_connected":true}`)
	resp, err := srv.GetWanSizing(context.Background(), &pb.GetWanSizingRequest{Name: "wan1", Bandwidth: "100Mbit"})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.Message)
	assert.Nil(t, resp.Proxy.InputP99Bps)
	assert.Contains(t, resp.PeakSource, "controller history")

	// A LAN resource is sized from history, and the sample supplies the ratio.
	require.NoError(t, ctrl.db.SaveResource(context.Background(), &database.Resource{Name: "lan1", Nodes: "p1,d1", Port: 7151}))
	hist, err := ctrl.db.LoadWriteHistory(context.Background(), "wan1")
	require.NoError(t, err)
	require.NoError(t, ctrl.db.SaveWriteHistory(context.Background(), "lan1", hist))
	resp, err = srv.GetWanSizing(context.Background(), &pb.GetWanSizingRequest{Name: "lan1", Bandwidth: "10Mbit",
		SampleCompression: true})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.Message)
	assert.False(t, resp.Wan)
	require.NotNil(t, resp.Sample)
	assert.Empty(t, resp.Sample.Error)
	assert.Equal(t, uint32(3), resp.Sample.Chunks)
	assert.Equal(t, uint32(1), resp.Sample.ZeroChunks)
	assert.InDelta(t, 4.0, resp.CompressionRatio, 0.001)
	// 2 MiB/s average against 1.25 MB/s: only compression (4:1) makes it fit.
	assert.Equal(t, string(wansizing.Compress), resp.Outcome)
	assert.Contains(t, strings.Join(resp.Notes, " "), "add-dr")
}

func TestWriteHistoryRecordsPollsAndSkipsResync(t *testing.T) {
	w := &writeHistory{log: zap.NewNop(), ctx: context.Background(), tracks: map[string]*historyTrack{}}
	now := time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }
	poll := func(kib uint64, resync bool) {
		w.Observed(writtenObs("r1", "n1", kib, resync))
		now = now.Add(30 * time.Second)
	}
	poll(0, false)
	poll(30*1024, false) // 1 MiB/s
	poll(60*1024, true)  // resync: not the application's writes
	poll(90*1024, false) // starts over after the resync
	poll(120*1024, false)
	win := w.Summary("r1", 1)
	require.NotNil(t, win)
	assert.Equal(t, 2, win.Samples, "no rate from the first reading, nor across the resync")
	assert.InDelta(t, float64(1<<20), win.AvgBps, 1)
}
