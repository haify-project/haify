package csi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
)

func capWith(mode csi.VolumeCapability_AccessMode_Mode, block bool) *csi.VolumeCapability {
	c := &csi.VolumeCapability{AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode}}
	if block {
		c.AccessType = &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}
	} else {
		c.AccessType = &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}}
	}
	return c
}

// ---- access modes and block -------------------------------------------------

// ReadWriteMany used to be provisioned and then fail when a second node tried to
// promote. It has to be refused where the message reaches the PVC.
func TestMultiNodeAccessIsRefusedAtProvisioning(t *testing.T) {
	ctrl := newTestController(newFakeBackend("n1", "n2"))
	_, err := ctrl.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-rwx-fs", Parameters: map[string]string{"pool": "vg0"},
		VolumeCapabilities: []*csi.VolumeCapability{capWith(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, false)},
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "volumeMode: Block", "a filesystem cannot be written from two nodes")
	for _, mode := range []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
	} {
		_, err := ctrl.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
			Name: "pvc-rwx", Parameters: map[string]string{"pool": "vg0"},
			VolumeCapabilities: []*csi.VolumeCapability{capWith(mode, false)},
		})
		require.Error(t, err, mode.String())
		assert.Equal(t, codes.InvalidArgument, status.Code(err), mode.String())
		assert.Contains(t, err.Error(), "one Primary", mode.String())
	}
}

func TestReadWriteOncePodAndBlockProvision(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  csi.VolumeCapability_AccessMode_Mode
		block bool
	}{
		{"rwop-fs", csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER, false},
		{"rwop-block", csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER, true},
		{"rwo-block", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, true},
		{"multi-writer-same-node", csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER, false},
		{"rwx-block-live-migration", csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := newTestController(newFakeBackend("n1", "n2"))
			_, err := ctrl.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
				Name: "pvc-" + tc.name, Parameters: map[string]string{"pool": "vg0"},
				VolumeCapabilities: []*csi.VolumeCapability{capWith(tc.mode, tc.block)},
			})
			require.NoError(t, err)
		})
	}
}

func TestValidateConfirmsBlockAndRWOPButNotRWX(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	require.NoError(t, b.CreateResourceWithPoolAndType(context.Background(), "v", 0, []string{"n1", "n2"}, "C", 1, "vg0", "lvm", nil))
	ctrl := newTestController(b)
	validate := func(c *csi.VolumeCapability) *csi.ValidateVolumeCapabilitiesResponse {
		resp, err := ctrl.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
			VolumeId: "v", VolumeCapabilities: []*csi.VolumeCapability{c}})
		require.NoError(t, err)
		return resp
	}
	assert.NotNil(t, validate(capWith(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, true)).Confirmed)
	assert.NotNil(t, validate(capWith(csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER, false)).Confirmed)
	rwx := validate(capWith(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, false))
	assert.Nil(t, rwx.Confirmed)
	assert.NotEmpty(t, rwx.Message, "an unconfirmed capability should say why")
}

func blockStageRequest(vol string) *csi.NodeStageVolumeRequest {
	return &csi.NodeStageVolumeRequest{VolumeId: vol, StagingTargetPath: "/stage/" + vol,
		VolumeCapability: capWith(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, true)}
}

// Staging a block volume promotes the node and stops there: formatting the
// device would destroy whatever the Pod keeps on it.
func TestBlockStagePromotesWithoutFormatting(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	require.NoError(t, b.CreateResourceWithPoolAndType(context.Background(), "v", 0, []string{"n1", "n2"}, "C", 1, "vg0", "lvm", nil))
	m := newRecordingMounter()
	_, err := newTestNode(b, m).NodeStageVolume(context.Background(), blockStageRequest("v"))
	require.NoError(t, err)
	assert.Equal(t, "n1", b.primary["v"])
	assert.Empty(t, m.formatted, "a block volume must never be formatted")
}

func TestBlockPublishBindsTheDeviceOntoAFile(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	require.NoError(t, b.CreateResourceWithPoolAndType(context.Background(), "v", 0, []string{"n1", "n2"}, "C", 1, "vg0", "lvm", nil))
	m := newRecordingMounter()
	_, err := newTestNode(b, m).NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: "v", StagingTargetPath: "/stage/v", TargetPath: "/pods/x/volumeDevices/v",
		VolumeCapability: capWith(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, true),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"/pods/x/volumeDevices/v"}, m.files, "the bind target of a block volume is a file")
	assert.Equal(t, []string{"/dev/drbd100->/pods/x/volumeDevices/v"}, m.bind,
		"the device itself is published, not the staging path")
}

func TestBlockExpandDoesNotResizeAFilesystem(t *testing.T) {
	b := newFakeBackend("n1")
	require.NoError(t, b.CreateResourceWithPoolAndType(context.Background(), "v", 0, []string{"n1"}, "C", 1, "vg0", "lvm", nil))
	m := &expandMounter{recordingMounter: *newRecordingMounter()}
	resp, err := newTestNode(b, m).NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId: "v", VolumePath: "/pods/x/volumeDevices/v",
		CapacityRange:    &csi.CapacityRange{RequiredBytes: 2 * giB},
		VolumeCapability: capWith(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, true),
	})
	require.NoError(t, err)
	assert.Empty(t, m.resizedDevice, "there is no filesystem on a raw block volume")
	assert.Equal(t, int64(2*giB), resp.CapacityBytes)
}

// ---- volume stats ------------------------------------------------------------

func TestVolumeStatsForAFilesystemReportBytesAndInodes(t *testing.T) {
	m := newRecordingMounter()
	m.stats = FSStats{TotalBytes: 10 * giB, AvailableBytes: 7 * giB, UsedBytes: 3 * giB,
		TotalInodes: 1000, AvailableInodes: 900, UsedInodes: 100}
	resp, err := newTestNode(newFakeBackend("n1"), m).NodeGetVolumeStats(context.Background(),
		&csi.NodeGetVolumeStatsRequest{VolumeId: "v", VolumePath: t.TempDir()})
	require.NoError(t, err)
	require.Len(t, resp.Usage, 2)
	assert.Equal(t, csi.VolumeUsage_BYTES, resp.Usage[0].Unit)
	assert.Equal(t, int64(3*giB), resp.Usage[0].Used)
	assert.Equal(t, int64(7*giB), resp.Usage[0].Available)
	assert.Equal(t, csi.VolumeUsage_INODES, resp.Usage[1].Unit)
	assert.Equal(t, int64(100), resp.Usage[1].Used)
}

// A block volume is published onto a file. Its only honest figure is the size:
// how much of it the Pod uses is invisible from the node.
func TestVolumeStatsForABlockVolumeReportOnlyTheSize(t *testing.T) {
	target := filepath.Join(t.TempDir(), "dev")
	require.NoError(t, os.WriteFile(target, nil, 0o600))
	m := newRecordingMounter()
	m.blockSize = 5 * giB
	resp, err := newTestNode(newFakeBackend("n1"), m).NodeGetVolumeStats(context.Background(),
		&csi.NodeGetVolumeStatsRequest{VolumeId: "v", VolumePath: target})
	require.NoError(t, err)
	require.Len(t, resp.Usage, 1)
	assert.Equal(t, int64(5*giB), resp.Usage[0].Total)
	assert.Zero(t, resp.Usage[0].Used)
}

func TestVolumeStatsForAMissingPathIsNotFound(t *testing.T) {
	_, err := newTestNode(newFakeBackend("n1"), newRecordingMounter()).NodeGetVolumeStats(context.Background(),
		&csi.NodeGetVolumeStatsRequest{VolumeId: "v", VolumePath: "/no/such/volume"})
	assert.Equal(t, codes.NotFound, status.Code(err))
}

// ---- capacity ----------------------------------------------------------------

func thinPool(node string, sizeGiB, percent float64) *haifypb.PoolInfo {
	return &haifypb.PoolInfo{Name: "haify_vg0", Node: node, ThinPoolLv: "haifythin",
		ThinSizeBytes: uint64(sizeGiB * float64(giB)), ThinDataPercent: percent}
}

func capacityOn(t *testing.T, b HaifyBackend, node string) (*csi.GetCapacityResponse, error) {
	t.Helper()
	return newTestController(b).GetCapacity(context.Background(), &csi.GetCapacityRequest{
		Parameters:         map[string]string{"pool": "vg0"},
		AccessibleTopology: &csi.Topology{Segments: map[string]string{TopologyKeyNode: node}},
	})
}

func TestCapacityIsTheNodesThinPoolFreeSpace(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	b.pools = []*haifypb.PoolInfo{thinPool("10.0.0.1", 20, 75), thinPool("10.0.0.2", 20, 10)}
	resp, err := capacityOn(t, b, "n1")
	require.NoError(t, err)
	assert.Equal(t, int64(5*giB), resp.AvailableCapacity)
	assert.Equal(t, int64(5*giB), resp.MaximumVolumeSize.GetValue())
}

// The 2026-09-19 case: a thin pool at 100%. That is a measured zero and must be
// published as one, so the scheduler stops placing volumes there.
func TestAFullThinPoolReportsZero(t *testing.T) {
	b := newFakeBackend("n1")
	b.pools = []*haifypb.PoolInfo{thinPool("10.0.0.1", 20, 100)}
	resp, err := capacityOn(t, b, "n1")
	require.NoError(t, err)
	assert.Zero(t, resp.AvailableCapacity)
}

// A pool that reports nothing is not a pool that is full. Publishing 0 would
// take the node out of scheduling over a figure nobody measured.
func TestUnreportedCapacityIsUnavailableNotZero(t *testing.T) {
	b := newFakeBackend("n1")
	b.pools = []*haifypb.PoolInfo{{Name: "haify_vg0", Node: "10.0.0.1"}}
	_, err := capacityOn(t, b, "n1")
	assert.Equal(t, codes.Unavailable, status.Code(err))
}

func TestANodeWithoutThePoolHasNoCapacity(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	b.pools = []*haifypb.PoolInfo{thinPool("10.0.0.1", 20, 10)}
	resp, err := capacityOn(t, b, "n2")
	require.NoError(t, err)
	assert.Zero(t, resp.AvailableCapacity)
}

// With remote access a Pod may run on a node that holds no replica. Reporting
// 0 there kept the scheduler off every such node, so the option did nothing
// for a Pod that was not already on a replica node.
func TestARemoteAccessNodeHasThePoolsRoom(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	b.pools = []*haifypb.PoolInfo{thinPool("10.0.0.1", 20, 10), thinPool("10.0.0.2", 20, 50)}
	resp, err := newTestController(b).GetCapacity(context.Background(), &csi.GetCapacityRequest{
		Parameters:         map[string]string{"pool": "vg0", "replicas": "2", paramAllowRemoteVolumeAccess: "true"},
		AccessibleTopology: &csi.Topology{Segments: map[string]string{TopologyKeyNode: "n3"}},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(10*giB), resp.MaximumVolumeSize.GetValue(), "bounded by the second-roomiest replica node")
	assert.Equal(t, int64(28*giB), resp.AvailableCapacity)
}

// Without a topology the biggest volume that fits is bounded by the node that
// would hold its last replica, not by the roomiest one.
func TestCapacityWithoutTopologyBoundsTheVolumeByItsReplicas(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	b.pools = []*haifypb.PoolInfo{thinPool("10.0.0.1", 20, 0), thinPool("10.0.0.2", 20, 50), thinPool("10.0.0.3", 20, 90)}
	resp, err := newTestController(b).GetCapacity(context.Background(), &csi.GetCapacityRequest{
		Parameters: map[string]string{"pool": "vg0", "replicas": "2"}})
	require.NoError(t, err)
	assert.Equal(t, int64(10*giB), resp.MaximumVolumeSize.GetValue(), "second-roomiest node: 20GiB at 50%")
}

// ---- list --------------------------------------------------------------------

func provision(t *testing.T, ctrl *controllerServer, name string) {
	t.Helper()
	_, err := ctrl.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: name, Parameters: map[string]string{"pool": "vg0"},
		VolumeCapabilities: []*csi.VolumeCapability{capWith(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, false)},
	})
	require.NoError(t, err)
}

// The cluster's own resources — its control plane among them — must never show
// up as volumes a CO might decide to garbage-collect.
func TestListVolumesReturnsOnlyTheDriversOwnAndPages(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	require.NoError(t, b.CreateResourceWithPoolAndType(context.Background(), "haify-meta", 0, []string{"n1", "n2"}, "C", 1, "vg0", "lvm", nil))
	ctrl := newTestController(b)
	provision(t, ctrl, "pvc-a")
	provision(t, ctrl, "pvc-b")
	provision(t, ctrl, "pvc-c")

	first, err := ctrl.ListVolumes(context.Background(), &csi.ListVolumesRequest{MaxEntries: 2})
	require.NoError(t, err)
	require.Len(t, first.Entries, 2)
	require.NotEmpty(t, first.NextToken)
	rest, err := ctrl.ListVolumes(context.Background(), &csi.ListVolumesRequest{StartingToken: first.NextToken})
	require.NoError(t, err)
	require.Len(t, rest.Entries, 1)
	assert.Empty(t, rest.NextToken)

	var ids []string
	for _, e := range append(first.Entries, rest.Entries...) {
		ids = append(ids, e.Volume.VolumeId)
	}
	assert.Equal(t, []string{"pvc_a", "pvc_b", "pvc_c"}, ids)

	_, err = ctrl.ListVolumes(context.Background(), &csi.ListVolumesRequest{StartingToken: "not-a-token"})
	assert.Equal(t, codes.Aborted, status.Code(err))
}

// The same LV carries the controller's scheduled snapshots. Reporting them
// would hand the CO objects it never created and must never delete.
func TestListSnapshotsReportsOnlyTheDriversOwn(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	ctrl := newTestController(b)
	provision(t, ctrl, "pvc-a")
	b.resources["pvc_a"].Volumes[0].BackingVolume = "pvc_a_data"
	path, _, err := snapshotSource(b.resources["pvc_a"])
	require.NoError(t, err)
	if b.snapshots == nil {
		b.snapshots = map[string][]*haifypb.SnapshotInfo{}
	}
	node := b.resources["pvc_a"].Nodes[0]
	b.snapshots[snapKey(path, node)] = []*haifypb.SnapshotInfo{
		{Name: "haifysnap_snapshot_1"},
		{Name: "pvc_a_data_sched_20260921T010000Z"},
	}

	resp, err := ctrl.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{SourceVolumeId: "pvc_a"})
	require.NoError(t, err)
	require.Len(t, resp.Entries, 1)
	assert.Equal(t, makeSnapshotID("pvc_a", node, "haifysnap_snapshot_1"), resp.Entries[0].Snapshot.SnapshotId)

	byID, err := ctrl.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{SnapshotId: resp.Entries[0].Snapshot.SnapshotId})
	require.NoError(t, err)
	assert.Len(t, byID.Entries, 1)

	all, err := ctrl.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{})
	require.NoError(t, err)
	assert.Len(t, all.Entries, 1)
}

// ---- health ------------------------------------------------------------------

func resourceWith(states map[string]*haifypb.NodeResourceState) *haifypb.ResourceInfo {
	return &haifypb.ResourceInfo{Name: "v", Nodes: []string{"n1", "n2"}, NodeStates: states}
}

func TestHealthCatchesTheReplicaThatReportsNothing(t *testing.T) {
	abnormal, msg := volumeHealth(resourceWith(map[string]*haifypb.NodeResourceState{
		"n1": {Role: "Primary", DiskState: "UpToDate", Node: "n1"},
		// A disconnected peer carries no disk state. Judged on disk state alone
		// it looks fine — the mistake that hid a split brain for a day.
		"n2": {Connection: "StandAlone", Node: "n2"},
	}))
	assert.True(t, abnormal)
	assert.Contains(t, msg, "n2")
	assert.Contains(t, msg, "StandAlone")
}

func TestHealthAcceptsAnExpectedDisklessClient(t *testing.T) {
	r := resourceWith(map[string]*haifypb.NodeResourceState{
		"n1": {Role: "Primary", DiskState: "UpToDate", Node: "n1"},
		"n3": {DiskState: "Diskless", Connection: "Connected", Node: "n3"},
	})
	r.DisklessClients = []string{"n3"}
	abnormal, _ := volumeHealth(r)
	assert.False(t, abnormal)
}

func TestHealthReportsAResyncAsDegraded(t *testing.T) {
	abnormal, msg := volumeHealth(resourceWith(map[string]*haifypb.NodeResourceState{
		"n1": {Role: "Primary", DiskState: "UpToDate", Node: "n1"},
		"n2": {DiskState: "Inconsistent", ReplicationState: "SyncTarget", SyncPercent: 42.5, Connection: "Connected", Node: "n2"},
	}))
	assert.True(t, abnormal)
	assert.Contains(t, msg, "resyncing (42.5%)")
}

// ---- health reporter ---------------------------------------------------------

type healthHarness struct {
	b        *fakeBackend
	rep      *HealthReporter
	recorder *record.FakeRecorder
}

func newHealthHarness(t *testing.T) *healthHarness {
	t.Helper()
	b := newFakeBackend("n1", "n2")
	require.NoError(t, b.CreateResourceWithPoolAndType(context.Background(), "pvc_x", 0, []string{"n1", "n2"}, "C", 1, "vg0", "lvm", nil))
	kube := kubefake.NewSimpleClientset(
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pv-x"},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: DriverName, VolumeHandle: "pvc_x"}},
				ClaimRef:               &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: "default", Name: "data"},
			},
		},
		// Someone else's volume: never the reporter's business.
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pv-other"},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "other.csi", VolumeHandle: "pvc_x"}},
				ClaimRef:               &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: "default", Name: "other"},
			},
		},
	)
	rec := record.NewFakeRecorder(100)
	return &healthHarness{b: b, rep: NewHealthReporter(b, kube, rec, 0, zap.NewNop()), recorder: rec}
}

func (h *healthHarness) setStates(states map[string]*haifypb.NodeResourceState) {
	h.b.resources["pvc_x"].NodeStates = states
}

func (h *healthHarness) drain() []string {
	var out []string
	for {
		select {
		case e := <-h.recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

var healthy = map[string]*haifypb.NodeResourceState{
	"n1": {Role: "Primary", DiskState: "UpToDate", Node: "n1"},
	"n2": {DiskState: "UpToDate", Connection: "Connected", Node: "n2"},
}

func TestReporterPostsOnlyOnTransitions(t *testing.T) {
	h := newHealthHarness(t)
	ctx := context.Background()

	h.setStates(healthy)
	h.rep.Sweep(ctx)
	assert.Empty(t, h.drain(), "a healthy volume seen for the first time is not news")

	h.setStates(map[string]*haifypb.NodeResourceState{
		"n1": {Role: "Primary", DiskState: "UpToDate", Node: "n1"},
		"n2": {Connection: "StandAlone", Node: "n2"},
	})
	h.rep.Sweep(ctx)
	evts := h.drain()
	require.Len(t, evts, 1, "exactly one event, and only for our own driver's PV")
	assert.True(t, strings.HasPrefix(evts[0], "Warning "+ReasonVolumeDegraded), evts[0])
	assert.Contains(t, evts[0], "StandAlone")

	h.rep.Sweep(ctx)
	assert.Empty(t, h.drain(), "the same degradation must not be reposted every sweep")

	h.setStates(healthy)
	h.rep.Sweep(ctx)
	evts = h.drain()
	require.Len(t, evts, 1)
	assert.True(t, strings.HasPrefix(evts[0], "Normal "+ReasonVolumeRecovered), evts[0])
}

// A resync reports a new percentage every sweep. That is progress, not news.
func TestReporterDoesNotRepostResyncProgress(t *testing.T) {
	h := newHealthHarness(t)
	ctx := context.Background()
	resync := func(pct float64) map[string]*haifypb.NodeResourceState {
		return map[string]*haifypb.NodeResourceState{
			"n1": {Role: "Primary", DiskState: "UpToDate", Node: "n1"},
			"n2": {DiskState: "Inconsistent", ReplicationState: "SyncTarget", SyncPercent: pct, Connection: "Connected", Node: "n2"},
		}
	}
	h.setStates(healthy)
	h.rep.Sweep(ctx)
	h.drain()
	h.setStates(resync(10))
	h.rep.Sweep(ctx)
	require.Len(t, h.drain(), 1)
	for _, pct := range []float64{20, 40, 80} {
		h.setStates(resync(pct))
		h.rep.Sweep(ctx)
		assert.Empty(t, h.drain(), "resync at %.0f%% is not a new condition", pct)
	}
}

// The controller being unreachable is not the volume being broken.
func TestReporterSaysNothingWhenTheControllerDoesNotAnswer(t *testing.T) {
	h := newHealthHarness(t)
	h.setStates(healthy)
	h.rep.Sweep(context.Background())
	h.drain()

	delete(h.b.resources, "pvc_x")
	h.rep.Sweep(context.Background())
	assert.Empty(t, h.drain())
}

// From the Primary's side a peer being brought up to date is SyncSource, not
// SyncTarget. Both are a resync and must read as one.
func TestHealthRecognisesAResyncSeenFromThePrimary(t *testing.T) {
	_, msg := volumeHealth(resourceWith(map[string]*haifypb.NodeResourceState{
		"n1": {Role: "Primary", DiskState: "UpToDate", Node: "n1"},
		"n2": {DiskState: "Inconsistent", ReplicationState: "SyncSource", SyncPercent: 12, Connection: "Connected", Node: "n2"},
	}))
	assert.Contains(t, msg, "resyncing (12.0%)")
}

// A new PVC's replica syncs once before it holds anything. That is not a fault,
// and a warning on every freshly created PVC would train people to ignore it.
func TestReporterStaysQuietDuringAVolumesInitialSync(t *testing.T) {
	h := newHealthHarness(t)
	ctx := context.Background()
	h.setStates(map[string]*haifypb.NodeResourceState{
		"n1": {Role: "Primary", DiskState: "UpToDate", Node: "n1"},
		"n2": {DiskState: "Inconsistent", ReplicationState: "SyncSource", SyncPercent: 30, Connection: "Connected", Node: "n2"},
	})
	h.rep.Sweep(ctx)
	assert.Empty(t, h.drain(), "the initial sync of a new volume is not news")

	h.setStates(healthy)
	h.rep.Sweep(ctx)
	assert.Empty(t, h.drain(), "and reaching full redundancy for the first time is not a recovery")

	// Once established, the same resync is a real loss of redundancy.
	h.setStates(map[string]*haifypb.NodeResourceState{
		"n1": {Role: "Primary", DiskState: "UpToDate", Node: "n1"},
		"n2": {DiskState: "Inconsistent", ReplicationState: "SyncSource", SyncPercent: 5, Connection: "Connected", Node: "n2"},
	})
	h.rep.Sweep(ctx)
	evts := h.drain()
	require.Len(t, evts, 1)
	assert.Contains(t, evts[0], ReasonVolumeDegraded)
}

// A disconnect is reported even before the volume has ever been healthy.
func TestReporterReportsADisconnectOnAVolumeNeverSeenHealthy(t *testing.T) {
	h := newHealthHarness(t)
	h.setStates(map[string]*haifypb.NodeResourceState{
		"n1": {Role: "Primary", DiskState: "UpToDate", Node: "n1"},
		"n2": {Connection: "StandAlone", Node: "n2"},
	})
	h.rep.Sweep(context.Background())
	evts := h.drain()
	require.Len(t, evts, 1)
	assert.Contains(t, evts[0], "StandAlone")
}
