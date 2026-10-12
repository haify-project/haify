package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestSetOptionsSuccessAndFailures(t *testing.T) {
	ctx := context.Background()
	dep := &fakeDeploymentClient{}
	dep.execFunc = func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		if strings.HasPrefix(cmd, "cat /etc/drbd.d/res1.res") {
			return successExecResult(hosts, drbdResFixture), nil
		}
		return successExecResult(hosts, "adjusted"), nil
	}
	ctrl := newBasicTestController(dep)
	ctrl.db = newTestDB(t)
	require.NoError(t, ctrl.db.SaveResource(ctx, &database.Resource{Name: "res1", Nodes: "n1,n2"}))
	ctrl.hostsMap["n1"] = "10.0.0.1"
	ctrl.hostsMap["n2"] = "10.0.0.2"

	require.NoError(t, ctrl.resources.SetOptions(ctx, "res1", map[string]string{"net/max-buffers": "8000"}))
	require.Len(t, dep.distributedConfigs, 1)
	assert.Contains(t, dep.distributedConfigs[0].content, "max-buffers 8000;")
	assert.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, dep.distributedConfigs[0].hosts)
	assert.Error(t, ctrl.resources.SetOptions(ctx, "res1", nil))
	assert.Error(t, ctrl.resources.SetOptions(ctx, "missing", map[string]string{"a": "b"}))

	dep.execFunc = func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		if strings.HasPrefix(cmd, "cat ") {
			return successExecResult(hosts, ""), nil
		}
		return successExecResult(hosts, ""), nil
	}
	err := ctrl.resources.SetOptions(ctx, "res1", map[string]string{"a": "b"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config not found")
}

func TestResourceMinorPortAndBackingHelpers(t *testing.T) {
	dep := &fakeDeploymentClient{execFunc: func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		switch {
		case strings.Contains(cmd, "cat /etc/drbd.d/*.res"):
			return successExecResult(hosts, "address 10.0.0.1:7000;\naddress 10.0.0.2:7002;"), nil
		default:
			return successExecResult(hosts, "removed"), nil
		}
	}}
	ctrl := newBasicTestController(dep)
	ctx := context.Background()
	port, err := ctrl.resources.nextGlobalPort(ctx, "n1")
	require.NoError(t, err)
	assert.Equal(t, uint32(7001), port)

	require.Error(t, ctrl.resources.deleteBackingVolume(ctx, []string{"n1"}, &database.Volume{}))
	require.NoError(t, ctrl.resources.deleteBackingVolume(ctx, []string{"n1"}, &database.Volume{ResourceName: "r", Pool: "vg", VolumeName: "data", Device: "/dev/vg/data"}))
	require.NoError(t, ctrl.resources.deleteBackingVolume(ctx, []string{"n1"}, &database.Volume{ResourceName: "r", Pool: "tank", VolumeName: "data", Device: "/dev/zvol/tank/data"}))
	// A zvol's snapshots go with it, but not datasets cloned from them (-R).
	assert.Contains(t, dep.execCalls[len(dep.execCalls)-1].cmd, "zfs destroy -r tank/data")

	dep.execFunc = func(_ context.Context, hosts []string, _ string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		return failedResult(hosts, "does not exist"), nil
	}
	require.NoError(t, ctrl.resources.deleteBackingVolume(ctx, []string{"n1"}, &database.Volume{ResourceName: "r", Pool: "vg", VolumeName: "data"}))
}

func TestPrimaryResolutionAndFilesystem(t *testing.T) {
	ctx := context.Background()
	dep := &fakeDeploymentClient{execFunc: func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		if strings.Contains(cmd, "mkfs.xfs") {
			assert.Contains(t, cmd, "-f /dev/drbd/by-res/res1/0")
		}
		return successExecResult(hosts, "ok"), nil
	}}
	dep.drbdStatusJSONFunc = func(_ context.Context, hosts []string, _ string) (*deployment.ExecResult, error) {
		return successExecResult(hosts, `[{"name":"res1","role":"Primary","devices":[{"volume":0,"minor":100,"disk-state":"UpToDate"}]}]`), nil
	}
	ctrl := newBasicTestController(dep)
	ctrl.db = newTestDB(t)
	ctrl.hostsMap["n1"] = "10.0.0.1"
	require.NoError(t, ctrl.db.SaveResource(ctx, &database.Resource{Name: "res1", Nodes: "n1"}))

	addr, err := ctrl.resources.primaryAddress(ctx, "res1")
	require.NoError(t, err)
	assert.Equal(t, "n1", addr)
	require.NoError(t, ctrl.resources.CreateFilesystemOnly(ctx, "res1", 0, "XFS", ""))

	dep.drbdStatusJSONFunc = func(_ context.Context, hosts []string, _ string) (*deployment.ExecResult, error) {
		return successExecResult(hosts, `[{"name":"res1","role":"Secondary"}]`), nil
	}
	_, err = ctrl.resources.primaryAddress(ctx, "res1")
	assert.Error(t, err)
}

func TestServerResourceValidationAndAdopt(t *testing.T) {
	srv, _ := newServerWithNodes(t)
	ctx := context.Background()

	for _, req := range []*haifypb.CreateResourceRequest{
		{Name: "r", DrNode: "dr"},
		{Name: "r", Wan: true},
		{Name: "r", Volumes: []*haifypb.VolumeSpec{{SizeGb: 1, Pool: "a"}, {SizeGb: 1, Pool: "b"}}},
		{Name: "r", Profile: "missing", Nodes: []string{"n1"}},
	} {
		resp, err := srv.CreateResource(ctx, req)
		require.NoError(t, err)
		assert.False(t, resp.Success)
	}
	_, _, err := singlePoolTotal(nil)
	assert.Error(t, err)
	pool, total, err := singlePoolTotal([]VolumeSpec{{Pool: "fast", SizeGB: 2}, {Pool: "fast", SizeGB: 3}})
	require.NoError(t, err)
	assert.Equal(t, "fast", pool)
	assert.Equal(t, uint32(5), total)

	adopt, err := srv.AdoptResource(ctx, &haifypb.AdoptResourceRequest{Name: "missing"})
	require.NoError(t, err)
	assert.False(t, adopt.Success)
}

func TestServerRestoreLvmSnapshot(t *testing.T) {
	dep := &fakeDeploymentClient{}
	merged := false
	dep.execFunc = func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		switch {
		case strings.Contains(cmd, "-o origin haify_fast/snap"):
			return successExecResult(hosts, "  fast_data\n"), nil
		case strings.Contains(decodeWrapped(cmd), "lvconvert --merge /dev/haify_fast/snap"):
			merged = true
		}
		return successExecResult(hosts, ""), nil
	}
	srv := NewServer(newBasicTestController(dep))
	resp, err := srv.RestoreLvmSnapshot(context.Background(), &haifypb.RestoreLvmSnapshotRequest{LvName: "fast", SnapshotName: "snap", Node: "n1"})
	require.NoError(t, err)
	assert.True(t, resp.Success, resp.Message)
	assert.True(t, merged)

	dep.execFunc = func(_ context.Context, hosts []string, _ string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		return nil, errors.New("ssh failed")
	}
	resp, err = srv.RestoreLvmSnapshot(context.Background(), &haifypb.RestoreLvmSnapshotRequest{LvName: "fast", SnapshotName: "snap", Node: "n1"})
	require.NoError(t, err)
	assert.False(t, resp.Success)
}

func TestResourceGuardsAndHostHelpers(t *testing.T) {
	ctx := context.Background()
	bareCtrl := &Controller{hostsMap: map[string]string{}, logger: zap.NewNop()}
	bare := NewResourceManager(bareCtrl)
	assert.Error(t, bare.SetOptions(ctx, "r", map[string]string{"a": "b"}))
	assert.Error(t, bare.DeleteResource(ctx, "r", false))
	assert.Error(t, bare.SetPrimary(ctx, "r", "n1", false))
	assert.Error(t, bare.CreateFilesystemOnly(ctx, "r", 0, "ext4", "n1"))
	_, err := bare.primaryAddress(ctx, "r")
	assert.Error(t, err)

	ctrl := newBasicTestController(&fakeDeploymentClient{})
	ctrl.resources.SetHosts([]string{"node1:10.0.0.1", "node2"})
	assert.Empty(t, ctrl.resources.getNodeHost("node1"))
	assert.Equal(t, "node2", ctrl.resources.getNodeHost("node2"))
	assert.Empty(t, ctrl.resources.getNodeHost("missing"))
	assert.Equal(t, "10.0.0.1", ctrl.resources.GetHosts()[0])
	addr, err := ctrl.resources.resolveNodeOrPrimary(ctx, "r", "node2")
	require.NoError(t, err)
	assert.Equal(t, "node2", addr)
}
