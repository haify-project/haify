package csi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type fakeAgentBackend struct {
	resources []*haifypb.AgentResource
	err       error
}

func (f *fakeAgentBackend) GetNodeAgentState(context.Context, string) ([]*haifypb.AgentResource, error) {
	return f.resources, f.err
}

// fakeDRBD stands in for drbdadm and drbdsetup: up resources and their roles.
type fakeDRBD struct {
	up       map[string]string // resource -> role
	two      map[string]bool   // resources allowing two Primaries
	peers    string            // connections JSON for every resource
	commands []string
	failCmd  string
}

func (f *fakeDRBD) run(_ context.Context, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	if name == "drbdsetup" && args[0] == "show" {
		if f.two[args[1]] {
			return "net {\n\t\tallow-two-primaries\tyes;\n}\n", nil
		}
		return "net {\n}\n", nil
	}
	if name == "drbdsetup" {
		role, ok := f.up[args[1]]
		if !ok {
			return "No such resource", fmt.Errorf("exit status 10")
		}
		peers := f.peers
		if peers == "" {
			peers = "[]"
		}
		return fmt.Sprintf(`[{"name":%q,"role":%q,"connections":%s}]`, args[1], role, peers), nil
	}
	f.commands = append(f.commands, cmd)
	if f.failCmd != "" && strings.HasPrefix(cmd, f.failCmd) {
		return "failed", fmt.Errorf("exit status 1")
	}
	switch args[0] {
	case "up":
		f.up[args[1]] = "Secondary"
	case "down":
		delete(f.up, args[1])
	case "primary":
		f.up[args[1]] = "Primary"
	case "secondary":
		f.up[args[1]] = "Secondary"
	case "adjust":
		delete(f.two, args[1])
	case "net-options":
		f.two[args[2]] = args[1] == "--allow-two-primaries=yes"
	}
	return "", nil
}

func newTestAgent(t *testing.T, b AgentBackend) (*LocalAgent, *fakeDRBD, string) {
	dir := t.TempDir()
	drbd := &fakeDRBD{up: map[string]string{}, two: map[string]bool{}}
	a := NewLocalAgent(b, "worker-1", dir, zap.NewNop())
	a.run = drbd.run
	return a, drbd, dir
}

func TestLocalAgentBringsUpAdjustsAndTakesDown(t *testing.T) {
	b := &fakeAgentBackend{resources: []*haifypb.AgentResource{{Name: "pvc-1", Config: "resource pvc-1 {}"}}}
	a, drbd, dir := newTestAgent(t, b)

	require.NoError(t, a.Reconcile(context.Background()))
	assert.Equal(t, []string{"drbdadm up pvc-1"}, drbd.commands)
	written, err := os.ReadFile(filepath.Join(dir, "pvc-1.res"))
	require.NoError(t, err)
	assert.Equal(t, agentHeader+"resource pvc-1 {}\n", string(written))

	drbd.commands = nil
	require.NoError(t, a.Reconcile(context.Background()))
	assert.Empty(t, drbd.commands, "nothing changed, nothing run")

	b.resources[0].Config = "resource pvc-1 { changed }"
	require.NoError(t, a.Reconcile(context.Background()))
	assert.Equal(t, []string{"drbdadm adjust pvc-1"}, drbd.commands)

	drbd.commands = nil
	b.resources = nil
	require.NoError(t, a.Reconcile(context.Background()))
	assert.Equal(t, []string{"drbdadm down pvc-1"}, drbd.commands)
	_, err = os.Stat(filepath.Join(dir, "pvc-1.res"))
	assert.True(t, os.IsNotExist(err))
}

// Files the agent did not write are neither replaced nor taken down.
func TestLocalAgentLeavesOtherFilesAlone(t *testing.T) {
	b := &fakeAgentBackend{resources: []*haifypb.AgentResource{{Name: "mine", Config: "resource mine {}"}}}
	a, drbd, dir := newTestAgent(t, b)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mine.res"), []byte("resource mine { by hand }\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "other.res"), []byte("resource other {}\n"), 0o600))
	drbd.up["other"] = "Secondary"

	assert.Error(t, a.Reconcile(context.Background()))
	assert.Empty(t, drbd.commands)
	got, _ := os.ReadFile(filepath.Join(dir, "mine.res"))
	assert.Equal(t, "resource mine { by hand }\n", string(got))
	_, err := os.Stat(filepath.Join(dir, "other.res"))
	assert.NoError(t, err)
}

// A resource still Primary here stays up when the controller stops listing it.
func TestLocalAgentKeepsPrimaryUp(t *testing.T) {
	b := &fakeAgentBackend{resources: []*haifypb.AgentResource{{Name: "pvc-1", Config: "resource pvc-1 {}"}}}
	a, drbd, dir := newTestAgent(t, b)
	require.NoError(t, a.Reconcile(context.Background()))
	require.NoError(t, a.Promote(context.Background(), "pvc-1"))

	b.resources = nil
	drbd.commands = nil
	require.NoError(t, a.Reconcile(context.Background()))
	assert.Empty(t, drbd.commands)
	_, err := os.Stat(filepath.Join(dir, "pvc-1.res"))
	assert.NoError(t, err)
}

// A controller that cannot answer takes nothing down.
func TestLocalAgentKeepsEverythingWhenControllerFails(t *testing.T) {
	b := &fakeAgentBackend{resources: []*haifypb.AgentResource{{Name: "pvc-1", Config: "resource pvc-1 {}"}}}
	a, drbd, _ := newTestAgent(t, b)
	require.NoError(t, a.Reconcile(context.Background()))
	b.resources, b.err = nil, fmt.Errorf("unavailable")
	drbd.commands = nil
	assert.Error(t, a.Reconcile(context.Background()))
	assert.Empty(t, drbd.commands)
	assert.Contains(t, drbd.up, "pvc-1")
}

func TestLocalAgentEnsureWaitsForData(t *testing.T) {
	b := &fakeAgentBackend{resources: []*haifypb.AgentResource{{Name: "pvc-1", Config: "resource pvc-1 {}"}}}
	a, drbd, _ := newTestAgent(t, b)
	drbd.peers = `[{"name":"storage-1","connection-state":"Connected","peer-role":"Primary","peer_devices":[{"peer-disk-state":"UpToDate"}]}]`
	require.NoError(t, a.Ensure(context.Background(), "pvc-1"))
	others, err := a.OtherPrimaries(context.Background(), "pvc-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"storage-1"}, others)
}

// An adjust resets allow-two-primaries; a migration window open at runtime is
// put back after it.
func TestLocalAgentRestoresTwoPrimariesAfterAdjust(t *testing.T) {
	b := &fakeAgentBackend{resources: []*haifypb.AgentResource{{Name: "vm-disk", Config: "resource vm-disk {}"}}}
	a, drbd, _ := newTestAgent(t, b)
	require.NoError(t, a.Reconcile(context.Background()))
	require.NoError(t, a.SetTwoPrimaries(context.Background(), "vm-disk", true))

	drbd.commands = nil
	b.resources[0].Config = "resource vm-disk { new client }"
	require.NoError(t, a.Reconcile(context.Background()))
	assert.Equal(t, []string{"drbdadm adjust vm-disk", "drbdadm net-options --allow-two-primaries=yes vm-disk"}, drbd.commands)

	require.NoError(t, a.SetTwoPrimaries(context.Background(), "vm-disk", false))
	drbd.commands = nil
	b.resources[0].Config = "resource vm-disk { client gone }"
	require.NoError(t, a.Reconcile(context.Background()))
	assert.Equal(t, []string{"drbdadm adjust vm-disk"}, drbd.commands)
}

func TestLocalAgentSkipsUnsafeNames(t *testing.T) {
	b := &fakeAgentBackend{resources: []*haifypb.AgentResource{{Name: "../etc/passwd", Config: "x"}}}
	a, drbd, dir := newTestAgent(t, b)
	require.NoError(t, a.Reconcile(context.Background()))
	assert.Empty(t, drbd.commands)
	entries, _ := os.ReadDir(dir)
	assert.Empty(t, entries)
}
