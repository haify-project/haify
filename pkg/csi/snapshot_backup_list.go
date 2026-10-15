package csi

import (
	"context"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ListSnapshots for snapshots that are also backups.
//
// A backed-up snapshot can exist without its local half: dropped after the
// upload, gone with its source volume, or — in another cluster that imported
// the target's backups — never there. external-snapshotter asks ListSnapshots
// about a pre-provisioned VolumeSnapshotContent before it lets a PVC be
// restored from it, so the backup record has to answer for it.

// backedSnapshotEntry answers ListSnapshots for one backed-up snapshot ID, or
// returns nil when neither its local snapshot nor its backup exists.
func (s *controllerServer) backedSnapshotEntry(ctx context.Context, ref snapshotRef) *csi.Snapshot {
	var local *csi.Snapshot
	start := time.Now()
	res, err := s.backend.GetResource(ctx, ref.resource)
	s.log.Debug("controllerServer.backedSnapshotEntry: got source resource", zap.String("resource", ref.resource),
		zap.Bool("found", err == nil && res != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err == nil && res != nil {
		if snap := s.localSnapshot(ctx, res, ref.node, ref.name); snap != nil {
			local = &csi.Snapshot{
				SnapshotId:     ref.id(),
				SourceVolumeId: ref.resource,
				SizeBytes:      snapshotSizeBytes(res),
				CreationTime:   snapshotCreationTime(snap.GetCreatedAt()),
			}
		}
	}
	var rec *haifypb.BackupInfo
	if bb, berr := s.backups(); berr == nil {
		if rec, err = s.findBackup(ctx, bb, ref.backupID(), ref.resource); err != nil {
			s.log.Debug("controllerServer.backedSnapshotEntry: backups unreadable", zap.Error(err))
		}
	}
	s.log.Debug("controllerServer.backedSnapshotEntry: found", zap.String("snapshot_id", ref.id()), zap.Bool("local", local != nil),
		zap.String("backup_state", rec.GetState()))
	switch {
	case local != nil:
		local.ReadyToUse = rec.GetState() == backupStateCompleted
		return local
	case rec.GetState() == backupStateCompleted:
		return remoteSnapshotEntry(ref, rec)
	default:
		return nil
	}
}

// remoteSnapshotEntry describes a backed-up snapshot by its backup alone.
func remoteSnapshotEntry(ref snapshotRef, rec *haifypb.BackupInfo) *csi.Snapshot {
	snap := &csi.Snapshot{
		SnapshotId:     ref.id(),
		SourceVolumeId: ref.resource,
		SizeBytes:      backupSizeBytes(rec),
		ReadyToUse:     true,
	}
	if t := backupCreationTime(rec); !t.IsZero() {
		snap.CreationTime = timestamppb.New(t)
	}
	return snap
}

// withBackups corrects a listing of local snapshots for the ones that are also
// backups: their IDs carry the backup marker, and they are ready only once the
// upload has finished. It adds the completed backups whose local snapshot is
// gone. resource, when set, limits that to one source volume.
func (s *controllerServer) withBackups(ctx context.Context, entries []*csi.Snapshot, resource string) []*csi.Snapshot {
	bb, ok := s.backend.(backupBackend)
	if !ok {
		return entries
	}
	start := time.Now()
	all, err := bb.ListBackups(ctx, resource, "")
	s.log.Debug("controllerServer.withBackups: listed backups", zap.String("resource", resource), zap.Int("count", len(all)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return entries
	}
	byID := make(map[string]*haifypb.BackupInfo, len(all))
	for _, b := range all {
		byID[b.GetId()] = b
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		ref, perr := parseSnapshotRef(e.GetSnapshotId())
		if perr != nil {
			continue
		}
		rec := byID[ref.backupID()]
		if rec == nil {
			continue
		}
		ref.backedUp = true
		e.SnapshotId = ref.id()
		e.ReadyToUse = rec.GetState() == backupStateCompleted
		seen[rec.GetId()] = true
		s.log.Debug("controllerServer.withBackups: local snapshot is backed up", zap.String("snapshot_id", e.GetSnapshotId()),
			zap.String("backup_state", rec.GetState()))
	}
	for _, rec := range all {
		if seen[rec.GetId()] || rec.GetState() != backupStateCompleted {
			continue
		}
		ref, ok := refFromBackupID(rec.GetId(), rec.GetResource(), rec.GetNode())
		if !ok {
			continue
		}
		s.log.Debug("controllerServer.withBackups: backup without its local snapshot", zap.String("snapshot_id", ref.id()))
		entries = append(entries, remoteSnapshotEntry(ref, rec))
	}
	return entries
}
