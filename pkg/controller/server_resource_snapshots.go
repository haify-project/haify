package controller

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// Resource snapshot RPCs (resource_snapshot.go).

func (s *Server) CreateResourceSnapshot(ctx context.Context, req *haifypb.CreateResourceSnapshotRequest) (*haifypb.CreateResourceSnapshotResponse, error) {
	log := s.serverLog()
	log.Debug("CreateResourceSnapshot: taking snapshot", zap.String("resource", req.Resource), zap.String("snapshot", req.Name))
	if err := s.resources.CreateResourceSnapshot(ctx, req.Resource, req.Name); err != nil {
		log.Debug("CreateResourceSnapshot: failed", zap.String("resource", req.Resource), zap.String("snapshot", req.Name), zap.Error(err))
		return &haifypb.CreateResourceSnapshotResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("CreateResourceSnapshot: taken", zap.String("resource", req.Resource), zap.String("snapshot", req.Name))
	return &haifypb.CreateResourceSnapshotResponse{Success: true,
		Message: fmt.Sprintf("snapshot %s of %s taken on every replica", req.Name, req.Resource)}, nil
}

func (s *Server) ListResourceSnapshots(ctx context.Context, req *haifypb.ListResourceSnapshotsRequest) (*haifypb.ListResourceSnapshotsResponse, error) {
	log := s.serverLog()
	log.Debug("ListResourceSnapshots: listing", zap.String("resource", req.Resource))
	names, err := s.resources.ListResourceSnapshots(ctx, req.Resource)
	if err != nil {
		log.Debug("ListResourceSnapshots: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.ListResourceSnapshotsResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("ListResourceSnapshots: listed", zap.String("resource", req.Resource), zap.Strings("snapshots", names))
	return &haifypb.ListResourceSnapshotsResponse{Success: true, Names: names}, nil
}

func (s *Server) RollbackResourceSnapshot(ctx context.Context, req *haifypb.RollbackResourceSnapshotRequest) (*haifypb.RollbackResourceSnapshotResponse, error) {
	log := s.serverLog()
	log.Debug("RollbackResourceSnapshot: rolling back", zap.String("resource", req.Resource), zap.String("snapshot", req.Name))
	if err := s.resources.RollbackResourceSnapshot(ctx, req.Resource, req.Name); err != nil {
		log.Debug("RollbackResourceSnapshot: failed", zap.String("resource", req.Resource), zap.String("snapshot", req.Name), zap.Error(err))
		return &haifypb.RollbackResourceSnapshotResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("RollbackResourceSnapshot: rolled back", zap.String("resource", req.Resource), zap.String("snapshot", req.Name))
	return &haifypb.RollbackResourceSnapshotResponse{Success: true,
		Message: fmt.Sprintf("%s rolled back to %s on every replica", req.Resource, req.Name)}, nil
}

func (s *Server) DeleteResourceSnapshot(ctx context.Context, req *haifypb.DeleteResourceSnapshotRequest) (*haifypb.DeleteResourceSnapshotResponse, error) {
	log := s.serverLog()
	log.Debug("DeleteResourceSnapshot: deleting", zap.String("resource", req.Resource), zap.String("snapshot", req.Name))
	if err := s.resources.DeleteResourceSnapshot(ctx, req.Resource, req.Name); err != nil {
		log.Debug("DeleteResourceSnapshot: failed", zap.String("resource", req.Resource), zap.String("snapshot", req.Name), zap.Error(err))
		return &haifypb.DeleteResourceSnapshotResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("DeleteResourceSnapshot: deleted", zap.String("resource", req.Resource), zap.String("snapshot", req.Name))
	return &haifypb.DeleteResourceSnapshotResponse{Success: true,
		Message: fmt.Sprintf("snapshot %s of %s deleted", req.Name, req.Resource)}, nil
}
