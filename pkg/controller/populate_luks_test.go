package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haify-project/haify/pkg/database"
)

// encryptedPopulateFixture is populateSourceFixture with "src" recorded as an
// encrypted resource, and a target "dst" whose encryption is dstEncrypted.
func encryptedPopulateFixture(t *testing.T, dstEncrypted bool, failOn string) (*Controller, *[]string) {
	t.Helper()
	ctrl, cmds := populateSourceFixture(t, "haify_vg0", "/dev/mapper/haify_haify_vg0_src_data", failOn)
	ctx := context.Background()
	require.NoError(t, ctrl.db.SaveResource(ctx, &database.Resource{Name: "src", Port: 7000, Nodes: "10.0.0.1,10.0.0.2", Encrypted: true}))
	require.NoError(t, ctrl.db.SaveResource(ctx, &database.Resource{Name: "dst", Port: 7001, Nodes: "10.0.0.1,10.0.0.2", Encrypted: dstEncrypted}))
	return ctrl, cmds
}

// A snapshot of an encrypted volume is ciphertext, so it is read through a
// read-only crypt mapping opened with the node's key file, and the mapping is
// closed again afterwards.
func TestResolveSnapshotSourceEncryptedOpensThroughLUKS(t *testing.T) {
	ctrl, cmds := encryptedPopulateFixture(t, true, "")
	dev, release, err := ctrl.snapshots.resolveSnapshotSource(context.Background(), "10.0.0.1", "n1", "dst", 0,
		SnapshotSource{Resource: "src", Name: "before", Replicated: true})
	require.NoError(t, err)
	mapping := populateLUKSMapping("dst", 0)
	assert.Equal(t, "/dev/mapper/"+mapping, dev)
	require.Len(t, *cmds, 2)
	open := (*cmds)[1]
	assert.Contains(t, open, "cryptsetup open --type luks --readonly --key-file /etc/haify/luks/haify_haify_vg0_src_data.key "+
		"/dev/haify_vg0/src_data_snap_before "+mapping)
	assert.Contains(t, open, "lvchange -ay -K haify_vg0/src_data_snap_before", "a thin snapshot is activated before it is opened")
	assert.NotContains(t, open, "$", "no shell variables: dispatch's quoting must not change the command")
	assert.Less(t, strings.Index(open, "cryptsetup close"), strings.Index(open, "cryptsetup open"),
		"a mapping left by an earlier copy goes first")

	release()
	require.Len(t, *cmds, 3)
	assert.Contains(t, (*cmds)[2], "cryptsetup close "+mapping)
}

// A node without the key cannot read the snapshot; the half-open mapping is
// cleaned up and the error says what failed.
func TestResolveSnapshotSourceEncryptedOpenFails(t *testing.T) {
	ctrl, cmds := encryptedPopulateFixture(t, true, "cryptsetup open")
	_, _, err := ctrl.snapshots.resolveSnapshotSource(context.Background(), "10.0.0.1", "n1", "dst", 0,
		SnapshotSource{Resource: "src", Name: "before", Replicated: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open encrypted snapshot /dev/haify_vg0/src_data_snap_before")
	require.Len(t, *cmds, 3)
	assert.Contains(t, (*cmds)[2], "cryptsetup close", "the mapping is closed on failure")
}

// The copy reads the plaintext mapping, never the raw ciphertext snapshot, and
// the mapping stays open until the copy is done.
func TestPopulateVolumeFromEncryptedSnapshot(t *testing.T) {
	ctrl, cmds := encryptedPopulateFixture(t, true, "")
	_, err := ctrl.snapshots.PopulateVolumeFromSnapshot(context.Background(), "dst", 0,
		SnapshotSource{Resource: "src", Name: "before", Replicated: true}, "10.0.0.1")
	require.NoError(t, err)
	joined := strings.Join(*cmds, "\n")
	mapping := populateLUKSMapping("dst", 0)
	opened := strings.Index(joined, "cryptsetup open --type luks --readonly")
	copied := strings.Index(joined, "dd if=/dev/mapper/"+mapping+" of=/dev/drbd/by-res/dst/0")
	closed := strings.LastIndex(joined, "cryptsetup close "+mapping+" 2>/dev/null")
	require.True(t, opened >= 0 && copied > opened && closed > copied, joined)
	assert.NotContains(t, joined, "dd if=/dev/haify_vg0/src_data_snap_before", "the ciphertext is never copied")
}

// An encrypted volume is never copied into an unencrypted one, and nothing is
// promoted or read before that is decided.
func TestPopulateRefusesPlaintextCopyOfEncryptedSource(t *testing.T) {
	ctrl, cmds := encryptedPopulateFixture(t, false, "")
	_, err := ctrl.snapshots.PopulateVolumeFromSnapshot(context.Background(), "dst", 0,
		SnapshotSource{Resource: "src", Name: "before", Replicated: true}, "10.0.0.1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "src is encrypted and dst is not")
	assert.Empty(t, *cmds)
}

// A plaintext source may fill an encrypted target: the copy is written
// through the target's DRBD device and so encrypted under the target's keys.
func TestPopulatePlaintextSourceIntoEncryptedTarget(t *testing.T) {
	ctrl, cmds := populateSourceFixture(t, "haify_vg0", "/dev/haify_vg0/src_data", "")
	require.NoError(t, ctrl.db.SaveResource(context.Background(),
		&database.Resource{Name: "dst", Port: 7001, Nodes: "10.0.0.1,10.0.0.2", Encrypted: true}))
	_, err := ctrl.snapshots.PopulateVolumeFromSnapshot(context.Background(), "dst", 0,
		SnapshotSource{Resource: "src", Name: "before", Replicated: true}, "10.0.0.1")
	require.NoError(t, err)
	joined := strings.Join(*cmds, "\n")
	assert.Contains(t, joined, "dd if=/dev/haify_vg0/src_data_snap_before of=/dev/drbd/by-res/dst/0")
	assert.NotContains(t, joined, "cryptsetup")
}
