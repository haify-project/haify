package csi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rwxBlockStage(res, staging string) *csi.NodeStageVolumeRequest {
	return &csi.NodeStageVolumeRequest{
		VolumeId:          res,
		StagingTargetPath: staging,
		VolumeCapability:  capWith(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, true),
	}
}

func setRoles(b *fakeBackend, res string, roles map[string]string) {
	states := map[string]*haifypb.NodeResourceState{}
	for node, role := range roles {
		states["host-"+node] = &haifypb.NodeResourceState{Node: node, Role: role}
	}
	b.resources[res].NodeStates = states
}

// The target of a live migration stages while the source is still Primary:
// two Primaries are allowed before the promote, and the target's unstage, once
// the source is gone, disallows them again.
func TestRWXBlockStageOpensDualPrimaryDuringMigration(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	require.NoError(t, b.CreateResourceWithPoolAndType(context.Background(), "pvc_vm", 0, []string{"n1", "n2"}, "C", 1, "vg0", "lvm", nil))
	setRoles(b, "pvc_vm", map[string]string{"n2": "Primary", "n1": "Secondary"})
	staging := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, os.MkdirAll(staging, 0o755))
	node := newTestNode(b, newRecordingMounter())

	_, err := node.NodeStageVolume(context.Background(), rwxBlockStage("pvc_vm", staging))
	require.NoError(t, err)
	assert.Equal(t, []string{"pvc_vm=true"}, b.dualPrimaryCalls, "opened before the promote")
	assert.Equal(t, []string{"pvc_vm"}, b.promoteCalls)
	assert.FileExists(t, filepath.Join(staging, multiWriterMarker))

	// The source still Primary: unstaging here must not close the window yet.
	setRoles(b, "pvc_vm", map[string]string{"n2": "Primary", "n1": "Primary"})
	b.resources["pvc_vm"].NodeStates["host-n1"].Role = "Secondary" // this node demoted
	b.resources["pvc_vm"].NodeStates["host-n3"] = &haifypb.NodeResourceState{Node: "n3", Role: "Primary"}
	_, err = node.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{VolumeId: "pvc_vm", StagingTargetPath: staging})
	require.NoError(t, err)
	assert.Equal(t, []string{"pvc_vm=true"}, b.dualPrimaryCalls, "two Primaries left: stays open")
	assert.NoFileExists(t, filepath.Join(staging, multiWriterMarker), "marker removed so kubelet can remove the directory")
}

// The source of a migration unstages after the guest moved: one Primary left,
// so two Primaries are disallowed.
func TestRWXBlockUnstageClosesDualPrimaryWhenOnePrimaryLeft(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	require.NoError(t, b.CreateResourceWithPoolAndType(context.Background(), "pvc_vm", 0, []string{"n1", "n2"}, "C", 1, "vg0", "lvm", nil))
	setRoles(b, "pvc_vm", map[string]string{"n1": "Secondary", "n2": "Secondary"})
	staging := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, os.MkdirAll(staging, 0o755))
	node := newTestNode(b, newRecordingMounter())

	_, err := node.NodeStageVolume(context.Background(), rwxBlockStage("pvc_vm", staging))
	require.NoError(t, err)
	assert.Empty(t, b.dualPrimaryCalls, "first attach of the VM: single Primary, nothing opened")

	setRoles(b, "pvc_vm", map[string]string{"n1": "Secondary", "n2": "Primary"})
	_, err = node.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{VolumeId: "pvc_vm", StagingTargetPath: staging})
	require.NoError(t, err)
	assert.Equal(t, []string{"pvc_vm=false"}, b.dualPrimaryCalls)
}

// ReadWriteOnce volumes never touch dual-primary.
func TestRWOStageNeverOpensDualPrimary(t *testing.T) {
	b := newFakeBackend("n1", "n2")
	require.NoError(t, b.CreateResourceWithPoolAndType(context.Background(), "pvc_db", 0, []string{"n1", "n2"}, "C", 1, "vg0", "lvm", nil))
	setRoles(b, "pvc_db", map[string]string{"n2": "Primary"})
	staging := t.TempDir()
	node := newTestNode(b, newRecordingMounter())
	req := rwxBlockStage("pvc_db", staging)
	req.VolumeCapability = capWith(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, true)
	_, err := node.NodeStageVolume(context.Background(), req)
	require.NoError(t, err)
	_, err = node.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{VolumeId: "pvc_db", StagingTargetPath: staging})
	require.NoError(t, err)
	assert.Empty(t, b.dualPrimaryCalls)
}
