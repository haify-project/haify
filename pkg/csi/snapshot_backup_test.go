package csi

import (
	"context"
	"errors"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSnapshotIDFormats(t *testing.T) {
	// The format every snapshot taken so far carries keeps its meaning.
	old, err := parseSnapshotRef("pvc_abc/n1/haifysnap_s1")
	require.NoError(t, err)
	assert.Equal(t, snapshotRef{resource: "pvc_abc", node: "n1", name: "haifysnap_s1"}, old)
	assert.Equal(t, "pvc_abc/n1/haifysnap_s1", old.id())

	backed, err := parseSnapshotRef("pvc_abc/n1/haifysnap_s1/backup")
	require.NoError(t, err)
	assert.True(t, backed.backedUp)
	assert.Equal(t, "pvc_abc/n1/haifysnap_s1/backup", backed.id())
	assert.Equal(t, "pvc_abc_haifysnap_s1", backed.backupID(), "the controller's id for a backup of that snapshot")

	// The local parts read the same through the old parser.
	res, node, snap, err := parseSnapshotID("pvc_abc/n1/haifysnap_s1/backup")
	require.NoError(t, err)
	assert.Equal(t, []string{"pvc_abc", "n1", "haifysnap_s1"}, []string{res, node, snap})

	for _, bad := range []string{"a/b/c/x", "a/b/c/backup/x", "a/b//backup", "/b/c/backup", "a/b/c/"} {
		_, err := parseSnapshotRef(bad)
		assert.Errorf(t, err, "%q should be rejected", bad)
	}
}

func TestRefFromBackupID(t *testing.T) {
	ref, ok := refFromBackupID("pvc_abc_haifysnap_s1", "pvc_abc", "n1")
	require.True(t, ok)
	assert.Equal(t, "pvc_abc/n1/haifysnap_s1/backup", ref.id())

	_, ok = refFromBackupID("pvc_abc_20261015T080000Z", "pvc_abc", "n1")
	assert.False(t, ok, "a backup taken by hand or by a schedule is not a CSI snapshot")
	_, ok = refFromBackupID("pvc_abc_haifysnap_s1", "pvc_abc", "")
	assert.False(t, ok)
}

func TestParseSnapshotBackupParams(t *testing.T) {
	p, err := parseSnapshotBackupParams(nil)
	require.NoError(t, err)
	assert.Equal(t, snapshotBackupParams{keepLocal: true}, p)

	p, err = parseSnapshotBackupParams(map[string]string{paramBackupTarget: " offsite ",
		"csi.storage.k8s.io/volumesnapshot/name": "x"})
	require.NoError(t, err)
	assert.Equal(t, snapshotBackupParams{target: "offsite", keepLocal: true}, p)

	p, err = parseSnapshotBackupParams(map[string]string{paramBackupTarget: "offsite", paramKeepLocalSnapshot: "false"})
	require.NoError(t, err)
	assert.False(t, p.keepLocal)

	_, err = parseSnapshotBackupParams(map[string]string{paramBackupTarget: "offsite", paramKeepLocalSnapshot: "maybe"})
	assert.Error(t, err)
	_, err = parseSnapshotBackupParams(map[string]string{paramKeepLocalSnapshot: "false"})
	assert.Error(t, err, "dropping the only copy is refused")
}

func backedSnapshotReq(params map[string]string) *csi.CreateSnapshotRequest {
	if params == nil {
		params = map[string]string{paramBackupTarget: "offsite"}
	}
	return &csi.CreateSnapshotRequest{Name: "snapshot-1", SourceVolumeId: "pvc_abc", Parameters: params}
}

func TestCreateBackedSnapshotUploadsThenIsReady(t *testing.T) {
	b := newFakeBackupBackend("n1", "n2")
	seedSnapshotSource(b.fakeBackend, "pvc_abc", "n1", "n2")
	b.gate = make(chan struct{})
	s := newTestController(b)
	ctx := context.Background()

	resp, err := s.CreateSnapshot(ctx, backedSnapshotReq(nil))
	require.NoError(t, err)
	snap := resp.GetSnapshot()
	assert.Equal(t, "pvc_abc/n1/haifysnap_snapshot_1/backup", snap.GetSnapshotId())
	assert.False(t, snap.GetReadyToUse(), "not ready while uploading")
	assert.NotNil(t, snap.GetCreationTime(), "the point in time is the local snapshot")
	assert.Equal(t, []string{"haify_vg0/pvc_abc_data/haifysnap_snapshot_1@n1"}, b.snapCreated)

	// The sidecar calls again while the upload runs: nothing new starts.
	resp, err = s.CreateSnapshot(ctx, backedSnapshotReq(nil))
	require.NoError(t, err)
	assert.False(t, resp.GetSnapshot().GetReadyToUse())
	close(b.gate)
	waitOp(t, s, backupOpKey("pvc_abc_haifysnap_snapshot_1"))
	assert.Equal(t, []string{"pvc_abc/haifysnap_snapshot_1@n1 -> offsite"}, b.uploads, "the upload reads that very snapshot")

	resp, err = s.CreateSnapshot(ctx, backedSnapshotReq(nil))
	require.NoError(t, err)
	assert.True(t, resp.GetSnapshot().GetReadyToUse())
	assert.Equal(t, "pvc_abc/n1/haifysnap_snapshot_1/backup", resp.GetSnapshot().GetSnapshotId(), "the ID never changes")
	assert.Len(t, b.snapCreated, 1)
	assert.Empty(t, b.snapDeleted, "the local snapshot is kept by default")
}

func TestCreateBackedSnapshotDropsLocalWhenAsked(t *testing.T) {
	b := newFakeBackupBackend("n1", "n2")
	seedSnapshotSource(b.fakeBackend, "pvc_abc", "n1", "n2")
	s := newTestController(b)
	ctx := context.Background()
	params := map[string]string{paramBackupTarget: "offsite", paramKeepLocalSnapshot: "false"}

	_, err := s.CreateSnapshot(ctx, backedSnapshotReq(params))
	require.NoError(t, err)
	waitOp(t, s, backupOpKey("pvc_abc_haifysnap_snapshot_1"))
	assert.Empty(t, b.snapDeleted, "never before the upload has finished")

	resp, err := s.CreateSnapshot(ctx, backedSnapshotReq(params))
	require.NoError(t, err)
	assert.True(t, resp.GetSnapshot().GetReadyToUse())
	assert.Equal(t, []string{"haify_vg0/pvc_abc_data/haifysnap_snapshot_1@n1"}, b.snapDeleted)

	// A later call neither takes the snapshot again nor fails.
	resp, err = s.CreateSnapshot(ctx, backedSnapshotReq(params))
	require.NoError(t, err)
	assert.True(t, resp.GetSnapshot().GetReadyToUse())
	assert.Len(t, b.snapCreated, 1)
}

func TestCreateBackedSnapshotUnknownTargetTakesNothing(t *testing.T) {
	b := newFakeBackupBackend("n1", "n2")
	seedSnapshotSource(b.fakeBackend, "pvc_abc", "n1", "n2")
	s := newTestController(b)

	_, err := s.CreateSnapshot(context.Background(), backedSnapshotReq(map[string]string{paramBackupTarget: "nowhere"}))
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "offsite", "the message lists the targets that exist")
	assert.Empty(t, b.snapCreated)
}

func TestCreateBackedSnapshotReportsFailedUploadThenRetries(t *testing.T) {
	b := newFakeBackupBackend("n1", "n2")
	seedSnapshotSource(b.fakeBackend, "pvc_abc", "n1", "n2")
	b.uploadErr = errors.New("target unreachable")
	s := newTestController(b)
	ctx := context.Background()
	key := backupOpKey("pvc_abc_haifysnap_snapshot_1")

	_, err := s.CreateSnapshot(ctx, backedSnapshotReq(nil))
	require.NoError(t, err)
	waitOp(t, s, key)

	_, err = s.CreateSnapshot(ctx, backedSnapshotReq(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target unreachable")

	b.mu.Lock()
	b.uploadErr = nil
	b.mu.Unlock()
	resp, err := s.CreateSnapshot(ctx, backedSnapshotReq(nil))
	require.NoError(t, err, "the next call starts the upload again")
	assert.False(t, resp.GetSnapshot().GetReadyToUse())
	waitOp(t, s, key)
	assert.Equal(t, backupStateCompleted, b.backupState("pvc_abc_haifysnap_snapshot_1"))
	assert.Len(t, b.uploads, 2)
}

func TestDeleteBackedSnapshotRemovesBothCopies(t *testing.T) {
	b := newFakeBackupBackend("n1", "n2")
	seedSnapshotSource(b.fakeBackend, "pvc_abc", "n1", "n2")
	s := newTestController(b)
	ctx := context.Background()
	_, err := s.CreateSnapshot(ctx, backedSnapshotReq(nil))
	require.NoError(t, err)
	waitOp(t, s, backupOpKey("pvc_abc_haifysnap_snapshot_1"))

	_, err = s.DeleteSnapshot(ctx, &csi.DeleteSnapshotRequest{SnapshotId: "pvc_abc/n1/haifysnap_snapshot_1/backup"})
	require.NoError(t, err)
	assert.Equal(t, []string{"pvc_abc_haifysnap_snapshot_1 force=false"}, b.deleted)
	assert.Equal(t, []string{"haify_vg0/pvc_abc_data/haifysnap_snapshot_1@n1"}, b.snapDeleted)

	// Idempotent, and an old-format ID never touches backups.
	_, err = s.DeleteSnapshot(ctx, &csi.DeleteSnapshotRequest{SnapshotId: "pvc_abc/n1/haifysnap_snapshot_1/backup"})
	require.NoError(t, err)
	_, err = s.DeleteSnapshot(ctx, &csi.DeleteSnapshotRequest{SnapshotId: "pvc_abc/n1/haifysnap_other"})
	require.NoError(t, err)
	assert.Len(t, b.deleted, 1)
}

func TestDeleteBackedSnapshotStopsARunningUpload(t *testing.T) {
	b := newFakeBackupBackend("n1", "n2")
	seedSnapshotSource(b.fakeBackend, "pvc_abc", "n1", "n2")
	b.gate = make(chan struct{})
	s := newTestController(b)
	ctx := context.Background()
	_, err := s.CreateSnapshot(ctx, backedSnapshotReq(nil))
	require.NoError(t, err)

	req := &csi.DeleteSnapshotRequest{SnapshotId: "pvc_abc/n1/haifysnap_snapshot_1/backup"}
	_, err = s.DeleteSnapshot(ctx, req)
	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Empty(t, b.snapDeleted, "the snapshot being read is left alone")

	waitOp(t, s, backupOpKey("pvc_abc_haifysnap_snapshot_1"))
	_, err = s.DeleteSnapshot(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, []string{"pvc_abc_haifysnap_snapshot_1 force=true"}, b.deleted, "a failed backup has only its record left")
	assert.Len(t, b.snapDeleted, 1)
}

func TestListSnapshotsAnswersForABackupAlone(t *testing.T) {
	// Another cluster: the source volume never existed here, only the
	// imported backup does.
	b := newFakeBackupBackend("m1")
	b.putBackup(&haifypb.BackupInfo{Id: "pvc_abc_haifysnap_s1", Resource: "pvc_abc", Node: "n1", Target: "offsite",
		State: backupStateCompleted, StartedAt: "2026-10-15T08:00:00Z", Volumes: []*haifypb.BackupVolumeInfo{{Bytes: 2*giB - 4<<20}}})
	s := newTestController(b)

	resp, err := s.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{SnapshotId: "pvc_abc/n1/haifysnap_s1/backup"})
	require.NoError(t, err)
	require.Len(t, resp.GetEntries(), 1)
	snap := resp.GetEntries()[0].GetSnapshot()
	assert.True(t, snap.GetReadyToUse())
	assert.Equal(t, int64(2*giB), snap.GetSizeBytes(), "the source's size, not its DRBD device's")
	assert.Equal(t, "pvc_abc", snap.GetSourceVolumeId())

	all, err := s.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{})
	require.NoError(t, err)
	require.Len(t, all.GetEntries(), 1)
	assert.Equal(t, "pvc_abc/n1/haifysnap_s1/backup", all.GetEntries()[0].GetSnapshot().GetSnapshotId())
}

func TestListSnapshotsMarksBackedLocalSnapshots(t *testing.T) {
	b := newFakeBackupBackend("n1", "n2")
	seedSnapshotSource(b.fakeBackend, "pvc_abc", "n1", "n2")
	b.resources["pvc_abc"].Labels = map[string]string{managedByLabel: managedByValue}
	s := newTestController(b)
	ctx := context.Background()
	_, err := s.CreateSnapshot(ctx, backedSnapshotReq(nil))
	require.NoError(t, err)
	waitOp(t, s, backupOpKey("pvc_abc_haifysnap_snapshot_1"))
	_, err = s.CreateSnapshot(ctx, &csi.CreateSnapshotRequest{Name: "snapshot-2", SourceVolumeId: "pvc_abc"})
	require.NoError(t, err)

	resp, err := s.ListSnapshots(ctx, &csi.ListSnapshotsRequest{SourceVolumeId: "pvc_abc"})
	require.NoError(t, err)
	var ids []string
	for _, e := range resp.GetEntries() {
		ids = append(ids, e.GetSnapshot().GetSnapshotId())
		assert.True(t, e.GetSnapshot().GetReadyToUse())
	}
	assert.Equal(t, []string{"pvc_abc/n1/haifysnap_snapshot_1/backup", "pvc_abc/n1/haifysnap_snapshot_2"}, ids)
}
