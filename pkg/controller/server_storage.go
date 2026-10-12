package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	pb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
)

// gRPC handlers for storage upkeep: trimming, disks, and storage jobs.

func (s *Server) TrimPools(ctx context.Context, req *pb.TrimPoolsRequest) (*pb.TrimPoolsResponse, error) {
	log := s.serverLog()
	log.Debug("TrimPools: trimming", zap.String("node", req.Node))
	results, err := s.storage.Trim(ctx, req.Node)
	if err != nil {
		log.Debug("TrimPools: failed", zap.String("node", req.Node), zap.Error(err))
		return &pb.TrimPoolsResponse{Success: false, Message: err.Error()}, nil
	}
	msg, failed := trimSummary(results)
	resp := &pb.TrimPoolsResponse{Success: !failed, Message: msg}
	for _, r := range results {
		log.Debug("TrimPools: result", zap.String("node", r.Node), zap.String("mount", r.Mount),
			zap.Uint64("bytes", r.Bytes), zap.String("error", r.Err))
		resp.Results = append(resp.Results, &pb.TrimResultInfo{Node: r.Node, Mount: r.Mount, Bytes: r.Bytes, Error: r.Err})
	}
	log.Debug("TrimPools: done", zap.Int("results", len(results)), zap.Bool("failed", failed))
	return resp, nil
}

func (s *Server) ListPoolDisks(ctx context.Context, req *pb.ListPoolDisksRequest) (*pb.ListPoolDisksResponse, error) {
	log := s.serverLog()
	log.Debug("ListPoolDisks: reading disk health", zap.String("pool", req.Pool), zap.String("node", req.Node))
	disks, err := s.storage.DiskHealth(ctx, req.Pool, req.Node)
	if err != nil {
		log.Debug("ListPoolDisks: failed", zap.String("pool", req.Pool), zap.Error(err))
		return &pb.ListPoolDisksResponse{Success: false, Message: err.Error()}, nil
	}
	resp := &pb.ListPoolDisksResponse{Success: true, Message: fmt.Sprintf("%d disk(s)", len(disks))}
	for _, d := range disks {
		log.Debug("ListPoolDisks: disk", zap.String("node", d.Node), zap.String("pool", d.Pool), zap.String("device", d.PV),
			zap.String("health", d.Status))
		resp.Disks = append(resp.Disks, &pb.PoolDiskInfo{Node: d.Node, Pool: d.Pool, Device: d.PV, SizeBytes: d.SizeBytes,
			UsedBytes: d.UsedBytes, Health: d.Status, HealthDetail: d.Detail, Model: d.Model, Serial: d.Serial})
	}
	log.Debug("ListPoolDisks: listed", zap.Int("count", len(resp.Disks)))
	return resp, nil
}

func (s *Server) ListNodeDisks(ctx context.Context, req *pb.ListNodeDisksRequest) (*pb.ListNodeDisksResponse, error) {
	log := s.serverLog()
	log.Debug("ListNodeDisks: listing", zap.String("node", req.Node))
	disks, err := s.storage.ListNodeDisks(ctx, req.Node)
	if err != nil {
		log.Debug("ListNodeDisks: failed", zap.String("node", req.Node), zap.Error(err))
		return &pb.ListNodeDisksResponse{Success: false, Message: err.Error()}, nil
	}
	resp := &pb.ListNodeDisksResponse{Success: true, Message: fmt.Sprintf("%d disk(s)", len(disks))}
	for _, d := range disks {
		log.Debug("ListNodeDisks: disk", zap.String("node", d.Node), zap.String("device", d.Device),
			zap.String("state", d.State), zap.String("pool", d.Pool))
		resp.Disks = append(resp.Disks, &pb.NodeDiskInfo{Node: d.Node, Device: d.Device, ById: d.ByID, Type: d.Type,
			SizeBytes: d.SizeBytes, Model: d.Model, Serial: d.Serial, Transport: d.Transport, Rotational: d.Rotational,
			State: d.State, Pool: d.Pool, Detail: d.Detail})
	}
	log.Debug("ListNodeDisks: listed", zap.String("node", req.Node), zap.Int("count", len(resp.Disks)))
	return resp, nil
}

func (s *Server) RemovePoolDisk(ctx context.Context, req *pb.RemovePoolDiskRequest) (*pb.StorageJobResponse, error) {
	log := s.serverLog()
	log.Debug("RemovePoolDisk: starting job", zap.String("pool", req.Pool), zap.String("node", req.Node), zap.String("disk", req.Disk))
	id, err := s.storage.StartDiskJob(ctx, req.Pool, req.Node, req.Disk, "", false)
	if err != nil {
		log.Debug("RemovePoolDisk: failed to start", zap.String("pool", req.Pool), zap.Error(err))
		return &pb.StorageJobResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("RemovePoolDisk: started", zap.String("job", id))
	return &pb.StorageJobResponse{Success: true, JobId: id,
		Message: fmt.Sprintf("moving the data off %s; it leaves the pool when that is done (haify pool jobs)", req.Disk)}, nil
}

func (s *Server) ReplacePoolDisk(ctx context.Context, req *pb.ReplacePoolDiskRequest) (*pb.StorageJobResponse, error) {
	log := s.serverLog()
	log.Debug("ReplacePoolDisk: starting job", zap.String("pool", req.Pool), zap.String("node", req.Node),
		zap.String("old_disk", req.OldDisk), zap.String("new_disk", req.NewDisk), zap.Bool("wipe", req.Wipe))
	if strings.TrimSpace(req.NewDisk) == "" {
		log.Debug("ReplacePoolDisk: no new disk given")
		return &pb.StorageJobResponse{Success: false, Message: "new_disk is required"}, nil
	}
	id, err := s.storage.StartDiskJob(ctx, req.Pool, req.Node, req.OldDisk, req.NewDisk, req.Wipe)
	if err != nil {
		log.Debug("ReplacePoolDisk: failed to start", zap.String("pool", req.Pool), zap.Error(err))
		return &pb.StorageJobResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("ReplacePoolDisk: started", zap.String("job", id))
	return &pb.StorageJobResponse{Success: true, JobId: id,
		Message: fmt.Sprintf("moving the data from %s to %s; %s leaves the pool when that is done (haify pool jobs)", req.OldDisk, req.NewDisk, req.OldDisk)}, nil
}

func (s *Server) MoveVolume(ctx context.Context, req *pb.MoveVolumeRequest) (*pb.StorageJobResponse, error) {
	log := s.serverLog()
	log.Debug("MoveVolume: starting job", zap.String("resource", req.Resource), zap.Any("volume", req.VolumeId), zap.String("pool", req.Pool))
	id, err := s.resources.MoveVolume(ctx, req.Resource, int(req.VolumeId), req.Pool)
	if err != nil {
		log.Debug("MoveVolume: failed to start", zap.String("resource", req.Resource), zap.Error(err))
		return &pb.StorageJobResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("MoveVolume: started", zap.String("job", id))
	return &pb.StorageJobResponse{Success: true, JobId: id,
		Message: fmt.Sprintf("moving volume %d of %s to %s one node at a time; it keeps serving meanwhile (haify pool jobs)",
			req.VolumeId, req.Resource, normalizeManagedName(req.Pool))}, nil
}

func (s *Server) ListStorageJobs(ctx context.Context, req *pb.ListStorageJobsRequest) (*pb.ListStorageJobsResponse, error) {
	log := s.serverLog()
	log.Debug("ListStorageJobs: listing", zap.Bool("include_finished", req.IncludeFinished))
	start := time.Now()
	jobs, err := s.ctrl.db.ListStorageJobs(ctx)
	if err != nil {
		log.Debug("ListStorageJobs: database read failed", zap.Duration("took", time.Since(start)), zap.Error(err))
		return &pb.ListStorageJobsResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("ListStorageJobs: read from database", zap.Int("count", len(jobs)), zap.Duration("took", time.Since(start)))
	resp := &pb.ListStorageJobsResponse{Success: true}
	for _, j := range jobs {
		if j.State != database.JobRunning && !req.IncludeFinished {
			log.Debug("ListStorageJobs: skipping finished job", zap.String("job", j.ID), zap.String("state", j.State))
			continue
		}
		log.Debug("ListStorageJobs: job", zap.String("job", j.ID), zap.String("kind", j.Kind), zap.String("state", j.State))
		resp.Jobs = append(resp.Jobs, &pb.StorageJobInfo{Id: j.ID, Kind: j.Kind, State: j.State, Subject: jobSubject(j),
			Progress: j.Progress, Message: j.Message, StartedUnix: j.StartedAt.Unix(), UpdatedUnix: j.UpdatedAt.Unix()})
	}
	log.Debug("ListStorageJobs: listed", zap.Int("count", len(resp.Jobs)))
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
	zap.L().Debug("jobSubject: unknown job kind, using the kind", zap.String("job", j.ID), zap.String("kind", j.Kind))
	return j.Kind
}
