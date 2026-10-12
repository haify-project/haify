package gateway

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Tests for taking a gateway in and out of drbd-reactor's hands. They assert on
// the decoded script that reaches the nodes, because the whole point of the
// disable-then-stop ordering (and of base64-wrapping the script) is lost if the
// command is merely well-formed.

func TestManagerStopGateway(t *testing.T) {
	logger := zap.NewNop()
	mockDeployment := &MockDeploymentClient{}
	manager := New(nil, mockDeployment, logger, []string{"node1", "node2"})

	err := manager.StopGateway(context.Background(), "test-resource")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(mockDeployment.ExecCommands), 3)
	// Stop must disable the reactor config FIRST: a plain systemctl stop is
	// undone within seconds because reactor re-promotes the resource. The
	// script travels base64-encoded to survive dispatch's sh -c "..."
	// quoting, which empties $variables.
	decoded := decodeScriptCommand(t, mockDeployment.ExecCommands[0])
	assert.Contains(t, decoded, `mv "$f" "$f.disabled"`)
	assert.Contains(t, strings.Join(mockDeployment.ExecCommands, "\n"), "reload drbd-reactor")
	assert.Contains(t, strings.Join(mockDeployment.ExecCommands, "\n"), "drbd-services@test\\x2dresource.target")
}

// decodeScriptCommand extracts and decodes the base64 payload from a
// runScript-style command ("echo <b64> | base64 -d | sudo /bin/sh").
func decodeScriptCommand(t *testing.T, cmd string) string {
	t.Helper()
	require.Contains(t, cmd, "base64 -d")
	fields := strings.Fields(cmd)
	require.GreaterOrEqual(t, len(fields), 2)
	raw, err := base64.StdEncoding.DecodeString(fields[1])
	require.NoError(t, err)
	return string(raw)
}

func TestManagerStartGatewayReenablesConfig(t *testing.T) {
	logger := zap.NewNop()
	mockDeployment := &MockDeploymentClient{}
	manager := New(nil, mockDeployment, logger, []string{"node1"})

	err := manager.StartGateway(context.Background(), "test-resource")
	require.NoError(t, err)
	// Stale portblock rules are flushed (failback safety) before the config
	// is re-enabled.
	scripts := scriptsOn(t, mockDeployment, "node1")
	flushAt, enableAt := -1, -1
	for i, s := range scripts {
		if strings.Contains(s, "iptables -D INPUT") && flushAt < 0 {
			flushAt = i
		}
		if strings.Contains(s, `mv "$f.disabled" "$f"`) {
			enableAt = i
		}
	}
	require.GreaterOrEqual(t, flushAt, 0)
	require.Greater(t, enableAt, flushAt)
}

// scriptsOn returns the decoded runScript payloads that ran on host.
func scriptsOn(t *testing.T, d *MockDeploymentClient, host string) []string {
	t.Helper()
	var out []string
	for i, cmd := range d.ExecCommands {
		if !strings.Contains(cmd, "base64 -d") {
			continue
		}
		for _, h := range d.ExecHosts[i] {
			if h == host {
				out = append(out, decodeScriptCommand(t, cmd))
			}
		}
	}
	return out
}

// tiebreakerCluster is three managed hosts where only two hold the resource;
// orange3 is its diskless tiebreaker.
func tiebreakerCluster() (*Manager, *MockDeploymentClient) {
	d := &MockDeploymentClient{}
	rm := &MockResourceManager{Resources: map[string]*ResourceInfo{
		"xplat": {Name: "xplat", Nodes: []string{"orange1", "orange2"}, Hosts: []string{"orange1", "orange2"}},
	}}
	return New(rm, d, zap.NewNop(), []string{"orange1", "orange2", "orange3"}), d
}

func TestPromoterConfigGoesOnlyToReplicas(t *testing.T) {
	m, d := tiebreakerCluster()
	require.NoError(t, m.writeReactorConfig(context.Background(), "xplat", "haify-nfs-xplat", "cfg"))

	assert.Equal(t, []string{"orange1", "orange2"}, d.ConfigHosts["/etc/drbd-reactor.d/haify-nfs-xplat.toml"])
	// The tiebreaker loses any promoter it already has, and the gateway is
	// stopped there in case it is the node currently running it.
	retired := strings.Join(scriptsOn(t, d, "orange3"), "\n")
	assert.Contains(t, retired, `rm -f "$f"`)
	assert.Contains(t, retired, "systemctl stop 'drbd-services@xplat.target'")
	for _, s := range scriptsOn(t, d, "orange1") {
		assert.NotContains(t, s, "rm -f", "a replica's promoter must not be retired")
	}
}

func TestUnknownResourceKeepsPromoterEverywhere(t *testing.T) {
	d := &MockDeploymentClient{}
	m := New(&MockResourceManager{}, d, zap.NewNop(), []string{"n1", "n2"})
	require.NoError(t, m.writeReactorConfig(context.Background(), "gone", "haify-iscsi-gone", "cfg"))

	assert.Equal(t, []string{"n1", "n2"}, d.ConfigHosts["/etc/drbd-reactor.d/haify-iscsi-gone.toml"])
	for _, h := range []string{"n1", "n2"} {
		for _, s := range scriptsOn(t, d, h) {
			assert.NotContains(t, s, "rm -f", "nothing may be retired on a guess")
		}
	}
}

func TestStartGatewayRetiresTheTiebreakersPromoter(t *testing.T) {
	m, d := tiebreakerCluster()
	require.NoError(t, m.StartGateway(context.Background(), "xplat"))

	// The drop-in is guarded to hosts that carry this gateway's NFS promoter.
	assert.Contains(t, strings.Join(scriptsOn(t, d, "orange1"), "\n"), "haify-nfs-xplat.toml* >/dev/null 2>&1 || exit 0")
	onTiebreaker := strings.Join(scriptsOn(t, d, "orange3"), "\n")
	assert.Contains(t, onTiebreaker, `rm -f "$f"`)
	assert.NotContains(t, onTiebreaker, `mv "$f.disabled" "$f"`,
		"re-enabling on the tiebreaker hands it the resource again")
	assert.Contains(t, strings.Join(scriptsOn(t, d, "orange1"), "\n"), `mv "$f.disabled" "$f"`)
}

func TestNFSGatewayTiesHelperDaemonsToServer(t *testing.T) {
	m, d := tiebreakerCluster()
	require.NoError(t, m.writeReactorConfig(context.Background(), "xplat", "haify-nfs-xplat", "cfg"))

	onReplica := strings.Join(scriptsOn(t, d, "orange1"), "\n")
	assert.Contains(t, onReplica, "for u in fsidd nfsdcld")
	assert.Contains(t, onReplica, "PartOf=nfs-server.service")
	assert.Contains(t, onReplica, "systemctl disable -q nfs-server")
	assert.Contains(t, onReplica, "'ocf.rs@nfsserver_*'",
		"strays are stopped only where no gateway is serving NFS")
	assert.Contains(t, onReplica, "systemctl stop nfs-server fsidd nfsdcld")
	assert.NotContains(t, strings.Join(scriptsOn(t, d, "orange3"), "\n"), "PartOf=")

	d2 := &MockDeploymentClient{}
	m2 := New(&MockResourceManager{}, d2, zap.NewNop(), []string{"n1"})
	require.NoError(t, m2.writeReactorConfig(context.Background(), "blk", "haify-iscsi-blk", "cfg"))
	assert.NotContains(t, strings.Join(scriptsOn(t, d2, "n1"), "\n"), "PartOf=")
}

// A gateway's state mount must leave /var/lib/haify, where the controller's own
// Self-HA mount covers it. Starting a stopped legacy gateway rewrites its
// disabled config — and only that — before re-enabling it.
func TestStartGatewayMovesStateMountOutOfSelfHaPath(t *testing.T) {
	mockDeployment := &MockDeploymentClient{}
	manager := New(nil, mockDeployment, zap.NewNop(), []string{"node1"})
	require.NoError(t, manager.StartGateway(context.Background(), "isc1"))

	scripts := scriptsOn(t, mockDeployment, "node1")
	moveAt, enableAt := -1, -1
	for i, s := range scripts {
		if strings.Contains(s, "old=/var/lib/haify/isc1 new=/var/lib/haify-gateway/isc1") {
			moveAt = i
		}
		if strings.Contains(s, `mv "$f.disabled" "$f"`) {
			enableAt = i
		}
	}
	require.GreaterOrEqual(t, moveAt, 0)
	require.Greater(t, enableAt, moveAt)

	// Run the rewrite for real against a copy of a legacy config.
	dir := t.TempDir()
	legacy := `      start = [
        "ocf:heartbeat:Filesystem fs_cluster_private device=/dev/drbd15 directory=/var/lib/haify/isc1 fstype=ext4 run_fsck=no",
        "ocf:heartbeat:nfsserver nfsserver nfs_ip=10.0.0.9 nfs_shared_infodir=/var/lib/haify/isc1/nfs nfs_server_scope=10.0.0.9",
        "ocf:heartbeat:Filesystem other directory=/var/lib/haify/isc10 fstype=ext4",
        "ocf:heartbeat:portblock portunblock0 ip=10.0.0.9 portno=3260 action=unblock protocol=tcp tickle_dir=/var/lib/haify/isc1",
      ]
`
	conf := filepath.Join(dir, "haify-iscsi-isc1.toml.disabled")
	require.NoError(t, os.WriteFile(conf, []byte(legacy), 0644))
	mountinfo := filepath.Join(dir, "mountinfo")
	require.NoError(t, os.WriteFile(mountinfo, []byte("66 32 147:7 / /var/lib/haify rw - ext4 /dev/drbd7 rw\n"), 0644))

	script := scripts[moveAt]
	script = strings.ReplaceAll(script, "/etc/drbd-reactor.d", dir)
	script = strings.ReplaceAll(script, "/proc/self/mountinfo", mountinfo)
	out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput()
	require.NoError(t, err, string(out))

	got, err := os.ReadFile(conf)
	require.NoError(t, err)
	assert.Contains(t, string(got), "directory=/var/lib/haify-gateway/isc1 fstype")
	assert.Contains(t, string(got), "nfs_shared_infodir=/var/lib/haify-gateway/isc1/nfs ")
	assert.Contains(t, string(got), `tickle_dir=/var/lib/haify-gateway/isc1",`)
	assert.Contains(t, string(got), "directory=/var/lib/haify/isc10 ", "another gateway's path must be left alone")

	// The old mount still exists but is not reachable by path: refuse.
	require.NoError(t, os.WriteFile(mountinfo, []byte("325 32 147:15 / /var/lib/haify/isc1 rw - ext4 /dev/drbd15 rw\n"), 0644))
	out, err = exec.Command("/bin/sh", "-c", script).CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(out), "hidden under the controller database mount")
}
