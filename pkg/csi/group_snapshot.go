package csi

import (
	"context"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// VolumeGroupSnapshot: snapshots of several volumes of one application, taken
// at the same instant.
//
// Each member is an ordinary snapshot of this driver — an LVM/ZFS snapshot of
// one replica's backing volume, with an ID from makeSnapshotID — so it can be
// restored, listed and deleted as a VolumeSnapshot like any other. What the
// group adds is the instant: the controller's CreateSnapshotGroup suspends I/O
// on every member volume (each Primary first, a timer on each node resuming it
// whatever happens), takes all the snapshots and resumes. A database with its
// log on one volume and its tables on another gets a set it can recover from,
// as from a power cut.

// GroupControllerGetCapabilities reports the one group capability there is.
func (s *controllerServer) GroupControllerGetCapabilities(context.Context, *csi.GroupControllerGetCapabilitiesRequest) (*csi.GroupControllerGetCapabilitiesResponse, error) {
	s.log.Debug("controllerServer.GroupControllerGetCapabilities: reporting group snapshots")
	return &csi.GroupControllerGetCapabilitiesResponse{Capabilities: []*csi.GroupControllerServiceCapability{{
		Type: &csi.GroupControllerServiceCapability_Rpc{Rpc: &csi.GroupControllerServiceCapability_RPC{
			Type: csi.GroupControllerServiceCapability_RPC_CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT}},
	}}}, nil
}

// plannedMember is one source volume of a group: where its snapshot goes, and
// whether an earlier attempt already took it.
type plannedMember struct {
	resource, volume, node, name string
	sizeBytes                    int64
	exists                       bool
	created                      *timestamppb.Timestamp
}

// planGroup resolves every source volume to the snapshot the group takes of
// it. Only this driver's volumes qualify: suspending a volume something else
// manages would stall I/O its owner knows nothing about.
func (s *controllerServer) planGroup(ctx context.Context, groupSnap string, sources []string) ([]plannedMember, error) {
	out := make([]plannedMember, 0, len(sources))
	for _, source := range sources {
		start := time.Now()
		res, err := s.backend.GetResource(ctx, source)
		s.log.Debug("controllerServer.planGroup: got source resource", zap.String("resource", source),
			zap.Bool("found", err == nil && res != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil || res == nil {
			return nil, status.Errorf(codes.NotFound, "source volume %q not found", source)
		}
		if !isCSIVolume(res) {
			s.log.Debug("controllerServer.planGroup: not a volume of this driver", zap.String("resource", source))
			return nil, status.Errorf(codes.InvalidArgument,
				"volume %q was not provisioned by %s; a group snapshot takes only volumes this driver created", source, DriverName)
		}
		volumePath, node, err := snapshotSource(res)
		if err != nil {
			s.log.Debug("controllerServer.planGroup: source cannot be snapshotted", zap.String("resource", source), zap.Error(err))
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		m := plannedMember{resource: source, volume: volumePath, node: node, name: memberSnapshotName(groupSnap, source),
			sizeBytes: snapshotSizeBytes(res)}
		start = time.Now()
		snaps, lerr := s.backend.ListSnapshots(ctx, volumePath, node)
		s.log.Debug("controllerServer.planGroup: listed existing snapshots", zap.String("volume", volumePath),
			zap.String("node", node), zap.Int("count", len(snaps)), zap.Duration("took", time.Since(start)), zap.Error(lerr))
		for _, snap := range snaps {
			if snap.GetName() == m.name {
				m.exists, m.created = true, snapshotCreationTime(snap.GetCreatedAt())
			}
		}
		s.log.Debug("controllerServer.planGroup: member", zap.String("resource", source), zap.String("snapshot", m.name),
			zap.String("node", node), zap.Bool("exists", m.exists))
		out = append(out, m)
	}
	return out, nil
}

// groupResponse builds the group as the CO records it. All members carry the
// group's time: they were taken at one instant.
func groupResponse(groupSnap string, members []plannedMember, created *timestamppb.Timestamp) *csi.VolumeGroupSnapshot {
	groupID := makeGroupSnapshotID(groupSnap)
	g := &csi.VolumeGroupSnapshot{GroupSnapshotId: groupID, CreationTime: created, ReadyToUse: true}
	for _, m := range members {
		g.Snapshots = append(g.Snapshots, &csi.Snapshot{
			SnapshotId:      makeSnapshotID(m.resource, m.node, m.name),
			SourceVolumeId:  m.resource,
			SizeBytes:       m.sizeBytes,
			CreationTime:    created,
			ReadyToUse:      true,
			GroupSnapshotId: groupID,
		})
	}
	return g
}

// CreateVolumeGroupSnapshot snapshots every source volume at one instant. It is
// idempotent on the name: a retry finds the members already taken and returns
// them. Members left by an attempt that died half-way (the controller undoes a
// failed group itself, so only a crash leaves them) are not from the instant a
// new attempt takes, so they are removed and the whole group taken again.
func (s *controllerServer) CreateVolumeGroupSnapshot(ctx context.Context, req *csi.CreateVolumeGroupSnapshotRequest) (*csi.CreateVolumeGroupSnapshotResponse, error) {
	s.log.Debug("controllerServer.CreateVolumeGroupSnapshot: start", zap.String("name", req.GetName()),
		zap.Strings("source_resources", req.GetSourceVolumeIds()), zap.Int("parameters", len(req.GetParameters())),
		zap.Int("secrets", len(req.GetSecrets())))
	if req.GetName() == "" {
		s.log.Debug("controllerServer.CreateVolumeGroupSnapshot: rejected, no name")
		return nil, status.Error(codes.InvalidArgument, "group snapshot name is required")
	}
	if len(req.GetSourceVolumeIds()) == 0 {
		s.log.Debug("controllerServer.CreateVolumeGroupSnapshot: rejected, no source volumes")
		return nil, status.Error(codes.InvalidArgument, "source volume ids are required")
	}
	requested := map[string]bool{}
	for _, id := range req.GetSourceVolumeIds() {
		if id == "" || requested[id] {
			s.log.Debug("controllerServer.CreateVolumeGroupSnapshot: rejected, empty or repeated source", zap.String("resource", id))
			return nil, status.Errorf(codes.InvalidArgument, "source volume %q is empty or named twice", id)
		}
		requested[id] = true
	}

	groupSnap := sanitizeSnapshotName(req.GetName())
	members, err := s.planGroup(ctx, groupSnap, req.GetSourceVolumeIds())
	if err != nil {
		return nil, err
	}
	present := 0
	var created *timestamppb.Timestamp
	for _, m := range members {
		if m.exists {
			present++
			if m.created != nil && (created == nil || m.created.AsTime().Before(created.AsTime())) {
				created = m.created
			}
		}
	}
	if present == len(members) {
		s.log.Debug("controllerServer.CreateVolumeGroupSnapshot: already exists, returning it (idempotent)",
			zap.String("group", groupSnap), zap.Int("members", present))
		if created == nil {
			created = timestamppb.Now()
		}
		return &csi.CreateVolumeGroupSnapshotResponse{GroupSnapshot: groupResponse(groupSnap, members, created)}, nil
	}

	// The name already taken by a group of other volumes is a conflict, not a
	// partial attempt to finish.
	found, err := s.findGroupMembers(ctx, groupSnap)
	if err != nil {
		s.log.Debug("controllerServer.CreateVolumeGroupSnapshot: could not look for the name elsewhere", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "list volumes: %v", err)
	}
	for _, snap := range found {
		if !requested[snap.GetSourceVolumeId()] {
			s.log.Debug("controllerServer.CreateVolumeGroupSnapshot: name used by a group of other volumes",
				zap.String("group", groupSnap), zap.String("other_resource", snap.GetSourceVolumeId()))
			return nil, status.Errorf(codes.AlreadyExists, "group snapshot name %q is already used for volume %q, which this request does not name",
				req.GetName(), snap.GetSourceVolumeId())
		}
	}

	pb := make([]*haifypb.SnapshotGroupMember, 0, len(members))
	for _, m := range members {
		if m.exists {
			start := time.Now()
			derr := s.backend.DeleteSnapshot(ctx, m.volume, m.name, m.node)
			s.log.Debug("controllerServer.CreateVolumeGroupSnapshot: removed member of an unfinished attempt",
				zap.String("volume", m.volume), zap.String("snapshot", m.name), zap.String("node", m.node),
				zap.Duration("took", time.Since(start)), zap.Error(derr))
			if derr != nil {
				return nil, status.Errorf(codes.Internal, "remove snapshot %q left by an unfinished attempt: %v", m.name, derr)
			}
		}
		pb = append(pb, &haifypb.SnapshotGroupMember{Resource: m.resource, Volume: m.volume, SnapshotName: m.name, Node: m.node})
	}
	start := time.Now()
	err = s.backend.CreateSnapshotGroup(ctx, pb)
	s.log.Debug("controllerServer.CreateVolumeGroupSnapshot: group returned", zap.String("group", groupSnap),
		zap.Int("members", len(pb)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "group snapshot %q: %v", req.GetName(), err)
	}
	s.log.Info("CSI group snapshot created", zap.String("group", groupSnap), zap.Strings("volumes", req.GetSourceVolumeIds()))
	return &csi.CreateVolumeGroupSnapshotResponse{GroupSnapshot: groupResponse(groupSnap, members, timestamppb.Now())}, nil
}

// GetVolumeGroupSnapshot reports a group whose members all still exist, and
// NotFound otherwise: a group missing a member no longer holds one instant.
func (s *controllerServer) GetVolumeGroupSnapshot(ctx context.Context, req *csi.GetVolumeGroupSnapshotRequest) (*csi.GetVolumeGroupSnapshotResponse, error) {
	s.log.Debug("controllerServer.GetVolumeGroupSnapshot: start", zap.String("group_snapshot_id", req.GetGroupSnapshotId()),
		zap.Strings("snapshot_ids", req.GetSnapshotIds()), zap.Int("secrets", len(req.GetSecrets())))
	if req.GetGroupSnapshotId() == "" {
		s.log.Debug("controllerServer.GetVolumeGroupSnapshot: rejected, no id")
		return nil, status.Error(codes.InvalidArgument, "group snapshot id is required")
	}
	groupSnap, err := parseGroupSnapshotID(req.GetGroupSnapshotId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "group snapshot %q not found", req.GetGroupSnapshotId())
	}
	ids, err := s.groupMemberIDs(ctx, groupSnap, req.GetSnapshotIds())
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		s.log.Debug("controllerServer.GetVolumeGroupSnapshot: no members", zap.String("group", groupSnap))
		return nil, status.Errorf(codes.NotFound, "group snapshot %q not found", req.GetGroupSnapshotId())
	}
	members, err := membersOfGroup(groupSnap, ids)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	var planned []plannedMember
	var created *timestamppb.Timestamp
	for _, m := range members {
		start := time.Now()
		res, gerr := s.backend.GetResource(ctx, m.resource)
		s.log.Debug("controllerServer.GetVolumeGroupSnapshot: got member's volume", zap.String("resource", m.resource),
			zap.Bool("found", gerr == nil && res != nil), zap.Duration("took", time.Since(start)), zap.Error(gerr))
		if gerr != nil || res == nil {
			return nil, status.Errorf(codes.NotFound, "volume %q of group snapshot member %q not found", m.resource, m.id)
		}
		volumePath, _, serr := snapshotSource(res)
		if serr != nil {
			return nil, status.Errorf(codes.NotFound, "group snapshot member %q: %v", m.id, serr)
		}
		start = time.Now()
		snaps, lerr := s.backend.ListSnapshots(ctx, volumePath, m.node)
		s.log.Debug("controllerServer.GetVolumeGroupSnapshot: listed member's node", zap.String("volume", volumePath),
			zap.String("node", m.node), zap.Int("count", len(snaps)), zap.Duration("took", time.Since(start)), zap.Error(lerr))
		if lerr != nil {
			return nil, status.Errorf(codes.Unavailable, "list snapshots of %q on %s: %v", m.resource, m.node, lerr)
		}
		p := plannedMember{resource: m.resource, volume: volumePath, node: m.node, name: m.name, sizeBytes: snapshotSizeBytes(res)}
		for _, snap := range snaps {
			if snap.GetName() == m.name {
				p.exists, p.created = true, snapshotCreationTime(snap.GetCreatedAt())
			}
		}
		if !p.exists {
			s.log.Debug("controllerServer.GetVolumeGroupSnapshot: member missing", zap.String("snapshot_id", m.id))
			return nil, status.Errorf(codes.NotFound, "group snapshot member %q not found", m.id)
		}
		if p.created != nil && (created == nil || p.created.AsTime().Before(created.AsTime())) {
			created = p.created
		}
		planned = append(planned, p)
	}
	if created == nil {
		created = timestamppb.Now()
	}
	return &csi.GetVolumeGroupSnapshotResponse{GroupSnapshot: groupResponse(groupSnap, planned, created)}, nil
}

// DeleteVolumeGroupSnapshot deletes every member. Like DeleteSnapshot, an
// unknown or already-deleted group is success.
func (s *controllerServer) DeleteVolumeGroupSnapshot(ctx context.Context, req *csi.DeleteVolumeGroupSnapshotRequest) (*csi.DeleteVolumeGroupSnapshotResponse, error) {
	s.log.Debug("controllerServer.DeleteVolumeGroupSnapshot: start", zap.String("group_snapshot_id", req.GetGroupSnapshotId()),
		zap.Strings("snapshot_ids", req.GetSnapshotIds()), zap.Int("secrets", len(req.GetSecrets())))
	if req.GetGroupSnapshotId() == "" {
		s.log.Debug("controllerServer.DeleteVolumeGroupSnapshot: rejected, no id")
		return nil, status.Error(codes.InvalidArgument, "group snapshot id is required")
	}
	groupSnap, err := parseGroupSnapshotID(req.GetGroupSnapshotId())
	if err != nil {
		s.log.Debug("controllerServer.DeleteVolumeGroupSnapshot: malformed id, nothing to delete", zap.Error(err))
		return &csi.DeleteVolumeGroupSnapshotResponse{}, nil
	}
	ids, err := s.groupMemberIDs(ctx, groupSnap, req.GetSnapshotIds())
	if err != nil {
		return nil, err
	}
	if _, err := membersOfGroup(groupSnap, ids); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	for _, id := range ids {
		s.log.Debug("controllerServer.DeleteVolumeGroupSnapshot: deleting member", zap.String("snapshot_id", id))
		if _, err := s.DeleteSnapshot(ctx, &csi.DeleteSnapshotRequest{SnapshotId: id, Secrets: req.GetSecrets()}); err != nil {
			s.log.Debug("controllerServer.DeleteVolumeGroupSnapshot: member not deleted", zap.String("snapshot_id", id), zap.Error(err))
			return nil, err
		}
	}
	s.log.Info("CSI group snapshot deleted", zap.String("group", groupSnap), zap.Int("members", len(ids)))
	return &csi.DeleteVolumeGroupSnapshotResponse{}, nil
}

// groupMemberIDs is the members the CO named, or, when it named none, the ones
// found by walking the driver's volumes.
func (s *controllerServer) groupMemberIDs(ctx context.Context, groupSnap string, given []string) ([]string, error) {
	if len(given) > 0 {
		s.log.Debug("controllerServer.groupMemberIDs: members named by the CO", zap.Int("members", len(given)))
		return given, nil
	}
	found, err := s.findGroupMembers(ctx, groupSnap)
	if err != nil {
		s.log.Debug("controllerServer.groupMemberIDs: lookup failed", zap.String("group", groupSnap), zap.Error(err))
		return nil, status.Errorf(codes.Internal, "list volumes: %v", err)
	}
	ids := make([]string, 0, len(found))
	for _, snap := range found {
		ids = append(ids, snap.GetSnapshotId())
	}
	s.log.Debug("controllerServer.groupMemberIDs: members found", zap.String("group", groupSnap), zap.Int("members", len(ids)))
	return ids, nil
}
