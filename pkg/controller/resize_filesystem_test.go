package controller

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
)

// growNode answers the filesystem script per host and records where it ran.
type growNode struct {
	mu      sync.Mutex
	answers map[string]string
	ran     []string
	scripts []string
}

func (n *growNode) exec(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if script, ok := decodeGuestScript(cmd); ok && strings.Contains(script, "resize2fs") {
		n.ran = append(n.ran, hosts[0])
		n.scripts = append(n.scripts, script)
		return successExecResult(hosts, n.answers[hosts[0]]), nil
	}
	return successExecResult(hosts, ""), nil
}

func growFixture(t *testing.T, answers map[string]string) (*Controller, *growNode) {
	t.Helper()
	n := &growNode{answers: answers}
	ctrl := guestFixture(t, &guestNode{})
	ctrl.deployment.(*fakeDeploymentClient).execFunc = n.exec
	return ctrl, n
}

// The filesystem is grown on the Primary, the one node that has it mounted.
func TestGrowFilesystemOnPrimary(t *testing.T) {
	ctrl, n := growFixture(t, map[string]string{
		"10.0.0.1": "gateway=nfs\nmount=/srv/gateway-exports/plain\nfstype=ext4\nsize=21474836480\nfs=grown\n",
	})
	note, err := ctrl.resources.GrowFilesystem(context.Background(), "plain", 0, 20<<30, false)
	require.NoError(t, err)
	assert.Equal(t, "the ext4 filesystem mounted at /srv/gateway-exports/plain on 10.0.0.1 was grown to 20.0 GiB", note)
	assert.Equal(t, []string{"10.0.0.1"}, n.ran)
	assert.Contains(t, n.scripts[0], "res='plain'")
	assert.Contains(t, n.scripts[0], "vol=0")
	assert.Contains(t, n.scripts[0], "want=21474836480")
	assert.Contains(t, n.scripts[0], "force=0")
}

// A resource without a file gateway is left alone unless the request asks.
func TestGrowFilesystemNotNeeded(t *testing.T) {
	ctrl, n := growFixture(t, map[string]string{"10.0.0.1": "gateway=none\nfs=not-needed\n"})
	note, err := ctrl.resources.GrowFilesystem(context.Background(), "plain", 0, 1<<30, false)
	require.NoError(t, err)
	assert.Empty(t, note)

	// Asked for, the script is told to grow whatever is mounted.
	n.answers["10.0.0.1"] = "gateway=none\nmount=/mnt/plain\nfstype=xfs\nsize=1073741824\nfs=grown\n"
	note, err = ctrl.resources.GrowFilesystem(context.Background(), "plain", 0, 1<<30, true)
	require.NoError(t, err)
	assert.Contains(t, note, "the xfs filesystem mounted at /mnt/plain on 10.0.0.1 was grown")
	assert.Contains(t, n.scripts[len(n.scripts)-1], "force=1")
}

// A gateway whose filesystem is not mounted, a grow that fails and a node
// that does not answer all leave the resize unfinished, with the reason.
func TestGrowFilesystemUnfinished(t *testing.T) {
	cases := []struct{ out, want string }{
		{"gateway=nfs\nfs=not-mounted\n", "10.0.0.1 is Primary but has no filesystem mounted from plain/0"},
		{"gateway=nfs\nmount=/x\nfstype=ext4\nfs=failed\ndetail=resize2fs: Device or resource busy\n", "10.0.0.1: resize2fs: Device or resource busy"},
		{"gateway=smb\nmount=/x\nfstype=f2fs\nfs=unsupported\ndetail=f2fs\n", "the f2fs filesystem on 10.0.0.1 cannot be grown online from here"},
	}
	for _, c := range cases {
		ctrl, _ := growFixture(t, map[string]string{"10.0.0.1": c.out})
		note, err := ctrl.resources.GrowFilesystem(context.Background(), "plain", 0, 1<<30, false)
		require.Error(t, err, c.out)
		assert.Empty(t, note)
		assert.Contains(t, err.Error(), c.want)
		assert.Contains(t, resizeIncomplete("plain", err), "Repeat the resize")
	}

	// A node that says nothing counts only when a filesystem was asked for.
	ctrl, _ := growFixture(t, map[string]string{})
	_, err := ctrl.resources.GrowFilesystem(context.Background(), "plain", 0, 1<<30, false)
	require.NoError(t, err)
	_, err = ctrl.resources.GrowFilesystem(context.Background(), "plain", 0, 1<<30, true)
	require.ErrorContains(t, err, "10.0.0.1: the node did not answer")
	// So does the controller's record of a file gateway on the resource.
	require.NoError(t, ctrl.db.SaveGateway(context.Background(), &database.Gateway{Name: "plain", Resource: "plain", Type: database.GatewayTypeNFS}))
	_, err = ctrl.resources.GrowFilesystem(context.Background(), "plain", 0, 1<<30, false)
	require.ErrorContains(t, err, "10.0.0.1: the node did not answer")
}

// With no Primary the filesystem is mounted nowhere: a resource with a
// gateway reports the resize unfinished, any other has nothing to grow.
func TestGrowFilesystemWithoutPrimary(t *testing.T) {
	ctrl, n := growFixture(t, map[string]string{
		"10.0.0.1": "gateway=nfs\nfs=not-mounted\n",
		"10.0.0.2": "gateway=nfs\nfs=not-mounted\n",
	})
	ctrl.deployment.(*fakeDeploymentClient).drbdStatusFunc = func(_ context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
		return successExecResult(hosts, resource+" role:Secondary\n"), nil
	}
	_, err := ctrl.resources.GrowFilesystem(context.Background(), "plain", 0, 1<<30, false)
	require.ErrorContains(t, err, "plain is Primary on no node")
	assert.Equal(t, []string{"10.0.0.1"}, n.ran, "the first replica node that answers has said whether there is a gateway")

	n.answers = map[string]string{"10.0.0.1": "gateway=none\nfs=not-needed\n", "10.0.0.2": "gateway=none\nfs=not-needed\n"}
	note, err := ctrl.resources.GrowFilesystem(context.Background(), "plain", 0, 1<<30, false)
	require.NoError(t, err)
	assert.Empty(t, note)
}

// The handler reports an unfinished resize as retryable, not as a success.
func TestResizeVolumeReportsUnfinishedFilesystem(t *testing.T) {
	assert.Empty(t, mustVerdict(t, nil, true, false))
	_, err := growFSVerdict("r", 1, []growFSResult{{node: "n1", outcome: "not-mounted", gateway: "nfs"}}, true, false, false)
	require.ErrorContains(t, err, "the filesystem on r/1 was not grown")
	_, err = ctrl0(t).resources.GrowFilesystem(context.Background(), "bad name;", 0, 1, true)
	require.ErrorContains(t, err, "not a resource name")
}

func mustVerdict(t *testing.T, results []growFSResult, primary, required bool) string {
	t.Helper()
	note, err := growFSVerdict("r", 0, results, primary, required, false)
	require.NoError(t, err)
	return note
}

func ctrl0(t *testing.T) *Controller {
	t.Helper()
	ctrl, _ := growFixture(t, nil)
	return ctrl
}
