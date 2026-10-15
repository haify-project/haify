package csi

import (
	"context"
	"strings"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The Haify backup API, as far as snapshots that leave the cluster need it.
//
// It is not part of HaifyBackend: only VolumeSnapshotClasses with backupTarget
// and restores of their snapshots use it, so a backend without it still
// serves everything else. *client.HaifyClient implements it.

// Backup states the controller reports (database.BackupState*).
const (
	backupStateRunning   = "running"
	backupStateCompleted = "completed"
	backupStateFailed    = "failed"
)

// backupBackend is the part of the controller API that ships snapshots to a
// backup target and restores them.
type backupBackend interface {
	ListBackupTargets(ctx context.Context) ([]*haifypb.BackupTargetInfo, error)
	// CreateBackupFromSnapshot uploads an existing snapshot as a full backup
	// with the id "<resource>_<snapshot>"; it returns when the upload has
	// finished.
	CreateBackupFromSnapshot(ctx context.Context, resource, target, node, snapshot string) (*haifypb.BackupInfo, error)
	ListBackups(ctx context.Context, resource, target string) ([]*haifypb.BackupInfo, error)
	RestoreBackup(ctx context.Context, id, resource, node string) (*haifypb.BackupInfo, error)
	DeleteBackup(ctx context.Context, id, node string, force bool) error
	SetResourceLabels(ctx context.Context, resource string, set map[string]string, remove []string) (map[string]string, error)
}

// backups returns the backend's backup API, or the error to give a CO that
// asked for something only it can do.
func (s *controllerServer) backups() (backupBackend, error) {
	bb, ok := s.backend.(backupBackend)
	if !ok {
		s.log.Debug("controllerServer.backups: backend has no backup API")
		return nil, status.Error(codes.FailedPrecondition, "the Haify backend does not support backups")
	}
	return bb, nil
}

// checkBackupTarget refuses a target the controller does not know, so a typo
// in a VolumeSnapshotClass surfaces on the first call, before a snapshot is
// taken for an upload that cannot happen.
func (s *controllerServer) checkBackupTarget(ctx context.Context, bb backupBackend, target string) error {
	start := time.Now()
	targets, err := bb.ListBackupTargets(ctx)
	s.log.Debug("controllerServer.checkBackupTarget: listed targets", zap.String("target", target), zap.Int("count", len(targets)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return status.Errorf(codes.Unavailable, "list backup targets: %v", err)
	}
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		if t.GetName() == target {
			return nil
		}
		names = append(names, t.GetName())
	}
	s.log.Debug("controllerServer.checkBackupTarget: unknown target", zap.String("target", target), zap.Strings("known", names))
	return status.Errorf(codes.InvalidArgument, "backup target %q does not exist in Haify (known: %s); add it with `haify backup target add`",
		target, strings.Join(names, ", "))
}

// findBackup returns the backup record with id, or nil when there is none.
// resource narrows the listing; the record names the resource it was taken
// of, in this cluster or, once imported, in another.
func (s *controllerServer) findBackup(ctx context.Context, bb backupBackend, id, resource string) (*haifypb.BackupInfo, error) {
	start := time.Now()
	all, err := bb.ListBackups(ctx, resource, "")
	s.log.Debug("controllerServer.findBackup: listed backups", zap.String("backup", id), zap.String("resource", resource),
		zap.Int("count", len(all)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	for _, b := range all {
		if b.GetId() == id {
			s.log.Debug("controllerServer.findBackup: found", zap.String("backup", id), zap.String("state", b.GetState()),
				zap.String("node", b.GetNode()), zap.String("target", b.GetTarget()))
			return b, nil
		}
	}
	s.log.Debug("controllerServer.findBackup: no such backup", zap.String("backup", id))
	return nil, nil
}

// backupSizeBytes is the restore size of a backup: its image, which is the
// source's DRBD device, rounded up to whole GiB — the size the source volume
// was provisioned at, since the device is that minus DRBD's metadata.
func backupSizeBytes(b *haifypb.BackupInfo) int64 {
	if vols := b.GetVolumes(); len(vols) > 0 {
		return int64(bytesToGiB(int64(vols[0].GetBytes()))) * giB
	}
	return 0
}

// backupCreationTime is when a backup started, which for a backup of a
// snapshot is shortly after the snapshot was taken. Used only when the local
// snapshot, which knows the exact time, is gone.
func backupCreationTime(b *haifypb.BackupInfo) time.Time {
	t, err := time.Parse(time.RFC3339, b.GetStartedAt())
	if err != nil {
		zap.L().Debug("backupCreationTime: unparseable start time", zap.String("backup", b.GetId()),
			zap.String("started_at", b.GetStartedAt()))
		return time.Time{}
	}
	return t
}
