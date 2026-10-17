package controller

import (
	"testing"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/alert"
	"github.com/haify-project/haify/pkg/event"
)

func u64(v uint64) *uint64 { return &v }
func boolp(v bool) *bool   { return &v }

func TestClassifyResource(t *testing.T) {
	pct := 63.0
	cases := []struct {
		name    string
		states  map[string]alert.NodeStateInfo
		state   string
		summary string
	}{
		{"healthy", map[string]alert.NodeStateInfo{
			"n1": {Role: "Primary", DiskState: "UpToDate"},
			"n2": {Role: "Secondary", DiskState: "UpToDate", ReplicationState: "Established", Connection: "Connected"},
			"n3": {DiskState: "Diskless", ExpectedDiskless: true, Connection: "Connected"},
		}, stateHealthy, "primary n1"},
		{"resyncing", map[string]alert.NodeStateInfo{
			"n1": {Role: "Primary", DiskState: "UpToDate"},
			"n2": {DiskState: "Inconsistent", ReplicationState: "SyncTarget", SyncPercent: &pct, Connection: "Connected"},
		}, stateResyncing, "n2 63%"},
		{"degraded", map[string]alert.NodeStateInfo{
			"n1": {Role: "Primary", DiskState: "UpToDate"},
			"n2": {DiskState: "Outdated", Connection: "Connected"},
		}, stateDegraded, "n2 Outdated"},
		{"no quorum wins", map[string]alert.NodeStateInfo{
			"n1": {Role: "Secondary", DiskState: "UpToDate", Quorum: boolp(false)},
			"n2": {Connection: "Connecting"},
		}, stateNoQuorum, "n2 Connecting"},
		{"no status", nil, stateUnknown, "no status"},
	}
	for _, tc := range cases {
		rv := classifyResource(alert.ResourceStatusInfo{Name: "r", NodeStates: tc.states})
		if rv.State != tc.state || rv.Summary != tc.summary {
			t.Errorf("%s: got %q %q, want %q %q", tc.name, rv.State, rv.Summary, tc.state, tc.summary)
		}
	}
}

func TestClusterViewRates(t *testing.T) {
	v := newClusterView(nil)
	now := time.Unix(1000, 0)
	v.now = func() time.Time { return now }
	poll := func(written, read uint64) {
		v.Observed(alert.Observation{Resources: alert.ResourceObservation{
			SourceStatus: alert.SourceStatus{Enabled: true, OK: true},
			Items: []alert.ResourceStatusInfo{{Name: "r", NodeStates: map[string]alert.NodeStateInfo{
				"n1": {Role: "Primary", DiskState: "UpToDate", WrittenKiB: u64(written), ReadKiB: u64(read)},
			}}},
		}})
	}
	poll(1000, 500)
	if snap := v.snapshot(); snap.WriteBps != -1 {
		t.Fatalf("one poll measured a rate: %v", snap.WriteBps)
	}
	now = now.Add(10 * time.Second)
	poll(1000+10*1024, 500+20)
	snap := v.snapshot()
	if snap.WriteBps != 1024*1024 || snap.ReadBps != 2*1024 {
		t.Fatalf("rates %v %v", snap.WriteBps, snap.ReadBps)
	}
	if rv, _ := v.resource("r"); rv.WriteBps != 1024*1024 {
		t.Fatalf("resource rate %v", rv.WriteBps)
	}
}

func TestClusterHistoryTiers(t *testing.T) {
	v := newClusterView(nil)
	h := &clusterHistory{view: v, log: v.log, loaded: true}
	now := time.Unix(1_700_000_000-1_700_000_000%3600, 0)
	h.now = func() time.Time { return now }
	for i := 0; i < 2*120; i++ { // two hours of 30-second polls
		v.writeBps = float64(i)
		h.Observed(alert.Observation{})
		now = now.Add(30 * time.Second)
	}
	if len(h.tiers.Raw) != 240 {
		t.Fatalf("raw %d", len(h.tiers.Raw))
	}
	// 12 ten-minute windows closed after two hours, one still open.
	if len(h.tiers.TenMin) != 11 {
		t.Fatalf("ten-minute %d", len(h.tiers.TenMin))
	}
	if got := h.tiers.TenMin[0].W; got != 9.5 {
		t.Fatalf("first ten-minute average %v", got)
	}
	if len(h.tiers.Hour) != 1 {
		t.Fatalf("hour %d", len(h.tiers.Hour))
	}
	samples, err := h.query("1h")
	if err != nil || len(samples) != 120 {
		t.Fatalf("1h query: %d %v", len(samples), err)
	}
	// Before the first hourly or ten-minute sample, the long ranges are served
	// from the finer tiers.
	h.tiers.Hour = nil
	if samples, _ := h.query("90d"); len(samples) == 0 || samples[len(samples)-1].T != h.tiers.Raw[len(h.tiers.Raw)-1].T {
		t.Fatalf("90d without hourly samples: %d", len(samples))
	}
	if _, err := h.query("2d"); err == nil {
		t.Fatal("unknown range accepted")
	}
}

func TestPageResources(t *testing.T) {
	c := &Controller{view: newClusterView(nil)}
	c.view.resources = map[string]resourceView{
		"a": {State: stateHealthy, WriteBps: 5},
		"b": {State: stateDegraded, WriteBps: -1},
		"c": {State: stateNoQuorum, WriteBps: 1},
	}
	c.view.at = time.Now()
	list := func() []*haifypb.ResourceInfo {
		return []*haifypb.ResourceInfo{
			{Name: "a", Nodes: []string{"n1", "n2"}, Labels: map[string]string{csiManagedByLabel: "csi", csiPVCLabel: "db/data-0"}},
			{Name: "b", Nodes: []string{"n2", "n3"}, WanMode: true},
			{Name: "c", Nodes: []string{"n1", "n3"}},
			{Name: "d", Nodes: []string{"n1"}},
		}
	}
	names := func(r *haifypb.ListResourcesResponse) string {
		s := ""
		for _, x := range r.Resources {
			s += x.Name
		}
		return s
	}
	out := c.pageResources(t.Context(), &haifypb.ListResourcesRequest{}, list())
	if names(out) != "cbda" || out.Total != 4 {
		t.Fatalf("default order %q total %d", names(out), out.Total)
	}
	if out.Counts["attention"] != 3 || out.Counts[stateHealthy] != 1 || out.Counts["offsite"] != 1 || out.Counts[stateUnknown] != 1 {
		t.Fatalf("counts %v", out.Counts)
	}
	if out.Resources[3].UsedBy != "pvc" || out.Resources[3].Owner != "PVC db/data-0" {
		t.Fatalf("a used by %q owner %q", out.Resources[3].UsedBy, out.Resources[3].Owner)
	}
	out = c.pageResources(t.Context(), &haifypb.ListResourcesRequest{Node: "n1", Sort: "name", Offset: 1, Limit: 1}, list())
	if names(out) != "c" || out.Total != 3 {
		t.Fatalf("node page %q total %d", names(out), out.Total)
	}
	out = c.pageResources(t.Context(), &haifypb.ListResourcesRequest{Query: "DATA-0"}, list())
	if names(out) != "a" {
		t.Fatalf("query %q", names(out))
	}
	out = c.pageResources(t.Context(), &haifypb.ListResourcesRequest{Sort: "writes", State: "attention"}, list())
	if names(out) != "cbd" {
		t.Fatalf("writes %q", names(out))
	}
}

func TestAttentionItems(t *testing.T) {
	items := attentionItems([]alert.FiringCondition{
		{Type: string(event.TypeResourceDegraded), Severity: string(event.SeverityWarning), Resource: "r2"},
		{Type: string(event.TypeResourceDegraded), Severity: string(event.SeverityWarning), Resource: "r1"},
		{Type: string(event.TypeNodeUnreachable), Severity: string(event.SeverityCritical), Node: "n17"},
		{Type: "pool.trimmed", Severity: string(event.SeverityInfo), Node: "n1"},
	})
	if len(items) != 2 || items[0].Type != string(event.TypeNodeUnreachable) || items[1].Count != 2 {
		t.Fatalf("items %+v", items)
	}
	if got := items[1].Subjects; len(got) != 2 || got[0] != "r1" {
		t.Fatalf("subjects %v", got)
	}
}

func TestFoldSeries(t *testing.T) {
	series := []promSeries{
		{Metric: map[string]string{"node": "n1"}, Values: [][2]any{{float64(100), "0.2"}, {float64(160), "0.4"}}},
		{Metric: map[string]string{"node": "n2"}, Values: [][2]any{{float64(100), "0.6"}, {float64(160), "NaN"}}},
	}
	s := foldSeries(series)
	if len(s.Unix) != 2 || s.Avg[0] != 0.4 || s.Min[0] != 0.2 || s.Max[0] != 0.6 || s.Sum[0] != 0.8 {
		t.Fatalf("fold %+v", s)
	}
	if s.NowAvg != 0.4 || s.Busiest.Node != "n2" || s.Busiest.Value != 0.6 {
		t.Fatalf("now %v busiest %+v", s.NowAvg, s.Busiest)
	}
	top := slowest(series, 5)
	if len(top) != 1 || top[0].Node != "n1" {
		t.Fatalf("slowest %+v", top)
	}
}
