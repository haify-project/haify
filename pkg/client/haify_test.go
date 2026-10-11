package client

import (
	"context"
	"testing"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHaifyClientNodeOperations(t *testing.T) {
	c, cleanup := setupMockClient(t)
	defer cleanup()

	ctx := context.Background()

	// RegisterNode
	node, err := c.RegisterNode(ctx, "test-node", "10.0.0.3")
	require.NoError(t, err)
	assert.Equal(t, "test-node", node.Name)

	_, err = c.RegisterNode(ctx, "error-node", "10.0.0.4")
	assert.Error(t, err)

	// UnregisterNode
	err = c.UnregisterNode(ctx, "10.0.0.3")
	require.NoError(t, err)

	err = c.UnregisterNode(ctx, "fail-addr")
	assert.Error(t, err)

	// ListNodes & GetNode & SetNodeLabels
	nodes, err := c.ListNodes(ctx)
	require.NoError(t, err)
	assert.Len(t, nodes, 2)

	gnode, err := c.GetNode(ctx, "10.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, "node1", gnode.Name)

	_, err = c.GetNode(ctx, "fail-node")
	assert.Error(t, err)

	lnode, err := c.SetNodeLabels(ctx, "node1", map[string]string{"env": "test"}, false)
	require.NoError(t, err)
	assert.Equal(t, "test", lnode.Labels["env"])

	_, err = c.SetNodeLabels(ctx, "fail-node", nil, false)
	assert.Error(t, err)

	// HealthCheck
	h, err := c.HealthCheck(ctx, "node1")
	require.NoError(t, err)
	assert.True(t, h.DrbdInstalled)

	_, err = c.HealthCheck(ctx, "fail-node")
	assert.Error(t, err)

	// DrainNode / UndrainNode
	moved, err := c.DrainNode(ctx, "node1")
	require.NoError(t, err)
	assert.Equal(t, []string{"res1"}, moved)

	_, err = c.DrainNode(ctx, "fail-node")
	assert.Error(t, err)

	err = c.UndrainNode(ctx, "node1")
	require.NoError(t, err)

	err = c.UndrainNode(ctx, "fail-node")
	assert.Error(t, err)
}

func TestHaifyClientPoolAndResourceOperations(t *testing.T) {
	c, cleanup := setupMockClient(t)
	defer cleanup()

	ctx := context.Background()

	// Pool Operations
	err := c.CreatePool(ctx, "pool1", "lvm", "node1", []string{"/dev/sdb"}, 100, false)
	require.NoError(t, err)

	err = c.CreatePool(ctx, "fail-pool", "lvm", "node1", nil, 0, false)
	assert.Error(t, err)

	pools, err := c.ListPools(ctx)
	require.NoError(t, err)
	assert.Len(t, pools, 1)

	pool, err := c.GetPool(ctx, "pool1", "node1")
	require.NoError(t, err)
	assert.Equal(t, "pool1", pool.Name)

	_, err = c.GetPool(ctx, "fail-pool", "node1")
	assert.Error(t, err)

	err = c.AddDiskToPool(ctx, "pool1", "/dev/sdc", "node1", false)
	require.NoError(t, err)

	err = c.AddDiskToPool(ctx, "fail-pool", "/dev/sdc", "node1", false)
	assert.Error(t, err)

	err = c.DeletePool(ctx, "pool1", "node1")
	require.NoError(t, err)

	err = c.DeletePool(ctx, "fail-pool", "node1")
	assert.Error(t, err)

	// Resource Operations
	err = c.CreateResourceWithPoolAndType(ctx, "res1", 7001, []string{"node1"}, "C", 10, "pool1", "lvm", nil)
	require.NoError(t, err)

	err = c.CreateResourceWithPoolAndType(ctx, "fail-res", 7001, []string{"node1"}, "C", 10, "pool1", "lvm", nil)
	assert.Error(t, err)

	rList, err := c.ListResources(ctx)
	require.NoError(t, err)
	assert.Len(t, rList, 1)

	rInfo, err := c.GetResource(ctx, "res1")
	require.NoError(t, err)
	assert.Equal(t, "res1", rInfo.Name)

	_, err = c.GetResource(ctx, "fail-res")
	assert.Error(t, err)

	err = c.SetPrimary(ctx, "res1", "node1", false)
	require.NoError(t, err)

	err = c.SetPrimary(ctx, "fail-res", "node1", false)
	assert.Error(t, err)

	err = c.SetSecondary(ctx, "res1", "node1")
	require.NoError(t, err)

	err = c.SetSecondary(ctx, "fail-res", "node1")
	assert.Error(t, err)

	err = c.AttachDisklessClient(ctx, "res1", "node2")
	require.NoError(t, err)

	err = c.AttachDisklessClient(ctx, "fail-res", "node2")
	assert.Error(t, err)

	err = c.DetachDisklessClient(ctx, "res1", "node2")
	require.NoError(t, err)

	err = c.DetachDisklessClient(ctx, "fail-res", "node2")
	assert.Error(t, err)

	err = c.ResizeVolume(ctx, "res1", 0, 20)
	require.NoError(t, err)

	err = c.ResizeVolume(ctx, "fail-res", 0, 20)
	assert.Error(t, err)

	err = c.DeleteResource(ctx, "res1")
	require.NoError(t, err)

	err = c.DeleteResource(ctx, "fail-res")
	assert.Error(t, err)
}

func TestHaifyClientHaSnapshotGatewayOperations(t *testing.T) {
	c, cleanup := setupMockClient(t)
	defer cleanup()

	ctx := context.Background()

	// HA
	configPath, err := c.MakeHa(ctx, "res1", []string{"svc1"}, "/mnt", "ext4", "10.0.0.100", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "/etc/ha", configPath)

	_, err = c.MakeHa(ctx, "fail-res", nil, "", "", "", nil, nil)
	assert.Error(t, err)

	haInfo, err := c.GetHa(ctx, "res1")
	require.NoError(t, err)
	assert.Equal(t, "res1", haInfo.Resource)

	_, err = c.GetHa(ctx, "fail-res")
	assert.Error(t, err)

	haList, err := c.ListHa(ctx)
	require.NoError(t, err)
	assert.Len(t, haList, 1)

	promoters, err := c.GetHaStatus(ctx, "res1")
	require.NoError(t, err)
	assert.Len(t, promoters, 1)

	// Snapshot
	err = c.CreateSnapshot(ctx, "vol1", "snap1", "node1")
	require.NoError(t, err)

	err = c.CreateSnapshot(ctx, "vol1", "fail-snap", "node1")
	assert.Error(t, err)

	snaps, err := c.ListSnapshots(ctx, "vol1", "node1")
	require.NoError(t, err)
	assert.Len(t, snaps, 1)

	err = c.DeleteSnapshot(ctx, "vol1", "snap1", "node1")
	require.NoError(t, err)

	err = c.DeleteSnapshot(ctx, "vol1", "fail-snap", "node1")
	assert.Error(t, err)

	// Gateway
	gwReq := &haifypb.CreateNFSGatewayRequest{
		Resource:   "res1",
		ServiceIp:  "10.0.0.100/24",
		ExportPath: "/export",
	}
	gw, err := c.CreateNFSGateway(ctx, gwReq)
	require.NoError(t, err)
	assert.True(t, gw.Success)

	gwFailReq := &haifypb.CreateNFSGatewayRequest{Resource: "fail-res"}
	_, err = c.CreateNFSGateway(ctx, gwFailReq)
	assert.Error(t, err)

	gwList, err := c.ListGateways(ctx)
	require.NoError(t, err)
	assert.Len(t, gwList, 1)

	err = c.DeleteGateway(ctx, "gw1")
	require.NoError(t, err)

	err = c.DeleteGateway(ctx, "fail-gw")
	assert.Error(t, err)
}

func TestHaifyClientRemainingAPIs(t *testing.T) {
	c, cleanup := setupMockClient(t)
	defer cleanup()

	ctx := context.Background()

	_ = c.Address()

	require.NoError(t, c.CreateResource(ctx, "res1", 7001, []string{"n1"}, "C", 10, nil))
	require.NoError(t, c.CreateResourceWithPool(ctx, "res1", 7001, []string{"n1"}, "C", 10, "vg0", nil))
	require.NoError(t, c.CreateResourceAutoPlace(ctx, "res1", 7001, 2, nil, nil, nil, "C", 10, "vg0", "lvm", nil))
	require.NoError(t, c.CreateResourceWithVolumes(ctx, "res1", 7001, []string{"n1"}, "C", "lvm", nil, nil))
	require.NoError(t, c.CreateZFSResource(ctx, "res1", 7001, []string{"n1"}, "C", 10, "tank", nil))
	require.NoError(t, c.CreateResourceWAN(ctx, "res1", 7001, "n1", 10, "vg0", "lvm", nil, "drNode", "drEndpoint", 7002))

	_, err := c.AdoptResource(ctx, "res1", []string{"n1"}, 7001, "C")
	require.NoError(t, err)

	require.NoError(t, c.PromoteForNode(ctx, "res1", "n1"))
	require.NoError(t, c.AddVolume(ctx, "res1", "vol1", "vg0", 10))
	require.NoError(t, c.UpdateResourceOptions(ctx, "res1", map[string]string{"opt": "val"}))
	require.NoError(t, c.RemoveVolume(ctx, "res1", 1))

	_, err = c.ResourceStatus(ctx, "res1")
	require.NoError(t, err)

	require.NoError(t, c.CreateFilesystem(ctx, "res1", 0, "ext4", "n1"))
	require.NoError(t, c.MountResource(ctx, "res1", 0, "/mnt", "n1", "ext4"))
	require.NoError(t, c.UnmountResource(ctx, "res1", 0, "n1"))

	_, _, err = c.EnableSelfHa(ctx, "10.0.0.100", "vg0", 1, 7999, nil)
	require.NoError(t, err)
	require.NoError(t, c.DisableSelfHa(ctx, "n1"))

	sh, err := c.GetSelfHaStatus(ctx)
	require.NoError(t, err)
	assert.True(t, sh.Enabled)

	require.NoError(t, c.EvictHa(ctx, "res1"))
	require.NoError(t, c.DeleteHa(ctx, "res1"))

	agents, err := c.ListResourceAgents(ctx)
	require.NoError(t, err)
	assert.Len(t, agents, 1)

	_, err = c.GetResourceAgentMetadata(ctx, "ocf", "ip")
	require.NoError(t, err)

	_, err = c.GetHaToml(ctx, "res1")
	require.NoError(t, err)

	_, err = c.SyncHaToml(ctx, "res1", "toml")
	require.NoError(t, err)

	require.NoError(t, c.CreateSnapshotSchedule(ctx, "res1", "0 * * * *", nil, true))
	_, err = c.ListSnapshotSchedules(ctx)
	require.NoError(t, err)
	require.NoError(t, c.DeleteSnapshotSchedule(ctx, "sched1"))
	require.NoError(t, c.RestoreSnapshot(ctx, "vol1", "snap1", "n1"))

	_, err = c.CreateISCSIGateway(ctx, &haifypb.CreateISCSIGatewayRequest{Resource: "res1"})
	require.NoError(t, err)

	_, err = c.CreateNVMeGateway(ctx, &haifypb.CreateNVMeGatewayRequest{Resource: "res1"})
	require.NoError(t, err)

	gw, err := c.GetGateway(ctx, "gw1")
	require.NoError(t, err)
	assert.Equal(t, "gw1", gw.Id)

	require.NoError(t, c.StartGateway(ctx, "gw1"))
	require.NoError(t, c.StopGateway(ctx, "gw1"))

	require.NoError(t, c.AddNFSExport(ctx, "gw1", "/export", 1, "10.0.0.0/24", "rw"))
	require.NoError(t, c.RemoveNFSExport(ctx, "gw1", "/export"))
	_, err = c.ListNFSExports(ctx, "gw1")
	require.NoError(t, err)

	require.NoError(t, c.AddISCSILUN(ctx, "gw1", 1, "/dev/drbd0"))
	require.NoError(t, c.RemoveISCSILUN(ctx, "gw1", 1))
	_, err = c.ListISCSILUNs(ctx, "gw1")
	require.NoError(t, err)

	require.NoError(t, c.AddISCSIInitiator(ctx, "gw1", "iqn.init"))
	require.NoError(t, c.RemoveISCSIInitiator(ctx, "gw1", "iqn.init"))
	_, err = c.ListISCSIInitiators(ctx, "gw1")
	require.NoError(t, err)

	require.NoError(t, c.SetISCSIChap(ctx, "gw1", "user", "pass", false))
	_, err = c.GetISCSIChap(ctx, "gw1")
	require.NoError(t, err)

	require.NoError(t, c.AddNVMeNamespace(ctx, "gw1", "/dev/drbd0"))
	require.NoError(t, c.RemoveNVMeNamespace(ctx, "gw1", 1))
	_, err = c.ListNVMeNamespaces(ctx, "gw1")
	require.NoError(t, err)

	require.NoError(t, c.AddNVMeHost(ctx, "gw1", "nqn.host"))
	require.NoError(t, c.RemoveNVMeHost(ctx, "gw1", "nqn.host"))
	_, err = c.ListNVMeHosts(ctx, "gw1")
	require.NoError(t, err)

	require.NoError(t, c.CreateZFSPool(ctx, "tank", "n1", []string{"/dev/sdb"}))
	require.NoError(t, c.DeleteZFSPool(ctx, "tank", "n1"))
	_, err = c.ListZFSpools(ctx)
	require.NoError(t, err)

	require.NoError(t, c.CreateZFSDataset(ctx, "tank/ds", "n1"))
	require.NoError(t, c.DeleteZFSDataset(ctx, "tank/ds", "n1"))
	require.NoError(t, c.CreateZFSVolume(ctx, "tank", "vol", "10G", "n1"))
	require.NoError(t, c.ResizeZFSVolume(ctx, "tank/vol", "20G", "n1"))
	require.NoError(t, c.CreateZFSSnapshot(ctx, "tank/ds", "snap1", "n1"))
	require.NoError(t, c.DeleteZFSSnapshot(ctx, "tank/ds@snap1", "n1"))
	_, err = c.ListZFSSnapshots(ctx, "tank/ds", "n1")
	require.NoError(t, err)
	require.NoError(t, c.RestoreZFSSnapshot(ctx, "tank/ds", "snap1", "n1"))
	require.NoError(t, c.CloneZFSSnapshot(ctx, "tank/ds@snap1", "tank/clone", "n1"))

	require.NoError(t, c.CreateLvmSnapshot(ctx, "vg0", "lv1", "snap1", "n1", "1G"))
	require.NoError(t, c.DeleteLvmSnapshot(ctx, "vg0", "snap1", "n1"))
	_, err = c.ListLvmSnapshots(ctx, "vg0", "n1", "")
	require.NoError(t, err)
	require.NoError(t, c.RestoreLvmSnapshot(ctx, "vg0", "snap1", "n1"))
}

func TestHaifyClientResourceProfiles(t *testing.T) {
	c, cleanup := setupMockClient(t)
	defer cleanup()

	ctx := context.Background()
	profile := &haifypb.ResourceProfile{Name: "production", Pool: "fast", Replicas: 3}
	created, err := c.CreateResourceProfile(ctx, profile)
	require.NoError(t, err)
	assert.Equal(t, profile.Name, created.Name)
	assert.Equal(t, profile.Pool, created.Pool)
	assert.Equal(t, profile.Replicas, created.Replicas)

	got, err := c.GetResourceProfile(ctx, "production")
	require.NoError(t, err)
	assert.Equal(t, "fast", got.Pool)

	profiles, err := c.ListResourceProfiles(ctx)
	require.NoError(t, err)
	require.Len(t, profiles, 2)
	assert.Equal(t, "archive", profiles[1].Name)

	require.NoError(t, c.DeleteResourceProfile(ctx, "production"))
	_, err = c.CreateResourceProfile(ctx, &haifypb.ResourceProfile{Name: "fail-profile"})
	assert.EqualError(t, err, "profile invalid")
	_, err = c.GetResourceProfile(ctx, "missing")
	assert.EqualError(t, err, "profile not found")
	assert.EqualError(t, c.DeleteResourceProfile(ctx, "missing"), "profile not found")
}

func TestHaifyClientProfileTransportErrors(t *testing.T) {
	c, cleanup := setupMockClient(t)
	require.NoError(t, c.Close())
	defer cleanup()

	ctx := context.Background()
	_, err := c.CreateResourceProfile(ctx, &haifypb.ResourceProfile{Name: "production"})
	assert.Error(t, err)
	_, err = c.GetResourceProfile(ctx, "production")
	assert.Error(t, err)
	_, err = c.ListResourceProfiles(ctx)
	assert.Error(t, err)
	assert.Error(t, c.DeleteResourceProfile(ctx, "production"))
}

func TestHaifyClientTransportErrors(t *testing.T) {
	c, cleanup := setupMockClient(t)
	require.NoError(t, c.Close())
	defer cleanup()
	ctx := context.Background()

	checks := []func() error{
		func() error { return c.CreatePool(ctx, "p", "lvm", "n", nil, 0, false) },
		func() error { _, err := c.GetPool(ctx, "p", "n"); return err },
		func() error { _, err := c.ListPools(ctx); return err },
		func() error { return c.AddDiskToPool(ctx, "p", "/dev/sdb", "n", false) },
		func() error { return c.DeletePool(ctx, "p", "n") },
		func() error { _, err := c.RegisterNode(ctx, "n", "a"); return err },
		func() error { _, err := c.SetNodeLabels(ctx, "n", nil, false); return err },
		func() error { _, err := c.ListNodes(ctx); return err },
		func() error { _, err := c.GetNode(ctx, "a"); return err },
		func() error { return c.UnregisterNode(ctx, "a") },
		func() error { _, err := c.DrainNode(ctx, "n"); return err },
		func() error { return c.UndrainNode(ctx, "n") },
		func() error { _, err := c.HealthCheck(ctx, "n"); return err },
		func() error { return c.CreateResourceRequest(ctx, &haifypb.CreateResourceRequest{Name: "r"}) },
		func() error { _, err := c.GetResource(ctx, "r"); return err },
		func() error { _, err := c.ListResources(ctx); return err },
		func() error { return c.SetPrimary(ctx, "r", "n", false) },
		func() error { return c.PromoteForNode(ctx, "r", "n") },
		func() error { return c.DeleteResource(ctx, "r") },
		func() error { return c.AddVolume(ctx, "r", "v", "p", 1) },
		func() error { return c.UpdateResourceOptions(ctx, "r", map[string]string{"a": "b"}) },
		func() error { return c.RemoveVolume(ctx, "r", 0) },
		func() error { return c.ResizeVolume(ctx, "r", 0, 2) },
		func() error { _, err := c.ResourceStatus(ctx, "r"); return err },
		func() error { return c.SetSecondary(ctx, "r", "n") },
		func() error { return c.AttachDisklessClient(ctx, "r", "n") },
		func() error { return c.DetachDisklessClient(ctx, "r", "n") },
		func() error { return c.CreateFilesystem(ctx, "r", 0, "ext4", "n") },
		func() error { return c.MountResource(ctx, "r", 0, "/mnt/r", "n", "ext4") },
		func() error { return c.UnmountResource(ctx, "r", 0, "n") },
		func() error { _, err := c.ListGateways(ctx); return err },
		func() error { _, err := c.GetGateway(ctx, "g"); return err },
		func() error { return c.StartGateway(ctx, "g") },
		func() error { return c.StopGateway(ctx, "g") },
		func() error { return c.DeleteGateway(ctx, "g") },
		func() error {
			_, err := c.CreateNFSGateway(ctx, &haifypb.CreateNFSGatewayRequest{Resource: "r"})
			return err
		},
		func() error {
			_, err := c.CreateISCSIGateway(ctx, &haifypb.CreateISCSIGatewayRequest{Resource: "r"})
			return err
		},
		func() error {
			_, err := c.CreateNVMeGateway(ctx, &haifypb.CreateNVMeGatewayRequest{Resource: "r"})
			return err
		},
		func() error { return c.AddNFSExport(ctx, "g", "/data", 1, "*", "rw") },
		func() error { return c.RemoveNFSExport(ctx, "g", "/data") },
		func() error { _, err := c.ListNFSExports(ctx, "g"); return err },
		func() error { return c.AddISCSILUN(ctx, "g", 1, "/dev/drbd0") },
		func() error { return c.RemoveISCSILUN(ctx, "g", 1) },
		func() error { _, err := c.ListISCSILUNs(ctx, "g"); return err },
		func() error { return c.AddISCSIInitiator(ctx, "g", "iqn.test") },
		func() error { return c.RemoveISCSIInitiator(ctx, "g", "iqn.test") },
		func() error { _, err := c.ListISCSIInitiators(ctx, "g"); return err },
		func() error { return c.SetISCSIChap(ctx, "g", "u", "p", false) },
		func() error { _, err := c.GetISCSIChap(ctx, "g"); return err },
		func() error { return c.AddNVMeNamespace(ctx, "g", "/dev/drbd0") },
		func() error { return c.RemoveNVMeNamespace(ctx, "g", 1) },
		func() error { _, err := c.ListNVMeNamespaces(ctx, "g"); return err },
		func() error { return c.AddNVMeHost(ctx, "g", "nqn.test") },
		func() error { return c.RemoveNVMeHost(ctx, "g", "nqn.test") },
		func() error { _, err := c.ListNVMeHosts(ctx, "g"); return err },
	}
	for i, check := range checks {
		assert.Errorf(t, check(), "transport check %d", i)
	}
}
