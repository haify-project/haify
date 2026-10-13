package wansizing

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const mib = 1 << 20

func TestHistogramQuantileNeverUndershoots(t *testing.T) {
	var h Histogram
	for i := 0; i < 99; i++ {
		h.Add(1000)
	}
	h.Add(50 * mib)
	if got := h.Quantile(0.5); got < 1000 || got > 1200 {
		t.Fatalf("p50 = %v, want the 1000 B/s bucket's upper bound", got)
	}
	if got := h.Quantile(1); got < 50*mib || got > 60*mib {
		t.Fatalf("max = %v", got)
	}
	if (Histogram{}).Quantile(0.99) != 0 {
		t.Fatal("an empty histogram has no quantile")
	}
}

func TestHistoryAveragesBytesOverTimeNotPolls(t *testing.T) {
	var h History
	day := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	// Ten short polls at 1 MiB/s, then one long idle one: the average is bytes
	// over seconds, not the mean of the eleven rates.
	for i := 0; i < 10; i++ {
		h.Add(day.Add(time.Duration(i)*30*time.Second), 30, 30*mib, "os1")
	}
	h.Add(day.Add(time.Hour), 2700, 0, "os1")
	w := h.Summary(day.Add(time.Hour), 7)
	if w == nil {
		t.Fatal("no summary")
	}
	if want := 300.0 * mib / 3000; w.AvgBps != want {
		t.Fatalf("avg = %v, want %v", w.AvgBps, want)
	}
	if w.PeakBps != mib || w.Samples != 11 || w.Node != "os1" {
		t.Fatalf("summary = %+v", w)
	}
	if w.P99Bps > w.PeakBps {
		t.Fatal("p99 must not exceed the busiest interval")
	}
}

func TestHistoryKeepsOnlyRecentDaysAndWindowsBySummary(t *testing.T) {
	var h History
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for d := 0; d < 20; d++ {
		h.Add(start.AddDate(0, 0, d), 300, float64(d)*300, "a")
	}
	if len(h.Days) != KeepDays {
		t.Fatalf("kept %d days, want %d", len(h.Days), KeepDays)
	}
	w := h.Summary(start.AddDate(0, 0, 19), 3)
	if w.Days != 3 || w.First != "2026-09-18" || w.Last != "2026-09-20" {
		t.Fatalf("window = %+v", w)
	}
	// Round-trips through JSON, which is how the controller stores it.
	data, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var back History
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Summary(start.AddDate(0, 0, 19), 3).AvgBps != w.AvgBps {
		t.Fatal("history changed across a JSON round trip")
	}
}

func proxy(avg, p99, peak, link float64, linkSecs float64) *ProxyFigures {
	return &ProxyFigures{UptimeSecs: 7200, RateSamplesSecs: 7200, InputAvgBps: avg, InputP99Bps: p99,
		InputPeakBps: peak, LinkP50Bps: link, LinkSamplesSecs: linkSecs, ZstdLevel: 3}
}

func TestDecideFourOutcomes(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want Outcome
	}{
		{"peak fits", Input{Proxy: proxy(2*mib, 8*mib, 9*mib, 0, 0), LinkBps: 10 * mib}, LinkSufficient},
		{"average fits", Input{Proxy: proxy(4*mib, 30*mib, 40*mib, 0, 0), LinkBps: 10 * mib}, Buffer},
		{"compressed average fits", Input{Proxy: proxy(15*mib, 30*mib, 40*mib, 0, 0),
			Sample: &SampleFigures{Ratio: 2.5, SampledBytes: 64 * mib, Chunks: 16}, LinkBps: 10 * mib}, Compress},
		{"nothing fits", Input{Proxy: proxy(40*mib, 60*mib, 80*mib, 0, 0),
			Sample: &SampleFigures{Ratio: 1.5, SampledBytes: 64 * mib, Chunks: 16}, LinkBps: 10 * mib}, AsyncBackup},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Decide(c.in)
			if r.Outcome != c.want {
				t.Fatalf("outcome = %s, want %s (%s)", r.Outcome, c.want, r.Summary)
			}
		})
	}
}

func TestDecideWithoutBandwidthSaysWhatTheLinkNeeds(t *testing.T) {
	r := Decide(Input{Proxy: proxy(4*mib, 30*mib, 40*mib, 0, 0)})
	if r.Outcome != Undecided || !strings.Contains(r.Summary, "--bandwidth") {
		t.Fatalf("got %s: %s", r.Outcome, r.Summary)
	}
	if r := Decide(Input{LinkBps: mib}); r.Outcome != Undecided {
		t.Fatalf("no rates must be undecided, got %s", r.Outcome)
	}
}

func TestDecideUsesTheProxysLinkMeasurementOnlyWithEnoughSamples(t *testing.T) {
	r := Decide(Input{Proxy: proxy(4*mib, 30*mib, 40*mib, 10*mib, 3)})
	if r.Outcome != Undecided {
		t.Fatalf("3 saturated seconds are not a measurement, got %s", r.Outcome)
	}
	r = Decide(Input{Proxy: proxy(4*mib, 30*mib, 40*mib, 10*mib, 120)})
	if r.Outcome != Buffer || !strings.Contains(r.LinkSource, "measured by the proxy") {
		t.Fatalf("got %s from %q", r.Outcome, r.LinkSource)
	}
	// A given figure wins, and a disagreement is pointed out.
	r = Decide(Input{Proxy: proxy(4*mib, 30*mib, 40*mib, 10*mib, 120), LinkBps: 50 * mib})
	if r.LinkSource != "given" || r.Outcome != LinkSufficient || !hasNote(r, "measured the link") {
		t.Fatalf("got %s from %q, notes %v", r.Outcome, r.LinkSource, r.Notes)
	}
}

func TestBufferSizing(t *testing.T) {
	p := proxy(4*mib, 30*mib, 40*mib, 0, 0)
	p.LongestBurstSecs = 20
	r := Decide(Input{Proxy: p, LinkBps: 10 * mib, BufferBytes: 256 * mib})
	// 2 x (30 - 10) MiB/s x 20 s = 800 MiB, rounded up to 1 GiB.
	if r.RecommendedBufferBytes != 1024*mib {
		t.Fatalf("recommended %v MiB (%s)", r.RecommendedBufferBytes/mib, r.BufferBasis)
	}
	if !hasNote(r, "smaller") {
		t.Fatalf("a too-small buffer must be called out: %v", r.Notes)
	}
	if r.OutageSecs != 64 {
		t.Fatalf("256 MiB at 4 MiB/s rides out 64 s, got %v", r.OutageSecs)
	}

	// A backlog the proxy actually held beats the model.
	p.HighWaterBytes = 3000 * mib
	r = Decide(Input{Proxy: p, LinkBps: 10 * mib})
	if r.RecommendedBufferBytes != 6144*mib || !strings.Contains(r.BufferBasis, "largest backlog") {
		t.Fatalf("recommended %v MiB (%s)", r.RecommendedBufferBytes/mib, r.BufferBasis)
	}
}

func TestHistoryOnlyIsHonestAboutResolutionAndShortWindows(t *testing.T) {
	w := &Window{Days: 1, Seconds: 3600, Samples: 12, ResolutionSecs: 300, AvgBps: 2 * mib, P99Bps: 6 * mib, PeakBps: 6 * mib}
	r := Decide(Input{History: w, LinkBps: 5 * mib})
	if r.Outcome != Buffer {
		t.Fatalf("outcome %s", r.Outcome)
	}
	if !hasNote(r, "averaged away") || !hasNote(r, "measure for days") || !hasNote(r, "compression ratio is not known") {
		t.Fatalf("notes %v", r.Notes)
	}
	if !strings.Contains(r.AvgSource, "controller history") {
		t.Fatalf("avg source %q", r.AvgSource)
	}
}

func TestRatioPrefersTheLiveStreamAndIgnoresUncompressedProxies(t *testing.T) {
	p := proxy(4*mib, 30*mib, 40*mib, 0, 0)
	p.CompressionRatio, p.SentBytes = 2.0, 512*mib
	s := &SampleFigures{Ratio: 3, SampledBytes: 64 * mib, Chunks: 16, ZeroChunks: 4}
	if r := Decide(Input{Proxy: p, Sample: s, LinkBps: mib}); r.Ratio != 2 {
		t.Fatalf("ratio %v from %q", r.Ratio, r.RatioSource)
	}
	p.ZstdLevel = 0 // compression off: the proxy's ratio is only framing
	r := Decide(Input{Proxy: p, Sample: s, LinkBps: mib})
	if r.Ratio != 3 || !strings.Contains(r.RatioSource, "12 places") || !strings.Contains(r.RatioSource, "4 never-written") {
		t.Fatalf("ratio %v from %q", r.Ratio, r.RatioSource)
	}
}

func TestCPUAtPeakScalesMeasuredCost(t *testing.T) {
	p := proxy(4*mib, 100*mib, 120*mib, 0, 0)
	p.InBytes, p.CPUSeconds = 1024*mib, 2 // 2 CPU-s per GiB
	r := Decide(Input{Proxy: p, LinkBps: 200 * mib})
	if want := 2.0 / 1024 * 100; r.CPUCoresAtPeak < want*0.99 || r.CPUCoresAtPeak > want*1.01 {
		t.Fatalf("cores at peak %v, want %v", r.CPUCoresAtPeak, want)
	}
}

func TestParseBandwidth(t *testing.T) {
	cases := map[string]float64{
		"100Mbit":   100e6 / 8,
		"100mbit/s": 100e6 / 8,
		"1Gbps":     1e9 / 8,
		"100Mbps":   100e6 / 8,
		"12MiB/s":   12 * mib,
		"40MB/s":    40e6,
		"1.5Gbit":   1.5e9 / 8,
	}
	for in, want := range cases {
		got, err := ParseBandwidth(in)
		if err != nil || got != want {
			t.Errorf("ParseBandwidth(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "100", "fast", "0Mbit", "10 parsecs"} {
		if _, err := ParseBandwidth(bad); err == nil {
			t.Errorf("ParseBandwidth(%q) accepted", bad)
		}
	}
}

func hasNote(r Result, sub string) bool {
	for _, n := range r.Notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
