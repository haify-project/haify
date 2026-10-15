package csi

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A copy of an encrypted volume must be encrypted too. Asking for a plaintext
// one is refused before anything happens: no temporary snapshot of the source,
// no new volume, no copy.
func TestCloneOfEncryptedVolumeIntoPlaintextRefused(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	seedSnapshotSource(b, "pvc_src", "n1", "n2")
	b.resources["pvc_src"].Encrypted = true
	_, err := newTestController(b).CreateVolume(context.Background(), cloneReq("pvc-copy", "pvc_src"))
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), `encryption: "true"`)
	assert.Empty(t, b.snapCreated, "no temporary snapshot of the source is taken")
	assert.Empty(t, b.createCalls)
	assert.Empty(t, b.populated)
}

func TestRestoreOfEncryptedSnapshotIntoPlaintextRefused(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	seedSnapshotSource(b, "pvc_src", "n2", "n3")
	b.resources["pvc_src"].Encrypted = true
	_, err := newTestController(b).CreateVolume(context.Background(), restoreReq("pvc-new", "pvc_src/n3/haifysnap_s1"))
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Empty(t, b.createCalls)
	assert.Empty(t, b.populated)
}

// An encrypted clone of an encrypted volume is created encrypted and filled;
// the controller reads the source through its LUKS container.
func TestCloneOfEncryptedVolumeIntoEncrypted(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	seedSnapshotSource(b, "pvc_src", "n1", "n2")
	b.resources["pvc_src"].Encrypted = true
	resp, err := newTestController(b).CreateVolume(context.Background(),
		withParams(cloneReq("pvc-copy", "pvc_src"), "encryption", "true"))
	require.NoError(t, err)
	require.Len(t, b.requestCalls, 1)
	assert.True(t, b.requestCalls[0].GetEncrypt())
	require.Len(t, b.populated, 1)
	assert.Equal(t, "true", resp.GetVolume().GetVolumeContext()["encryption"])
}

// A plaintext volume may be copied into an encrypted one: the copy is written
// through the new volume's DRBD device and encrypted on the way in.
func TestRestoreOfPlaintextSnapshotIntoEncrypted(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	seedSnapshotSource(b, "pvc_src", "n2", "n3")
	_, err := newTestController(b).CreateVolume(context.Background(),
		withParams(restoreReq("pvc-new", "pvc_src/n3/haifysnap_s1"), "encryption", "true"))
	require.NoError(t, err)
	require.Len(t, b.requestCalls, 1)
	assert.True(t, b.requestCalls[0].GetEncrypt())
	require.Len(t, b.populated, 1)
}

// With replicationTLS the links are encrypted before the copy, so the data of
// a clone never crosses the network in the clear.
func TestCloneWithReplicationTLSEncryptsLinksBeforeCopy(t *testing.T) {
	b := newTLSFakeBackend("n1", "n2", "n3")
	seedSnapshotSource(b.fakeBackend, "pvc_src", "n1", "n2")
	_, err := newTestController(b).CreateVolume(context.Background(),
		withParams(cloneReq("pvc-copy", "pvc_src"), "replicationTLS", "true"))
	require.NoError(t, err)
	assert.Equal(t, []string{"pvc_copy=true populated=0"}, b.setCalls)
	require.Len(t, b.populated, 1)
}
