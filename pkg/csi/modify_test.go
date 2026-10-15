package csi

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func modifyVolume(t *testing.T, f *modifyFake, id string, params map[string]string) error {
	t.Helper()
	s := &controllerServer{backend: f, log: zap.NewNop()}
	_, err := s.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{VolumeId: id, MutableParameters: params})
	return err
}

func mountCap() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

func TestParseMutableParameters(t *testing.T) {
	c, err := parseMutableParameters(map[string]string{
		"replicas": "3", "resourceProfile": "gold", "readIOPS": "500", "writeBytesPerSecond": "10Mi", "writeIOPS": "max",
	})
	require.NoError(t, err)
	assert.Equal(t, 3, c.replicas)
	require.NotNil(t, c.profile)
	assert.Equal(t, "gold", *c.profile)
	assert.Equal(t, map[string]string{"readIOPS": "500", "writeBytesPerSecond": "10485760", "writeIOPS": "max"}, c.qos)

	empty, err := parseMutableParameters(nil)
	require.NoError(t, err)
	assert.True(t, empty.empty())

	for _, bad := range []map[string]string{
		{"replicas": "0"}, {"replicas": "two"}, {"readIOPS": "-1"}, {"readIOPS": "0"}, {"readBytesPerSecond": "fast"},
	} {
		_, err := parseMutableParameters(bad)
		assert.Error(t, err, "%v", bad)
	}
	_, err = parseMutableParameters(map[string]string{"pool": "vg1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"pool"`)
	assert.Contains(t, err.Error(), "readBytesPerSecond, readIOPS, replicas, resourceProfile, writeBytesPerSecond, writeIOPS")
}

func TestModifyVolumeRejectsUnknownKeysBeforeAnyChange(t *testing.T) {
	f := newModifyFake("n1", "n2", "n3")
	f.seedVolume("pvc_a", []string{"n1", "n2"}, map[string]string{placementTopologyLabel: topologyAnywhere})
	err := modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "3", "storageType": "zfs"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "storageType")
	assert.Empty(t, f.added, "a bad key changes nothing, not even the valid keys beside it")
}

func TestModifyVolumeMissingVolumeAndEmptyParameters(t *testing.T) {
	f := newModifyFake("n1", "n2")
	assert.Equal(t, codes.InvalidArgument, status.Code(modifyVolume(t, f, "", nil)))
	assert.Equal(t, codes.NotFound, status.Code(modifyVolume(t, f, "pvc_missing", map[string]string{"readIOPS": "5"})))
	f.seedVolume("pvc_a", []string{"n1", "n2"}, nil)
	require.NoError(t, modifyVolume(t, f, "pvc_a", nil))
	assert.Empty(t, f.labelCalls)
}

func TestModifyVolumeQoSIsRecordedInLabelsOnce(t *testing.T) {
	f := newModifyFake("n1", "n2")
	f.seedVolume("pvc_a", []string{"n1", "n2"}, nil)
	params := map[string]string{"readIOPS": "500", "writeBytesPerSecond": "max"}
	require.NoError(t, modifyVolume(t, f, "pvc_a", params))
	assert.Equal(t, "500", f.resources["pvc_a"].Labels["haify.csi/qos-readIOPS"])
	assert.Equal(t, "max", f.resources["pvc_a"].Labels["haify.csi/qos-writeBytesPerSecond"])
	require.Len(t, f.labelCalls, 1)

	require.NoError(t, modifyVolume(t, f, "pvc_a", params))
	assert.Len(t, f.labelCalls, 1, "the same class again writes nothing")
}

func TestModifyVolumeProfile(t *testing.T) {
	f := newModifyFake("n1", "n2")
	f.profiles["gold"] = &haifypb.ResourceProfile{Name: "gold", Pool: "vg0"}
	f.seedVolume("pvc_a", []string{"n1", "n2"}, nil)

	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"resourceProfile": "gold"}))
	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"resourceProfile": "gold"}))
	assert.Equal(t, []string{"pvc_a=gold"}, f.profileCalls, "idempotent")

	err := modifyVolume(t, f, "pvc_a", map[string]string{"resourceProfile": "platinum"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"resourceProfile": ""}))
	assert.Equal(t, []string{"pvc_a=gold", "pvc_a="}, f.profileCalls, "an empty profile leaves the profile")
}

func TestModifyVolumeGrowsReplicasAcrossFaultDomains(t *testing.T) {
	f := newModifyFake("n1", "n2", "n3", "n4")
	f.nodes[0].Labels = map[string]string{"host": "a"}
	f.nodes[1].Labels = map[string]string{"host": "b"}
	f.nodes[2].Labels = map[string]string{"host": "a"} // same machine as n1
	f.nodes[3].Labels = map[string]string{"host": "c"}
	f.pools[2].FreeBytes = 900 * giB // the roomiest, but in n1's domain
	f.pools[3].FreeBytes = 100 * giB
	f.seedVolume("pvc_a", []string{"n1", "n2"}, map[string]string{placementTopologyLabel: topologyAnywhere})

	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "3"}))
	assert.Equal(t, []string{"pvc_a/n4"}, f.added)
	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "3"}))
	assert.Equal(t, []string{"pvc_a/n4"}, f.added, "idempotent")
}

func TestModifyVolumeGrowIsPoolAwareAndCapacityChecked(t *testing.T) {
	f := newModifyFake("n1", "n2", "n3", "n4")
	f.onlyPoolOnNodes("n1", "n2", "n3")
	f.seedVolume("pvc_a", []string{"n1", "n2"}, map[string]string{placementTopologyLabel: topologyAnywhere})
	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "3"}))
	assert.Equal(t, []string{"pvc_a/n3"}, f.added, "n4 has no copy of the pool")

	err := modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "4"})
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
}

func TestModifyVolumeGrowDoesNotNeedRoomOnExistingReplicas(t *testing.T) {
	f := newModifyFake("n1", "n2", "n3")
	f.pools[0].FreeBytes = 1 // n1's pool is nearly full, its copy is already there
	f.seedVolume("pvc_a", []string{"n1", "n2"}, map[string]string{placementTopologyLabel: topologyAnywhere})
	f.resources["pvc_a"].Volumes[0].SizeGb = 10
	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "3"}))
	assert.Equal(t, []string{"pvc_a/n3"}, f.added)
}

func TestModifyVolumeGrowPrefersTheDisklessClientsNode(t *testing.T) {
	f := newModifyFake("n1", "n2", "n3", "n4")
	f.pools[2].FreeBytes = 900 * giB
	r := f.seedVolume("pvc_a", []string{"n1", "n2"}, map[string]string{placementTopologyLabel: topologyAnywhere})
	r.DisklessClients = []string{"n4"}
	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "3"}))
	assert.Equal(t, []string{"pvc_a/n4"}, f.added, "the Pod's node gets the copy and reads locally")
}

func TestModifyVolumeGrowRecordsTheTopologyOfAnOlderVolume(t *testing.T) {
	f := newModifyFake("n1", "n2", "n3")
	f.seedVolume("pvc_a", []string{"n1", "n2"}, nil)
	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "3"}))
	assert.Equal(t, "n1+n2", f.resources["pvc_a"].Labels[placementTopologyLabel])
	assert.Equal(t, []string{"pvc_a/n3"}, f.added)

	// Back to two: the added copy goes, the PV's own nodes stay.
	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "2"}))
	assert.Equal(t, []string{"pvc_a/n3"}, f.removed)
}

func TestModifyVolumeShrinkKeepsThePrimaryAndDropsALaggingCopyFirst(t *testing.T) {
	f := newModifyFake("n1", "n2", "n3")
	r := f.seedVolume("pvc_a", []string{"n1", "n2", "n3"}, map[string]string{placementTopologyLabel: topologyAnywhere})
	r.NodeStates["10.0.0.3"].DiskState = "Inconsistent"
	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "2"}))
	assert.Equal(t, []string{"pvc_a/n3"}, f.removed)
	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "2"}))
	assert.Equal(t, []string{"pvc_a/n3"}, f.removed, "idempotent")
}

func TestModifyVolumeShrinkNeverRemovesThePrimary(t *testing.T) {
	f := newModifyFake("n1", "n2", "n3")
	r := f.seedVolume("pvc_a", []string{"n1", "n2", "n3"}, map[string]string{placementTopologyLabel: topologyAnywhere})
	// The Primary is the crowded, fullest one; it still stays.
	f.nodes[0].Labels = map[string]string{"host": "x"}
	f.nodes[1].Labels = map[string]string{"host": "x"}
	f.pools[0].FreeBytes = 1
	delete(r.NodeStates, "10.0.0.3") // n3's state unknown
	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "2"}))
	assert.Equal(t, []string{"pvc_a/n2"}, f.removed)
}

func TestModifyVolumeShrinkWaitsWhileOnlyBusyReplicasRemain(t *testing.T) {
	f := newModifyFake("n1", "n2", "n3")
	r := f.seedVolume("pvc_a", []string{"n1", "n2", "n3"}, map[string]string{placementTopologyLabel: topologyAnywhere})
	delete(r.NodeStates, "10.0.0.2")
	delete(r.NodeStates, "10.0.0.3")
	err := modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "2"})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err), "retryable: the states may come back")
	assert.Empty(t, f.removed)
}

func TestModifyVolumeShrinkKeepsThePinnedNodes(t *testing.T) {
	f := newModifyFake("n1", "n2", "n3")
	f.seedVolume("pvc_a", []string{"n1", "n2", "n3"}, map[string]string{placementTopologyLabel: "n1+n2+n3"})
	err := modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "2"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "node affinity")
	assert.Empty(t, f.removed)
}

func TestModifyVolumeShrinkRefusesToLeaveNoUpToDateCopy(t *testing.T) {
	f := newModifyFake("n1", "n2", "n3")
	r := f.seedVolume("pvc_a", []string{"n1", "n2", "n3"}, map[string]string{placementTopologyLabel: "n1"})
	r.NodeStates["10.0.0.1"].DiskState = "Inconsistent" // Primary, reading through a peer
	delete(r.NodeStates, "10.0.0.2")
	err := modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "2"})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "UpToDate")
	assert.Empty(t, f.removed)
}

func TestModifyVolumeShrinkBelowTwoIsRefused(t *testing.T) {
	f := newModifyFake("n1", "n2")
	f.seedVolume("pvc_a", []string{"n1", "n2"}, map[string]string{placementTopologyLabel: topologyAnywhere})
	err := modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "1"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Empty(t, f.removed)
}

func TestModifyVolumeSkipsTheWanDRWhenCounting(t *testing.T) {
	f := newModifyFake("n1", "n2", "dr")
	r := f.seedVolume("pvc_a", []string{"n1", "n2", "dr"}, map[string]string{placementTopologyLabel: topologyAnywhere})
	r.WanMode, r.DrNode = true, "dr"
	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "2"}))
	assert.Empty(t, f.added)
	assert.Empty(t, f.removed)
}

func TestReplicaViewsResolveStateKeys(t *testing.T) {
	nodes := []*haifypb.NodeInfo{
		{Name: "n1", Address: "10.0.0.1"},
		{Name: "n2", Address: "10.0.0.2", Hostname: "box2"},
		{Name: "n3", Address: "10.0.0.3"},
	}
	r := &haifypb.ResourceInfo{Name: "pvc_a", NodeStates: map[string]*haifypb.NodeResourceState{
		"10.0.0.1": {Role: "Primary", DiskState: "UpToDate"},
		"box2":     {Role: "Secondary", DiskState: "UpToDate"},
		"whatever": {Role: "Secondary", DiskState: "Outdated", Node: "n3"},
	}}
	views := replicaViews(r, []string{"n1", "n2", "n3"}, nodes, nil)
	require.Len(t, views, 3)
	assert.True(t, views[0].known && views[0].primary && views[0].upToDate)
	assert.True(t, views[1].known && !views[1].primary && views[1].upToDate)
	assert.True(t, views[2].known && !views[2].upToDate)
}

func TestCreateVolumeTakesMutableParameters(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	s := &controllerServer{backend: b, log: zap.NewNop()}
	resp, err := s.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		VolumeCapabilities: []*csi.VolumeCapability{mountCap()},
		Parameters:         map[string]string{"pool": "vg0", "replicas": "2", "readIOPS": "100", "writeIOPS": "50"},
		MutableParameters:  map[string]string{"replicas": "3", "readIOPS": "900", "writeIOPS": "max"},
	})
	require.NoError(t, err)
	require.Len(t, b.requestCalls, 1)
	assert.Len(t, b.requestCalls[0].Nodes, 3, "the class's replicas win")
	assert.Equal(t, "900", resp.GetVolume().GetVolumeContext()["readIOPS"])
	assert.NotContains(t, resp.GetVolume().GetVolumeContext(), "writeIOPS", "max is no limit")
	assert.Equal(t, "n1+n2+n3", b.requestCalls[0].Labels[placementTopologyLabel])
	assert.Equal(t, "host", b.requestCalls[0].Labels[faultDomainLabelKey])

	_, err = s.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-2",
		VolumeCapabilities: []*csi.VolumeCapability{mountCap()},
		Parameters:         map[string]string{"pool": "vg0"},
		MutableParameters:  map[string]string{"XXX_FakeKey": "x"},
	})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestCreateVolumeRecordsRemoteAccessTopology(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	s := &controllerServer{backend: b, log: zap.NewNop()}
	_, err := s.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		VolumeCapabilities: []*csi.VolumeCapability{mountCap()},
		Parameters:         map[string]string{"pool": "vg0", "allowRemoteVolumeAccess": "true"},
	})
	require.NoError(t, err)
	assert.Equal(t, topologyAnywhere, b.requestCalls[0].Labels[placementTopologyLabel])
}
