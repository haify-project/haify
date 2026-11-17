package csi

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testServiceIPs = "192.168.50.200-192.168.50.202/24"

func fsCap(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}

func rawCap(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}

func sharedCreateReq(name string) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:          name,
		CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 30},
		Parameters: map[string]string{"pool": "vg0", paramSharedFilesystem: sharedNFS,
			paramNFSServiceIPs: testServiceIPs},
		VolumeCapabilities: []*csi.VolumeCapability{fsCap(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)},
	}
}

func TestParseSharedParams(t *testing.T) {
	p, err := ParseVolumeParams(map[string]string{"pool": "vg0"})
	require.NoError(t, err)
	assert.False(t, p.Shared.Enabled)
	assert.False(t, p.AllowRemoteVolumeAccess, "a class without sharedFilesystem is parsed as before")

	p, err = ParseVolumeParams(map[string]string{"pool": "vg0", paramSharedFilesystem: "nfs",
		paramNFSServiceIPs: testServiceIPs, paramNFSAllowedClients: "10.0.0.0/16, 192.168.50.7"})
	require.NoError(t, err)
	assert.True(t, p.Shared.Enabled)
	assert.Len(t, p.Shared.ServiceIPs, 1)
	assert.Equal(t, []string{"10.0.0.0/16", "192.168.50.7"}, p.Shared.AllowedClients)
	assert.True(t, p.AllowRemoteVolumeAccess, "a shared volume is usable from every node")

	for name, bad := range map[string]map[string]string{
		"unknown mode":           {"pool": "vg0", paramSharedFilesystem: "smb", paramNFSServiceIPs: testServiceIPs},
		"no address pool":        {"pool": "vg0", paramSharedFilesystem: "nfs"},
		"pool without the mode":  {"pool": "vg0", paramNFSServiceIPs: testServiceIPs},
		"bad allowed client":     {"pool": "vg0", paramSharedFilesystem: "nfs", paramNFSServiceIPs: testServiceIPs, paramNFSAllowedClients: "everyone"},
		"I/O limits do not work": {"pool": "vg0", paramSharedFilesystem: "nfs", paramNFSServiceIPs: testServiceIPs, "readIOPS": "100"},
	} {
		_, err := ParseVolumeParams(bad)
		assert.Errorf(t, err, name)
	}
}

func TestCreateSharedVolume(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	req := sharedCreateReq("pvc-shared")
	req.Parameters[paramNFSAllowedClients] = "10.0.0.0/16"
	// Under WaitForFirstConsumer the provisioner names the Pod's node.
	req.AccessibilityRequirements = &csi.TopologyRequirement{
		Requisite: []*csi.Topology{{Segments: map[string]string{TopologyKeyNode: "n3"}}}}
	ctrl := newTestController(b)
	resp, err := ctrl.CreateVolume(context.Background(), req)
	require.NoError(t, err)

	r := b.resources["pvc_shared"]
	require.NotNil(t, r)
	assert.Equal(t, sharedNFS, r.Labels[sharedLabel])
	assert.Equal(t, "192.168.50.200/24", r.Labels[nfsServiceIPLabel])
	assert.Equal(t, managedByValue, r.Labels[managedByLabel])
	assert.Equal(t, topologyAnywhere, r.Labels[placementTopologyLabel])

	require.Len(t, b.gatewayCreates, 1)
	gw := b.gatewayCreates[0]
	assert.Equal(t, "pvc_shared", gw.Resource)
	assert.Equal(t, "192.168.50.200/24", gw.ServiceIp)
	assert.Empty(t, gw.ExportPath, "the controller picks the export directory")
	assert.Equal(t, []string{"10.0.0.0/16"}, gw.AllowedIps)

	assert.Equal(t, map[string]string{
		paramSharedFilesystem: sharedNFS,
		ctxNFSServer:          "192.168.50.200",
		ctxNFSExport:          "/srv/gateway-exports/pvc_shared",
	}, resp.Volume.VolumeContext, "everything a node needs to mount, and no diskless-client flag")
	assert.Empty(t, resp.Volume.AccessibleTopology, "a shared volume is not tied to the replica nodes")
	assert.Equal(t, []string{"n1", "n2"}, r.Nodes, "replicas go by free space, not to the Pod's node")
	assert.Empty(t, ctrl.shared.ips.pending, "the label holds the address now")
	assert.Empty(t, ctrl.shared.busy)
}

func TestCreateSharedVolumeAccessModes(t *testing.T) {
	for _, mode := range []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
	} {
		assert.Emptyf(t, capabilityProblem(fsCap(mode), true), "%s", mode)
	}
	assert.Contains(t, capabilityProblem(rawCap(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER), true), "volumeMode: Block")
	xfs := fsCap(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)
	xfs.GetMount().FsType = "xfs"
	assert.Contains(t, capabilityProblem(xfs, true), "fsType")

	b := newFakeBackend("n1", "n2")
	req := sharedCreateReq("pvc-blk")
	req.VolumeCapabilities = []*csi.VolumeCapability{rawCap(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)}
	_, err := newTestController(b).CreateVolume(context.Background(), req)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Empty(t, b.resources)
}

// A class without sharedFilesystem behaves exactly as before: ReadWriteMany is
// refused for a filesystem and allowed for a block volume, and neither goes
// near a gateway.
func TestOrdinaryVolumesAreUntouched(t *testing.T) {
	for _, mode := range []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
	} {
		assert.Equal(t, unsupportedCapability(fsCap(mode)), capabilityProblem(fsCap(mode), false), "%s mount", mode)
		assert.Equal(t, unsupportedCapability(rawCap(mode)), capabilityProblem(rawCap(mode), false), "%s block", mode)
	}

	b := newFakeBackend("n1", "n2")
	ctrl := newTestController(b)
	rwxFS := validCreateReq("pvc-rwx-fs")
	rwxFS.VolumeCapabilities = []*csi.VolumeCapability{fsCap(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)}
	_, err := ctrl.CreateVolume(context.Background(), rwxFS)
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "ReadWriteMany filesystem needs the class to opt in")

	rwxBlock := validCreateReq("pvc-rwx-blk")
	rwxBlock.VolumeCapabilities = []*csi.VolumeCapability{rawCap(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)}
	resp, err := ctrl.CreateVolume(context.Background(), rwxBlock)
	require.NoError(t, err)
	assert.Len(t, resp.Volume.AccessibleTopology, 2)
	assert.Empty(t, resp.Volume.VolumeContext)

	rwo, err := ctrl.CreateVolume(context.Background(), validCreateReq("pvc-rwo"))
	require.NoError(t, err)
	assert.Len(t, rwo.Volume.AccessibleTopology, 2, "pinned to its replica nodes")
	assert.Empty(t, rwo.Volume.VolumeContext)
	assert.NotContains(t, b.resources["pvc_rwo"].Labels, sharedLabel)
	assert.NotContains(t, b.resources["pvc_rwo"].Labels, nfsServiceIPLabel)

	assert.Empty(t, b.gatewayCreates)
	_, err = ctrl.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "pvc_rwo"})
	require.NoError(t, err)
	assert.Empty(t, b.gatewayDeletes)
	assert.Empty(t, ctrl.shared.busy)
}

func TestCreateSharedVolumesConcurrently(t *testing.T) {
	const volumes = 20
	b := &lockedBackend{fakeBackend: newFakeBackend("n1", "n2", "n3")}
	ctrl := newTestController(b)
	var wg sync.WaitGroup
	errs := make(chan error, volumes)
	for i := 0; i < volumes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := sharedCreateReq(fmt.Sprintf("pvc-c%d", i))
			req.Parameters[paramNFSServiceIPs] = "192.168.50.100-192.168.50.119/24"
			if _, err := ctrl.CreateVolume(context.Background(), req); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	seen := map[string]string{}
	for name, r := range b.resources {
		ip := r.Labels[nfsServiceIPLabel]
		require.NotEmpty(t, ip, name)
		assert.Emptyf(t, seen[ip], "%s given to %s and %s", ip, seen[ip], name)
		seen[ip] = name
	}
	assert.Len(t, seen, volumes)
	assert.Empty(t, ctrl.shared.ips.pending)
}

// A controller plugin that starts fresh knows nothing, and still must not
// hand out an address that is taken: by a volume an earlier instance made, by
// a gateway someone created by hand, or by a node.
func TestServiceAddressesSurviveARestart(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	_, err := newTestController(b).CreateVolume(context.Background(), sharedCreateReq("pvc-one"))
	require.NoError(t, err)
	assert.Equal(t, "192.168.50.200/24", b.resources["pvc_one"].Labels[nfsServiceIPLabel])

	// An earlier instance died between creating a resource and its gateway:
	// the label alone holds the address.
	b.resources["pvc_half"] = &haifypb.ResourceInfo{Name: "pvc_half", Nodes: []string{"n1", "n2"},
		Volumes: []*haifypb.VolumeInfo{{VolumeId: 0, SizeGb: 2}},
		Labels:  map[string]string{managedByLabel: managedByValue, sharedLabel: sharedNFS, nfsServiceIPLabel: "192.168.50.201/24"}}
	// An operator's own gateway and a node sit on pool addresses too.
	b.gateways["manual"] = &haifypb.GatewayInfo{Resource: "manual", Type: "iscsi",
		Options: map[string]string{"service_ip": "192.168.50.202/24"}}
	b.nodes[2].Address = "192.168.50.203"

	req := sharedCreateReq("pvc-two")
	req.Parameters[paramNFSServiceIPs] = "192.168.50.200-192.168.50.204/24"
	_, err = newTestController(b).CreateVolume(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, "192.168.50.204/24", b.resources["pvc_two"].Labels[nfsServiceIPLabel])

	// The retry of the half-made volume gets its gateway on the address it
	// was given, not a new one.
	resp, err := newTestController(b).CreateVolume(context.Background(), sharedCreateReq("pvc-half"))
	require.NoError(t, err)
	assert.Equal(t, "192.168.50.201", resp.Volume.VolumeContext[ctxNFSServer])
	assert.Equal(t, "192.168.50.201/24", b.gateways["pvc_half"].Options["service_ip"])
}

func TestCreateSharedVolumePoolExhausted(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	ctrl := newTestController(b)
	for i := 0; i < 3; i++ {
		_, err := ctrl.CreateVolume(context.Background(), sharedCreateReq(fmt.Sprintf("pvc-%d", i)))
		require.NoError(t, err)
	}
	_, err := ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-3"))
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
	assert.Contains(t, err.Error(), paramNFSServiceIPs)
	assert.NotContains(t, b.resources, "pvc_3", "nothing is created without an address")

	// Deleting a volume gives its address back.
	_, err = ctrl.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "pvc_1"})
	require.NoError(t, err)
	_, err = ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-3"))
	require.NoError(t, err)
	assert.Equal(t, "192.168.50.201/24", b.resources["pvc_3"].Labels[nfsServiceIPLabel])
}

func TestCreateSharedVolumeFailsWhenAddressesInUseCannotBeRead(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	b.listGatewaysErr = fmt.Errorf("controller busy")
	_, err := newTestController(b).CreateVolume(context.Background(), sharedCreateReq("pvc-x"))
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Empty(t, b.resources)
}

func TestCreateSharedVolumeIsIdempotent(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	ctrl := newTestController(b)
	first, err := ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-again"))
	require.NoError(t, err)
	second, err := ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-again"))
	require.NoError(t, err)
	assert.Equal(t, first.Volume.VolumeContext, second.Volume.VolumeContext)
	assert.Empty(t, second.Volume.AccessibleTopology)
	assert.Len(t, b.createCalls, 1)
	assert.Len(t, b.gatewayCreates, 1, "an existing gateway is found, not created again")

	// The same name asked for as the other kind of volume is a conflict.
	_, err = ctrl.CreateVolume(context.Background(), validCreateReq("pvc-again"))
	assert.Equal(t, codes.AlreadyExists, status.Code(err))
	_, err = ctrl.CreateVolume(context.Background(), validCreateReq("pvc-plain"))
	require.NoError(t, err)
	_, err = ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-plain"))
	assert.Equal(t, codes.AlreadyExists, status.Code(err))
	assert.Len(t, b.gatewayCreates, 1)
}

func TestCreateSharedVolumeWhileAnotherCallHasIt(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	ctrl := newTestController(b)
	done, ok := ctrl.shared.begin("pvc_busy")
	require.True(t, ok)
	_, err := ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-busy"))
	assert.Equal(t, codes.Aborted, status.Code(err))
	done()
	_, err = ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-busy"))
	require.NoError(t, err)
}

func TestCreateSharedVolumeRollsBackWhenGatewayCannotBeCreated(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	ctrl := newTestController(b)
	b.gatewayErr = status.Error(codes.FailedPrecondition, "nfs-kernel-server is not installed on n1")
	_, err := ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-nogw"))
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "nfs-kernel-server")
	assert.NotContains(t, b.resources, "pvc_nogw", "the resource does not outlive a gateway that cannot be made")

	b.gatewayErr = nil
	_, err = ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-next"))
	require.NoError(t, err)
	assert.Equal(t, "192.168.50.200/24", b.resources["pvc_next"].Labels[nfsServiceIPLabel], "the address went back to the pool")
}

func TestCreateSharedVolumeKeepsResourceWhenControllerStopsAnswering(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	ctrl := newTestController(b)
	b.gatewayErr = status.Error(codes.Unavailable, "connection lost")
	_, err := ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-slow"))
	assert.Equal(t, codes.Unavailable, status.Code(err))
	require.Contains(t, b.resources, "pvc_slow", "the gateway may still appear; the retry finishes the volume")

	b.gatewayErr = nil
	resp, err := ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-slow"))
	require.NoError(t, err)
	assert.Equal(t, "192.168.50.200", resp.Volume.VolumeContext[ctxNFSServer])
	assert.Len(t, b.createCalls, 1)
}

func TestDeleteSharedVolume(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	ctrl := newTestController(b)
	_, err := ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-del"))
	require.NoError(t, err)

	_, err = ctrl.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "pvc_del"})
	require.NoError(t, err)
	assert.Equal(t, []string{"pvc_del"}, b.gatewayDeletes, "the gateway goes before the resource")
	assert.NotContains(t, b.gateways, "pvc_del")
	assert.NotContains(t, b.resources, "pvc_del")

	_, err = ctrl.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "pvc_del"})
	require.NoError(t, err, "deleting it again is success")
	assert.Len(t, b.gatewayDeletes, 1)
}

// A DeleteVolume that removed the gateway and then failed is retried: the
// gateway is gone, the resource is not.
func TestDeleteSharedVolumeAfterGatewayIsGone(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	ctrl := newTestController(b)
	_, err := ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-part"))
	require.NoError(t, err)
	delete(b.gateways, "pvc_part")

	_, err = ctrl.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "pvc_part"})
	require.NoError(t, err)
	assert.Empty(t, b.gatewayDeletes)
	assert.NotContains(t, b.resources, "pvc_part")
}

func TestSharedVolumeRefusesWhatCannotWork(t *testing.T) {
	b := newFakeBackend("n1", "n2", "dr1")
	ctrl := newTestController(b)

	limited := sharedCreateReq("pvc-qos")
	limited.Parameters["writeIOPS"] = "500"
	_, err := ctrl.CreateVolume(context.Background(), limited)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "I/O limits")

	dr := sharedCreateReq("pvc-dr")
	dr.Parameters[paramDRNode], dr.Parameters[paramDREndpoint] = "dr1", "dr.example.com"
	_, err = ctrl.CreateVolume(context.Background(), dr)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), paramDRNode)
	assert.Empty(t, b.resources)

	_, err = ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-ok"))
	require.NoError(t, err)
	shared := b.resources["pvc_ok"]
	assert.Error(t, sharedModifyProblem(shared, mutableChange{qos: map[string]string{"readIOPS": "100"}}))
	assert.NoError(t, sharedModifyProblem(shared, mutableChange{replicas: 3}), "replicas can still be changed")
	assert.NoError(t, sharedModifyProblem(&haifypb.ResourceInfo{Name: "plain"}, mutableChange{qos: map[string]string{"readIOPS": "100"}}))
}

func TestSharedVolumeContentSource(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	ctrl := newTestController(b)
	_, err := ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-src"))
	require.NoError(t, err)
	b.resources["pvc_src"].Volumes[0].BackingVolume = "pvc_src_data"
	b.resources["pvc_src"].Volumes[0].Pool = "haify_vg0"
	seedSnapshotSource(b, "pvc_rwo", "n1", "n2")

	from := func(name, source string, bytes int64) *csi.CreateVolumeRequest {
		req := sharedCreateReq(name)
		req.CapacityRange.RequiredBytes = bytes
		req.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
			Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: source}}}
		return req
	}

	_, err = ctrl.CreateVolume(context.Background(), from("pvc-from-rwo", "pvc_rwo", 2<<30))
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "the filesystem of an ordinary volume may not be one the gateway mounts")
	_, err = ctrl.CreateVolume(context.Background(), from("pvc-bigger", "pvc_src", 4<<30))
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "nothing would grow the filesystem to the new size")
	assert.Empty(t, b.snapCreated, "refused before the clone's snapshot is taken")

	resp, err := ctrl.CreateVolume(context.Background(), from("pvc-clone", "pvc_src", 2<<30))
	require.NoError(t, err)
	assert.Equal(t, "192.168.50.201", resp.Volume.VolumeContext[ctxNFSServer])
	require.Len(t, b.populated, 1)
	assert.Contains(t, b.populated[0], "pvc_clone/0<-pvc_src/0", "the data volume is copied, before the gateway mounts it")

	backed := sharedCreateReq("pvc-from-backup")
	backed.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
		Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: snapshotRef{resource: "pvc_src", node: "n1", name: "s1", backedUp: true}.id()}}}
	_, err = ctrl.CreateVolume(context.Background(), backed)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

// A snapshot of a shared volume is of its data, volume 0; the state volume
// the gateway added after it is not what gets snapshotted.
func TestSnapshotOfSharedVolume(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	ctrl := newTestController(b)
	_, err := ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-snap"))
	require.NoError(t, err)
	r := b.resources["pvc_snap"]
	require.Len(t, r.Volumes, 2)
	r.Volumes[0].BackingVolume, r.Volumes[0].Pool = "pvc_snap_data", "haify_vg0"

	_, err = ctrl.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{SourceVolumeId: "pvc_snap", Name: "snap-1"})
	require.NoError(t, err)
	require.Len(t, b.snapCreated, 1)
	assert.Contains(t, b.snapCreated[0], "haify_vg0/pvc_snap_data/")
}

func TestValidateAndCapacityForSharedVolume(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	ctrl := newTestController(b)
	_, err := ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-val"))
	require.NoError(t, err)
	_, err = ctrl.CreateVolume(context.Background(), validCreateReq("pvc-rwo"))
	require.NoError(t, err)
	rwx := []*csi.VolumeCapability{fsCap(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)}

	resp, err := ctrl.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "pvc_val", VolumeCapabilities: rwx})
	require.NoError(t, err)
	assert.NotNil(t, resp.Confirmed)
	resp, err = ctrl.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "pvc_rwo", VolumeCapabilities: rwx})
	require.NoError(t, err)
	assert.Nil(t, resp.Confirmed, "an ordinary volume is still not ReadWriteMany")

	b.pools[0].FreeBytes, b.pools[1].FreeBytes = 50*giB, 40*giB
	capResp, err := ctrl.GetCapacity(context.Background(), &csi.GetCapacityRequest{
		Parameters: sharedCreateReq("x").Parameters, VolumeCapabilities: rwx,
		AccessibleTopology: &csi.Topology{Segments: map[string]string{TopologyKeyNode: "elsewhere"}}})
	require.NoError(t, err)
	assert.Positive(t, capResp.AvailableCapacity, "a node without the pool can still run Pods of a shared volume")
}

// The HA controller moves Pods off a node that dropped out of a volume. For a
// shared volume that would be the outage: the gateway moves by itself and the
// Pods, wherever they run, keep their mount.
func TestHAJudgeNodeLossLeavesSharedVolumesAlone(t *testing.T) {
	id := haNodeIdentity{"node-a": true}
	plain := &haifypb.ResourceInfo{Name: "pvc_plain", NodeStates: survivorView("Connecting")}
	require.True(t, haJudgeNodeLoss(plain, id).lost)

	shared := &haifypb.ResourceInfo{Name: "pvc_shared", NodeStates: survivorView("Connecting"),
		Labels: map[string]string{sharedLabel: sharedNFS}}
	v := haJudgeNodeLoss(shared, id)
	assert.False(t, v.lost)
	assert.Contains(t, v.reason, "NFS")
}
