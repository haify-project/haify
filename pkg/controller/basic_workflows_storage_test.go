package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStorageManagerGetPoolResolvesNodeAndParsesOutput(t *testing.T) {
	dep := &fakeDeploymentClient{
		execFunc: func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
			require.Equal(t, []string{"10.0.0.1"}, hosts)
			require.Contains(t, cmd, "vgs")
			return successExecResult(hosts, "haify_data-pool|10737418240B|5368709120B\n"), nil
		},
	}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1", Hostname: "node1.local"}
	ctrl.hostsMap["node1"] = "10.0.0.1"

	pool, err := ctrl.storage.GetPool(context.Background(), "data-pool", "node1")
	require.NoError(t, err)
	assert.Equal(t, "haify_data-pool", pool.Name)
	assert.Equal(t, uint64(10), pool.TotalGB)
	assert.Equal(t, uint64(5), pool.FreeGB)
}

func TestStorageManagerAddAndDeletePoolUseNormalizedName(t *testing.T) {
	var extended string
	dep := &fakeDeploymentClient{vgExtendFunc: func(_ context.Context, hosts []string, vg, dev string) (*deployment.ExecResult, error) {
		extended = vg + " " + dev
		return successExecResult(hosts, ""), nil
	}}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.hostsMap["node1"] = "10.0.0.1"

	err := ctrl.storage.AddDiskToPool(context.Background(), "data-pool", "/dev/sdb", "node1", false)
	require.NoError(t, err)
	require.Len(t, dep.pvCreateCalls, 1)
	assert.Equal(t, []string{"10.0.0.1"}, dep.pvCreateCalls[0].hosts)
	assert.Equal(t, "haify_data-pool /dev/sdb", extended)

	err = ctrl.storage.DeletePool(context.Background(), "data-pool", "node1")
	require.NoError(t, err)
	script := decodeWrapped(dep.execCalls[len(dep.execCalls)-1].cmd)
	assert.Contains(t, script, "vg=haify_data-pool")
	assert.Contains(t, script, `vgremove -f "$vg"`)
	assert.Contains(t, script, "pvremove", "the disks must be released for reuse")
}

// vgremove -f takes every LV with it. A pool that still holds replicas must be
// refused, not emptied.
func TestStorageManagerRefusesToDeleteAPoolInUse(t *testing.T) {
	dep := &fakeDeploymentClient{
		execFunc: func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
			return failedExecResult(hosts, "pool haify_tp still holds volumes: r3_data r5_data — delete or move the resources on it first"), nil
		},
	}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.hostsMap["node1"] = "10.0.0.1"

	err := ctrl.storage.DeletePool(context.Background(), "tp", "node1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "r3_data")
	assert.Empty(t, dep.zfsDestroyPoolCalls, "an LVM pool in use must not fall through to a ZFS destroy")
}

func TestStorageManagerCreatePoolPersistsDatabaseState(t *testing.T) {
	dep := &fakeDeploymentClient{}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.hostsMap["node1"] = "10.0.0.1"

	db := newTestDB(t)
	ctrl.db = db

	err := ctrl.storage.CreatePool(context.Background(), "data-pool", "lvm-thin", "node1", []string{"/dev/sdb", "/dev/sdc"}, 100, false)
	require.NoError(t, err)

	stored, err := ctrl.db.GetPool(context.Background(), "haify_data-pool")
	require.NoError(t, err)
	assert.Equal(t, "thin_pool", stored.Type)
	assert.Equal(t, "node1", stored.Node)
	assert.Equal(t, "/dev/sdb,/dev/sdc", stored.Devices)
	require.Len(t, dep.lvCreateThinPoolCalls, 1)
	assert.Equal(t, "haify_data-pool", dep.lvCreateThinPoolCalls[0].vgName)
}

func TestStorageManagerCreateZFSPoolPersistsThinState(t *testing.T) {
	dep := &fakeDeploymentClient{}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.hostsMap["node1"] = "10.0.0.1"

	db := newTestDB(t)
	ctrl.db = db

	err := ctrl.storage.CreateZFSPool(context.Background(), "tank", "node1", []string{"/dev/nvme0n1"}, "", false, false)
	require.NoError(t, err)

	require.Len(t, dep.zfsCreatePoolCalls, 1)
	assert.Equal(t, []string{"10.0.0.1"}, dep.zfsCreatePoolCalls[0].hosts)
	assert.Equal(t, "haify_tank", dep.zfsCreatePoolCalls[0].poolName)
	assert.Equal(t, []string{"/dev/nvme0n1"}, dep.zfsCreatePoolCalls[0].vdevs)
	assert.Equal(t, 2, dep.zfsCreatePoolCalls[0].optCount, "compression and dedup are always passed; empty and false add nothing")

	stored, err := ctrl.db.GetPool(context.Background(), "haify_tank")
	require.NoError(t, err)
	assert.Equal(t, "zfs", stored.Type)
	assert.Equal(t, "node1", stored.Node)
	assert.Equal(t, "/dev/nvme0n1", stored.Devices)
}

func TestStorageManagerAddDiskUpdatesPersistedPoolDevices(t *testing.T) {
	dep := &fakeDeploymentClient{}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.hostsMap["node1"] = "10.0.0.1"

	db := newTestDB(t)
	ctrl.db = db

	require.NoError(t, ctrl.db.SavePool(context.Background(), &database.Pool{
		Name:    "haify_data-pool",
		Type:    "vg",
		Node:    "node1",
		Devices: "/dev/sdb",
	}))

	err := ctrl.storage.AddDiskToPool(context.Background(), "data-pool", "/dev/sdc", "node1", false)
	require.NoError(t, err)

	stored, err := ctrl.db.GetPool(context.Background(), "haify_data-pool")
	require.NoError(t, err)
	assert.Equal(t, "/dev/sdb,/dev/sdc", stored.Devices)
}

func TestStorageManagerDeletePoolRemovesPersistedState(t *testing.T) {
	dep := &fakeDeploymentClient{}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.hostsMap["node1"] = "10.0.0.1"

	db := newTestDB(t)
	ctrl.db = db

	require.NoError(t, ctrl.db.SavePool(context.Background(), &database.Pool{
		Name: "haify_data-pool",
		Type: "vg",
		Node: "node1",
	}))

	err := ctrl.storage.DeletePool(context.Background(), "data-pool", "node1")
	require.NoError(t, err)
	_, err = ctrl.db.GetPool(context.Background(), "haify_data-pool")
	assert.ErrorContains(t, err, "not found")
}

func TestStorageManagerGetPoolFallsBackToDatabase(t *testing.T) {
	dep := &fakeDeploymentClient{
		execFunc: func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
			return nil, assert.AnError
		},
	}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.hostsMap["node1"] = "10.0.0.1"

	db := newTestDB(t)
	ctrl.db = db

	require.NoError(t, ctrl.db.SavePool(context.Background(), &database.Pool{
		Name:    "haify_data-pool",
		Type:    "vg",
		Node:    "node1",
		TotalGB: 123,
		FreeGB:  45,
		Devices: "/dev/sdb,/dev/sdc",
	}))

	pool, err := ctrl.storage.GetPool(context.Background(), "data-pool", "node1")
	require.NoError(t, err)
	assert.Equal(t, "haify_data-pool", pool.Name)
	assert.Equal(t, uint64(123), pool.TotalGB)
	assert.Equal(t, uint64(45), pool.FreeGB)
	assert.Equal(t, []string{"/dev/sdb", "/dev/sdc"}, pool.Devices)
}

func TestStorageManagerGetPoolFallsBackToZFS(t *testing.T) {
	dep := &fakeDeploymentClient{
		execFunc: func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
			require.Equal(t, []string{"10.0.0.1"}, hosts)
			switch {
			case strings.Contains(cmd, "vgs"):
				return successExecResult(hosts, ""), nil
			case strings.Contains(cmd, "zpool list -Hp -o name,size,free,cap haify_tank"):
				return successExecResult(hosts, "haify_tank\t21474836480\t10737418240\t50%\n"), nil
			case strings.Contains(cmd, "base64 -d"): // compression and its ratio
				return successExecResult(hosts, "haify_tank\tcompression\tlz4\n"), nil
			default:
				t.Fatalf("unexpected command: %s", cmd)
				return nil, nil
			}
		},
	}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.hostsMap["node1"] = "10.0.0.1"

	pool, err := ctrl.storage.GetPool(context.Background(), "tank", "node1")
	require.NoError(t, err)
	assert.Equal(t, "haify_tank", pool.Name)
	assert.Equal(t, "zfs", pool.Type)
	assert.Equal(t, "lz4", pool.Compression)
	assert.Equal(t, uint64(20), pool.TotalGB)
	assert.Equal(t, uint64(10), pool.FreeGB)
}

func TestStorageManagerDeletePoolUsesZFSPathFromPersistedType(t *testing.T) {
	dep := &fakeDeploymentClient{}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.hostsMap["node1"] = "10.0.0.1"

	db := newTestDB(t)
	ctrl.db = db

	require.NoError(t, ctrl.db.SavePool(context.Background(), &database.Pool{
		Name: "haify_tank",
		Type: "zfs",
		Node: "node1",
	}))

	err := ctrl.storage.DeletePool(context.Background(), "tank", "node1")
	require.NoError(t, err)
	require.Len(t, dep.zfsDestroyPoolCalls, 1)
	assert.Equal(t, []string{"10.0.0.1"}, dep.zfsDestroyPoolCalls[0].hosts)
	assert.Equal(t, "haify_tank", dep.zfsDestroyPoolCalls[0].poolName)
	// The only command besides the destroy is the check that it is empty.
	for _, c := range dep.execCalls {
		assert.Contains(t, c.cmd, "zfs list -H -o name -r haify_tank")
	}

	_, err = ctrl.db.GetPool(context.Background(), "haify_tank")
	assert.ErrorContains(t, err, "not found")
}

func TestStorageManagerDeletePoolFallsBackToZFS(t *testing.T) {
	dep := &fakeDeploymentClient{
		execFunc: func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
			require.Equal(t, []string{"10.0.0.1"}, hosts)
			if strings.Contains(cmd, "zfs list") {
				return successExecResult(hosts, ""), nil
			}
			require.Contains(t, decodeWrapped(cmd), "vg=haify_tank")
			return nil, assert.AnError
		},
	}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.hostsMap["node1"] = "10.0.0.1"

	err := ctrl.storage.DeletePool(context.Background(), "tank", "node1")
	require.NoError(t, err)
	require.Len(t, dep.zfsDestroyPoolCalls, 1)
	assert.Equal(t, "haify_tank", dep.zfsDestroyPoolCalls[0].poolName)
}
