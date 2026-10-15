package csi

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Snapshots that leave the cluster.
//
// A VolumeSnapshotClass with backupTarget takes the local snapshot as usual
// and then uploads exactly that snapshot to a Haify backup target as a full
// backup. The snapshot's point in time is the moment the local snapshot was
// taken — the moment CreateSnapshot first returns — and the backup holds that
// same point, not a later one. Until the upload has finished the snapshot is
// reported not ready to use, which makes external-snapshotter call
// CreateSnapshot again until it is.
//
// The upload outlives a single CreateSnapshot call, so it runs in the
// background (asyncOps). The controller's backup record, whose id the driver
// knows in advance, says how far it got; a failed record is retried.

// VolumeSnapshotClass parameters.
const (
	// paramBackupTarget names the Haify backup target the snapshot is also
	// uploaded to.
	paramBackupTarget = "backupTarget"
	// paramKeepLocalSnapshot, "false", deletes the local snapshot once the
	// upload has finished; a restore then reads the backup. Default true.
	paramKeepLocalSnapshot = "keepLocalSnapshot"
)

// snapshotBackupParams is the parsed VolumeSnapshotClass parameters.
type snapshotBackupParams struct {
	target    string // empty: a local snapshot only
	keepLocal bool
}

// parseSnapshotBackupParams validates the backup parameters of a
// VolumeSnapshotClass. Other keys (the snapshotter's own metadata) are
// ignored.
func parseSnapshotBackupParams(p map[string]string) (snapshotBackupParams, error) {
	out := snapshotBackupParams{target: strings.TrimSpace(p[paramBackupTarget]), keepLocal: true}
	if v, ok := p[paramKeepLocalSnapshot]; ok {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			zap.L().Debug("parseSnapshotBackupParams: invalid keepLocalSnapshot", zap.String("value", v))
			return out, fmt.Errorf("invalid %s %q (want true or false)", paramKeepLocalSnapshot, v)
		}
		if out.target == "" {
			zap.L().Debug("parseSnapshotBackupParams: keepLocalSnapshot without backupTarget")
			return out, fmt.Errorf("%s needs %s: without a backup the local snapshot is the only copy", paramKeepLocalSnapshot, paramBackupTarget)
		}
		out.keepLocal = b
	}
	zap.L().Debug("parseSnapshotBackupParams: parsed", zap.String("target", out.target), zap.Bool("keep_local", out.keepLocal))
	return out, nil
}

// backupOpKey names the background upload of a snapshot's backup.
func backupOpKey(backupID string) string { return "backup/" + backupID }

// createBackedSnapshot is CreateSnapshot for a class with backupTarget.
func (s *controllerServer) createBackedSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest, res *haifypb.ResourceInfo,
	bp snapshotBackupParams) (*csi.CreateSnapshotResponse, error) {
	bb, err := s.backups()
	if err != nil {
		return nil, err
	}
	_, node, err := snapshotSource(res)
	if err != nil {
		s.log.Debug("controllerServer.createBackedSnapshot: source cannot be snapshotted", zap.String("resource", res.GetName()), zap.Error(err))
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	ref := snapshotRef{resource: res.GetName(), node: node, name: sanitizeSnapshotName(req.GetName()), backedUp: true}
	s.log.Debug("controllerServer.createBackedSnapshot: start", zap.String("snapshot_id", ref.id()), zap.String("backup", ref.backupID()),
		zap.String("target", bp.target))
	if err := s.checkBackupTarget(ctx, bb, bp.target); err != nil {
		return nil, err
	}
	rec, err := s.findBackup(ctx, bb, ref.backupID(), ref.resource)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "list backups of %q: %v", ref.resource, err)
	}
	if rec.GetState() == backupStateCompleted {
		return s.finishBackedSnapshot(ctx, ref, res, rec, bp)
	}

	// Taken (or found) before anything is uploaded: this is the point in time.
	snapshot, err := s.takeLocalSnapshot(ctx, req, res)
	if err != nil {
		return nil, err
	}
	snapshot.SnapshotId = ref.id()
	snapshot.ReadyToUse = false
	if err := s.driveBackup(ctx, bb, ref, bp.target, rec); err != nil {
		return nil, err
	}
	s.log.Debug("controllerServer.createBackedSnapshot: upload pending", zap.String("snapshot_id", ref.id()))
	return &csi.CreateSnapshotResponse{Snapshot: snapshot}, nil
}

// driveBackup starts the upload of ref's snapshot unless one is under way,
// and reports one that failed — once, after which the next call starts again.
func (s *controllerServer) driveBackup(ctx context.Context, bb backupBackend, ref snapshotRef, target string, rec *haifypb.BackupInfo) error {
	key := backupOpKey(ref.backupID())
	st, opErr := s.ops.state(key)
	s.log.Debug("controllerServer.driveBackup: state", zap.String("backup", ref.backupID()), zap.Int("op_state", int(st)),
		zap.String("record_state", rec.GetState()), zap.Error(opErr))
	switch st {
	case opRunning:
		return nil
	case opFailed:
		s.ops.clear(key)
		return status.Errorf(codes.Internal, "uploading snapshot %s to backup target %q failed (the next attempt starts again): %v",
			ref.name, target, opErr)
	case opSucceeded:
		// Finished after the record was read; the next call sees it completed.
		s.ops.clear(key)
		return nil
	}
	if rec.GetState() == backupStateRunning {
		// Started by a process that is gone (a previous leader); its call was
		// cancelled with it, and the controller is about to record the failure.
		s.log.Debug("controllerServer.driveBackup: running elsewhere, waiting", zap.String("backup", ref.backupID()))
		return nil
	}
	s.ops.start(ctx, key, func(c context.Context) error {
		start := time.Now()
		b, err := bb.CreateBackupFromSnapshot(c, ref.resource, target, ref.node, ref.name)
		s.log.Debug("controllerServer.driveBackup: upload returned", zap.String("backup", ref.backupID()),
			zap.Uint64("bytes", b.GetTotalBytes()), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err == nil {
			s.log.Info("CSI snapshot uploaded to backup target", zap.String("snapshot", ref.name),
				zap.String("backup", b.GetId()), zap.String("target", target))
		}
		return err
	})
	return nil
}

// finishBackedSnapshot reports a snapshot whose backup has completed as ready,
// dropping the local snapshot first when the class asks for that.
func (s *controllerServer) finishBackedSnapshot(ctx context.Context, ref snapshotRef, res *haifypb.ResourceInfo,
	rec *haifypb.BackupInfo, bp snapshotBackupParams) (*csi.CreateSnapshotResponse, error) {
	if n := rec.GetNode(); n != "" {
		// The node the backup was read on is the node the snapshot is on.
		ref.node = n
	}
	snapshot := &csi.Snapshot{
		SnapshotId:     ref.id(),
		SourceVolumeId: ref.resource,
		SizeBytes:      snapshotSizeBytes(res),
		ReadyToUse:     true,
	}
	local := s.localSnapshot(ctx, res, ref.node, ref.name)
	if local != nil {
		snapshot.CreationTime = snapshotCreationTime(local.GetCreatedAt())
	}
	if snapshot.CreationTime == nil {
		snapshot.CreationTime = timestamppb.New(backupCreationTime(rec))
	}
	if local != nil && !bp.keepLocal {
		s.log.Debug("controllerServer.finishBackedSnapshot: dropping the local snapshot", zap.String("snapshot", ref.name),
			zap.String("node", ref.node))
		if err := s.deleteLocalSnapshot(ctx, ref.resource, ref.node, ref.name); err != nil {
			return nil, err
		}
	}
	s.log.Debug("controllerServer.finishBackedSnapshot: ready", zap.String("snapshot_id", ref.id()),
		zap.String("backup", rec.GetId()), zap.Bool("local_kept", local != nil && bp.keepLocal))
	return &csi.CreateSnapshotResponse{Snapshot: snapshot}, nil
}

// localSnapshot returns the snapshot name of res on node, or nil when it is not
// there (or the node cannot be asked).
func (s *controllerServer) localSnapshot(ctx context.Context, res *haifypb.ResourceInfo, node, name string) *haifypb.SnapshotInfo {
	volumePath, _, err := snapshotSource(res)
	if err != nil {
		s.log.Debug("controllerServer.localSnapshot: no snapshot source", zap.String("resource", res.GetName()), zap.Error(err))
		return nil
	}
	start := time.Now()
	snaps, err := s.backend.ListSnapshots(ctx, volumePath, node)
	s.log.Debug("controllerServer.localSnapshot: listed snapshots", zap.String("volume", volumePath), zap.String("node", node),
		zap.Int("count", len(snaps)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil
	}
	for _, snap := range snaps {
		if snap.GetName() == name {
			return snap
		}
	}
	s.log.Debug("controllerServer.localSnapshot: not there", zap.String("snapshot", name), zap.String("node", node))
	return nil
}

// deleteBackup removes the backup of a backed-up snapshot. An upload still
// running is stopped first, and the call fails until it has stopped, so the
// sidecar retries rather than leaving an upload to finish after its snapshot
// was deleted.
func (s *controllerServer) deleteBackup(ctx context.Context, ref snapshotRef) error {
	bb, err := s.backups()
	if err != nil {
		return err
	}
	id := ref.backupID()
	key := backupOpKey(id)
	if st, _ := s.ops.state(key); st == opRunning {
		s.log.Debug("controllerServer.deleteBackup: stopping the running upload", zap.String("backup", id))
		s.ops.cancelOp(key)
		return status.Errorf(codes.Unavailable, "the upload of backup %s is being stopped; the delete is retried", id)
	}
	s.ops.clear(key)
	rec, err := s.findBackup(ctx, bb, id, ref.resource)
	if err != nil {
		return status.Errorf(codes.Unavailable, "list backups of %q: %v", ref.resource, err)
	}
	if rec == nil {
		s.log.Debug("controllerServer.deleteBackup: no backup, nothing to delete", zap.String("backup", id))
		return nil
	}
	if rec.GetState() == backupStateRunning {
		s.log.Debug("controllerServer.deleteBackup: still running elsewhere", zap.String("backup", id))
		return status.Errorf(codes.Unavailable, "backup %s is still running; the delete is retried", id)
	}
	// A failed backup's objects were removed when it failed; only its record
	// is left, and nothing on the target is there to remove.
	force := rec.GetState() == backupStateFailed
	start := time.Now()
	err = bb.DeleteBackup(ctx, id, "", force)
	s.log.Debug("controllerServer.deleteBackup: delete returned", zap.String("backup", id), zap.Bool("force", force),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "delete backup %s: %v", id, err)
	}
	s.log.Info("CSI snapshot backup deleted", zap.String("backup", id), zap.String("target", rec.GetTarget()))
	return nil
}
