package csi

import (
	"context"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Restoring a volume from a backed-up snapshot.
//
// The local snapshot is the fast way back, so it is used whenever it is still
// there. When it is not — dropped after the upload (keepLocalSnapshot:
// "false"), gone with its source volume, or never in this cluster because the
// snapshot was taken in another one and its backups imported here — the new
// volume is written from the backup on the target instead.
//
// Such a restore takes as long as reading the whole image from the target, far
// longer than the provisioner waits for one call. So CreateVolume creates the
// volume, starts the restore in the background and answers Aborted, which
// external-provisioner treats as "still in progress" and calls again. While
// the restore runs, the volume carries restoringLabel: a volume with that
// label is never handed out. If the process running the restore goes away,
// the next call finds the label with no restore behind it, deletes the
// half-written volume and starts over.

// restoringLabel marks a volume being written from a backup; its value is the
// backup's id.
const restoringLabel = "haify.csi/restoring-from"

// restoreSource is where a restore of a backed-up snapshot reads from.
type restoreSource int

const (
	restoreFromLocal restoreSource = iota + 1
	restoreFromBackup
)

// chooseRestoreSource decides between the local snapshot and the backup, or
// says why neither can be used.
func chooseRestoreSource(localPresent bool, backup *haifypb.BackupInfo, snapshotID string) (restoreSource, error) {
	log := zap.L()
	log.Debug("chooseRestoreSource: deciding", zap.String("snapshot_id", snapshotID), zap.Bool("local_present", localPresent),
		zap.String("backup_state", backup.GetState()))
	switch {
	case localPresent:
		return restoreFromLocal, nil
	case backup == nil:
		return 0, status.Errorf(codes.NotFound,
			"snapshot %q: neither the local snapshot nor its backup exists here; in another cluster, import the target's backups first (haify backup import <target>)",
			snapshotID)
	case backup.GetState() == backupStateCompleted:
		return restoreFromBackup, nil
	case backup.GetState() == backupStateRunning:
		return 0, status.Errorf(codes.Unavailable, "snapshot %q: its backup %s is still uploading", snapshotID, backup.GetId())
	default:
		return 0, status.Errorf(codes.FailedPrecondition, "snapshot %q: its local copy is gone and its backup %s is %s",
			snapshotID, backup.GetId(), backup.GetState())
	}
}

// resolveBackedSnapshot is resolveVolumeSource for a backed-up snapshot.
func (s *controllerServer) resolveBackedSnapshot(ctx context.Context, ref snapshotRef, wantGiB uint32) (*volumeSource, error) {
	id := ref.id()
	start := time.Now()
	res, gerr := s.backend.GetResource(ctx, ref.resource)
	s.log.Debug("controllerServer.resolveBackedSnapshot: looked up the source volume", zap.String("resource", ref.resource),
		zap.Bool("found", gerr == nil && res != nil), zap.Duration("took", time.Since(start)), zap.Error(gerr))
	localPresent := gerr == nil && res != nil && s.localSnapshot(ctx, res, ref.node, ref.name) != nil

	var backup *haifypb.BackupInfo
	if !localPresent {
		bb, err := s.backups()
		if err != nil {
			return nil, err
		}
		if backup, err = s.findBackup(ctx, bb, ref.backupID(), ref.resource); err != nil {
			return nil, status.Errorf(codes.Unavailable, "list backups of %q: %v", ref.resource, err)
		}
	}
	from, err := chooseRestoreSource(localPresent, backup, id)
	if err != nil {
		return nil, err
	}
	if from == restoreFromLocal {
		if err := checkSourceFits(res, wantGiB); err != nil {
			return nil, err
		}
		s.log.Debug("controllerServer.resolveBackedSnapshot: restoring from the local snapshot", zap.String("snapshot_id", id),
			zap.String("node", ref.node))
		return &volumeSource{snapshot: oneNodeSnapshot(res, ref.name), node: ref.node}, nil
	}
	if _, ok := s.backend.(interface {
		CreateResourceRequest(context.Context, *haifypb.CreateResourceRequest) error
	}); !ok {
		s.log.Debug("controllerServer.resolveBackedSnapshot: backend cannot label a volume")
		return nil, status.Error(codes.FailedPrecondition, "the Haify backend cannot label volumes, which a restore from a backup needs")
	}
	if got := bytesToGiB(backupSizeBytes(backup)); wantGiB > 0 && got > wantGiB {
		s.log.Debug("controllerServer.resolveBackedSnapshot: backup larger than request", zap.Uint32("backup_gb", got),
			zap.Uint32("want_gb", wantGiB))
		return nil, status.Errorf(codes.InvalidArgument, "requested size %d GiB is smaller than snapshot %q (%d GiB)", wantGiB, id, got)
	}
	s.log.Debug("controllerServer.resolveBackedSnapshot: restoring from the backup", zap.String("snapshot_id", id),
		zap.String("backup", backup.GetId()), zap.String("target", backup.GetTarget()))
	return &volumeSource{backup: backup}, nil
}

// restoreOpKey names the background restore into a volume.
func restoreOpKey(resource string) string { return "restore/" + resource }

// startRestore writes backup into the freshly created volume name in the
// background, and answers the CreateVolume call that created it.
func (s *controllerServer) startRestore(ctx context.Context, name string, backup *haifypb.BackupInfo) error {
	bb, err := s.backups()
	if err != nil {
		return err
	}
	s.ops.start(ctx, restoreOpKey(name), func(c context.Context) error {
		start := time.Now()
		_, err := bb.RestoreBackup(c, backup.GetId(), name, "")
		s.log.Debug("controllerServer.startRestore: restore returned", zap.String("resource", name),
			zap.String("backup", backup.GetId()), zap.Duration("took", time.Since(start)), zap.Error(err))
		return err
	})
	return status.Errorf(codes.Aborted, "volume %q is being restored from backup %s", name, backup.GetId())
}

// checkRestore is the first thing CreateVolume does with a volume that
// already exists: one still being restored is not handed out. It returns nil
// for a volume that is not (or no longer) being restored.
func (s *controllerServer) checkRestore(ctx context.Context, existing *haifypb.ResourceInfo) error {
	name := existing.GetName()
	backupID := existing.GetLabels()[restoringLabel]
	if backupID == "" {
		return nil
	}
	key := restoreOpKey(name)
	st, opErr := s.ops.state(key)
	s.log.Debug("controllerServer.checkRestore: volume is being restored", zap.String("resource", name),
		zap.String("backup", backupID), zap.Int("op_state", int(st)), zap.Error(opErr))
	switch st {
	case opRunning:
		return status.Errorf(codes.Aborted, "volume %q is being restored from backup %s", name, backupID)
	case opSucceeded:
		bb, err := s.backups()
		if err != nil {
			return err
		}
		start := time.Now()
		_, err = bb.SetResourceLabels(ctx, name, nil, []string{restoringLabel})
		s.log.Debug("controllerServer.checkRestore: cleared the restoring label", zap.String("resource", name),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return status.Errorf(codes.Unavailable, "volume %q is restored, but clearing its %s label failed: %v", name, restoringLabel, err)
		}
		s.ops.clear(key)
		s.log.Info("CSI volume restored from backup", zap.String("volume", name), zap.String("backup", backupID))
		return nil
	case opFailed:
		s.ops.clear(key)
		s.discardRestore(ctx, name)
		return status.Errorf(codes.Internal, "restoring volume %q from backup %s failed; it was deleted and is created again on the next attempt: %v",
			name, backupID, opErr)
	default:
		// The process that ran the restore is gone, and its half-written
		// volume with it as far as anyone can tell.
		s.discardRestore(ctx, name)
		return status.Errorf(codes.Aborted, "an interrupted restore of volume %q from backup %s was discarded; it starts again", name, backupID)
	}
}

// discardRestore deletes a volume whose restore did not finish, so the next
// attempt starts from an empty one. A failure is logged: the next call finds
// the label again and retries the delete.
func (s *controllerServer) discardRestore(ctx context.Context, name string) {
	start := time.Now()
	err := s.backend.DeleteResource(context.WithoutCancel(ctx), name)
	s.log.Debug("controllerServer.discardRestore: deleted the half-restored volume", zap.String("resource", name),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		s.log.Warn("failed to delete a volume whose restore did not finish", zap.String("volume", name), zap.Error(err))
	}
}

// existingVolumeResponse answers CreateVolume for a volume that already
// exists — a retry, or the call that finds a background restore finished — with
// what the first call would have returned.
func existingVolumeResponse(req *csi.CreateVolumeRequest, existing *haifypb.ResourceInfo, sizeGB uint32) *csi.CreateVolumeResponse {
	// The parameters were valid when the volume was created; a request that is
	// now refused only loses its volume context, not the answer.
	params, err := ParseVolumeParams(req.GetParameters())
	zap.L().Debug("existingVolumeResponse: answering for an existing volume", zap.String("resource", existing.GetName()),
		zap.Bool("allow_remote", params.AllowRemoteVolumeAccess), zap.Error(err))
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId:           existing.GetName(),
		CapacityBytes:      int64(sizeGB) * giB,
		AccessibleTopology: topologyFor(localReplicas(existing), params.AllowRemoteVolumeAccess),
		VolumeContext:      volumeContextFor(params.AllowRemoteVolumeAccess, params.QoS),
		ContentSource:      contentSourceOf(req.GetVolumeContentSource()),
	}}
}
