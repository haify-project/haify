package csi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// snapshotIDSep separates the three parts of a CSI snapshot ID. Neither a
// sanitized resource name (letters/digits/underscore) nor a node name (a
// hostname) can contain it, so the encoding round-trips unambiguously.
const snapshotIDSep = "/"

// makeSnapshotID encodes everything DeleteSnapshot needs to find the snapshot
// again: the source resource, the node whose replica actually holds it, and its
// backend name. A snapshot lives on exactly ONE node (it is an LVM/ZFS snapshot
// of that node's local backing volume, not a replicated DRBD object), so the
// node must be part of the ID — re-resolving it later could pick a different
// replica that has no such snapshot.
func makeSnapshotID(resource, node, snapshotName string) string {
	return strings.Join([]string{resource, node, snapshotName}, snapshotIDSep)
}

// parseSnapshotID splits an ID produced by makeSnapshotID, or the ID of a
// backed-up snapshot (snapshot_id.go), into the local snapshot's parts.
func parseSnapshotID(id string) (resource, node, snapshotName string, err error) {
	ref, err := parseSnapshotRef(id)
	if err != nil {
		return "", "", "", err
	}
	return ref.resource, ref.node, ref.name, nil
}

// snapshotSource locates the backing volume to snapshot for a resource: the
// `<pool>/<lv>` path of its first volume, and a node that holds a diskful
// replica (GetNodes reports diskful nodes only, so a diskless client is never
// chosen — it has no local storage to snapshot).
func snapshotSource(res *haifypb.ResourceInfo) (volumePath, node string, err error) {
	log := zap.L()
	vols := res.GetVolumes()
	if len(vols) == 0 {
		log.Debug("snapshotSource: resource has no volumes", zap.String("resource", res.GetName()))
		return "", "", fmt.Errorf("resource %q has no volumes", res.GetName())
	}
	pool, backing := vols[0].GetPool(), vols[0].GetBackingVolume()
	if pool == "" || backing == "" {
		log.Debug("snapshotSource: no backing volume recorded", zap.String("resource", res.GetName()),
			zap.String("pool", pool), zap.String("backing_volume", backing))
		return "", "", fmt.Errorf("resource %q volume 0 has no backing volume recorded", res.GetName())
	}
	nodes := res.GetNodes()
	if len(nodes) == 0 {
		log.Debug("snapshotSource: no diskful replica", zap.String("resource", res.GetName()))
		return "", "", fmt.Errorf("resource %q has no diskful replica to snapshot", res.GetName())
	}
	log.Debug("snapshotSource: chosen", zap.String("resource", res.GetName()), zap.String("volume", pool+"/"+backing),
		zap.String("node", nodes[0]))
	return pool + "/" + backing, nodes[0], nil
}

// snapshotSizeBytes reports the source volume's size, which is what the CO
// records as the snapshot's restore size.
func snapshotSizeBytes(res *haifypb.ResourceInfo) int64 {
	if vols := res.GetVolumes(); len(vols) > 0 {
		return int64(vols[0].GetSizeGb()) * giB
	}
	return 0
}

// CreateSnapshot takes a point-in-time snapshot of a volume's backing LV/dataset
// on one replica node. It is idempotent per the CSI spec: calling it again with
// the same name and source returns the existing snapshot instead of failing.
//
// The snapshot is local to one node — it is not replicated by DRBD — so it
// protects against logical faults (bad writes, accidental deletion) rather than
// loss of that node. A VolumeSnapshotClass with backupTarget also ships it to a
// Haify backup target (snapshot_backup.go).
func (s *controllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	s.log.Debug("controllerServer.CreateSnapshot: start", zap.String("name", req.GetName()),
		zap.String("source_resource", req.GetSourceVolumeId()), zap.Int("secrets", len(req.GetSecrets())))
	if req.GetName() == "" {
		s.log.Debug("controllerServer.CreateSnapshot: rejected, no name")
		return nil, status.Error(codes.InvalidArgument, "snapshot name is required")
	}
	if req.GetSourceVolumeId() == "" {
		s.log.Debug("controllerServer.CreateSnapshot: rejected, no source volume")
		return nil, status.Error(codes.InvalidArgument, "source volume id is required")
	}

	source := req.GetSourceVolumeId()
	start := time.Now()
	res, err := s.backend.GetResource(ctx, source)
	s.log.Debug("controllerServer.CreateSnapshot: got source resource", zap.String("resource", source),
		zap.Bool("found", err == nil && res != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || res == nil {
		return nil, status.Errorf(codes.NotFound, "source volume %q not found", source)
	}

	bp, err := parseSnapshotBackupParams(req.GetParameters())
	if err != nil {
		s.log.Debug("controllerServer.CreateSnapshot: invalid parameters", zap.String("resource", source), zap.Error(err))
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if bp.target != "" {
		s.log.Debug("controllerServer.CreateSnapshot: snapshot is also backed up", zap.String("resource", source),
			zap.String("target", bp.target), zap.Bool("keep_local", bp.keepLocal))
		return s.createBackedSnapshot(ctx, req, res, bp)
	}
	snapshot, err := s.takeLocalSnapshot(ctx, req, res)
	if err != nil {
		return nil, err
	}
	return &csi.CreateSnapshotResponse{Snapshot: snapshot}, nil
}

// takeLocalSnapshot takes the snapshot on the source's first diskful replica,
// or returns the one an earlier call took.
func (s *controllerServer) takeLocalSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest, res *haifypb.ResourceInfo) (*csi.Snapshot, error) {
	source := res.GetName()
	volumePath, node, err := snapshotSource(res)
	if err != nil {
		s.log.Debug("controllerServer.takeLocalSnapshot: source cannot be snapshotted", zap.String("resource", source), zap.Error(err))
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	// The CO's name (e.g. "snapshot-<uuid>") must survive as an LVM/ZFS object
	// name — including staying clear of LVM's reserved "snapshot*" namespace.
	snapName := sanitizeSnapshotName(req.GetName())

	snapshot := &csi.Snapshot{
		SnapshotId:     makeSnapshotID(source, node, snapName),
		SourceVolumeId: source,
		SizeBytes:      snapshotSizeBytes(res),
		// LVM/ZFS snapshots are usable the moment lvcreate/zfs snapshot
		// returns; there is no background copy to wait for.
		ReadyToUse: true,
	}

	// Idempotency: an existing snapshot of the same name on the same source is
	// the same snapshot, so report success rather than trying to create it twice
	// (which lvcreate would reject).
	start := time.Now()
	existing, lerr := s.backend.ListSnapshots(ctx, volumePath, node)
	s.log.Debug("controllerServer.takeLocalSnapshot: listed existing snapshots", zap.String("volume", volumePath),
		zap.String("node", node), zap.Int("count", len(existing)), zap.Duration("took", time.Since(start)), zap.Error(lerr))
	if lerr == nil {
		for _, snap := range existing {
			if snap.GetName() == snapName {
				s.log.Debug("controllerServer.takeLocalSnapshot: already exists, returning it (idempotent)",
					zap.String("snapshot", snapName), zap.String("node", node))
				snapshot.CreationTime = snapshotCreationTime(snap.GetCreatedAt())
				return snapshot, nil
			}
		}
	}

	// A name the CO already used for a snapshot of another volume is a
	// conflict, not a second snapshot. Kubernetes names snapshots after their
	// UID so it never asks, but the spec requires the answer, and giving it
	// costs one walk of the driver's volumes on the uncommon path — the
	// idempotent retry above has already returned.
	if other := s.snapshotNamedElsewhere(ctx, snapName, source); other != "" {
		s.log.Debug("controllerServer.takeLocalSnapshot: name already used by another volume", zap.String("snapshot", snapName),
			zap.String("other_resource", other))
		return nil, status.Errorf(codes.AlreadyExists,
			"snapshot name %q is already used by a snapshot of volume %q", req.GetName(), other)
	}

	start = time.Now()
	err = s.backend.CreateSnapshot(ctx, volumePath, snapName, node)
	s.log.Debug("controllerServer.takeLocalSnapshot: create returned", zap.String("volume", volumePath),
		zap.String("snapshot", snapName), zap.String("node", node), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create snapshot %q of %q: %v", snapName, volumePath, err)
	}
	snapshot.CreationTime = timestamppb.New(time.Now())

	s.log.Info("CSI snapshot created",
		zap.String("snapshot", snapName),
		zap.String("volume", volumePath),
		zap.String("node", node))

	return snapshot, nil
}

// DeleteSnapshot removes a snapshot. It is idempotent: an unknown, malformed or
// already-deleted snapshot is reported as success, as the CSI spec requires.
func (s *controllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	s.log.Debug("controllerServer.DeleteSnapshot: start", zap.String("snapshot_id", req.GetSnapshotId()),
		zap.Int("secrets", len(req.GetSecrets())))
	if req.GetSnapshotId() == "" {
		s.log.Debug("controllerServer.DeleteSnapshot: rejected, no snapshot id")
		return nil, status.Error(codes.InvalidArgument, "snapshot id is required")
	}

	ref, err := parseSnapshotRef(req.GetSnapshotId())
	if err != nil {
		// Not an ID this driver ever handed out, so there is nothing to delete.
		s.log.Debug("controllerServer.DeleteSnapshot: malformed id, nothing to delete", zap.Error(err))
		return &csi.DeleteSnapshotResponse{}, nil
	}

	// The backup goes first: while it is still uploading it reads the local
	// snapshot, which therefore cannot be removed under it.
	if ref.backedUp {
		if err := s.deleteBackup(ctx, ref); err != nil {
			return nil, err
		}
	}
	if err := s.deleteLocalSnapshot(ctx, ref.resource, ref.node, ref.name); err != nil {
		return nil, err
	}
	return &csi.DeleteSnapshotResponse{}, nil
}

// deleteLocalSnapshot removes the local snapshot snapName of resource on node.
// A snapshot that is already gone, with its source or on its own, is success.
func (s *controllerServer) deleteLocalSnapshot(ctx context.Context, resource, node, snapName string) error {
	start := time.Now()
	res, gerr := s.backend.GetResource(ctx, resource)
	s.log.Debug("controllerServer.deleteLocalSnapshot: got source resource", zap.String("resource", resource),
		zap.Bool("found", gerr == nil && res != nil), zap.Duration("took", time.Since(start)), zap.Error(gerr))
	if gerr != nil || res == nil {
		// The source volume is gone; its backing LV — and with it any snapshot
		// of that LV — went away when the resource was deleted.
		return nil
	}

	volumePath, _, serr := snapshotSource(res)
	if serr != nil {
		s.log.Debug("controllerServer.deleteLocalSnapshot: no snapshot source, nothing to delete", zap.String("resource", resource),
			zap.Error(serr))
		return nil
	}

	start = time.Now()
	err := s.backend.DeleteSnapshot(ctx, volumePath, snapName, node)
	s.log.Debug("controllerServer.deleteLocalSnapshot: delete returned", zap.String("volume", volumePath),
		zap.String("snapshot", snapName), zap.String("node", node), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return status.Errorf(codes.Internal, "delete snapshot %q: %v", snapName, err)
	}
	return nil
}

// snapshotCreationTime converts a backend timestamp into a protobuf timestamp.
// The backend does not always record one, so an empty or unparseable value
// yields nil, which the CSI spec permits.
func snapshotCreationTime(raw string) *timestamppb.Timestamp {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05 -0700", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return timestamppb.New(t)
		}
	}
	zap.L().Debug("snapshotCreationTime: unparseable timestamp", zap.String("raw", raw))
	return nil
}

// snapshotNamedElsewhere returns the volume that already has a CSI snapshot
// called snapName, other than source, or "" when none does.
func (s *controllerServer) snapshotNamedElsewhere(ctx context.Context, snapName, source string) string {
	start := time.Now()
	all, err := s.backend.ListResources(ctx)
	s.log.Debug("controllerServer.snapshotNamedElsewhere: listed resources", zap.String("snapshot", snapName),
		zap.Int("count", len(all)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return ""
	}
	for _, r := range all {
		if !isCSIVolume(r) || r.GetName() == source {
			continue
		}
		s.log.Debug("controllerServer.snapshotNamedElsewhere: checking volume", zap.String("resource", r.GetName()))
		for _, snap := range s.snapshotsOf(ctx, r) {
			if strings.HasSuffix(snap.GetSnapshotId(), snapshotIDSep+snapName) {
				s.log.Debug("controllerServer.snapshotNamedElsewhere: found", zap.String("snapshot", snapName),
					zap.String("resource", r.GetName()))
				return r.GetName()
			}
		}
	}
	s.log.Debug("controllerServer.snapshotNamedElsewhere: name is free", zap.String("snapshot", snapName))
	return ""
}
