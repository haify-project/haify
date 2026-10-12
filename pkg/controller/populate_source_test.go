package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
)

// populateSourceFixture is a controller whose database holds resource "src"
// with one volume on device, and whose nodes answer every command with
// success unless failOn appears in it. It returns the commands run.
func populateSourceFixture(t *testing.T, pool, device, failOn string) (*Controller, *[]string) {
	t.Helper()
	var cmds []string
	dep := &fakeDeploymentClient{}
	dep.execFunc = func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		cmds = append(cmds, cmd)
		if failOn != "" && strings.Contains(cmd, failOn) {
			return failedExecResult(hosts, "dataset does not exist"), nil
		}
		return successExecResult(hosts, ""), nil
	}
	ctrl := newBasicTestController(dep)
	ctrl.db = newTestDB(t)
	ctx := context.Background()
	require.NoError(t, ctrl.db.SaveResource(ctx, &database.Resource{Name: "src", Port: 7000, Nodes: "10.0.0.1,10.0.0.2"}))
	require.NoError(t, ctrl.db.SaveVolume(ctx, &database.Volume{ResourceName: "src", VolumeName: "src_data", Pool: pool,
		SizeGB: 2, Device: device}))
	return ctrl, &cmds
}

// Each kind of snapshot has its own name on each kind of pool, and only the
// controller knows which kind of pool a volume is on.
func TestSnapshotBackingName(t *testing.T) {
	lvm := &database.Volume{Pool: "haify_vg0", VolumeName: "src_data", Device: "/dev/haify_vg0/src_data"}
	zfs := &database.Volume{Pool: "haify_tank", VolumeName: "src_data", Device: "/dev/zvol/haify_tank/src_data"}
	assert.Equal(t, "haify_vg0/src_data_snap_before", snapshotBackingName(lvm, "before", true))
	assert.Equal(t, "haify_vg0/haifysnap_s1", snapshotBackingName(lvm, "haifysnap_s1", false))
	assert.Equal(t, "haify_tank/src_data@snap_before", snapshotBackingName(zfs, "before", true))
	assert.Equal(t, "haify_tank/src_data@haifysnap_s1", snapshotBackingName(zfs, "haifysnap_s1", false))
}

// On LVM the snapshot is read where it is, once it is known to exist there.
func TestResolveSnapshotSourceLVM(t *testing.T) {
	ctrl, cmds := populateSourceFixture(t, "haify_vg0", "/dev/haify_vg0/src_data", "")
	dev, release, err := ctrl.snapshots.resolveSnapshotSource(context.Background(), "10.0.0.1", "n1", "dst", 0,
		SnapshotSource{Resource: "src", Name: "before", Replicated: true})
	require.NoError(t, err)
	assert.Equal(t, "/dev/haify_vg0/src_data_snap_before", dev)
	release()
	assert.Equal(t, []string{"sudo lvs --noheadings -o lv_name haify_vg0/src_data_snap_before"}, *cmds,
		"nothing is created, so nothing is removed")

	_, _, err = ctrl.snapshots.resolveSnapshotSource(context.Background(), "10.0.0.1", "n1", "dst", 0,
		SnapshotSource{Resource: "src", Name: "haifysnap_s1"})
	require.NoError(t, err)
	assert.Contains(t, (*cmds)[1], "haify_vg0/haifysnap_s1", "a one-node snapshot has its own name")
}

// A missing LVM snapshot is reported by name before anything is promoted or
// written.
func TestResolveSnapshotSourceLVMMissing(t *testing.T) {
	ctrl, _ := populateSourceFixture(t, "haify_vg0", "/dev/haify_vg0/src_data", "lvs")
	_, _, err := ctrl.snapshots.resolveSnapshotSource(context.Background(), "10.0.0.1", "n1", "dst", 0,
		SnapshotSource{Resource: "src", Name: "gone", Replicated: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "snapshot gone of src (haify_vg0/src_data_snap_gone) is not on n1")
}

// A ZFS snapshot has no device of its own: it is read through a read-only
// clone, which goes again once the copy is done.
func TestResolveSnapshotSourceZFS(t *testing.T) {
	ctrl, cmds := populateSourceFixture(t, "haify_tank", "/dev/zvol/haify_tank/src_data", "")
	dev, release, err := ctrl.snapshots.resolveSnapshotSource(context.Background(), "10.0.0.1", "n1", "dst", 0,
		SnapshotSource{Resource: "src", Name: "before", Replicated: true})
	require.NoError(t, err)
	assert.Equal(t, "/dev/zvol/haify_tank/haify-populate-dst-0", dev)
	require.Len(t, *cmds, 1)
	assert.Contains(t, (*cmds)[0], "sudo zfs clone -o readonly=on haify_tank/src_data@snap_before haify_tank/haify-populate-dst-0")
	assert.Contains(t, (*cmds)[0], "[ -e /dev/zvol/haify_tank/haify-populate-dst-0 ]", "the device node is waited for")
	assert.Less(t, strings.Index((*cmds)[0], "zfs destroy"), strings.Index((*cmds)[0], "zfs clone"),
		"a clone left by an earlier copy goes first")

	release()
	require.Len(t, *cmds, 2)
	assert.Contains(t, (*cmds)[1], "sudo zfs destroy haify_tank/haify-populate-dst-0")

	_, release, err = ctrl.snapshots.resolveSnapshotSource(context.Background(), "10.0.0.1", "n1", "dst", 0,
		SnapshotSource{Resource: "src", Name: "haifysnap_s1"})
	require.NoError(t, err)
	release()
	assert.Contains(t, (*cmds)[2], "haify_tank/src_data@haifysnap_s1 ", "a one-node snapshot has its own name")
}

// A clone that could not be made, or whose device never appeared, is removed
// and the error names the snapshot.
func TestResolveSnapshotSourceZFSFailure(t *testing.T) {
	ctrl, cmds := populateSourceFixture(t, "haify_tank", "/dev/zvol/haify_tank/src_data", "zfs clone")
	_, _, err := ctrl.snapshots.resolveSnapshotSource(context.Background(), "10.0.0.1", "n1", "dst", 0,
		SnapshotSource{Resource: "src", Name: "gone", Replicated: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "snapshot gone of src (haify_tank/src_data@snap_gone)")
	require.Len(t, *cmds, 2)
	assert.True(t, strings.HasPrefix((*cmds)[1], "sudo udevadm settle"), "the partial clone is removed")
}

// Names go into shell commands, and the source volume must exist.
func TestResolveSnapshotSourceRejects(t *testing.T) {
	ctrl, cmds := populateSourceFixture(t, "haify_vg0", "/dev/haify_vg0/src_data", "")
	for _, src := range []SnapshotSource{
		{Resource: "src", Name: "a; rm -rf /"},
		{Resource: "src", Name: ""},
		{Resource: "", Name: "before"},
		{Resource: "src", VolumeID: 3, Name: "before"},
		{Resource: "ghost", Name: "before"},
	} {
		_, _, err := ctrl.snapshots.resolveSnapshotSource(context.Background(), "10.0.0.1", "n1", "dst", 0, src)
		assert.Error(t, err, "%+v", src)
	}
	assert.Empty(t, *cmds, "nothing runs on a node for a source that does not resolve")
}

// The copy reads the clone, and the clone goes only after the copy.
func TestPopulateVolumeFromZFSSnapshot(t *testing.T) {
	ctrl, cmds := populateSourceFixture(t, "haify_tank", "/dev/zvol/haify_tank/src_data", "")
	_, err := ctrl.snapshots.PopulateVolumeFromSnapshot(context.Background(), "dst", 0,
		SnapshotSource{Resource: "src", Name: "before", Replicated: true}, "10.0.0.1")
	require.NoError(t, err)
	joined := strings.Join(*cmds, "\n")
	clone := strings.Index(joined, "zfs clone -o readonly=on haify_tank/src_data@snap_before")
	copied := strings.Index(joined, "dd if=/dev/zvol/haify_tank/haify-populate-dst-0 of=/dev/drbd/by-res/dst/0")
	destroyed := strings.LastIndex(joined, "zfs destroy haify_tank/haify-populate-dst-0")
	require.True(t, clone >= 0 && copied > clone && destroyed > copied, joined)
}

// The API takes a snapshot or a device, not both.
func TestPopulateVolumeRequestSourceIsOneOrTheOther(t *testing.T) {
	ctrl, cmds := populateSourceFixture(t, "haify_vg0", "/dev/haify_vg0/src_data", "")
	srv := &Server{snapshots: ctrl.snapshots}
	resp, err := srv.PopulateVolume(context.Background(), &haifypb.PopulateVolumeRequest{
		Resource: "dst", Node: "n1", SourceDevice: "/dev/haify_vg0/x",
		SourceSnapshot: &haifypb.PopulateSnapshotSource{Resource: "src", Name: "before"},
	})
	require.NoError(t, err)
	assert.False(t, resp.GetSuccess())
	assert.Contains(t, resp.GetMessage(), "not both")
	assert.Empty(t, *cmds)
}

// The one-node snapshot calls go to ZFS for a volume recorded as a zvol, so a
// CSI snapshot of a volume on a ZFS pool is a ZFS snapshot.
func TestOneNodeSnapshotsFollowThePoolType(t *testing.T) {
	ctrl, cmds := populateSourceFixture(t, "haify_tank", "/dev/zvol/haify_tank/src_data", "")
	ds, ok := ctrl.snapshots.zfsVolumeDataset(context.Background(), "haify_tank/src_data")
	assert.True(t, ok)
	assert.Equal(t, "haify_tank/src_data", ds)
	require.NoError(t, ctrl.snapshots.CreateSnapshot(context.Background(), "haify_tank/src_data", "haifysnap_s1", "n1"))
	for _, c := range *cmds {
		assert.NotContains(t, c, "lvcreate", "a zvol is not snapshotted with LVM")
	}

	lvmCtrl, lvmCmds := populateSourceFixture(t, "haify_vg0", "/dev/haify_vg0/src_data", "")
	_, ok = lvmCtrl.snapshots.zfsVolumeDataset(context.Background(), "haify_vg0/src_data")
	assert.False(t, ok)
	require.NoError(t, lvmCtrl.snapshots.CreateSnapshot(context.Background(), "haify_vg0/src_data", "haifysnap_s1", "n1"))
	require.Len(t, *lvmCmds, 1)
	assert.Contains(t, (*lvmCmds)[0], "lvcreate -s")
}
