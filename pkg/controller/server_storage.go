package controller

import (
	"context"
	"fmt"
	"strings"

	pb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
)

// gRPC handlers for storage upkeep: trimming, disks, and storage jobs.

func (s *Server) TrimPools(ctx context.Context, req *pb.TrimPoolsRequest) (*pb.TrimPoolsResponse, error) {
	results, err := s.storage.Trim(ctx, req.Node)
	if err != nil {
		return &pb.TrimPoolsResponse{Success: false, Message: err.Error()}, nil
	}
	msg, failed := trimSummary(results)
	resp := &pb.TrimPoolsResponse{Success: !failed, Message: msg}
	for _, r := range results {
		resp.Results = append(resp.Results, &pb.TrimResultInfo{Node: r.Node, Mount: r.Mount, Bytes: r.Bytes, Error: r.Err})
	}
	return resp, nil
}

func (s *Server) ListPoolDisks(ctx context.Context, req *pb.ListPoolDisksRequest) (*pb.ListPoolDisksResponse, error) {
	disks, err := s.storage.DiskHealth(ctx, req.Pool, req.Node)
	if err != nil {
		return &pb.ListPoolDisksResponse{Success: false, Message: err.Error()}, nil
	}
	resp := &pb.ListPoolDisksResponse{Success: true, Message: fmt.Sprintf("%d disk(s)", len(disks))}
	for _, d := range disks {
		resp.Disks = append(resp.Disks, &pb.PoolDiskInfo{Node: d.Node, Pool: d.Pool, Device: d.PV, SizeBytes: d.SizeBytes,
			UsedBytes: d.UsedBytes, Health: d.Status, HealthDetail: d.Detail, Model: d.Model, Serial: d.Serial})
	}
	return resp, nil
}

func (s *Server) ListNodeDisks(ctx context.Context, req *pb.ListNodeDisksRequest) (*pb.ListNodeDisksResponse, error) {
	disks, err := s.storage.ListNodeDisks(ctx, req.Node)
	if err != nil {
		return &pb.ListNodeDisksResponse{Success: false, Message: err.Error()}, nil
	}
	resp := &pb.ListNodeDisksResponse{Success: true, Message: fmt.Sprintf("%d disk(s)", len(disks))}
	for _, d := range disks {
		resp.Disks = append(resp.Disks, &pb.NodeDiskInfo{Node: d.Node, Device: d.Device, ById: d.ByID, Type: d.Type,
			SizeBytes: d.SizeBytes, Model: d.Model, Serial: d.Serial, Transport: d.Transport, Rotational: d.Rotational,
			State: d.State, Pool: d.Pool, Detail: d.Detail})
	}
	return resp, nil
}

func (s *Server) RemovePoolDisk(ctx context.Context, req *pb.RemovePoolDiskRequest) (*pb.StorageJobResponse, error) {
	id, err := s.storage.StartDiskJob(ctx, req.Pool, req.Node, req.Disk, "", false)
	if err != nil {
		return &pb.StorageJobResponse{Success: false, Message: err.Error()}, nil
	}
	return &pb.StorageJobResponse{Success: true, JobId: id,
		Message: fmt.Sprintf("moving the data off %s; it leaves the pool when that is done (haify pool jobs)", req.Disk)}, nil
}

func (s *Server) ReplacePoolDisk(ctx context.Context, req *pb.ReplacePoolDiskRequest) (*pb.StorageJobResponse, error) {
	if strings.TrimSpace(req.NewDisk) == "" {
		return &pb.StorageJobResponse{Success: false, Message: "new_disk is required"}, nil
	}
	id, err := s.storage.StartDiskJob(ctx, req.Pool, req.Node, req.OldDisk, req.NewDisk, req.Wipe)
	if err != nil {
		return &pb.StorageJobResponse{Success: false, Message: err.Error()}, nil
	}
	return &pb.StorageJobResponse{Success: true, JobId: id,
		Message: fmt.Sprintf("moving the data from %s to %s; %s leaves the pool when that is done (haify pool jobs)", req.OldDisk, req.NewDisk, req.OldDisk)}, nil
}

func (s *Server) MoveVolume(ctx context.Context, req *pb.MoveVolumeRequest) (*pb.StorageJobResponse, error) {
	id, err := s.resources.MoveVolume(ctx, req.Resource, int(req.VolumeId), req.Pool)
	if err != nil {
		return &pb.StorageJobResponse{Success: false, Message: err.Error()}, nil
	}
	return &pb.StorageJobResponse{Success: true, JobId: id,
		Message: fmt.Sprintf("moving volume %d of %s to %s one node at a time; it keeps serving meanwhile (haify pool jobs)",
			req.VolumeId, req.Resource, normalizeManagedName(req.Pool))}, nil
}

func (s *Server) ListStorageJobs(ctx context.Context, req *pb.ListStorageJobsRequest) (*pb.ListStorageJobsResponse, error) {
	jobs, err := s.ctrl.db.ListStorageJobs(ctx)
	if err != nil {
		return &pb.ListStorageJobsResponse{Success: false, Message: err.Error()}, nil
	}
	resp := &pb.ListStorageJobsResponse{Success: true}
	for _, j := range jobs {
		if j.State != database.JobRunning && !req.IncludeFinished {
			continue
		}
		resp.Jobs = append(resp.Jobs, &pb.StorageJobInfo{Id: j.ID, Kind: j.Kind, State: j.State, Subject: jobSubject(j),
			Progress: j.Progress, Message: j.Message, StartedUnix: j.StartedAt.Unix(), UpdatedUnix: j.UpdatedAt.Unix()})
	}
	return resp, nil
}

func jobSubject(j *database.StorageJob) string {
	switch j.Kind {
	case database.JobRemoveDisk:
		return fmt.Sprintf("%s on %s: remove %s", j.Pool, j.Node, j.Disk)
	case database.JobReplaceDisk:
		return fmt.Sprintf("%s on %s: %s → %s", j.Pool, j.Node, j.Disk, j.NewDisk)
	case database.JobMoveVolume:
		return fmt.Sprintf("%s volume %d: %s → %s", j.Resource, j.VolumeID, j.FromPool, j.TargetPool)
	case database.JobShip:
		return fmt.Sprintf("%s → %s on %s", j.Resource, j.TargetName, j.Target)
	}
	return j.Kind
}
