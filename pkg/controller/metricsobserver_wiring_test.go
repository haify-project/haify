package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/alert"
	"github.com/haify-project/haify/pkg/config"
	"github.com/haify-project/haify/pkg/metrics"
)

// The observer is what makes the storage and DRBD gauges anything other than
// zero. It is also exactly the kind of connection that can be written, tested
// in isolation and never actually attached — in which case nothing fails, the
// detector still raises events, /metrics still answers, and only the numbers on
// it are quietly empty. Assert the attachment itself.
func TestAlertOptionsCarryTheMetricsObserver(t *testing.T) {
	m, err := metrics.New(zap.NewNop())
	require.NoError(t, err)

	c := &Controller{logger: zap.NewNop(), metrics: m, config: &config.Config{}}
	c.config.Alert.CheckIntervalSec = 30

	opts := c.alertOptions()

	require.NotNil(t, opts.Observer, "a controller with metrics must feed them from the health poll")
	obs, ok := opts.Observer.(observers)
	require.True(t, ok)
	// The dashboard's view of the poll and its history come first, the
	// history after the view it reads.
	require.Len(t, obs, 3)
	_, ok = obs[0].(*clusterView)
	assert.True(t, ok)
	_, ok = obs[1].(*clusterHistory)
	assert.True(t, ok)
	_, ok = obs[2].(*metricsObserver)
	assert.True(t, ok, "the observer attached must be the metrics adapter")

	// The write-anomaly detector reads the same poll.
	c.config.Alert.WriteAnomaly.Enabled = true
	obs = c.alertOptions().Observer.(observers)
	require.Len(t, obs, 4)
	_, ok = obs[3].(*writeAnomalyDetector)
	assert.True(t, ok)
}

// Metrics are optional, and a controller without them must not invent a
// metrics observer that would dereference a nil *Metrics on the first poll.
// The dashboard's own view is kept either way.
func TestAlertOptionsHaveNoMetricsObserverWithoutMetrics(t *testing.T) {
	c := &Controller{logger: zap.NewNop(), config: &config.Config{}}
	c.config.Alert.CheckIntervalSec = 30

	obs, ok := c.alertOptions().Observer.(observers)
	require.True(t, ok)
	require.Len(t, obs, 2)
	for _, o := range obs {
		_, isMetrics := o.(*metricsObserver)
		assert.False(t, isMetrics)
	}
}

// The observer only ever sees what the detector was told to gather, so the
// pool gauges depend on a switch that has nothing to do with metrics. Pin that
// relationship: it is the reason an operator can enable metrics, see an empty
// capacity panel, and find nothing wrong in either config section.
func TestAlertOptionsGatherPoolsOnlyWhenPoolChecksAreOn(t *testing.T) {
	m, err := metrics.New(zap.NewNop())
	require.NoError(t, err)

	c := &Controller{logger: zap.NewNop(), metrics: m, storage: &StorageManager{}, config: &config.Config{}}
	c.config.Alert = config.AlertConfig{CheckIntervalSec: 30}
	assert.Nil(t, c.alertOptions().Pools, "pool checks off means no pool capacity is ever observed")

	c.config.Alert.CheckPools = true
	assert.NotNil(t, c.alertOptions().Pools)
}

// Compile-time proof that the adapter still satisfies the interface the monitor
// takes, so a change to either side is a build failure rather than a silently
// unattached observer.
var _ alert.Observer = (*metricsObserver)(nil)
