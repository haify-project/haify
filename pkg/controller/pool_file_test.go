package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/haify-project/haify/pkg/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFileDevice(t *testing.T) {
	f, err := parseFileDevice("file:/var/lib/haify-pools/data.img:50G")
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/haify-pools/data.img", f.Path)
	assert.Equal(t, uint64(50<<30), f.Size)

	for _, bad := range []string{
		"file:/var/lib/data.img",              // no size
		"file:data.img:10G",                   // relative
		"file:/var/../etc/x.img:10G",          // not clean
		"file:/dev/sdb:10G",                   // a device, not a file
		"file:/var/lib/a b.img:10G",           // shell metacharacters
		"file:/var/lib/x.img:100M",            // too small
		"file:/var/lib/x.img;rm -rf /:10G",    // injection
		"file:/var/lib/haify/pools/x.img:10G", // the Self-HA mount point
	} {
		_, err := parseFileDevice(bad)
		assert.Error(t, err, bad)
	}
}

// A thin pool's file is sparse; a thick pool's is allocated, because a thick
// volume's promise is that its space exists.
func TestFileSetupScript(t *testing.T) {
	f := fileSpec{Path: "/var/lib/haify-pools/data.img", Size: 50 << 30}
	thin := fileSetupScript(f, "haify_data", true)
	assert.Contains(t, thin, "truncate -s 53687091200 /var/lib/haify-pools/data.img")
	assert.Contains(t, fileSetupScript(f, "haify_data", false), "fallocate -l 53687091200")
	assert.Contains(t, thin, "Before=drbd.service drbd-reactor.service haify-controller.service")
	assert.Contains(t, thin, "ExecStartPost=-/sbin/vgchange -ay haify_data")
	assert.Contains(t, thin, "systemctl enable --now "+fileUnitName(f.Path))
}

func TestCreatePoolOnAFile(t *testing.T) {
	ctrl, dep := diskNode(t, oldDisksListing)
	ctrl.db = newTestDB(t)
	dep.execFunc = func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		if strings.Contains(cmd, "base64 -d") && strings.Contains(decodeWrapped(cmd), "losetup") {
			return successExecResult(hosts, "/dev/loop3"), nil
		}
		return successExecResult(hosts, ""), nil
	}
	require.NoError(t, ctrl.storage.CreatePool(context.Background(), "data", "thin_pool", "node1",
		[]string{"file:/var/lib/haify-pools/data.img:20G"}, 0, false))
	require.Len(t, dep.pvCreateCalls, 1)
	assert.Equal(t, "/dev/loop3", dep.pvCreateCalls[0].device, "the pool is built on the loop device")
	stored, err := ctrl.db.GetPool(context.Background(), "haify_data")
	require.NoError(t, err)
	assert.Equal(t, "file:/var/lib/haify-pools/data.img:20G", stored.Devices, "recorded as given, so delete can remove it")
}
