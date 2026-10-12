package controller

import (
	"context"
	"fmt"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
)

func (s *Server) CreateSnapshot(ctx context.Context, req *haifypb.CreateSnapshotRequest) (*haifypb.CreateSnapshotResponse, error) {
	log := s.serverLog()
	log.Debug("CreateSnapshot: taking", zap.String("volume", req.Volume), zap.String("snapshot", req.SnapshotName), zap.String("node", req.Node))
	err := s.snapshots.CreateSnapshot(ctx, req.Volume, req.SnapshotName, req.Node)
	if err != nil {
		log.Debug("CreateSnapshot: failed", zap.String("volume", req.Volume), zap.Error(err))
		return &haifypb.CreateSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("CreateSnapshot: taken", zap.String("volume", req.Volume), zap.String("snapshot", req.SnapshotName))
	return &haifypb.CreateSnapshotResponse{
		Success: true,
		Message: "Snapshot created successfully",
	}, nil
}

func (s *Server) DeleteSnapshot(ctx context.Context, req *haifypb.DeleteSnapshotRequest) (*haifypb.DeleteSnapshotResponse, error) {
	log := s.serverLog()
	log.Debug("DeleteSnapshot: deleting", zap.String("volume", req.Volume), zap.String("snapshot", req.SnapshotName), zap.String("node", req.Node))
	err := s.snapshots.DeleteSnapshot(ctx, req.Volume, req.SnapshotName, req.Node)
	if err != nil {
		log.Debug("DeleteSnapshot: failed", zap.String("volume", req.Volume), zap.Error(err))
		return &haifypb.DeleteSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("DeleteSnapshot: deleted", zap.String("volume", req.Volume), zap.String("snapshot", req.SnapshotName))
	return &haifypb.DeleteSnapshotResponse{
		Success: true,
		Message: "Snapshot deleted successfully",
	}, nil
}

func (s *Server) RestoreSnapshot(ctx context.Context, req *haifypb.RestoreSnapshotRequest) (*haifypb.RestoreSnapshotResponse, error) {
	log := s.serverLog()
	log.Debug("RestoreSnapshot: restoring", zap.String("volume", req.Volume), zap.String("snapshot", req.SnapshotName), zap.String("node", req.Node))
	err := s.snapshots.RestoreSnapshot(ctx, req.Volume, req.SnapshotName, req.Node)
	if err != nil {
		log.Debug("RestoreSnapshot: failed", zap.String("volume", req.Volume), zap.Error(err))
		return &haifypb.RestoreSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("RestoreSnapshot: restored", zap.String("volume", req.Volume), zap.String("snapshot", req.SnapshotName))
	return &haifypb.RestoreSnapshotResponse{
		Success: true,
		Message: "Snapshot restored successfully",
	}, nil
}

// PopulateVolume fills a freshly created, still-empty resource from a snapshot
// or a source block device (another volume's backing store). It is the server
// side of restore-from-snapshot and volume cloning in CSI, Cinder and XCP-ng.
func (s *Server) PopulateVolume(ctx context.Context, req *haifypb.PopulateVolumeRequest) (*haifypb.PopulateVolumeResponse, error) {
	log := s.serverLog()
	log.Debug("PopulateVolume: populating", zap.String("resource", req.GetResource()), zap.Uint32("volume", req.GetVolumeId()),
		zap.String("source_device", req.GetSourceDevice()), zap.Bool("from_snapshot", req.GetSourceSnapshot() != nil),
		zap.String("node", req.GetNode()))
	var copied uint64
	var err error
	if snap := req.GetSourceSnapshot(); snap != nil {
		if req.GetSourceDevice() != "" {
			log.Debug("PopulateVolume: both a device and a snapshot given, refusing")
			err = fmt.Errorf("give a source device or a source snapshot, not both")
		} else {
			log.Debug("PopulateVolume: from snapshot", zap.String("source_resource", snap.GetResource()),
				zap.Uint32("source_volume", snap.GetVolumeId()), zap.String("snapshot", snap.GetName()),
				zap.Bool("replicated", snap.GetReplicated()))
			copied, err = s.snapshots.PopulateVolumeFromSnapshot(ctx, req.GetResource(), req.GetVolumeId(), SnapshotSource{
				Resource:   snap.GetResource(),
				VolumeID:   snap.GetVolumeId(),
				Name:       snap.GetName(),
				Replicated: snap.GetReplicated(),
			}, req.GetNode())
		}
	} else {
		log.Debug("PopulateVolume: from source device", zap.String("source_device", req.GetSourceDevice()))
		copied, err = s.snapshots.PopulateVolume(ctx, req.GetResource(), req.GetVolumeId(), req.GetSourceDevice(), req.GetNode())
	}
	if err != nil {
		log.Debug("PopulateVolume: failed", zap.String("resource", req.GetResource()), zap.Uint64("bytes_copied", copied), zap.Error(err))
		return &haifypb.PopulateVolumeResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("PopulateVolume: populated", zap.String("resource", req.GetResource()), zap.Uint64("bytes_copied", copied))
	return &haifypb.PopulateVolumeResponse{
		Success:     true,
		Message:     "Volume populated successfully",
		BytesCopied: copied,
	}, nil
}

func (s *Server) ListSnapshots(ctx context.Context, req *haifypb.ListSnapshotsRequest) (*haifypb.ListSnapshotsResponse, error) {
	log := s.serverLog()
	log.Debug("ListSnapshots: listing", zap.String("volume", req.Volume), zap.String("node", req.Node))
	snapshots, err := s.snapshots.ListSnapshots(ctx, req.Volume, req.Node)
	if err != nil {
		log.Debug("ListSnapshots: failed", zap.String("volume", req.Volume), zap.Error(err))
		return &haifypb.ListSnapshotsResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	var pbSnapshots []*haifypb.SnapshotInfo
	for _, snap := range snapshots {
		log.Debug("ListSnapshots: snapshot", zap.String("snapshot", snap.Name), zap.String("volume", snap.Volume))
		pbSnapshots = append(pbSnapshots, &haifypb.SnapshotInfo{
			Name:      snap.Name,
			Volume:    snap.Volume,
			SizeGb:    snap.SizeGB,
			CreatedAt: snap.CreatedAt,
			Origin:    snap.Origin,
		})
	}

	log.Debug("ListSnapshots: listed", zap.Int("count", len(pbSnapshots)))
	return &haifypb.ListSnapshotsResponse{
		Success:   true,
		Message:   "Snapshots listed successfully",
		Snapshots: pbSnapshots,
	}, nil
}

func (s *Server) CreateSnapshotSchedule(ctx context.Context, req *haifypb.CreateSnapshotScheduleRequest) (*haifypb.CreateSnapshotScheduleResponse, error) {
	log := s.serverLog()
	log.Debug("CreateSnapshotSchedule: saving", zap.String("resource", req.Resource), zap.String("cron", req.Cron),
		zap.Bool("enabled", req.Enabled), zap.Bool("has_lock_days", req.LockDays != nil))
	var lockDays *int
	if req.LockDays != nil {
		n := int(*req.LockDays)
		log.Debug("CreateSnapshotSchedule: lock days given", zap.Int("lock_days", n))
		lockDays = &n
	}
	err := s.ctrl.schedules.CreateSchedule(ctx, req.Resource, req.Cron, gfsFromProto(req.Keep), req.Enabled, lockDays)
	if err != nil {
		log.Debug("CreateSnapshotSchedule: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.CreateSnapshotScheduleResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("CreateSnapshotSchedule: saved", zap.String("resource", req.Resource))
	return &haifypb.CreateSnapshotScheduleResponse{
		Success: true,
		Message: fmt.Sprintf("Snapshot schedule for %q created", req.Resource),
	}, nil
}

func (s *Server) ListSnapshotSchedules(ctx context.Context, req *haifypb.ListSnapshotSchedulesRequest) (*haifypb.ListSnapshotSchedulesResponse, error) {
	log := s.serverLog()
	log.Debug("ListSnapshotSchedules: listing")
	schedules, err := s.ctrl.schedules.ListSchedules(ctx)
	if err != nil {
		log.Debug("ListSnapshotSchedules: failed", zap.Error(err))
		return &haifypb.ListSnapshotSchedulesResponse{Success: false, Message: err.Error()}, nil
	}
	now := time.Now()
	var out []*haifypb.SnapshotScheduleInfo
	for _, sc := range schedules {
		info := &haifypb.SnapshotScheduleInfo{
			Name:     sc.Name,
			Resource: sc.Resource,
			Cron:     sc.Cron,
			Enabled:  sc.Enabled,
			Keep:     gfsToProto(sc.Keep),
			LockDays: uint32(sc.LockDays),
		}
		if until := scheduleLockedUntil(sc, lockNow()); !until.IsZero() {
			info.LockedUntil = until.UTC().Format(time.RFC3339)
		}
		if until := scheduleFrozenUntil(sc, lockNow()); !until.IsZero() {
			info.FrozenUntil, info.FrozenReason = until.UTC().Format(time.RFC3339), sc.FrozenReason
		}
		if !sc.LastRun.IsZero() {
			info.LastRun = sc.LastRun.UTC().Format(time.RFC3339)
		}
		if sc.Enabled {
			if next := NextRun(sc.Cron, now); !next.IsZero() {
				info.NextRun = next.UTC().Format(time.RFC3339)
			}
		}
		log.Debug("ListSnapshotSchedules: schedule", zap.String("schedule", sc.Name), zap.String("resource", sc.Resource),
			zap.Bool("enabled", sc.Enabled), zap.String("locked_until", info.LockedUntil),
			zap.String("frozen_until", info.FrozenUntil), zap.String("next_run", info.NextRun))
		out = append(out, info)
	}
	log.Debug("ListSnapshotSchedules: listed", zap.Int("count", len(out)))
	return &haifypb.ListSnapshotSchedulesResponse{
		Success:   true,
		Message:   "Snapshot schedules listed successfully",
		Schedules: out,
	}, nil
}

func (s *Server) DeleteSnapshotSchedule(ctx context.Context, req *haifypb.DeleteSnapshotScheduleRequest) (*haifypb.DeleteSnapshotScheduleResponse, error) {
	log := s.serverLog()
	log.Debug("DeleteSnapshotSchedule: deleting", zap.String("schedule", req.Name))
	if err := s.ctrl.schedules.DeleteSchedule(ctx, req.Name); err != nil {
		log.Debug("DeleteSnapshotSchedule: failed", zap.String("schedule", req.Name), zap.Error(err))
		return &haifypb.DeleteSnapshotScheduleResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("DeleteSnapshotSchedule: deleted", zap.String("schedule", req.Name))
	return &haifypb.DeleteSnapshotScheduleResponse{
		Success: true,
		Message: fmt.Sprintf("Snapshot schedule %q deleted", req.Name),
	}, nil
}

// FreezeSnapshotSchedule freezes a resource's schedule (snapshot_freeze.go).
func (s *Server) FreezeSnapshotSchedule(ctx context.Context, req *haifypb.FreezeSnapshotScheduleRequest) (*haifypb.FreezeSnapshotScheduleResponse, error) {
	log := s.serverLog()
	log.Debug("FreezeSnapshotSchedule: freezing", zap.String("resource", req.Resource), zap.Any("hours", req.Hours),
		zap.String("reason", req.Reason))
	reason := req.Reason
	if reason == "" {
		log.Debug("FreezeSnapshotSchedule: no reason given, using the default")
		reason = "frozen by hand"
	}
	until, err := s.ctrl.schedules.FreezeSchedule(ctx, req.Resource, time.Duration(req.Hours)*time.Hour, reason)
	if err != nil {
		log.Debug("FreezeSnapshotSchedule: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.FreezeSnapshotScheduleResponse{Success: false, Message: err.Error()}, nil
	}
	at := until.UTC().Format(time.RFC3339)
	log.Debug("FreezeSnapshotSchedule: frozen", zap.String("resource", req.Resource), zap.String("until", at))
	return &haifypb.FreezeSnapshotScheduleResponse{Success: true, FrozenUntil: at,
		Message: fmt.Sprintf("snapshot schedule of %s frozen until %s", req.Resource, at)}, nil
}

// UnfreezeSnapshotSchedule ends a freeze early.
func (s *Server) UnfreezeSnapshotSchedule(ctx context.Context, req *haifypb.UnfreezeSnapshotScheduleRequest) (*haifypb.UnfreezeSnapshotScheduleResponse, error) {
	log := s.serverLog()
	log.Debug("UnfreezeSnapshotSchedule: unfreezing", zap.String("resource", req.Resource))
	if err := s.ctrl.schedules.UnfreezeSchedule(ctx, req.Resource); err != nil {
		log.Debug("UnfreezeSnapshotSchedule: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.UnfreezeSnapshotScheduleResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("UnfreezeSnapshotSchedule: unfrozen", zap.String("resource", req.Resource))
	return &haifypb.UnfreezeSnapshotScheduleResponse{Success: true,
		Message: fmt.Sprintf("snapshot schedule of %s unfrozen", req.Resource)}, nil
}

func gfsFromProto(p *haifypb.GFSRetention) database.GFSPolicy {
	if p == nil {
		zap.L().Debug("gfsFromProto: no retention given, keeping the zero policy")
		return database.GFSPolicy{}
	}
	zap.L().Debug("gfsFromProto: retention", zap.Int32("hourly", p.Hourly), zap.Int32("daily", p.Daily),
		zap.Int32("weekly", p.Weekly), zap.Int32("monthly", p.Monthly), zap.Int32("yearly", p.Yearly))
	return database.GFSPolicy{
		Hourly:  int(p.Hourly),
		Daily:   int(p.Daily),
		Weekly:  int(p.Weekly),
		Monthly: int(p.Monthly),
		Yearly:  int(p.Yearly),
	}
}

func gfsToProto(p database.GFSPolicy) *haifypb.GFSRetention {
	return &haifypb.GFSRetention{
		Hourly:  int32(p.Hourly),
		Daily:   int32(p.Daily),
		Weekly:  int32(p.Weekly),
		Monthly: int32(p.Monthly),
		Yearly:  int32(p.Yearly),
	}
}

func (s *Server) CreateLvmSnapshot(ctx context.Context, req *haifypb.CreateLvmSnapshotRequest) (*haifypb.CreateLvmSnapshotResponse, error) {
	log := s.serverLog()
	log.Debug("CreateLvmSnapshot: taking", zap.String("resource", req.Resource), zap.String("lv", req.LvName),
		zap.String("snapshot", req.SnapshotName), zap.String("node", req.Node), zap.String("size", req.Size))
	err := s.storage.CreateLvmSnapshot(ctx, req.Resource, req.LvName, req.SnapshotName, req.Node, req.Size)
	if err != nil {
		log.Debug("CreateLvmSnapshot: failed", zap.String("lv", req.LvName), zap.Error(err))
		return &haifypb.CreateLvmSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("CreateLvmSnapshot: taken", zap.String("lv", req.LvName), zap.String("snapshot", req.SnapshotName))
	return &haifypb.CreateLvmSnapshotResponse{
		Success: true,
		Message: "LVM snapshot created successfully",
	}, nil
}

func (s *Server) DeleteLvmSnapshot(ctx context.Context, req *haifypb.DeleteLvmSnapshotRequest) (*haifypb.DeleteLvmSnapshotResponse, error) {
	log := s.serverLog()
	log.Debug("DeleteLvmSnapshot: deleting", zap.String("lv", req.LvName), zap.String("snapshot", req.SnapshotName), zap.String("node", req.Node))
	err := s.storage.DeleteLvmSnapshot(ctx, req.LvName, req.SnapshotName, req.Node)
	if err != nil {
		log.Debug("DeleteLvmSnapshot: failed", zap.String("lv", req.LvName), zap.Error(err))
		return &haifypb.DeleteLvmSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("DeleteLvmSnapshot: deleted", zap.String("lv", req.LvName), zap.String("snapshot", req.SnapshotName))
	return &haifypb.DeleteLvmSnapshotResponse{
		Success: true,
		Message: "LVM snapshot deleted successfully",
	}, nil
}

func (s *Server) ListLvmSnapshots(ctx context.Context, req *haifypb.ListLvmSnapshotsRequest) (*haifypb.ListLvmSnapshotsResponse, error) {
	log := s.serverLog()
	log.Debug("ListLvmSnapshots: listing", zap.String("lv", req.LvName), zap.String("node", req.Node), zap.String("resource", req.Resource))
	snapshots, err := s.storage.ListLvmSnapshots(ctx, req.LvName, req.Node, req.Resource)
	if err != nil {
		log.Debug("ListLvmSnapshots: failed", zap.String("lv", req.LvName), zap.Error(err))
		return &haifypb.ListLvmSnapshotsResponse{
			Success:   false,
			Message:   err.Error(),
			Snapshots: nil,
		}, nil
	}
	// Convert to proto SnapshotInfo
	var protoSnapshots []*haifypb.SnapshotInfo
	for _, snap := range snapshots {
		log.Debug("ListLvmSnapshots: snapshot", zap.String("snapshot", snap.Name), zap.String("volume", snap.Volume))
		protoSnapshots = append(protoSnapshots, &haifypb.SnapshotInfo{
			Name:      snap.Name,
			Volume:    snap.Volume,
			SizeGb:    snap.SizeGB,
			CreatedAt: snap.CreatedAt,
			Origin:    snap.Origin,
		})
	}
	log.Debug("ListLvmSnapshots: listed", zap.Int("count", len(protoSnapshots)))
	return &haifypb.ListLvmSnapshotsResponse{
		Success:   true,
		Message:   "LVM snapshots listed successfully",
		Snapshots: protoSnapshots,
	}, nil
}

func (s *Server) RestoreLvmSnapshot(ctx context.Context, req *haifypb.RestoreLvmSnapshotRequest) (*haifypb.RestoreLvmSnapshotResponse, error) {
	log := s.serverLog()
	log.Debug("RestoreLvmSnapshot: restoring", zap.String("lv", req.LvName), zap.String("snapshot", req.SnapshotName), zap.String("node", req.Node))
	err := s.storage.RestoreLvmSnapshot(ctx, req.LvName, req.SnapshotName, req.Node)
	if err != nil {
		log.Debug("RestoreLvmSnapshot: failed", zap.String("lv", req.LvName), zap.Error(err))
		return &haifypb.RestoreLvmSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("RestoreLvmSnapshot: restored", zap.String("lv", req.LvName), zap.String("snapshot", req.SnapshotName))
	return &haifypb.RestoreLvmSnapshotResponse{
		Success: true,
		Message: "LVM snapshot restored successfully",
	}, nil
}
