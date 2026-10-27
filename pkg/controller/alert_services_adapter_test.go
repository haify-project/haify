package controller

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/alert"
	"github.com/haify-project/haify/pkg/config"
)

// chainOn is a promoter running its chain on node: a mount, the service, a
// service IP, and the drbd-services target.
func chainOn(node string, service HaServiceStatus) *HaPromoterStatus {
	return &HaPromoterStatus{
		DRBDResource: "openclaw",
		ActiveNode:   node,
		Deps: []HaServiceStatus{
			{Name: "var-lib-openclaw.mount", Kind: haKindMount, Status: "active", SubState: "mounted"},
			service,
			{Name: "service-ip@192.168.123.252-24.service", Kind: haKindServiceIP, Status: "active", SubState: "exited"},
		},
	}
}

func serviceUnit(status, sub string, restarts int) HaServiceStatus {
	return HaServiceStatus{Name: "openclaw.service", Kind: haKindService, Label: "OpenClaw Gateway",
		Status: status, SubState: sub, Restarts: restarts}
}

func TestServiceInfosCoverOnlyTheServicesOfAChain(t *testing.T) {
	infos := serviceInfosFromHaStatus([]*HaPromoterStatus{chainOn("node-e", serviceUnit("active", "running", 4))})

	require.Len(t, infos, 1, "the mount and the service IP are not services; their own alerts cover them")
	assert.Equal(t, alert.ServiceStatusInfo{
		Resource: "openclaw", Unit: "openclaw.service", Label: "OpenClaw Gateway",
		Node: "node-e", Active: true, SubState: "running", Restarts: 4,
	}, infos[0])
}

// What counts as running is what systemd says, not what drbd-reactor reports
// for the chain: a unit crash-looping under Restart=on-failure is "active" to
// the reactor for as long as systemd has not given up.
func TestAServiceIsActiveOnlyWhenSystemdSaysItIsRunning(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status, sub string
		want        bool
	}{
		{"running", "active", "running", true},
		{"a unit that finished by design", "active", "exited", true},
		{"no finer state reported", "active", "", true},
		{"waiting to be restarted", "active", "auto-restart", false},
		{"still starting", "active", "start", false},
		{"stopped", "inactive", "dead", false},
		{"failed", "failed", "failed", false},
		{"reactor says inactive, whatever systemd says", "inactive", "running", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			infos := serviceInfosFromHaStatus([]*HaPromoterStatus{chainOn("n1", serviceUnit(tc.status, tc.sub, 0))})
			require.Len(t, infos, 1)
			assert.Equal(t, tc.want, infos[0].Active)
		})
	}
}

// A chain that runs nowhere still lists its service, without a node, so the
// monitor can tell "the chain is down" from "the unit was removed".
func TestAChainRunningNowhereIsStillListed(t *testing.T) {
	infos := serviceInfosFromHaStatus([]*HaPromoterStatus{chainOn("", serviceUnit("inactive", "dead", 0))})

	require.Len(t, infos, 1)
	assert.Equal(t, "openclaw.service", infos[0].Unit)
	assert.Empty(t, infos[0].Node)
}

func TestServiceInfosSkipEntriesWithoutAResource(t *testing.T) {
	assert.Empty(t, serviceInfosFromHaStatus(nil))
	assert.Empty(t, serviceInfosFromHaStatus([]*HaPromoterStatus{nil, {DRBDResource: ""}}))
}

// The service lister is one more connection that can be written, tested and
// never attached; the checks would then be off whatever the config says.
func TestAlertOptionsGatherServicesOnlyWhenServiceChecksAreOn(t *testing.T) {
	c := &Controller{logger: zap.NewNop(), config: &config.Config{}}
	c.config.Alert.CheckIntervalSec = 30

	assert.Nil(t, c.alertOptions().Services, "service checks off means the chains are never probed by the alert poll")

	c.config.Alert.CheckServices = true
	assert.NotNil(t, c.alertOptions().Services)
}

func TestServiceChecksAreOnByDefault(t *testing.T) {
	cfg := loadConfigWith(t, "[alert]\nenabled = true\n")
	assert.True(t, cfg.Alert.CheckServices)

	// The example config is what operators copy, so it has to say the same.
	example, err := os.ReadFile(filepath.Join("..", "..", "configs", "controller.toml.example"))
	require.NoError(t, err)
	assert.Contains(t, string(example), "check_services = true")
}

// loadConfigWith loads a controller config made of the given TOML, so what is
// asserted is what the loader defaults, not what a struct literal says.
func loadConfigWith(t *testing.T, toml string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "controller.toml")
	require.NoError(t, os.WriteFile(path, []byte(toml), 0o600))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	return cfg
}
