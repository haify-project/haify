package controller

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/haify-project/haify/pkg/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// haifyCLITestNode answers the haify probe on 10.0.0.1 with answers in turn,
// the last one repeating, and places the controller in a directory holding
// the given files.
func haifyCLITestNode(t *testing.T, files []string, answers ...string) (*Controller, *fakeDeploymentClient, string) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "haify-controller")
	for _, name := range append([]string{"haify-controller"}, files...) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("bin"), 0o755))
	}
	orig := controllerExecutable
	controllerExecutable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { controllerExecutable = orig })

	probe := base64Std(haifyCLIProbe())
	dep := &fakeDeploymentClient{}
	dep.execFunc = func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
		if !strings.Contains(cmd, probe) {
			return successExecResult(hosts, ""), nil
		}
		out := answers[0]
		if len(answers) > 1 {
			answers = answers[1:]
		}
		return successExecResult(hosts, out+"\n"), nil
	}
	ctrl := newBasicTestController(dep)
	ctrl.hostsMap = map[string]string{"n1": "10.0.0.1"}
	return ctrl, dep, dir
}

func TestHaifyCLIProbePrefersAnAdministratorsCopy(t *testing.T) {
	p := haifyCLIProbe()
	local, pkg, own := strings.Index(p, "/usr/local/bin/haify"), strings.Index(p, "/usr/bin/haify"), strings.Index(p, haifyCLIInstallPath)
	assert.True(t, local >= 0 && local < pkg && pkg < own, "probe order: %s", p)
	assert.Contains(t, p, `"$p" ship --help`, "a haify without the ship command does not count")
}

func TestParseHaifyCLIProbe(t *testing.T) {
	path, machine, err := parseHaifyCLIProbe("found /usr/bin/haify\n")
	require.NoError(t, err)
	assert.Equal(t, "/usr/bin/haify", path)
	assert.Empty(t, machine)

	path, machine, err = parseHaifyCLIProbe("missing aarch64\n")
	require.NoError(t, err)
	assert.Empty(t, path)
	assert.Equal(t, "aarch64", machine)

	for _, bad := range []string{"", "found", "missing ", "sudo: a password is required"} {
		_, _, err := parseHaifyCLIProbe(bad)
		assert.Error(t, err, "%q", bad)
	}
}

func TestHaifyCLISources(t *testing.T) {
	assert.Equal(t, []string{"/opt/haify/bin/haify", "/usr/local/bin/haify", "/usr/bin/haify"},
		haifyCLISources("/opt/haify/bin", runtime.GOARCH))
	assert.Equal(t, []string{"/opt/haify/bin/haify-" + foreignArch()},
		haifyCLISources("/opt/haify/bin", foreignArch()),
		"a node of another architecture is only given a build made for it")
}

func TestEnsureHaifyCLIUsesTheNodesOwn(t *testing.T) {
	ctrl, dep, _ := haifyCLITestNode(t, nil, "found /usr/bin/haify")
	path, err := ctrl.resources.ensureHaifyCLI(context.Background(), "10.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, "/usr/bin/haify", path)
	assert.Empty(t, dep.installedFiles)
}

func TestEnsureHaifyCLIInstallsTheControllersCopy(t *testing.T) {
	ctrl, dep, dir := haifyCLITestNode(t, []string{"haify"},
		"missing "+unameMachine(runtime.GOARCH), "found "+haifyCLIInstallPath)
	path, err := ctrl.resources.ensureHaifyCLI(context.Background(), "10.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, haifyCLIInstallPath, path)
	require.Len(t, dep.installedFiles, 1)
	assert.Equal(t, []string{"10.0.0.1"}, dep.installedFiles[0].hosts)
	assert.Equal(t, filepath.Join(dir, "haify"), dep.installedFiles[0].localPath)
	assert.Equal(t, haifyCLIInstallPath, dep.installedFiles[0].remotePath)
}

func TestEnsureHaifyCLIInstallsAPerArchitectureBuild(t *testing.T) {
	ctrl, dep, dir := haifyCLITestNode(t, []string{"haify", "haify-" + foreignArch()},
		"missing "+unameMachine(foreignArch()), "found "+haifyCLIInstallPath)
	_, err := ctrl.resources.ensureHaifyCLI(context.Background(), "10.0.0.1")
	require.NoError(t, err)
	require.Len(t, dep.installedFiles, 1)
	assert.Equal(t, filepath.Join(dir, "haify-"+foreignArch()), dep.installedFiles[0].localPath)
}

// The controller's own build would fail on the node only when the unit
// starts, as an exec format error in a transient unit's journal.
func TestEnsureHaifyCLIRefusesOtherArchitecture(t *testing.T) {
	machine := unameMachine(foreignArch())
	ctrl, dep, dir := haifyCLITestNode(t, []string{"haify"}, "missing "+machine)
	_, err := ctrl.resources.ensureHaifyCLI(context.Background(), "10.0.0.1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "installed on n1 ("+machine+")")
	assert.Contains(t, err.Error(), "/usr/local/bin/haify on n1")
	assert.Contains(t, err.Error(), filepath.Join(dir, "haify-"+foreignArch()))
	assert.Empty(t, dep.installedFiles)
}

func TestEnsureHaifyCLIRejectsACopyThatCannotShip(t *testing.T) {
	machine := unameMachine(runtime.GOARCH)
	ctrl, dep, _ := haifyCLITestNode(t, []string{"haify"}, "missing "+machine)
	_, err := ctrl.resources.ensureHaifyCLI(context.Background(), "10.0.0.1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot ship")
	assert.Len(t, dep.installedFiles, 1)
}
