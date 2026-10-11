package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haify-project/haify/pkg/config"
	"github.com/haify-project/haify/pkg/deployment"
)

// The default used to live in haify, so the CLI created thin pools while the
// REST gateway, MCP and the web UI created thick ones from the same omitted
// field — and storage.default_pool_type, the setting that exists to decide it,
// was read by nobody. Pin the resolution here, where every client shares it.
func TestOmittedPoolTypeComesFromTheConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		requested  string
		want       string
	}{
		{"omitted takes the configured default", "thin_pool", "", "thin_pool"},
		{"omitted takes a thick default too", "vg", "", "vg"},
		{"whitespace counts as omitted", "thin_pool", "   ", "thin_pool"},
		{"an explicit type always wins", "thin_pool", "vg", "vg"},
		{"an explicit thin type wins over a thick default", "vg", "lvm-thin", "lvm-thin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm := &StorageManager{controller: &Controller{
				config: &config.Config{Storage: config.StorageConfig{DefaultPoolType: tc.configured}},
			}}
			assert.Equal(t, tc.want, sm.defaultedPoolType(tc.requested))
		})
	}
}

// A StorageManager built without configuration — tests, and any embedded use —
// must keep working rather than panic on a nil dereference deep inside pool
// creation. It falls through to normalizeLVMPoolType's own handling of "".
func TestOmittedPoolTypeSurvivesAMissingConfiguration(t *testing.T) {
	assert.Equal(t, "", (&StorageManager{}).defaultedPoolType(""))
	assert.Equal(t, "", (&StorageManager{controller: &Controller{}}).defaultedPoolType(""))
	assert.Equal(t, "vg", (&StorageManager{controller: &Controller{}}).defaultedPoolType("vg"))
}

// Whatever the configuration says, it has to survive normalisation into one of
// the two names the backend actually stores. A default that normalises to an
// error would turn every type-less pool creation into a failure.
func TestEveryAcceptedDefaultNormalises(t *testing.T) {
	for _, in := range []string{"vg", "lvm", "lvm-thin", "thin-pool", "thin_pool"} {
		got, err := normalizeLVMPoolType(in)
		assert.NoError(t, err, "%q is accepted by config validation and must normalise", in)
		assert.Contains(t, []string{"vg", "thin_pool"}, got)
	}
}

// The resolution above is worth nothing if CreatePool stops calling it, and
// that is exactly the failure this whole change is about: a helper that is
// written, tested and never reached. Drive the real entry point and watch which
// LVM path the omitted type actually takes.
func TestCreatePoolAppliesTheConfiguredDefault(t *testing.T) {
	ctx := context.Background()

	newRecorder := func() (*fakeDeploymentClient, *bool) {
		thin := false
		dep := &fakeDeploymentClient{
			lvCreateThinPoolAllFreeFunc: func(context.Context, []string, string, string, uint64) (*deployment.ExecResult, error) {
				thin = true
				return successExecResult([]string{"n1"}, ""), nil
			},
			lvCreateThinPoolFunc: func(context.Context, []string, string, string, string) (*deployment.ExecResult, error) {
				thin = true
				return successExecResult([]string{"n1"}, ""), nil
			},
		}
		return dep, &thin
	}

	withDefault := func(t *testing.T, poolType string) (*Controller, *bool) {
		t.Helper()
		dep, thin := newRecorder()
		ctrl := newBasicTestController(dep)
		ctrl.config = &config.Config{Storage: config.StorageConfig{DefaultPoolType: poolType}}
		return ctrl, thin
	}

	t.Run("a thin default makes an omitted type thin", func(t *testing.T) {
		ctrl, thin := withDefault(t, "thin_pool")
		require.NoError(t, ctrl.storage.CreatePool(ctx, "fast", "", "n1", []string{"/dev/sdb"}, 0, false))
		assert.True(t, *thin, "storage.default_pool_type = thin_pool must build a thin pool")
	})

	t.Run("a thick default makes an omitted type thick", func(t *testing.T) {
		ctrl, thin := withDefault(t, "vg")
		require.NoError(t, ctrl.storage.CreatePool(ctx, "fast", "", "n1", []string{"/dev/sdb"}, 0, false))
		assert.False(t, *thin, "storage.default_pool_type = vg must build a plain volume group")
	})

	t.Run("an explicit type still overrides the default", func(t *testing.T) {
		ctrl, thin := withDefault(t, "thin_pool")
		require.NoError(t, ctrl.storage.CreatePool(ctx, "fast", "vg", "n1", []string{"/dev/sdb"}, 0, false))
		assert.False(t, *thin, "--type vg must win over a thin default")
	})
}
