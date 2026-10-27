package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/alert"
	"github.com/haify-project/haify/pkg/config"
)

// The failover loop thresholds are one more connection that can be written,
// tested in the alert package and never attached: the detector would then run
// on its built-in defaults whatever the operator configured, and nothing would
// say so. Assert the attachment.
func TestAlertOptionsCarryTheFailoverLoopSettings(t *testing.T) {
	c := &Controller{logger: zap.NewNop(), config: &config.Config{}}
	c.config.Alert.CheckIntervalSec = 30
	c.config.Alert.FailoverLoopCount = 5
	c.config.Alert.FailoverLoopWindowSec = 600

	opts := c.alertOptions()

	assert.Equal(t, 5, opts.FailoverLoopCount)
	assert.Equal(t, 10*time.Minute, opts.FailoverLoopWindow)
}

// A config with no failover loop keys, the shape every existing install has,
// must leave the alert package to choose its own defaults rather than turn the
// detector off with a zero.
func TestUnsetFailoverLoopSettingsFallBackToTheAlertDefaults(t *testing.T) {
	c := &Controller{logger: zap.NewNop(), config: &config.Config{}}
	c.config.Alert.CheckIntervalSec = 30

	opts := c.alertOptions()
	assert.Zero(t, opts.FailoverLoopCount)
	assert.Zero(t, opts.FailoverLoopWindow)

	// NewMonitor turns those zeros into the documented defaults.
	assert.Equal(t, 3, alert.DefaultFailoverLoopCount)
	assert.Equal(t, 15*time.Minute, alert.DefaultFailoverLoopWindow)
}

// The loaded defaults are what the example config and the user guide document,
// so they are pinned here where changing one without the others fails.
func TestFailoverLoopConfigDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.toml")
	require.NoError(t, os.WriteFile(path, []byte("[alert]\nenabled = true\n"), 0o600))

	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, alert.DefaultFailoverLoopCount, cfg.Alert.FailoverLoopCount)
	assert.Equal(t, int(alert.DefaultFailoverLoopWindow/time.Second), cfg.Alert.FailoverLoopWindowSec)

	// The example config is what operators copy, so it has to say the same.
	example, err := os.ReadFile(filepath.Join("..", "..", "configs", "controller.toml.example"))
	require.NoError(t, err)
	assert.Contains(t, string(example), fmt.Sprintf("failover_loop_count = %d", alert.DefaultFailoverLoopCount))
	assert.Contains(t, string(example), fmt.Sprintf("failover_loop_window_sec = %d", int(alert.DefaultFailoverLoopWindow/time.Second)))
}
