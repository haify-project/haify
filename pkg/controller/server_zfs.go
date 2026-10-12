package controller

import (
	"context"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

func (s *Server) CreateZFSPool(ctx context.Context, req *haifypb.CreateZFSPoolRequest) (*haifypb.CreateZFSPoolResponse, error) {
	log := s.serverLog()
	log.Debug("CreateZFSPool: creating", zap.String("pool", req.Name), zap.String("node", req.Node), zap.Strings("vdevs", req.Vdevs),
		zap.String("compression", req.Compression), zap.Bool("dedup", req.Dedup), zap.Bool("wipe", req.Wipe))
	err := s.storage.CreateZFSPool(ctx, req.Name, req.Node, req.Vdevs, req.Compression, req.Dedup, req.Wipe)
	if err != nil {
		log.Debug("CreateZFSPool: failed", zap.String("pool", req.Name), zap.Error(err))
		return &haifypb.CreateZFSPoolResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("CreateZFSPool: created", zap.String("pool", req.Name), zap.String("node", req.Node))
	return &haifypb.CreateZFSPoolResponse{
		Success: true,
		Message: "ZFS pool created successfully",
	}, nil
}

func (s *Server) DeleteZFSPool(ctx context.Context, req *haifypb.DeleteZFSPoolRequest) (*haifypb.DeleteZFSPoolResponse, error) {
	log := s.serverLog()
	log.Debug("DeleteZFSPool: deleting", zap.String("pool", req.Name), zap.String("node", req.Node))
	err := s.storage.DeleteZFSPool(ctx, req.Name, req.Node)
	if err != nil {
		log.Debug("DeleteZFSPool: failed", zap.String("pool", req.Name), zap.Error(err))
		return &haifypb.DeleteZFSPoolResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("DeleteZFSPool: deleted", zap.String("pool", req.Name))
	return &haifypb.DeleteZFSPoolResponse{
		Success: true,
		Message: "ZFS pool deleted successfully",
	}, nil
}

func (s *Server) ListZFSpools(ctx context.Context, req *haifypb.ListZFSPoolsRequest) (*haifypb.ListZFSPoolsResponse, error) {
	log := s.serverLog()
	log.Debug("ListZFSpools: listing")
	pools, err := s.storage.ListZFSpools(ctx)
	if err != nil {
		log.Debug("ListZFSpools: failed", zap.Error(err))
		return &haifypb.ListZFSPoolsResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	var pbPools []*haifypb.PoolInfo
	for _, p := range pools {
		log.Debug("ListZFSpools: pool", zap.String("pool", p.Name), zap.String("node", p.Node))
		pbPools = append(pbPools, pbPoolInfo(p))
	}

	log.Debug("ListZFSpools: listed", zap.Int("count", len(pbPools)))
	return &haifypb.ListZFSPoolsResponse{
		Success: true,
		Message: "ZFS pools listed successfully",
		Pools:   pbPools,
	}, nil
}

func (s *Server) CreateZFSDataset(ctx context.Context, req *haifypb.CreateZFSDatasetRequest) (*haifypb.CreateZFSDatasetResponse, error) {
	log := s.serverLog()
	log.Debug("CreateZFSDataset: creating", zap.String("dataset", req.DatasetPath), zap.String("node", req.Node))
	err := s.storage.CreateZFSDataset(ctx, req.DatasetPath, req.Node)
	if err != nil {
		log.Debug("CreateZFSDataset: failed", zap.String("dataset", req.DatasetPath), zap.Error(err))
		return &haifypb.CreateZFSDatasetResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("CreateZFSDataset: created", zap.String("dataset", req.DatasetPath))
	return &haifypb.CreateZFSDatasetResponse{
		Success: true,
		Message: "ZFS dataset created successfully",
	}, nil
}

func (s *Server) CreateZFSVolume(ctx context.Context, req *haifypb.CreateZFSVolumeRequest) (*haifypb.CreateZFSVolumeResponse, error) {
	log := s.serverLog()
	log.Debug("CreateZFSVolume: creating", zap.String("pool", req.PoolName), zap.String("volume", req.VolumeName),
		zap.String("size", req.Size), zap.String("node", req.Node))
	err := s.storage.CreateZFSThinVolume(ctx, req.PoolName, req.VolumeName, req.Size, req.Node)
	if err != nil {
		log.Debug("CreateZFSVolume: failed", zap.String("volume", req.VolumeName), zap.Error(err))
		return &haifypb.CreateZFSVolumeResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("CreateZFSVolume: created", zap.String("pool", req.PoolName), zap.String("volume", req.VolumeName))
	return &haifypb.CreateZFSVolumeResponse{
		Success: true,
		Message: "ZFS volume created successfully",
	}, nil
}

func (s *Server) ResizeZFSVolume(ctx context.Context, req *haifypb.ResizeZFSVolumeRequest) (*haifypb.ResizeZFSVolumeResponse, error) {
	log := s.serverLog()
	log.Debug("ResizeZFSVolume: resizing", zap.String("volume", req.VolumePath), zap.String("size", req.NewSize), zap.String("node", req.Node))
	err := s.storage.ZFSResizeVolume(ctx, req.VolumePath, req.NewSize, req.Node)
	if err != nil {
		log.Debug("ResizeZFSVolume: failed", zap.String("volume", req.VolumePath), zap.Error(err))
		return &haifypb.ResizeZFSVolumeResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("ResizeZFSVolume: resized", zap.String("volume", req.VolumePath), zap.String("size", req.NewSize))
	return &haifypb.ResizeZFSVolumeResponse{
		Success: true,
		Message: "ZFS volume resized successfully",
	}, nil
}

func (s *Server) DeleteZFSDataset(ctx context.Context, req *haifypb.DeleteZFSDatasetRequest) (*haifypb.DeleteZFSDatasetResponse, error) {
	log := s.serverLog()
	log.Debug("DeleteZFSDataset: destroying", zap.String("dataset", req.DatasetPath), zap.String("node", req.Node))
	// Use ZFS destroy for both datasets and volumes
	err := s.storage.ZFSDeleteDataset(ctx, req.DatasetPath, req.Node)
	if err != nil {
		log.Debug("DeleteZFSDataset: failed", zap.String("dataset", req.DatasetPath), zap.Error(err))
		return &haifypb.DeleteZFSDatasetResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("DeleteZFSDataset: destroyed", zap.String("dataset", req.DatasetPath))
	return &haifypb.DeleteZFSDatasetResponse{
		Success: true,
		Message: "ZFS dataset deleted successfully",
	}, nil
}

func (s *Server) CreateZFSSnapshot(ctx context.Context, req *haifypb.CreateZFSSnapshotRequest) (*haifypb.CreateZFSSnapshotResponse, error) {
	log := s.serverLog()
	log.Debug("CreateZFSSnapshot: taking", zap.String("dataset", req.Dataset), zap.String("snapshot", req.SnapshotName),
		zap.String("node", req.Node))
	err := s.storage.ZFSSnapshot(ctx, req.Dataset, req.SnapshotName, req.Node)
	if err != nil {
		log.Debug("CreateZFSSnapshot: failed", zap.String("dataset", req.Dataset), zap.Error(err))
		return &haifypb.CreateZFSSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("CreateZFSSnapshot: taken", zap.String("dataset", req.Dataset), zap.String("snapshot", req.SnapshotName))
	return &haifypb.CreateZFSSnapshotResponse{
		Success: true,
		Message: "ZFS snapshot created successfully",
	}, nil
}

func (s *Server) DeleteZFSSnapshot(ctx context.Context, req *haifypb.DeleteZFSSnapshotRequest) (*haifypb.DeleteZFSSnapshotResponse, error) {
	log := s.serverLog()
	log.Debug("DeleteZFSSnapshot: destroying", zap.String("snapshot", req.Snapshot), zap.String("node", req.Node))
	err := s.storage.ZFSDeleteSnapshot(ctx, req.Snapshot, req.Node)
	if err != nil {
		log.Debug("DeleteZFSSnapshot: failed", zap.String("snapshot", req.Snapshot), zap.Error(err))
		return &haifypb.DeleteZFSSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("DeleteZFSSnapshot: destroyed", zap.String("snapshot", req.Snapshot))
	return &haifypb.DeleteZFSSnapshotResponse{
		Success: true,
		Message: "ZFS snapshot deleted successfully",
	}, nil
}

func (s *Server) ListZFSSnapshots(ctx context.Context, req *haifypb.ListZFSSnapshotsRequest) (*haifypb.ListZFSSnapshotsResponse, error) {
	log := s.serverLog()
	log.Debug("ListZFSSnapshots: listing", zap.String("dataset", req.Dataset), zap.String("node", req.Node))
	snapshots, err := s.storage.ZFSListSnapshots(ctx, req.Dataset, req.Node)
	if err != nil {
		log.Debug("ListZFSSnapshots: failed", zap.String("dataset", req.Dataset), zap.Error(err))
		return &haifypb.ListZFSSnapshotsResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	var pbSnapshots []*haifypb.SnapshotInfo
	for _, snap := range snapshots {
		log.Debug("ListZFSSnapshots: snapshot", zap.String("snapshot", snap.Name), zap.String("volume", snap.Volume))
		pbSnapshots = append(pbSnapshots, &haifypb.SnapshotInfo{
			Name:      snap.Name,
			Volume:    snap.Volume,
			SizeGb:    snap.SizeGB,
			CreatedAt: snap.CreatedAt,
			Origin:    snap.Origin,
		})
	}

	log.Debug("ListZFSSnapshots: listed", zap.String("dataset", req.Dataset), zap.Int("count", len(pbSnapshots)))
	return &haifypb.ListZFSSnapshotsResponse{
		Success:   true,
		Message:   "ZFS snapshots listed successfully",
		Snapshots: pbSnapshots,
	}, nil
}

func (s *Server) RestoreZFSSnapshot(ctx context.Context, req *haifypb.RestoreZFSSnapshotRequest) (*haifypb.RestoreZFSSnapshotResponse, error) {
	log := s.serverLog()
	log.Debug("RestoreZFSSnapshot: rolling back", zap.String("dataset", req.Dataset), zap.String("snapshot", req.SnapshotName),
		zap.String("node", req.Node))
	err := s.storage.ZFSRestoreSnapshot(ctx, req.Dataset, req.SnapshotName, req.Node)
	if err != nil {
		log.Debug("RestoreZFSSnapshot: failed", zap.String("dataset", req.Dataset), zap.Error(err))
		return &haifypb.RestoreZFSSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("RestoreZFSSnapshot: restored", zap.String("dataset", req.Dataset), zap.String("snapshot", req.SnapshotName))
	return &haifypb.RestoreZFSSnapshotResponse{
		Success: true,
		Message: "ZFS snapshot restored successfully",
	}, nil
}

func (s *Server) CloneZFSSnapshot(ctx context.Context, req *haifypb.CloneZFSSnapshotRequest) (*haifypb.CloneZFSSnapshotResponse, error) {
	log := s.serverLog()
	log.Debug("CloneZFSSnapshot: cloning", zap.String("snapshot", req.Snapshot), zap.String("clone", req.ClonePath),
		zap.String("node", req.Node))
	err := s.storage.ZFSCloneSnapshot(ctx, req.Snapshot, req.ClonePath, req.Node)
	if err != nil {
		log.Debug("CloneZFSSnapshot: failed", zap.String("snapshot", req.Snapshot), zap.Error(err))
		return &haifypb.CloneZFSSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("CloneZFSSnapshot: cloned", zap.String("snapshot", req.Snapshot), zap.String("clone", req.ClonePath))
	return &haifypb.CloneZFSSnapshotResponse{
		Success: true,
		Message: "ZFS snapshot cloned successfully",
	}, nil
}
