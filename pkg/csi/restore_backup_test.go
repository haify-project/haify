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

func TestChooseRestoreSource(t *testing.T) {
	completed := &haifypb.BackupInfo{Id: "b", State: backupStateCompleted}
	cases := []struct {
		name   string
		local  bool
		backup *haifypb.BackupInfo
		want   restoreSource
		code   codes.Code
	}{
		{"local snapshot wins", true, completed, restoreFromLocal, codes.OK},
		{"local snapshot without a record", true, nil, restoreFromLocal, codes.OK},
		{"backup when the local one is gone", false, completed, restoreFromBackup, codes.OK},
		{"nothing at all", false, nil, 0, codes.NotFound},
		{"upload not finished", false, &haifypb.BackupInfo{Id: "b", State: backupStateRunning}, 0, codes.Unavailable},
		{"upload failed", false, &haifypb.BackupInfo{Id: "b", State: backupStateFailed}, 0, codes.FailedPrecondition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chooseRestoreSource(tc.local, tc.backup, "pvc_abc/n1/haifysnap_s1/backup")
			assert.Equal(t, tc.code, status.Code(err))
			assert.Equal(t, tc.want, got)
		})
	}
}

func backupRestoreReq(name string) *csi.CreateVolumeRequest {
	req := validCreateReq(name)
	req.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
		Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "pvc_abc/n1/haifysnap_s1/backup"},
	}}
	return req
}

func importedBackup() *haifypb.BackupInfo {
	return &haifypb.BackupInfo{Id: "pvc_abc_haifysnap_s1", Resource: "pvc_abc", Node: "n1", Target: "offsite",
		State: backupStateCompleted, StartedAt: "2026-10-15T08:00:00Z", Volumes: []*haifypb.BackupVolumeInfo{{Bytes: 2*giB - 4<<20}}}
}

func TestRestorePrefersTheLocalSnapshot(t *testing.T) {
	b := newFakeBackupBackend("n1", "n2")
	seedSnapshotSource(b.fakeBackend, "pvc_abc", "n1", "n2")
	_ = b.CreateSnapshot(context.Background(), "haify_vg0/pvc_abc_data", "haifysnap_s1", "n1")
	b.putBackup(importedBackup())
	s := newTestController(b)

	resp, err := s.CreateVolume(context.Background(), backupRestoreReq("pvc-new"))
	require.NoError(t, err)
	assert.Equal(t, "pvc_new", resp.GetVolume().GetVolumeId())
	require.Len(t, b.populated, 1)
	assert.Contains(t, b.populated[0], "on n1", "copied locally on the snapshot's node")
	assert.Empty(t, b.restores)
}

func TestRestoreFromBackupRunsInTheBackground(t *testing.T) {
	// The source volume is gone (or was never in this cluster).
	b := newFakeBackupBackend("m1", "m2")
	b.putBackup(importedBackup())
	b.gate = make(chan struct{})
	s := newTestController(b)
	ctx := context.Background()

	_, err := s.CreateVolume(ctx, backupRestoreReq("pvc-new"))
	require.Error(t, err)
	assert.Equal(t, codes.Aborted, status.Code(err), "the provisioner calls again")
	require.Contains(t, b.resources, "pvc_new")
	assert.Equal(t, "pvc_abc_haifysnap_s1", b.resources["pvc_new"].GetLabels()[restoringLabel])
	assert.Empty(t, b.populated)

	_, err = s.CreateVolume(ctx, backupRestoreReq("pvc-new"))
	assert.Equal(t, codes.Aborted, status.Code(err), "never handed out while it is being written")

	close(b.gate)
	waitOp(t, s, restoreOpKey("pvc_new"))
	resp, err := s.CreateVolume(ctx, backupRestoreReq("pvc-new"))
	require.NoError(t, err)
	assert.Equal(t, "pvc_new", resp.GetVolume().GetVolumeId())
	assert.NotNil(t, resp.GetVolume().GetContentSource())
	assert.NotContains(t, b.resources["pvc_new"].GetLabels(), restoringLabel)
	assert.Equal(t, []string{"pvc_abc_haifysnap_s1 -> pvc_new"}, b.restores)
}

func TestRestoreFromBackupFailureStartsOver(t *testing.T) {
	b := newFakeBackupBackend("m1", "m2")
	b.putBackup(importedBackup())
	b.restoreErr = errors.New("rclone: 403")
	s := newTestController(b)
	ctx := context.Background()

	_, err := s.CreateVolume(ctx, backupRestoreReq("pvc-new"))
	require.Equal(t, codes.Aborted, status.Code(err))
	waitOp(t, s, restoreOpKey("pvc_new"))

	_, err = s.CreateVolume(ctx, backupRestoreReq("pvc-new"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rclone: 403")
	assert.NotContains(t, b.resources, "pvc_new", "the half-written volume is gone")
}

func TestInterruptedRestoreIsDiscarded(t *testing.T) {
	// A volume left with the label by a process that is gone.
	b := newFakeBackupBackend("m1", "m2")
	b.resources["pvc_new"] = &haifypb.ResourceInfo{Name: "pvc_new", Nodes: []string{"m1", "m2"},
		Labels: map[string]string{restoringLabel: "pvc_abc_haifysnap_s1"}}
	s := newTestController(b)

	_, err := s.CreateVolume(context.Background(), backupRestoreReq("pvc-new"))
	require.Error(t, err)
	assert.Equal(t, codes.Aborted, status.Code(err))
	assert.NotContains(t, b.resources, "pvc_new")
}

func TestRestoreFromBackupRefusesTooSmall(t *testing.T) {
	b := newFakeBackupBackend("m1", "m2")
	b.putBackup(importedBackup())
	s := newTestController(b)
	req := backupRestoreReq("pvc-new")
	req.CapacityRange = &csi.CapacityRange{RequiredBytes: 1 << 30}

	_, err := s.CreateVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Empty(t, b.createCalls)
}

func TestRestoreWithoutLocalOrBackupIsNotFound(t *testing.T) {
	b := newFakeBackupBackend("m1", "m2")
	s := newTestController(b)

	_, err := s.CreateVolume(context.Background(), backupRestoreReq("pvc-new"))
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
	assert.Contains(t, err.Error(), "haify backup import")
}
