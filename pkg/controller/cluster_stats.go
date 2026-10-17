package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// Node statistics for the dashboard, read from the Prometheus server that
// scrapes the nodes' node_exporter ([metrics] prometheus_url).
//
// The queries are fixed here; a client chooses only the range. Each is one
// value per node (labelled node, as /discovery/node-exporter labels the
// targets) and is folded into the average, lowest, highest and sum across
// nodes, so the answer is the same size for five nodes or five hundred.

// statsQueries are the per-node expressions; NODES and RATE are filled in.
var statsQueries = map[string]string{
	"cpu":     `1 - avg by (node) (rate(node_cpu_seconds_total{mode="idle",NODES}[RATE]))`,
	"memory":  `1 - sum by (node) (node_memory_MemAvailable_bytes{NODES}) / sum by (node) (node_memory_MemTotal_bytes{NODES})`,
	"latency": `max by (node) (rate(node_disk_write_time_seconds_total{device!~"loop.*|ram.*|sr.*",NODES}[RATE]) / clamp_min(rate(node_disk_writes_completed_total{device!~"loop.*|ram.*|sr.*",NODES}[RATE]), 0.01))`,
	"network": `sum by (node) (rate(node_network_transmit_bytes_total{device!~"lo|veth.*|docker.*|br-.*|cni.*|flannel.*|cali.*|tailscale.*|virbr.*|vnet.*|tap.*|fw.*",NODES}[RATE]))`,
}

// statsSteps maps a range to its length and the step between points.
var statsSteps = map[string][2]time.Duration{
	"1h": {time.Hour, 30 * time.Second}, "6h": {6 * time.Hour, 3 * time.Minute}, "24h": {24 * time.Hour, 10 * time.Minute},
	"7d": {7 * 24 * time.Hour, time.Hour}, "30d": {30 * 24 * time.Hour, 4 * time.Hour},
}

type promSeries struct {
	Metric map[string]string `json:"metric"`
	Values [][2]any          `json:"values"`
}

// promQueryRange runs one range query and returns each node's points.
func promQueryRange(ctx context.Context, base, query string, start, end time.Time, step time.Duration) ([]promSeries, error) {
	v := url.Values{}
	v.Set("query", query)
	v.Set("start", strconv.FormatInt(start.Unix(), 10))
	v.Set("end", strconv.FormatInt(end.Unix(), 10))
	v.Set("step", strconv.FormatInt(int64(step.Seconds()), 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/v1/query_range?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	var out struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []promSeries `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("prometheus answered %s: %w", resp.Status, err)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s", out.Error)
	}
	return out.Data.Result, nil
}

// foldSeries turns per-node series into one StatSeries.
func foldSeries(series []promSeries) *haifypb.StatSeries {
	type acc struct {
		sum, min, max float64
		n             int
	}
	steps := map[int64]*acc{}
	latest := map[string][2]float64{} // node -> (unix, value)
	for _, s := range series {
		node := s.Metric["node"]
		for _, pt := range s.Values {
			ts, _ := pt[0].(float64)
			str, _ := pt[1].(string)
			val, err := strconv.ParseFloat(str, 64)
			if err != nil || math.IsNaN(val) || math.IsInf(val, 0) {
				continue
			}
			t := int64(ts)
			a, ok := steps[t]
			if !ok {
				a = &acc{min: val, max: val}
				steps[t] = a
			}
			a.sum += val
			a.n++
			a.min, a.max = math.Min(a.min, val), math.Max(a.max, val)
			if t >= int64(latest[node][0]) {
				latest[node] = [2]float64{float64(t), val}
			}
		}
	}
	out := &haifypb.StatSeries{}
	times := make([]int64, 0, len(steps))
	for t := range steps {
		times = append(times, t)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	for _, t := range times {
		a := steps[t]
		out.Unix = append(out.Unix, t)
		out.Avg = append(out.Avg, a.sum/float64(a.n))
		out.Min = append(out.Min, a.min)
		out.Max = append(out.Max, a.max)
		out.Sum = append(out.Sum, a.sum)
	}
	if n := len(times); n > 0 {
		out.NowAvg, out.NowSum = out.Avg[n-1], out.Sum[n-1]
	}
	for node, l := range latest {
		if out.Busiest == nil || l[1] > out.Busiest.Value {
			out.Busiest = &haifypb.NodeStat{Node: node, Value: l[1]}
		}
	}
	return out
}

// slowest is the nodes with the highest latest value, highest first.
func slowest(series []promSeries, n int) []*haifypb.NodeStat {
	var out []*haifypb.NodeStat
	for _, s := range series {
		if len(s.Values) == 0 {
			continue
		}
		str, _ := s.Values[len(s.Values)-1][1].(string)
		if v, err := strconv.ParseFloat(str, 64); err == nil && !math.IsNaN(v) {
			out = append(out, &haifypb.NodeStat{Node: s.Metric["node"], Value: v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value > out[j].Value })
	return out[:min(n, len(out))]
}

// clusterStats reads the node statistics of one range from Prometheus.
func (c *Controller) clusterStats(ctx context.Context, rng string) (*haifypb.GetClusterStatsResponse, error) {
	log := c.traceLogger()
	base := ""
	if c.config != nil {
		base = c.config.Metrics.PrometheusURL
	}
	if base == "" {
		log.Debug("Controller.clusterStats: no prometheus_url")
		return &haifypb.GetClusterStatsResponse{Success: true}, nil
	}
	if rng == "" {
		rng = "24h"
	}
	st, ok := statsSteps[rng]
	if !ok {
		return nil, fmt.Errorf("unknown range %q: use 1h, 6h, 24h, 7d or 30d", rng)
	}
	names := []string{}
	if c.nodes != nil {
		nodes, _ := c.nodes.ListNodes(ctx)
		for _, n := range nodes {
			names = append(names, regexp.QuoteMeta(n.Name))
		}
	}
	selector := `node!=""`
	if len(names) > 0 {
		selector = `node=~"` + strings.Join(names, "|") + `"`
	}
	window := max(st[1]*2, 5*time.Minute)
	end := time.Now()
	start := end.Add(-st[0])
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	results := map[string][]promSeries{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	for key, q := range statsQueries {
		q = strings.ReplaceAll(strings.ReplaceAll(q, "NODES", selector), "RATE", fmt.Sprintf("%ds", int(window.Seconds())))
		wg.Add(1)
		go func(key, q string) {
			defer wg.Done()
			t0 := time.Now()
			series, err := promQueryRange(ctx, base, q, start, end, st[1])
			log.Debug("Controller.clusterStats: query", zap.String("stat", key), zap.Int("series", len(series)),
				zap.Duration("took", time.Since(t0)), zap.Error(err))
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			results[key] = series
		}(key, q)
	}
	wg.Wait()
	if firstErr != nil && len(results["cpu"]) == 0 {
		return nil, fmt.Errorf("reading %s: %w", base, firstErr)
	}
	return &haifypb.GetClusterStatsResponse{Success: true, Available: true, Nodes: int32(len(results["cpu"])),
		Cpu: foldSeries(results["cpu"]), Memory: foldSeries(results["memory"]),
		DiskWriteLatency: foldSeries(results["latency"]), Network: foldSeries(results["network"]),
		SlowestDisks: slowest(results["latency"], 5)}, nil
}
