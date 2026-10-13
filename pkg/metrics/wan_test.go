package metrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A proxy's counters become one series per figure; a figure an older proxy
// does not publish is absent, and a resource whose proxy stops answering takes
// all of its series with it.
func TestSetWANProxiesExportsWhatWasPublishedAndNothingElse(t *testing.T) {
	m := getTestMetrics(t)
	hw := uint64(512 << 20)
	m.SetWANProxies([]WANProxyReading{
		{Resource: "dr1", BacklogBytes: 1 << 20, BufferCapBytes: 8 << 30, Connected: true, CompressionRatio: 2.5,
			Reconnects: 3, HighWaterBytes: &hw, InputP99Bps: ptr(30.0 * (1 << 20))},
		{Resource: "old", BacklogBytes: 0, Connected: false},
	})

	id := labels{"resource": "dr1"}
	assert.Equal(t, float64(1<<20), requireGauge(t, m, "haify_wan_backlog_bytes", id))
	assert.Equal(t, 1.0, requireGauge(t, m, "haify_wan_connected", id))
	assert.Equal(t, 2.5, requireGauge(t, m, "haify_wan_compression_ratio", id))
	assert.Equal(t, float64(hw), requireGauge(t, m, "haify_wan_buffer_high_water_bytes", id))
	assert.Equal(t, 30.0*(1<<20), requireGauge(t, m, "haify_wan_input_p99_bytes_per_second", id))
	_, ok := gaugeValue(t, m, "haify_wan_link_bytes_per_second", id)
	assert.False(t, ok, "no link measurement means no series, not a zero link")

	old := labels{"resource": "old"}
	assert.Equal(t, 0.0, requireGauge(t, m, "haify_wan_connected", old))
	_, ok = gaugeValue(t, m, "haify_wan_compression_ratio", old)
	assert.False(t, ok, "nothing sent yet is not a 0:1 ratio")
	_, ok = gaugeValue(t, m, "haify_wan_input_p99_bytes_per_second", old)
	assert.False(t, ok, "an older proxy publishes no rates")

	m.SetWANProxies([]WANProxyReading{{Resource: "old"}})
	_, ok = gaugeValue(t, m, "haify_wan_backlog_bytes", id)
	assert.False(t, ok, "a resource whose proxy was not read must vanish")
}
