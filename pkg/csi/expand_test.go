package csi

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type expandFakeBackend struct {
	fakeBackend
	resizedVolume string
	resizedSizeGB uint32
	resizeErr     error
}

func (f *expandFakeBackend) ResizeVolume(_ context.Context, resource string, volumeID uint32, sizeGB uint32) error {
	if f.resizeErr != nil {
		return f.resizeErr
	}
	f.resizedVolume = resource
	f.resizedSizeGB = sizeGB
	return nil
}

func TestControllerExpandVolume(t *testing.T) {
	b := &expandFakeBackend{fakeBackend: *newFakeBackend("n1")}
	ctrl := NewControllerServer(b, zap.NewNop())

	// 1. Missing volume ID
	_, err := ctrl.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{})
	assert.Error(t, err)

	// 2. Successful expansion
	req := &csi.ControllerExpandVolumeRequest{
		VolumeId: "res1",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 10 * giB,
		},
	}
	resp, err := ctrl.ControllerExpandVolume(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, int64(10*giB), resp.CapacityBytes)
	assert.True(t, resp.NodeExpansionRequired)
	assert.Equal(t, "res1", b.resizedVolume)
	assert.Equal(t, uint32(10), b.resizedSizeGB)
}

type expandMounter struct {
	recordingMounter
	resizedDevice string
	resizedMount  string
}

func (m *expandMounter) ResizeFS(devicePath, mountPath string) error {
	m.resizedDevice = devicePath
	m.resizedMount = mountPath
	return nil
}

func TestNodeExpandVolume(t *testing.T) {
	b := newFakeBackend("n1")
	require.NoError(t, b.CreateResourceWithPoolAndType(context.Background(), "res1", 0, []string{"n1"}, "C", 1, "vg0", "lvm", nil))

	// Ensure GetResource returns device path
	res, err := b.GetResource(context.Background(), "res1")
	require.NoError(t, err)
	res.Volumes = []*haifypb.VolumeInfo{{VolumeId: 0, Device: "/dev/drbd100"}}

	m := &expandMounter{recordingMounter: *newRecordingMounter()}
	node := newTestNode(b, m)

	// 1. Missing volume ID
	_, err = node.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{})
	assert.Error(t, err)

	// 2. Success
	req := &csi.NodeExpandVolumeRequest{
		VolumeId:   "res1",
		VolumePath: "/stage/res1",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 20 * giB,
		},
	}
	resp, err := node.NodeExpandVolume(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, int64(20*giB), resp.CapacityBytes)
	assert.Equal(t, "/dev/drbd100", m.resizedDevice)
	assert.Equal(t, "/stage/res1", m.resizedMount)
}

// retryLater is how the Haify client marks a resize whose volume grew and
// whose filesystem could not be.
type retryLater struct{ msg string }

func (e retryLater) Error() string   { return e.msg }
func (e retryLater) Retryable() bool { return true }

// A shared volume expands in the controller call alone: the Haify controller
// grows the filesystem on the node serving the NFS gateway, and no node is
// asked to.
func TestExpandSharedVolume(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	ctrl := newTestController(b)
	_, err := ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-grow"))
	require.NoError(t, err)
	_, err = ctrl.CreateVolume(context.Background(), validCreateReq("pvc-rwo"))
	require.NoError(t, err)

	resp, err := ctrl.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "pvc_grow", CapacityRange: &csi.CapacityRange{RequiredBytes: 10 * giB}})
	require.NoError(t, err)
	assert.Equal(t, int64(10*giB), resp.CapacityBytes)
	assert.False(t, resp.NodeExpansionRequired, "the Pods' nodes mount NFS; there is nothing for them to grow")
	assert.Equal(t, []string{"pvc_grow/0=10Gi"}, b.resizes)

	resp, err = ctrl.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "pvc_rwo", CapacityRange: &csi.CapacityRange{RequiredBytes: 10 * giB}})
	require.NoError(t, err, "any other volume expands as before")
	assert.True(t, resp.NodeExpansionRequired)
}

// While the gateway is failing over the filesystem cannot be grown. That is
// an error the resizer retries, never a success at a size the Pods do not
// see; the retry finds the device grown and finishes.
func TestExpandSharedVolumeDuringFailover(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	ctrl := newTestController(b)
	_, err := ctrl.CreateVolume(context.Background(), sharedCreateReq("pvc-grow"))
	require.NoError(t, err)
	req := &csi.ControllerExpandVolumeRequest{VolumeId: "pvc_grow", CapacityRange: &csi.CapacityRange{RequiredBytes: 10 * giB}}

	b.resizeErr = fmt.Errorf("resize: %w", retryLater{"the volume of pvc_grow is grown, but the filesystem was not grown: pvc_grow is Primary on no node"})
	_, err = ctrl.ControllerExpandVolume(context.Background(), req)
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Contains(t, err.Error(), "Primary on no node")

	b.resizeErr = errors.New("pool haify_vg0 on n2 has 1.0 GiB free")
	_, err = ctrl.ControllerExpandVolume(context.Background(), req)
	assert.Equal(t, codes.Internal, status.Code(err), "a failure that waiting does not fix stays one")

	b.resizeErr = nil
	resp, err := ctrl.ControllerExpandVolume(context.Background(), req)
	require.NoError(t, err)
	assert.False(t, resp.NodeExpansionRequired)
	assert.Len(t, b.resizes, 3)
}

// A node asked to expand a shared volume has an NFS mount and nothing to
// grow: it says so, and never looks for a DRBD device that is not there.
func TestNodeExpandSharedVolumeDoesNothing(t *testing.T) {
	b := newFakeBackend("n1")
	m := newNFSMounter()
	node := newSharedTestNode(b, m)
	_, err := node.NodeStageVolume(context.Background(), sharedStageReq())
	require.NoError(t, err)

	for _, req := range []*csi.NodeExpandVolumeRequest{
		{VolumeId: "pvc_s", VolumePath: "/stage/pvc_s"},
		{VolumeId: "pvc_s", VolumePath: "/pods/x/mount", StagingTargetPath: "/stage/pvc_s"},
	} {
		req.CapacityRange = &csi.CapacityRange{RequiredBytes: 20 * giB}
		resp, err := node.NodeExpandVolume(context.Background(), req)
		require.NoError(t, err)
		assert.Equal(t, int64(20*giB), resp.CapacityBytes)
	}
}
