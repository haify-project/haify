package controller

import (
	"context"
	"testing"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// diskNode is a controller with node1 at 10.0.0.1 whose disks are listing.
func diskNode(t *testing.T, listing string) (*Controller, *fakeDeploymentClient) {
	t.Helper()
	dep := &fakeDeploymentClient{listBlockDevicesFunc: func(host string) (*deployment.ExecResult, error) {
		return successExecResult([]string{host}, listing), nil
	}}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1", State: NodeStateOnline}
	ctrl.hostsMap["node1"] = "10.0.0.1"
	return ctrl, dep
}

func TestAddDiskRefusesOldDataUnlessWiped(t *testing.T) {
	ctrl, dep := diskNode(t, oldDisksListing)

	err := ctrl.storage.AddDiskToPool(context.Background(), "vg0", "/dev/sdj", "node1", true)
	assert.ErrorContains(t, err, "in use: backing disk of a DRBD resource", "--wipe never reaches a disk in use")

	err = ctrl.storage.AddDiskToPool(context.Background(), "vg0", "/dev/sdf", "node1", false)
	assert.ErrorContains(t, err, "retry with --wipe")
	assert.Empty(t, dep.pvCreateCalls, "a refused disk is not touched")
	assert.Empty(t, dep.wipeCalls)

	require.NoError(t, ctrl.storage.AddDiskToPool(context.Background(), "vg0", "/dev/sdf", "node1", true))
	assert.Equal(t, []string{"/dev/sdf"}, dep.wipeCalls)
	require.Len(t, dep.pvCreateCalls, 1)
	assert.Equal(t, "/dev/sdf", dep.pvCreateCalls[0].device)
}

func TestAddDiskByIDUsesTheDevice(t *testing.T) {
	ctrl, dep := diskNode(t, oldDisksListing)
	require.NoError(t, ctrl.storage.AddDiskToPool(context.Background(), "vg0", "/dev/disk/by-id/wwn-0x5000c500a1", "node1", false))
	require.Len(t, dep.pvCreateCalls, 1)
	assert.Equal(t, "/dev/sdi", dep.pvCreateCalls[0].device)
	assert.Empty(t, dep.wipeCalls, "an empty disk is not erased")
}

// A ZFS pool grows with zpool add; the LVM commands would only leave a PV
// label on the disk and fail at vgextend.
func TestAddDiskToZFSPool(t *testing.T) {
	ctrl, dep := diskNode(t, oldDisksListing)
	ctrl.db = newTestDB(t)
	require.NoError(t, ctrl.db.SavePool(context.Background(), &database.Pool{Name: "haify_tank", Type: "zfs", Node: "node1", Devices: "/dev/sdi"}))

	require.NoError(t, ctrl.storage.AddDiskToPool(context.Background(), "tank", "/dev/sdk", "node1", false))
	assert.Equal(t, []string{"haify_tank /dev/sdk"}, dep.zfsAddCalls)
	assert.Empty(t, dep.pvCreateCalls)
	stored, err := ctrl.db.GetPool(context.Background(), "haify_tank")
	require.NoError(t, err)
	assert.Equal(t, "/dev/sdi,/dev/sdk", stored.Devices)
}

// Every disk is checked before any is touched: one bad disk in the list
// leaves the others as they were.
func TestCreatePoolChecksAllDisksFirst(t *testing.T) {
	ctrl, dep := diskNode(t, oldDisksListing)
	err := ctrl.storage.CreatePool(context.Background(), "p", "vg", "node1", []string{"/dev/sde", "/dev/sdh"}, 0, true)
	assert.ErrorContains(t, err, "/dev/sdh on node1 is in use: used by md0")
	assert.Empty(t, dep.wipeCalls)
	assert.Empty(t, dep.pvCreateCalls)
	assert.Empty(t, dep.vgCreateCalls)

	err = ctrl.storage.CreatePool(context.Background(), "p", "vg", "node1", []string{"/dev/sdi", "/dev/disk/by-id/ata-X_S5"}, 0, false)
	assert.ErrorContains(t, err, "are the same disk")
}

func TestCreateZFSPoolChecksDevicesNotKeywords(t *testing.T) {
	ctrl, dep := diskNode(t, oldDisksListing)
	err := ctrl.storage.CreateZFSPool(context.Background(), "tank", "node1", []string{"mirror", "/dev/sdi", "/dev/sdf"}, "", false, false)
	assert.ErrorContains(t, err, "/dev/sdf on node1 holds a ZFS label")
	assert.Empty(t, dep.zfsCreatePoolCalls)

	require.NoError(t, ctrl.storage.CreateZFSPool(context.Background(), "tank", "node1", []string{"mirror", "/dev/sdi", "/dev/sdf"}, "", false, true))
	assert.Equal(t, []string{"/dev/sdf"}, dep.wipeCalls)
}

func TestListNodeDisksSkipsUnreachableNodes(t *testing.T) {
	ctrl, dep := diskNode(t, oldDisksListing)
	ctrl.nodes.nodes["10.0.0.2"] = &NodeInfo{Name: "node2", Address: "10.0.0.2", State: NodeStateOnline}
	dep.listBlockDevicesFunc = func(host string) (*deployment.ExecResult, error) {
		if host == "10.0.0.2" {
			return failedExecResult([]string{host}, "ssh: connect timed out"), nil
		}
		return successExecResult([]string{host}, oldDisksListing), nil
	}
	disks, err := ctrl.storage.ListNodeDisks(context.Background(), "")
	require.NoError(t, err)
	assert.NotEmpty(t, disks)
	for _, d := range disks {
		assert.Equal(t, "node1", d.Node)
	}

	_, err = ctrl.storage.ListNodeDisks(context.Background(), "node2")
	assert.ErrorContains(t, err, "connect timed out", "asked about one node, its failure is the answer")
}

// A disk that joined the group on an attempt whose thin pool growth failed is
// finished by running the same add again, not refused as already in a pool.
func TestAddDiskRetryFinishesThinGrowth(t *testing.T) {
	ctrl, dep := diskNode(t, "D|/dev/sdc||6442450944|disk|0|1||||\nB|/dev/sdc|LVM2_member|\nP|/dev/sdc|haify_vg0\n")
	grown := false
	dep.lvThinPoolInFunc = func(context.Context, string, string) (string, error) { return "haify_vg0_thin", nil }
	dep.lvSizeBytesFunc = func(string, string, string) (uint64, error) { return 12 * gib, nil }
	dep.vgFreeBytesFunc = func(string, string) (uint64, error) { return 6 * gib, nil }
	dep.lvExtendThinPoolPercentFreeFunc = func(hosts []string, _, _ string, _ int) (*deployment.ExecResult, error) {
		grown = true
		return successExecResult(hosts, ""), nil
	}
	require.NoError(t, ctrl.storage.AddDiskToPool(context.Background(), "vg0", "/dev/sdc", "node1", false))
	assert.True(t, grown)
	assert.Empty(t, dep.pvCreateCalls)

	_, err := ctrl.storage.prepareDisks(context.Background(), "10.0.0.1", "node1", []string{"/dev/sdc"}, false)
	assert.ErrorContains(t, err, "already in pool haify_vg0", "another pool still cannot take it")
}
