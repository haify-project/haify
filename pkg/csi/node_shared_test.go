package csi

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// nfsMounter is a recordingMounter that also knows what is mounted where, as
// the production mounter does.
type nfsMounter struct {
	*recordingMounter
	types       map[string]string   // target -> filesystem type
	sources     map[string]string   // target -> what was mounted there
	options     map[string][]string // target -> mount options
	nfsUnmounts []string
}

func newNFSMounter() *nfsMounter {
	return &nfsMounter{recordingMounter: newRecordingMounter(), types: map[string]string{},
		sources: map[string]string{}, options: map[string][]string{}}
}

func (m *nfsMounter) Mount(source, target, fsType string, options []string) error {
	m.types[target], m.sources[target], m.options[target] = fsType, source, options
	return m.recordingMounter.Mount(source, target, fsType, options)
}

func (m *nfsMounter) FormatAndMount(source, target, fsType string, options []string) error {
	m.types[target] = fsType
	return m.recordingMounter.FormatAndMount(source, target, fsType, options)
}

func (m *nfsMounter) MountType(target string) (string, error) { return m.types[target], nil }

func (m *nfsMounter) UnmountNFS(target string, _ time.Duration) error {
	m.nfsUnmounts = append(m.nfsUnmounts, target)
	delete(m.types, target)
	delete(m.mounted, target)
	return nil
}

func sharedContext() map[string]string {
	return map[string]string{paramSharedFilesystem: sharedNFS, ctxNFSServer: "192.168.50.200",
		ctxNFSExport: "/srv/gateway-exports/pvc_s"}
}

func sharedStageReq(flags ...string) *csi.NodeStageVolumeRequest {
	c := fsCap(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)
	c.GetMount().MountFlags = flags
	return &csi.NodeStageVolumeRequest{VolumeId: "pvc_s", StagingTargetPath: "/stage/pvc_s",
		VolumeCapability: c, VolumeContext: sharedContext()}
}

func newSharedTestNode(b HaifyBackend, m Mounter) *nodeServer {
	n := newTestNode(b, m)
	n.nfsClient = func() error { return nil }
	return n
}

// Staging a shared volume mounts NFS and asks the Haify controller nothing:
// the backend here does not even know the volume, and this node is not a
// Haify node.
func TestNodeStageSharedMountsNFS(t *testing.T) {
	b := newFakeBackend()
	m := newNFSMounter()
	_, err := newSharedTestNode(b, m).NodeStageVolume(context.Background(), sharedStageReq("noatime"))
	require.NoError(t, err)
	assert.Equal(t, "192.168.50.200:/srv/gateway-exports/pvc_s", m.sources["/stage/pvc_s"])
	assert.Equal(t, "nfs", m.types["/stage/pvc_s"])
	assert.Equal(t, []string{"noatime", "vers=4"}, m.options["/stage/pvc_s"])
	assert.Empty(t, m.formatted, "nothing is formatted on the Pod's node")
	assert.Empty(t, b.promoteCalls, "DRBD is not promoted on the Pod's node")
	assert.Empty(t, b.attachCalls, "no diskless client is attached")

	// Staging again finds it mounted.
	_, err = newSharedTestNode(b, m).NodeStageVolume(context.Background(), sharedStageReq())
	require.NoError(t, err)
	assert.Len(t, m.bind, 1)
}

func TestNodeStageSharedReportsMissingNFSClient(t *testing.T) {
	m := newNFSMounter()
	n := newTestNode(newFakeBackend(), m)
	n.nfsClient = func() error { return fmt.Errorf("the NFS client is missing: mount.nfs was not found") }
	_, err := n.NodeStageVolume(context.Background(), sharedStageReq())
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "mount.nfs")
	assert.Contains(t, err.Error(), `node "n1"`)
	assert.Empty(t, m.bind)
}

func TestNodeStageSharedRejectsWhatItCannotMount(t *testing.T) {
	n := newSharedTestNode(newFakeBackend(), newNFSMounter())

	incomplete := sharedStageReq()
	delete(incomplete.VolumeContext, ctxNFSExport)
	_, err := n.NodeStageVolume(context.Background(), incomplete)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), ctxNFSExport)

	block := sharedStageReq()
	block.VolumeCapability = rawCap(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)
	_, err = n.NodeStageVolume(context.Background(), block)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestNodePublishSharedBindsWithoutIOLimits(t *testing.T) {
	b := newFakeBackend()
	m := newNFSMounter()
	n := newSharedTestNode(b, m)
	_, err := n.NodeStageVolume(context.Background(), sharedStageReq())
	require.NoError(t, err)
	_, err = n.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: "pvc_s", StagingTargetPath: "/stage/pvc_s", TargetPath: "/pods/a/pvc_s",
		VolumeCapability: fsCap(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER), VolumeContext: sharedContext()})
	require.NoError(t, err)
	assert.Equal(t, "/stage/pvc_s", m.sources["/pods/a/pvc_s"])
	assert.Equal(t, []string{"bind"}, m.options["/pods/a/pvc_s"])
	assert.Empty(t, n.qos.records, "a shared volume is not tracked for I/O limits")
}

// The node serving a shared volume's gateway can also run one of its Pods.
// Unstaging the Pod's mount there must not demote the resource: that is the
// NFS service every other node is using.
func TestNodeUnstageSharedLeavesDRBDAlone(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	b.resources["pvc_s"] = &haifypb.ResourceInfo{Name: "pvc_s", Nodes: []string{"n1", "n2"},
		Labels: map[string]string{sharedLabel: sharedNFS}}
	b.primary["pvc_s"] = "n1"
	m := newNFSMounter()
	n := newSharedTestNode(b, m)
	_, err := n.NodeStageVolume(context.Background(), sharedStageReq())
	require.NoError(t, err)

	unstage := &csi.NodeUnstageVolumeRequest{VolumeId: "pvc_s", StagingTargetPath: "/stage/pvc_s"}
	_, err = n.NodeUnstageVolume(context.Background(), unstage)
	require.NoError(t, err)
	assert.Equal(t, []string{"/stage/pvc_s"}, m.nfsUnmounts)
	assert.Equal(t, "n1", b.primary["pvc_s"], "the gateway keeps the resource Primary")
	assert.Empty(t, b.detachCalls)

	// Retried after the unmount: nothing is mounted to tell what it was, so
	// the volume's label decides, and DRBD is still left alone.
	_, err = n.NodeUnstageVolume(context.Background(), unstage)
	require.NoError(t, err)
	assert.Equal(t, "n1", b.primary["pvc_s"])
}

// A mounter that cannot say what is mounted leaves the label to decide.
func TestNodeUnstageSharedWithoutMountTypes(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	b.resources["pvc_s"] = &haifypb.ResourceInfo{Name: "pvc_s", Nodes: []string{"n1", "n2"},
		Labels: map[string]string{sharedLabel: sharedNFS}}
	b.primary["pvc_s"] = "n2"
	m := newRecordingMounter()
	m.mounted["/stage/pvc_s"] = true
	_, err := newSharedTestNode(b, m).NodeUnstageVolume(context.Background(),
		&csi.NodeUnstageVolumeRequest{VolumeId: "pvc_s", StagingTargetPath: "/stage/pvc_s"})
	require.NoError(t, err)
	assert.Equal(t, []string{"/stage/pvc_s"}, m.unmounted)
	assert.Equal(t, "n2", b.primary["pvc_s"])
}

// A volume staged from its DRBD device is unstaged as before: unmounted and
// demoted.
func TestNodeUnstageOrdinaryVolumeStillDemotes(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	require.NoError(t, b.CreateResourceWithPoolAndType(context.Background(), "pvc_x", 0, []string{"n1", "n2"}, "C", 1, "vg0", "lvm", nil))
	m := newNFSMounter()
	n := newSharedTestNode(b, m)
	_, err := n.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{VolumeId: "pvc_x",
		StagingTargetPath: "/stage/pvc_x", VolumeCapability: fsCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)})
	require.NoError(t, err)
	assert.Equal(t, "n1", b.primary["pvc_x"])
	assert.Equal(t, "ext4", m.types["/stage/pvc_x"])

	_, err = n.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{VolumeId: "pvc_x", StagingTargetPath: "/stage/pvc_x"})
	require.NoError(t, err)
	assert.Equal(t, []string{"/stage/pvc_x"}, m.unmounted)
	assert.Empty(t, m.nfsUnmounts)
	assert.NotContains(t, b.primary, "pvc_x", "demoted")
}

func TestNFSSourceAndOptions(t *testing.T) {
	assert.Equal(t, "10.0.0.5:/srv/x", nfsSource("10.0.0.5", "/srv/x"))
	assert.Equal(t, "[fd00::5]:/srv/x", nfsSource("fd00::5", "/srv/x"))

	assert.Equal(t, []string{"vers=4"}, nfsMountOptions(nil))
	assert.Equal(t, []string{"hard", "nfsvers=4.1"}, nfsMountOptions([]string{"hard", " nfsvers=4.1 "}))
	assert.Equal(t, []string{"vers=3", "nolock"}, nfsMountOptions([]string{"vers=3", "nolock"}))
}
