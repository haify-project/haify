package wanproxy

import (
	"strings"
	"testing"
)

func TestParseCompression(t *testing.T) {
	for in, want := range map[string]int{"off": CompressionOff, "none": CompressionOff, "default": 0, "zstd": 0,
		"zstd:1": 1, "ZSTD:9": 9, "zstd:19": 19} {
		got, err := ParseCompression(in)
		if err != nil || got != want {
			t.Errorf("ParseCompression(%q) = %d, %v; want %d", in, got, err, want)
		}
		back, _ := ParseCompression(CompressionString(got))
		_, a := Tuning{ZstdLevel: got}.Effective()
		_, b := Tuning{ZstdLevel: back}.Effective()
		if a != b {
			t.Errorf("CompressionString(%d) reads back as a different level", got)
		}
	}
	for _, bad := range []string{"", "zstd:0", "zstd:20", "lz4", "3"} {
		if _, err := ParseCompression(bad); err == nil {
			t.Errorf("ParseCompression(%q) accepted", bad)
		}
	}
}

func TestTuningRendersIntoBothEnds(t *testing.T) {
	spec := sampleSpec()
	spec.Tuning = Tuning{BufferBytes: 1 << 30, ZstdLevel: CompressionOff}
	for name, cfg := range map[string]string{"dialer": RenderDialerConfig(spec), "acceptor": RenderAcceptorConfig(spec)} {
		if !strings.Contains(cfg, "bulk_cap_bytes = 1073741824\n") || !strings.Contains(cfg, "zstd_level = 0\n") {
			t.Errorf("%s config does not carry the tuning:\n%s", name, cfg)
		}
	}
	spec.Tuning = Tuning{ZstdLevel: 7}
	if cfg := RenderDialerConfig(spec); !strings.Contains(cfg, "zstd_level = 7\n") ||
		!strings.Contains(cfg, "bulk_cap_bytes = 4294967296\n") {
		t.Errorf("a level alone must keep the default buffer:\n%s", cfg)
	}
}

func TestTuningValidate(t *testing.T) {
	for _, ok := range []Tuning{{}, {BufferBytes: MinBufferBytes}, {ZstdLevel: CompressionOff}, {ZstdLevel: MaxZstdLevel}} {
		if err := ok.Validate(); err != nil {
			t.Errorf("%+v refused: %v", ok, err)
		}
	}
	for _, bad := range []Tuning{{BufferBytes: 1 << 20}, {BufferBytes: MaxBufferBytes + 1}, {ZstdLevel: -2}, {ZstdLevel: 20}} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

// The rate figures are pointers so an older proxy's absence of them reads as
// unknown; a new proxy's zeros are real readings.
func TestParseMetricsTellsAbsentRatesFromZero(t *testing.T) {
	old := ParseMetrics(`{"buffer_used_bytes": 5, "wan_connected": true}`)
	if old == nil || old.HasRates() || old.InputP99Bps != nil {
		t.Fatalf("an older proxy must have no rates: %+v", old)
	}
	cur := ParseMetrics(`{"version":"0.2.0","rate_samples_secs":10,"input_p99_bps":0,"input_peak_bps":0,
		"link_samples_secs":0,"link_p50_bps":0,"buffer_high_water_bytes":0}`)
	if cur == nil || !cur.HasRates() || *cur.InputP99Bps != 0 || cur.Version != "0.2.0" {
		t.Fatalf("a current proxy's zero rates must be kept: %+v", cur)
	}
}
