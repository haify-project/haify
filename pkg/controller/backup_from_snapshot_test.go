package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
)

// fromSnapshotFixture is the backup fixture with thin volumes, so a backup that
// took its own snapshot, or kept one as a base, would show it.
func fromSnapshotFixture(t *testing.T, stub *backupExecStub) (*Controller, *[]string) {
	t.Helper()
	ctrl := newBackupFixture(t, stub, "Secondary")
	dep, ok := ctrl.deployment.(*fakeDeploymentClient)
	require.True(t, ok)
	var taken []string
	dep.lvIsThinFunc = func(context.Context, string, string, string) (bool, error) { return true, nil }
	dep.lvCreateThinSnapshotFunc = func(_ context.Context, hosts []string, _, _, name string) (*deployment.ExecResult, error) {
		taken = append(taken, name)
		return successExecResult(hosts, ""), nil
	}
	return ctrl, &taken
}

func TestBackupFromSnapshotReadsThatSnapshot(t *testing.T) {
	stub := &backupExecStub{storedBytes: backupVolumeBytes}
	ctrl, taken := fromSnapshotFixture(t, stub)

	rec, err := ctrl.backups.CreateBackupFromSnapshot(context.Background(), "data", "offsite", "10.0.0.1", "haifysnap_s1")
	require.NoError(t, err)

	assert.Equal(t, "data_haifysnap_s1", rec.ID, "the caller names the backup before it finishes")
	assert.Equal(t, database.BackupStateCompleted, rec.State)
	assert.Equal(t, database.BackupKindFull, rec.Kind, "every backup of a snapshot is deletable on its own")
	assert.Equal(t, "10.0.0.1", rec.Node)
	require.Len(t, stub.uploads, 1)
	assert.Contains(t, stub.uploads[0], "SRC='\\''/dev/vg0/haifysnap_s1'\\''", "the image is the caller's snapshot")
	assert.Empty(t, *taken, "no snapshot of its own")
	require.Len(t, rec.Volumes, 1)
	assert.Empty(t, rec.Volumes[0].Snapshot, "no base is kept, so nothing can be built on it")
}

func TestBackupFromSnapshotRefusals(t *testing.T) {
	stub := &backupExecStub{storedBytes: backupVolumeBytes}
	ctrl, _ := fromSnapshotFixture(t, stub)
	ctx := context.Background()

	_, err := ctrl.backups.CreateBackupFromSnapshot(ctx, "data", "offsite", "", "haifysnap_s1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs the node")

	_, err = ctrl.backups.CreateBackupFromSnapshot(ctx, "data", "offsite", "10.0.0.1", "x; rm -rf /")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid snapshot name")
	assert.Empty(t, stub.uploads)
}

func TestBackupFromSnapshotRetriesUnderTheSameID(t *testing.T) {
	stub := &backupExecStub{storedBytes: backupVolumeBytes, uploadFails: true}
	ctrl, _ := fromSnapshotFixture(t, stub)
	ctx := context.Background()

	_, err := ctrl.backups.CreateBackupFromSnapshot(ctx, "data", "offsite", "10.0.0.1", "haifysnap_s1")
	require.Error(t, err)
	failed, err := ctrl.db.GetBackup(ctx, "data_haifysnap_s1")
	require.NoError(t, err)
	assert.Equal(t, database.BackupStateFailed, failed.State)

	stub.uploadFails = false
	rec, err := ctrl.backups.CreateBackupFromSnapshot(ctx, "data", "offsite", "10.0.0.1", "haifysnap_s1")
	require.NoError(t, err, "a failed backup is no backup: its id is free again")
	assert.Equal(t, database.BackupStateCompleted, rec.State)

	_, err = ctrl.backups.CreateBackupFromSnapshot(ctx, "data", "offsite", "10.0.0.1", "haifysnap_s1")
	require.Error(t, err, "a completed one is not overwritten")
	assert.Contains(t, err.Error(), "already exists")
}

func TestBackupFromSnapshotLeavesTheChainBaseAlone(t *testing.T) {
	stub := &backupExecStub{storedBytes: backupVolumeBytes}
	ctrl, _ := fromSnapshotFixture(t, stub)
	ctx := context.Background()

	first, err := ctrl.backups.CreateBackup(ctx, "data", "offsite", "10.0.0.1", false)
	require.NoError(t, err)
	require.NotEmpty(t, first.Volumes[0].Snapshot, "an ordinary thin backup keeps its base")

	_, err = ctrl.backups.CreateBackupFromSnapshot(ctx, "data", "offsite", "10.0.0.1", "haifysnap_s1")
	require.NoError(t, err)

	again, err := ctrl.db.GetBackup(ctx, first.ID)
	require.NoError(t, err)
	assert.Equal(t, first.Volumes[0].Snapshot, again.Volumes[0].Snapshot, "the chain's base survives")
}
