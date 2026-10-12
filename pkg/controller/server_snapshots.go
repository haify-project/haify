package controller

import (
	"context"
	"fmt"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
)

func (s *Server) CreateSnapshot(ctx context.Context, req *haifypb.CreateSnapshotRequest) (*haifypb.CreateSnapshotResponse, error) {
	err := s.snapshots.CreateSnapshot(ctx, req.Volume, req.SnapshotName, req.Node)
	if err != nil {
		return &haifypb.CreateSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.CreateSnapshotResponse{
		Success: true,
		Message: "Snapshot created successfully",
	}, nil
}

func (s *Server) DeleteSnapshot(ctx context.Context, req *haifypb.DeleteSnapshotRequest) (*haifypb.DeleteSnapshotResponse, error) {
	err := s.snapshots.DeleteSnapshot(ctx, req.Volume, req.SnapshotName, req.Node)
	if err != nil {
		return &haifypb.DeleteSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.DeleteSnapshotResponse{
		Success: true,
		Message: "Snapshot deleted successfully",
	}, nil
}

func (s *Server) RestoreSnapshot(ctx context.Context, req *haifypb.RestoreSnapshotRequest) (*haifypb.RestoreSnapshotResponse, error) {
	err := s.snapshots.RestoreSnapshot(ctx, req.Volume, req.SnapshotName, req.Node)
	if err != nil {
		return &haifypb.RestoreSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.RestoreSnapshotResponse{
		Success: true,
		Message: "Snapshot restored successfully",
	}, nil
}

// PopulateVolume fills a freshly created, still-empty resource from a snapshot
// or a source block device (another volume's backing store). It is the server
// side of restore-from-snapshot and volume cloning in CSI, Cinder and XCP-ng.
func (s *Server) PopulateVolume(ctx context.Context, req *haifypb.PopulateVolumeRequest) (*haifypb.PopulateVolumeResponse, error) {
	var copied uint64
	var err error
	if snap := req.GetSourceSnapshot(); snap != nil {
		if req.GetSourceDevice() != "" {
			err = fmt.Errorf("give a source device or a source snapshot, not both")
		} else {
			copied, err = s.snapshots.PopulateVolumeFromSnapshot(ctx, req.GetResource(), req.GetVolumeId(), SnapshotSource{
				Resource:   snap.GetResource(),
				VolumeID:   snap.GetVolumeId(),
				Name:       snap.GetName(),
				Replicated: snap.GetReplicated(),
			}, req.GetNode())
		}
	} else {
		copied, err = s.snapshots.PopulateVolume(ctx, req.GetResource(), req.GetVolumeId(), req.GetSourceDevice(), req.GetNode())
	}
	if err != nil {
		return &haifypb.PopulateVolumeResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.PopulateVolumeResponse{
		Success:     true,
		Message:     "Volume populated successfully",
		BytesCopied: copied,
	}, nil
}

func (s *Server) ListSnapshots(ctx context.Context, req *haifypb.ListSnapshotsRequest) (*haifypb.ListSnapshotsResponse, error) {
	snapshots, err := s.snapshots.ListSnapshots(ctx, req.Volume, req.Node)
	if err != nil {
		return &haifypb.ListSnapshotsResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	var pbSnapshots []*haifypb.SnapshotInfo
	for _, snap := range snapshots {
		pbSnapshots = append(pbSnapshots, &haifypb.SnapshotInfo{
			Name:      snap.Name,
			Volume:    snap.Volume,
			SizeGb:    snap.SizeGB,
			CreatedAt: snap.CreatedAt,
			Origin:    snap.Origin,
		})
	}

	return &haifypb.ListSnapshotsResponse{
		Success:   true,
		Message:   "Snapshots listed successfully",
		Snapshots: pbSnapshots,
	}, nil
}

func (s *Server) CreateSnapshotSchedule(ctx context.Context, req *haifypb.CreateSnapshotScheduleRequest) (*haifypb.CreateSnapshotScheduleResponse, error) {
	var lockDays *int
	if req.LockDays != nil {
		n := int(*req.LockDays)
		lockDays = &n
	}
	err := s.ctrl.schedules.CreateSchedule(ctx, req.Resource, req.Cron, gfsFromProto(req.Keep), req.Enabled, lockDays)
	if err != nil {
		return &haifypb.CreateSnapshotScheduleResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.CreateSnapshotScheduleResponse{
		Success: true,
		Message: fmt.Sprintf("Snapshot schedule for %q created", req.Resource),
	}, nil
}

func (s *Server) ListSnapshotSchedules(ctx context.Context, req *haifypb.ListSnapshotSchedulesRequest) (*haifypb.ListSnapshotSchedulesResponse, error) {
	schedules, err := s.ctrl.schedules.ListSchedules(ctx)
	if err != nil {
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
		out = append(out, info)
	}
	return &haifypb.ListSnapshotSchedulesResponse{
		Success:   true,
		Message:   "Snapshot schedules listed successfully",
		Schedules: out,
	}, nil
}

func (s *Server) DeleteSnapshotSchedule(ctx context.Context, req *haifypb.DeleteSnapshotScheduleRequest) (*haifypb.DeleteSnapshotScheduleResponse, error) {
	if err := s.ctrl.schedules.DeleteSchedule(ctx, req.Name); err != nil {
		return &haifypb.DeleteSnapshotScheduleResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.DeleteSnapshotScheduleResponse{
		Success: true,
		Message: fmt.Sprintf("Snapshot schedule %q deleted", req.Name),
	}, nil
}

// FreezeSnapshotSchedule freezes a resource's schedule (snapshot_freeze.go).
func (s *Server) FreezeSnapshotSchedule(ctx context.Context, req *haifypb.FreezeSnapshotScheduleRequest) (*haifypb.FreezeSnapshotScheduleResponse, error) {
	reason := req.Reason
	if reason == "" {
		reason = "frozen by hand"
	}
	until, err := s.ctrl.schedules.FreezeSchedule(ctx, req.Resource, time.Duration(req.Hours)*time.Hour, reason)
	if err != nil {
		return &haifypb.FreezeSnapshotScheduleResponse{Success: false, Message: err.Error()}, nil
	}
	at := until.UTC().Format(time.RFC3339)
	return &haifypb.FreezeSnapshotScheduleResponse{Success: true, FrozenUntil: at,
		Message: fmt.Sprintf("snapshot schedule of %s frozen until %s", req.Resource, at)}, nil
}

// UnfreezeSnapshotSchedule ends a freeze early.
func (s *Server) UnfreezeSnapshotSchedule(ctx context.Context, req *haifypb.UnfreezeSnapshotScheduleRequest) (*haifypb.UnfreezeSnapshotScheduleResponse, error) {
	if err := s.ctrl.schedules.UnfreezeSchedule(ctx, req.Resource); err != nil {
		return &haifypb.UnfreezeSnapshotScheduleResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.UnfreezeSnapshotScheduleResponse{Success: true,
		Message: fmt.Sprintf("snapshot schedule of %s unfrozen", req.Resource)}, nil
}

func gfsFromProto(p *haifypb.GFSRetention) database.GFSPolicy {
	if p == nil {
		return database.GFSPolicy{}
	}
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
	err := s.storage.CreateLvmSnapshot(ctx, req.Resource, req.LvName, req.SnapshotName, req.Node, req.Size)
	if err != nil {
		return &haifypb.CreateLvmSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.CreateLvmSnapshotResponse{
		Success: true,
		Message: "LVM snapshot created successfully",
	}, nil
}

func (s *Server) DeleteLvmSnapshot(ctx context.Context, req *haifypb.DeleteLvmSnapshotRequest) (*haifypb.DeleteLvmSnapshotResponse, error) {
	err := s.storage.DeleteLvmSnapshot(ctx, req.LvName, req.SnapshotName, req.Node)
	if err != nil {
		return &haifypb.DeleteLvmSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.DeleteLvmSnapshotResponse{
		Success: true,
		Message: "LVM snapshot deleted successfully",
	}, nil
}

func (s *Server) ListLvmSnapshots(ctx context.Context, req *haifypb.ListLvmSnapshotsRequest) (*haifypb.ListLvmSnapshotsResponse, error) {
	snapshots, err := s.storage.ListLvmSnapshots(ctx, req.LvName, req.Node, req.Resource)
	if err != nil {
		return &haifypb.ListLvmSnapshotsResponse{
			Success:   false,
			Message:   err.Error(),
			Snapshots: nil,
		}, nil
	}
	// Convert to proto SnapshotInfo
	var protoSnapshots []*haifypb.SnapshotInfo
	for _, snap := range snapshots {
		protoSnapshots = append(protoSnapshots, &haifypb.SnapshotInfo{
			Name:      snap.Name,
			Volume:    snap.Volume,
			SizeGb:    snap.SizeGB,
			CreatedAt: snap.CreatedAt,
			Origin:    snap.Origin,
		})
	}
	return &haifypb.ListLvmSnapshotsResponse{
		Success:   true,
		Message:   "LVM snapshots listed successfully",
		Snapshots: protoSnapshots,
	}, nil
}

func (s *Server) RestoreLvmSnapshot(ctx context.Context, req *haifypb.RestoreLvmSnapshotRequest) (*haifypb.RestoreLvmSnapshotResponse, error) {
	err := s.storage.RestoreLvmSnapshot(ctx, req.LvName, req.SnapshotName, req.Node)
	if err != nil {
		return &haifypb.RestoreLvmSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.RestoreLvmSnapshotResponse{
		Success: true,
		Message: "LVM snapshot restored successfully",
	}, nil
}
