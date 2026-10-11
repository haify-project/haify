package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStorageLivePoolDiscovery(t *testing.T) {
	dep := &fakeDeploymentClient{}
	dep.execFunc = func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		switch {
		case strings.Contains(cmd, "vgs"):
			return successExecResult(hosts, "haify_fast|107374182400|53687091200|/dev/sdb\nhaify_fast|107374182400|53687091200|/dev/sdc\nforeign|1|1|/dev/sdd"), nil
		case strings.Contains(cmd, "zpool list"):
			return successExecResult(hosts, "haify_tank 214748364800 161061273600 25"), nil
		default:
			return successExecResult(hosts, ""), nil
		}
	}
	dep.zfsListPoolsFunc = func(_ context.Context, hosts []string) (*deployment.ExecResult, error) {
		return successExecResult(hosts, "haify_tank 214748364800 161061273600 25\nforeign 1 1 0"), nil
	}
	ctrl := newBasicTestController(dep)
	ctrl.hosts = []string{"10.0.0.1"}
	ctrl.hostsMap["10.0.0.1"] = "n1"

	pools, err := ctrl.storage.ListPools(context.Background())
	require.NoError(t, err)
	require.Len(t, pools, 2)
	assert.Equal(t, "haify_fast", pools[0].Name)
	assert.Equal(t, []string{"/dev/sdb", "/dev/sdc"}, pools[0].Devices)
	assert.Equal(t, uint64(100), pools[0].TotalGB)
	assert.Equal(t, "haify_tank", pools[1].Name)
	assert.Equal(t, "zfs", pools[1].Type)

	got, err := ctrl.storage.GetPool(context.Background(), "fast", "10.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, uint64(50), got.FreeGB)

	zfs, err := ctrl.storage.GetZFSPool(context.Background(), "tank", "10.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, uint64(200), zfs.TotalGB)
}

func TestStoragePersistedFallbacks(t *testing.T) {
	dep := &fakeDeploymentClient{execFunc: func(context.Context, []string, string, ...deployment.ExecOption) (*deployment.ExecResult, error) {
		return nil, errors.New("ssh unavailable")
	}}
	ctrl := newBasicTestController(dep)
	ctrl.db = newTestDB(t)
	ctx := context.Background()
	require.NoError(t, ctrl.db.SavePool(ctx, &database.Pool{Name: "haify_fast", Type: "vg", Node: "n1", TotalGB: 20, FreeGB: 10, Devices: "/dev/sdb"}))
	require.NoError(t, ctrl.db.SavePool(ctx, &database.Pool{Name: "haify_tank", Type: "zfs", Node: "n1", TotalGB: 40, FreeGB: 30, Devices: "/dev/sdc"}))

	pools, err := ctrl.storage.ListPools(ctx)
	require.NoError(t, err)
	require.Len(t, pools, 2)
	got, err := ctrl.storage.GetPool(ctx, "fast", "n1")
	require.NoError(t, err)
	assert.Equal(t, uint64(10), got.FreeGB)
	zfs, err := ctrl.storage.GetPool(ctx, "tank", "n1")
	require.NoError(t, err)
	assert.Equal(t, "zfs", zfs.Type)

	bare := NewStorageManager(&Controller{})
	_, err = bare.getPersistedPool(ctx, "missing")
	assert.Error(t, err)
	_, err = bare.listPersistedPools(ctx)
	assert.Error(t, err)
}

func TestStorageSnapshotParsingAndRestore(t *testing.T) {
	dep := &fakeDeploymentClient{}
	dep.zfsListSnapshotsFunc = func(_ context.Context, hosts []string, dataset string) (*deployment.ExecResult, error) {
		assert.Equal(t, "haify_tank/data", dataset)
		return successExecResult(hosts, "haify_tank/data@snap1 1G 1G 2026-07-23\ninvalid"), nil
	}
	dep.lvListSnapshotsFunc = func(_ context.Context, hosts []string, vg string) (*deployment.ExecResult, error) {
		assert.Equal(t, "haify_fast", vg)
		// "name|size_bytes|time|origin" — pipe-separated because lv_time
		// carries spaces. The previous fixture used two space-separated
		// columns, which is what the command emitted back when it still had
		// the "VG/LV" placeholder in it.
		return successExecResult(hosts,
			"snap1|1073741824|2026-08-09 13:00:02 +0000|origin\n"+
				"snap2|1073741824|2026-08-09 14:00:02 +0000|origin"), nil
	}
	mergeCalled := false
	dep.lvMergeSnapshotFunc = func(_ context.Context, hosts []string, vg, snapshot string) (*deployment.ExecResult, error) {
		mergeCalled = true
		assert.Equal(t, "haify_fast", vg)
		assert.Equal(t, "snap1", snapshot)
		return successExecResult(hosts, "merged"), nil
	}
	ctrl := newBasicTestController(dep)
	ctx := context.Background()

	zfs, err := ctrl.storage.ZFSListSnapshots(ctx, "tank/data", "n1")
	require.NoError(t, err)
	require.Len(t, zfs, 1)
	assert.Equal(t, "snap1", zfs[0].Name)
	lvm, err := ctrl.storage.ListLvmSnapshots(ctx, "fast", "n1", "")
	require.NoError(t, err)
	require.Len(t, lvm, 2)
	assert.Equal(t, "snap2", lvm[1].Name)
	// Restore is covered by snapshot_restore_test.go; the merge now goes
	// through the replica-aware path rather than LVMergeSnapshot.
	_ = mergeCalled
}

func TestStorageOperationFailures(t *testing.T) {
	ctx := context.Background()
	failed := failedResult([]string{"n1"}, "operation failed")

	tests := []struct {
		name string
		call func(*StorageManager) error
	}{
		{"create dataset", func(sm *StorageManager) error { return sm.CreateZFSDataset(ctx, "tank/data", "n1") }},
		{"delete dataset", func(sm *StorageManager) error { return sm.ZFSDeleteDataset(ctx, "tank/data", "n1") }},
		{"snapshot", func(sm *StorageManager) error { return sm.ZFSSnapshot(ctx, "tank/data", "snap", "n1") }},
		{"delete snapshot", func(sm *StorageManager) error { return sm.ZFSDeleteSnapshot(ctx, "tank/data@snap", "n1") }},
		{"restore snapshot", func(sm *StorageManager) error { return sm.ZFSRestoreSnapshot(ctx, "tank/data", "snap", "n1") }},
		{"clone snapshot", func(sm *StorageManager) error { return sm.ZFSCloneSnapshot(ctx, "tank/data@snap", "tank/clone", "n1") }},
		{"resize zfs", func(sm *StorageManager) error { return sm.ZFSResizeVolume(ctx, "tank/vol", "20G", "n1") }},
		{"delete lvm snapshot", func(sm *StorageManager) error { return sm.DeleteLvmSnapshot(ctx, "fast", "snap", "n1") }},
		{"restore lvm snapshot", func(sm *StorageManager) error { return sm.RestoreLvmSnapshot(ctx, "fast", "snap", "n1") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dep := &failingStorageDeployment{fakeDeploymentClient: fakeDeploymentClient{}, result: failed}
			ctrl := newBasicTestController(dep)
			err := tt.call(ctrl.storage)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "failed")
		})
	}
}

func TestStoragePoolFailureBranches(t *testing.T) {
	ctx := context.Background()

	t.Run("create PV error", func(t *testing.T) {
		dep := &fakeDeploymentClient{pvCreateFunc: func(context.Context, []string, string, ...deployment.LVMOption) (*deployment.ExecResult, error) {
			return nil, errors.New("pv failed")
		}}
		err := newBasicTestController(dep).storage.CreatePool(ctx, "fast", "lvm", "n1", []string{"/dev/sdb"}, 0, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create PV")
	})

	t.Run("create VG error", func(t *testing.T) {
		dep := &fakeDeploymentClient{vgCreateFunc: func(context.Context, []string, string, []string) (*deployment.ExecResult, error) {
			return nil, errors.New("vg failed")
		}}
		err := newBasicTestController(dep).storage.CreatePool(ctx, "fast", "lvm", "n1", []string{"/dev/sdb"}, 0, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create pool")
	})

	t.Run("thin pool error", func(t *testing.T) {
		dep := &fakeDeploymentClient{lvCreateThinPoolFunc: func(context.Context, []string, string, string, string) (*deployment.ExecResult, error) {
			return nil, errors.New("thin failed")
		}}
		err := newBasicTestController(dep).storage.CreatePool(ctx, "fast", "lvm-thin", "n1", []string{"/dev/sdb"}, 10, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "thin pool")
	})

	t.Run("add disk errors", func(t *testing.T) {
		dep := &fakeDeploymentClient{pvCreateFunc: func(_ context.Context, hosts []string, _ string, _ ...deployment.LVMOption) (*deployment.ExecResult, error) {
			return failedResult(hosts, "pv failed"), nil
		}}
		err := newBasicTestController(dep).storage.AddDiskToPool(ctx, "fast", "/dev/sdb", "n1", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create PV")

		dep = &fakeDeploymentClient{vgExtendFunc: func(context.Context, []string, string, string) (*deployment.ExecResult, error) {
			return nil, errors.New("extend failed")
		}}
		err = newBasicTestController(dep).storage.AddDiskToPool(ctx, "fast", "/dev/sdb", "n1", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "add disk")
	})

	t.Run("zfs create and delete errors", func(t *testing.T) {
		dep := &fakeDeploymentClient{
			zfsCreatePoolFunc: func(context.Context, []string, string, []string, ...deployment.ZFSOption) (*deployment.ExecResult, error) {
				return nil, errors.New("create failed")
			},
			zfsDestroyPoolFunc: func(context.Context, []string, string) (*deployment.ExecResult, error) {
				return nil, errors.New("destroy failed")
			},
		}
		sm := newBasicTestController(dep).storage
		assert.Error(t, sm.CreateZFSPool(ctx, "tank", "n1", []string{"/dev/sdb"}, "", false, false))
		assert.Error(t, sm.DeleteZFSPool(ctx, "tank", "n1"))
	})
}

type failingStorageDeployment struct {
	fakeDeploymentClient
	result *deployment.ExecResult
}

func (f *failingStorageDeployment) ZFSCreateDataset(context.Context, []string, string, ...deployment.ZFSOption) (*deployment.ExecResult, error) {
	return f.result, nil
}
func (f *failingStorageDeployment) ZFSDestroyDataset(context.Context, []string, string) (*deployment.ExecResult, error) {
	return f.result, nil
}
func (f *failingStorageDeployment) ZFSSnapshot(context.Context, []string, string, string) (*deployment.ExecResult, error) {
	return f.result, nil
}
func (f *failingStorageDeployment) ZFSDestroySnapshot(context.Context, []string, string) (*deployment.ExecResult, error) {
	return f.result, nil
}
func (f *failingStorageDeployment) ZFSRollback(context.Context, []string, string, string) (*deployment.ExecResult, error) {
	return f.result, nil
}
func (f *failingStorageDeployment) ZFSClone(context.Context, []string, string, string) (*deployment.ExecResult, error) {
	return f.result, nil
}
func (f *failingStorageDeployment) ZFSResizeVolume(context.Context, []string, string, string) (*deployment.ExecResult, error) {
	return f.result, nil
}
func (f *failingStorageDeployment) LVRemoveSnapshot(context.Context, []string, string, string) (*deployment.ExecResult, error) {
	return f.result, nil
}
func (f *failingStorageDeployment) LVMergeSnapshot(context.Context, []string, string, string) (*deployment.ExecResult, error) {
	return f.result, nil
}
