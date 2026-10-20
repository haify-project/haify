package csi

import (
	"context"
	"errors"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newTestController(b HaifyBackend) *controllerServer {
	return NewControllerServer(b, zap.NewNop()).(*controllerServer)
}

func validCreateReq(name string) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:               name,
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 2 << 30}, // 2 GiB
		Parameters:         map[string]string{"pool": "vg0"},
		VolumeCapabilities: []*csi.VolumeCapability{{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}}},
	}
}

func TestCreateVolumeCreatesResource(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	resp, err := newTestController(b).CreateVolume(context.Background(), validCreateReq("pvc-abc"))
	require.NoError(t, err)
	assert.Equal(t, "pvc_abc", resp.Volume.VolumeId)
	assert.Equal(t, int64(2<<30), resp.Volume.CapacityBytes)
	assert.Len(t, b.createCalls, 1)
	assert.Equal(t, uint32(2), b.createCalls[0].sizeGB)
	assert.Equal(t, uint32(0), b.createCalls[0].port, "port must be auto (0) for controller to allocate")
	assert.Len(t, resp.Volume.AccessibleTopology, 2)
}

func TestCreateVolumeIdempotent(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	c := newTestController(b)
	_, err := c.CreateVolume(context.Background(), validCreateReq("pvc-abc"))
	require.NoError(t, err)
	_, err = c.CreateVolume(context.Background(), validCreateReq("pvc-abc"))
	require.NoError(t, err)
	assert.Len(t, b.createCalls, 1, "second call must not create again")
}

func TestCreateVolumeHonorsRequisiteTopology(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	req := validCreateReq("pvc-top")
	req.AccessibilityRequirements = &csi.TopologyRequirement{
		Requisite: []*csi.Topology{{Segments: map[string]string{TopologyKeyNode: "n3"}}},
	}
	_, err := newTestController(b).CreateVolume(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, "n3", b.createCalls[0].nodes[0])
}

func TestCreateVolumeOnlyPlacesOnPoolNodes(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	b.onlyPoolOnNodes("n1", "n2") // n3 has no backing pool
	_, err := newTestController(b).CreateVolume(context.Background(), validCreateReq("pvc-pool"))
	require.NoError(t, err)
	require.Len(t, b.createCalls, 1)
	for _, n := range b.createCalls[0].nodes {
		assert.NotEqual(t, "n3", n, "must not place a replica on a node without the pool")
	}
	assert.Len(t, b.createCalls[0].nodes, 2)
}

func TestCreateVolumeSkipsPoollessRequisiteNode(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	b.onlyPoolOnNodes("n1", "n2")
	req := validCreateReq("pvc-req")
	// The scheduler prefers n3, but n3 lacks the pool: it must be dropped, not
	// placed on and failed at LV-creation time.
	req.AccessibilityRequirements = &csi.TopologyRequirement{
		Preferred: []*csi.Topology{{Segments: map[string]string{TopologyKeyNode: "n3"}}},
	}
	_, err := newTestController(b).CreateVolume(context.Background(), req)
	require.NoError(t, err)
	for _, n := range b.createCalls[0].nodes {
		assert.NotEqual(t, "n3", n)
	}
}

func TestCreateVolumeInsufficientPoolNodes(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	b.onlyPoolOnNodes("n1") // only one node has the pool, but replicas default to 2
	_, err := newTestController(b).CreateVolume(context.Background(), validCreateReq("pvc-few"))
	require.Error(t, err)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
	assert.Empty(t, b.createCalls, "no resource must be created when the pool lacks enough nodes")
}

func TestDeleteVolumeIdempotent(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	c := newTestController(b)
	_, err := c.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "does-not-exist"})
	require.NoError(t, err, "deleting a missing volume must succeed")
}

func TestCreateVolumeMissingPool(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	req := validCreateReq("pvc-x")
	req.Parameters = map[string]string{}
	_, err := newTestController(b).CreateVolume(context.Background(), req)
	assert.Error(t, err)
}

func TestCreateVolumeUsesProfilePlacementDefaults(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	b.profiles["production"] = &haifypb.ResourceProfile{
		Name: "production", Pool: "vg0", StorageType: "lvm", Replicas: 3,
	}
	req := validCreateReq("pvc-profile")
	req.Parameters = map[string]string{"resourceProfile": "production"}

	_, err := newTestController(b).CreateVolume(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, b.createCalls, 1)
	assert.Len(t, b.createCalls[0].nodes, 3)
	assert.Equal(t, "vg0", b.createCalls[0].pool)
}

func TestCreateVolumeProfileOverridesAndMetadata(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	b.profiles["production"] = &haifypb.ResourceProfile{
		Name: "production", Pool: "archive", StorageType: "zfs", Replicas: 3,
	}
	req := validCreateReq("pvc-profile-overrides")
	req.Parameters = map[string]string{
		"resourceProfile": "production",
		"pool":            "vg0",
		"replicas":        "2",
		"storageType":     "lvm",
		"resourceLabels":  "env=prod,tier=critical",
	}

	resp, err := newTestController(b).CreateVolume(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, b.requestCalls, 1)
	created := b.requestCalls[0]
	assert.Equal(t, "production", created.Profile)
	assert.Equal(t, "vg0", created.Pool)
	assert.Equal(t, "lvm", created.StorageType)
	assert.Len(t, created.Nodes, 2)
	assert.Equal(t, "prod", created.Labels["env"])
	assert.Equal(t, "critical", created.Labels["tier"])
	assert.Equal(t, "csi", created.Labels["haify.csi/managed-by"])
	assert.Equal(t, "pvc_profile_overrides", resp.Volume.VolumeId)
}

func TestCreateVolumeLabelsTheClaim(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	req := validCreateReq("pvc-claim")
	req.Parameters = map[string]string{
		"pool": "vg0", paramPVCName: "data-postgres-0", paramPVCNamespace: "db",
	}
	_, err := newTestController(b).CreateVolume(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, b.requestCalls, 1)
	assert.Equal(t, "db/data-postgres-0", b.requestCalls[0].Labels[pvcLabel])
	assert.Equal(t, "csi", b.requestCalls[0].Labels[managedByLabel])
}

func TestCreateVolumeProfileErrors(t *testing.T) {
	tests := []struct {
		name     string
		prepare  func(*fakeBackend)
		params   map[string]string
		code     codes.Code
		contains string
	}{
		{
			name: "missing profile", params: map[string]string{"resourceProfile": "missing"},
			code: codes.InvalidArgument, contains: `resource profile "missing"`,
		},
		{
			name: "profile lookup failure", params: map[string]string{"resourceProfile": "production"},
			prepare: func(b *fakeBackend) { b.profileErr = errors.New("database unavailable") },
			code:    codes.InvalidArgument, contains: "database unavailable",
		},
		{
			name: "profile lacks pool", params: map[string]string{"resourceProfile": "empty"},
			prepare: func(b *fakeBackend) { b.profiles["empty"] = &haifypb.ResourceProfile{Name: "empty"} },
			code:    codes.InvalidArgument, contains: "does not define a pool",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newFakeBackend("n1", "n2")
			if tt.prepare != nil {
				tt.prepare(b)
			}
			req := validCreateReq("pvc-error")
			req.Parameters = tt.params
			_, err := newTestController(b).CreateVolume(context.Background(), req)
			require.Error(t, err)
			assert.Equal(t, tt.code, status.Code(err))
			assert.Contains(t, err.Error(), tt.contains)
			assert.Empty(t, b.createCalls)
		})
	}
}

func TestCreateVolumeBackendErrors(t *testing.T) {
	t.Run("list nodes", func(t *testing.T) {
		b := newFakeBackend("n1", "n2")
		b.listNodesErr = errors.New("nodes offline")
		_, err := newTestController(b).CreateVolume(context.Background(), validCreateReq("pvc-nodes"))
		assert.Equal(t, codes.Internal, status.Code(err))
		assert.Contains(t, err.Error(), "nodes offline")
	})

	t.Run("list pools", func(t *testing.T) {
		b := newFakeBackend("n1", "n2")
		b.listPoolsErr = errors.New("pool scan failed")
		_, err := newTestController(b).CreateVolume(context.Background(), validCreateReq("pvc-pools"))
		assert.Equal(t, codes.Internal, status.Code(err))
		assert.Contains(t, err.Error(), "pool scan failed")
	})

	t.Run("create resource", func(t *testing.T) {
		b := newFakeBackend("n1", "n2")
		b.createErr = errors.New("allocation failed")
		_, err := newTestController(b).CreateVolume(context.Background(), validCreateReq("pvc-create"))
		assert.Equal(t, codes.Internal, status.Code(err))
		assert.Contains(t, err.Error(), "allocation failed")
	})
}

type legacyBackend struct{ HaifyBackend }

func TestCreateVolumeRejectsProfileOnLegacyBackend(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	legacy := &legacyBackend{HaifyBackend: b}
	req := validCreateReq("pvc-legacy")
	req.Parameters = map[string]string{"resourceProfile": "production"}

	_, err := newTestController(legacy).CreateVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Contains(t, err.Error(), "does not support resource profiles")
}

// Raw block volumes are provisioned: a DRBD device is a block device, and the
// node plugin publishes it as one.
func TestCreateVolumeAcceptsBlockMode(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	req := validCreateReq("blockvol")
	req.VolumeCapabilities[0].AccessType = &csi.VolumeCapability_Block{
		Block: &csi.VolumeCapability_BlockVolume{},
	}
	_, err := newTestController(b).CreateVolume(context.Background(), req)
	require.NoError(t, err)
	assert.Len(t, b.createCalls, 1)
}

// Filesystem mode — the mode the driver actually implements — must keep working.
func TestCreateVolumeAcceptsFilesystemMode(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	req := validCreateReq("fsvol")
	req.VolumeCapabilities[0].AccessType = &csi.VolumeCapability_Mount{
		Mount: &csi.VolumeCapability_MountVolume{},
	}

	_, err := newTestController(b).CreateVolume(context.Background(), req)
	require.NoError(t, err, "filesystem mode must still be accepted")
	assert.Len(t, b.createCalls, 1)
}

// setPoolFree gives each node's copy of the test pool a free-space figure, so a
// test can pit capacity-aware placement against candidate-list order.
func (f *fakeBackend) setPoolFree(freeGBByNode map[string]uint64) {
	addrByName := map[string]string{}
	for _, n := range f.nodes {
		addrByName[n.GetName()] = n.GetAddress()
	}
	for _, p := range f.pools {
		for name, gb := range freeGBByNode {
			if addrByName[name] == p.GetNode() {
				p.FreeBytes = gb * giB
			}
		}
	}
}

// A PVC must land where `haify resource create` would: on the nodes with the
// most room. Before capacity-aware placement this took n1 and n2 purely because
// ListNodes returned them first, filling up an already-tight node.
func TestCreateVolumePrefersNodesWithMostFreeSpace(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	b.setPoolFree(map[string]uint64{"n1": 4, "n2": 200, "n3": 500})

	_, err := newTestController(b).CreateVolume(context.Background(), validCreateReq("pvc-cap"))
	require.NoError(t, err)
	require.Len(t, b.createCalls, 1)
	assert.Equal(t, []string{"n3", "n2"}, b.createCalls[0].nodes)
}

// Capacity must never outrank topology: with --strict-topology the requisite
// node is the one the scheduler already bound the Pod to, so a volume placed
// elsewhere leaves that Pod unable to mount it.
func TestCreateVolumeKeepsRequisiteNodeDespiteLowCapacity(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	b.setPoolFree(map[string]uint64{"n1": 3, "n2": 200, "n3": 500})

	req := validCreateReq("pvc-topo")
	req.AccessibilityRequirements = &csi.TopologyRequirement{Preferred: []*csi.Topology{
		{Segments: map[string]string{TopologyKeyNode: "n1"}},
	}}
	_, err := newTestController(b).CreateVolume(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, b.createCalls, 1)
	assert.Equal(t, []string{"n1", "n3"}, b.createCalls[0].nodes)
}

// Out of room everywhere must fail as ResourceExhausted, which is the status
// external-provisioner reacts to by rescheduling rather than retrying forever.
func TestCreateVolumeOutOfCapacityIsResourceExhausted(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	b.setPoolFree(map[string]uint64{"n1": 1, "n2": 1, "n3": 1})

	_, err := newTestController(b).CreateVolume(context.Background(), validCreateReq("pvc-full"))
	require.Error(t, err)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
	assert.Empty(t, b.createCalls, "no resource may be created when placement fails")
}

// Nomad passes the volume name the user chose; a Haify resource of that name
// that the driver did not make (a gateway's, haify-meta) is not handed out.
func TestCreateVolumeRefusesForeignResource(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	b.resources["share"] = &haifypb.ResourceInfo{Name: "share", Nodes: []string{"n1", "n2"}}
	_, err := newTestController(b).CreateVolume(context.Background(), validCreateReq("share"))
	require.Error(t, err)
	assert.Equal(t, codes.AlreadyExists, status.Code(err))
	assert.Contains(t, err.Error(), "not a CSI volume")
	assert.Empty(t, b.createCalls)
}
