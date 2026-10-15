package csi

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestControllerCapabilitiesAndValidation(t *testing.T) {
	b := newFakeBackend("n1")
	require.NoError(t, b.CreateResourceWithPoolAndType(context.Background(), "vol1", 0, []string{"n1"}, "C", 1, "vg0", "lvm", nil))

	ctrl := NewControllerServer(b, zap.NewNop())

	// ControllerGetCapabilities
	caps, err := ctrl.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	require.NoError(t, err)
	var types []csi.ControllerServiceCapability_RPC_Type
	for _, c := range caps.Capabilities {
		types = append(types, c.GetRpc().GetType())
	}
	assert.ElementsMatch(t, []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
		csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
		csi.ControllerServiceCapability_RPC_GET_VOLUME,
		csi.ControllerServiceCapability_RPC_GET_CAPACITY,
		csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
		csi.ControllerServiceCapability_RPC_MODIFY_VOLUME,
	}, types)

	// ValidateVolumeCapabilities
	_, err = ctrl.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{})
	assert.Error(t, err)

	_, err = ctrl.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{VolumeId: "vol1"})
	assert.Error(t, err)

	_, err = ctrl.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "nonexistent",
		VolumeCapabilities: []*csi.VolumeCapability{
			{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}},
		},
	})
	assert.Error(t, err)

	valResp, err := ctrl.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "vol1",
		VolumeCapabilities: []*csi.VolumeCapability{
			{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}},
		},
	})
	require.NoError(t, err)
	assert.NotNil(t, valResp.Confirmed)

	// Single node multi writer (unsupported -> empty Confirmed)
	valResp2, err := ctrl.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "vol1",
		VolumeCapabilities: []*csi.VolumeCapability{
			{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER}},
		},
	})
	require.NoError(t, err)
	assert.Nil(t, valResp2.Confirmed)

	// DeleteVolume
	_, err = ctrl.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{})
	assert.Error(t, err)

	// Idempotent delete for non-existent volume
	_, err = ctrl.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "nonexistent"})
	require.NoError(t, err)

	_, err = ctrl.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "vol1"})
	require.NoError(t, err)
}

func TestNodeCapabilitiesAndUnpublish(t *testing.T) {
	b := newFakeBackend("n1")
	m := newRecordingMounter()
	node := newTestNode(b, m)

	// NodeGetCapabilities
	caps, err := node.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	require.NoError(t, err)
	var nodeTypes []csi.NodeServiceCapability_RPC_Type
	for _, c := range caps.Capabilities {
		nodeTypes = append(nodeTypes, c.GetRpc().GetType())
	}
	assert.ElementsMatch(t, []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
		csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	}, nodeTypes)

	// NodeUnpublishVolume
	_, err = node.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{})
	assert.Error(t, err)

	tmpDir := t.TempDir()
	targetPath := filepath.Join(tmpDir, "mount-target")
	require.NoError(t, os.MkdirAll(targetPath, 0755))

	_, err = node.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{TargetPath: targetPath})
	require.NoError(t, err)
	assert.NoDirExists(t, targetPath)
}

func TestParseEndpoint(t *testing.T) {
	proto, addr, err := parseEndpoint("unix:///tmp/csi.sock")
	require.NoError(t, err)
	assert.Equal(t, "unix", proto)
	assert.Equal(t, "/tmp/csi.sock", addr)

	proto, addr, err = parseEndpoint("tcp://127.0.0.1:10000")
	require.NoError(t, err)
	assert.Equal(t, "tcp", proto)
	assert.Equal(t, "127.0.0.1:10000", addr)

	_, _, err = parseEndpoint("http://invalid")
	assert.Error(t, err)
}

func TestDriverRun(t *testing.T) {
	tmpDir := t.TempDir()
	sock := filepath.Join(tmpDir, "csi.sock")
	ep := "unix://" + sock

	id := NewIdentityServer()
	drv := NewDriver(ep, zap.NewNop(), id, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		errCh <- drv.Run(ctx)
	}()

	require.Eventually(t, func() bool {
		// Closed at once: an open connection that never speaks HTTP/2 holds
		// GracefulStop until gRPC's 120s handshake timeout drops it.
		c, err := net.Dial("unix", sock)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}, 2*time.Second, 50*time.Millisecond)

	cancel()
	err := <-errCh
	require.NoError(t, err)
}

func TestSafeMounterEnsureDir(t *testing.T) {
	m := NewMounter()
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "sub", "dir")
	require.NoError(t, m.EnsureDir(target))
	assert.DirExists(t, target)

	_, _ = m.IsMountPoint(target)
	_ = m.Unmount(target)
}

func TestSanitySupportHelpers(t *testing.T) {
	sb := NewSanityFakeBackend()
	ctx := context.Background()

	require.NoError(t, sb.CreateResourceWithPoolAndType(ctx, "r1", 7001, []string{"n1"}, "C", 10, "vg0", "lvm", nil))
	res, err := sb.GetResource(ctx, "r1")
	require.NoError(t, err)
	assert.Equal(t, "r1", res.Name)

	_, err = sb.RegisterNode(ctx, "n2", "10.0.0.2")
	require.NoError(t, err)

	require.NoError(t, sb.SetPrimary(ctx, "r1", "n1", false))
	require.NoError(t, sb.PromoteForNode(ctx, "r1", "n1"))
	require.NoError(t, sb.SetSecondary(ctx, "r1", "n1"))
	require.NoError(t, sb.AttachDisklessClient(ctx, "r1", "n2"))
	require.NoError(t, sb.DetachDisklessClient(ctx, "r1", "n2"))
	require.NoError(t, sb.ResizeVolume(ctx, "r1", 0, 20))
	require.NoError(t, sb.DeleteResource(ctx, "r1"))

	nm := NewNopMounter()
	require.NoError(t, nm.FormatAndMount("src", "target", "ext4", nil))
	require.NoError(t, nm.Mount("src", "target", "", nil))
	isMnt, err := nm.IsMountPoint("target")
	require.NoError(t, err)
	assert.True(t, isMnt)
	require.NoError(t, nm.EnsureDir("target"))
	require.NoError(t, nm.ResizeFS("src", "target"))
	require.NoError(t, nm.Unmount("target"))
}
