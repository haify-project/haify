package controller

import (
	"context"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

func (s *Server) CreateZFSPool(ctx context.Context, req *haifypb.CreateZFSPoolRequest) (*haifypb.CreateZFSPoolResponse, error) {
	err := s.storage.CreateZFSPool(ctx, req.Name, req.Node, req.Vdevs, req.Compression, req.Dedup, req.Wipe)
	if err != nil {
		return &haifypb.CreateZFSPoolResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.CreateZFSPoolResponse{
		Success: true,
		Message: "ZFS pool created successfully",
	}, nil
}

func (s *Server) DeleteZFSPool(ctx context.Context, req *haifypb.DeleteZFSPoolRequest) (*haifypb.DeleteZFSPoolResponse, error) {
	err := s.storage.DeleteZFSPool(ctx, req.Name, req.Node)
	if err != nil {
		return &haifypb.DeleteZFSPoolResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.DeleteZFSPoolResponse{
		Success: true,
		Message: "ZFS pool deleted successfully",
	}, nil
}

func (s *Server) ListZFSpools(ctx context.Context, req *haifypb.ListZFSPoolsRequest) (*haifypb.ListZFSPoolsResponse, error) {
	pools, err := s.storage.ListZFSpools(ctx)
	if err != nil {
		return &haifypb.ListZFSPoolsResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	var pbPools []*haifypb.PoolInfo
	for _, p := range pools {
		pbPools = append(pbPools, pbPoolInfo(p))
	}

	return &haifypb.ListZFSPoolsResponse{
		Success: true,
		Message: "ZFS pools listed successfully",
		Pools:   pbPools,
	}, nil
}

func (s *Server) CreateZFSDataset(ctx context.Context, req *haifypb.CreateZFSDatasetRequest) (*haifypb.CreateZFSDatasetResponse, error) {
	err := s.storage.CreateZFSDataset(ctx, req.DatasetPath, req.Node)
	if err != nil {
		return &haifypb.CreateZFSDatasetResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.CreateZFSDatasetResponse{
		Success: true,
		Message: "ZFS dataset created successfully",
	}, nil
}

func (s *Server) CreateZFSVolume(ctx context.Context, req *haifypb.CreateZFSVolumeRequest) (*haifypb.CreateZFSVolumeResponse, error) {
	err := s.storage.CreateZFSThinVolume(ctx, req.PoolName, req.VolumeName, req.Size, req.Node)
	if err != nil {
		return &haifypb.CreateZFSVolumeResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.CreateZFSVolumeResponse{
		Success: true,
		Message: "ZFS volume created successfully",
	}, nil
}

func (s *Server) ResizeZFSVolume(ctx context.Context, req *haifypb.ResizeZFSVolumeRequest) (*haifypb.ResizeZFSVolumeResponse, error) {
	err := s.storage.ZFSResizeVolume(ctx, req.VolumePath, req.NewSize, req.Node)
	if err != nil {
		return &haifypb.ResizeZFSVolumeResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.ResizeZFSVolumeResponse{
		Success: true,
		Message: "ZFS volume resized successfully",
	}, nil
}

func (s *Server) DeleteZFSDataset(ctx context.Context, req *haifypb.DeleteZFSDatasetRequest) (*haifypb.DeleteZFSDatasetResponse, error) {
	// Use ZFS destroy for both datasets and volumes
	err := s.storage.ZFSDeleteDataset(ctx, req.DatasetPath, req.Node)
	if err != nil {
		return &haifypb.DeleteZFSDatasetResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.DeleteZFSDatasetResponse{
		Success: true,
		Message: "ZFS dataset deleted successfully",
	}, nil
}

func (s *Server) CreateZFSSnapshot(ctx context.Context, req *haifypb.CreateZFSSnapshotRequest) (*haifypb.CreateZFSSnapshotResponse, error) {
	err := s.storage.ZFSSnapshot(ctx, req.Dataset, req.SnapshotName, req.Node)
	if err != nil {
		return &haifypb.CreateZFSSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.CreateZFSSnapshotResponse{
		Success: true,
		Message: "ZFS snapshot created successfully",
	}, nil
}

func (s *Server) DeleteZFSSnapshot(ctx context.Context, req *haifypb.DeleteZFSSnapshotRequest) (*haifypb.DeleteZFSSnapshotResponse, error) {
	err := s.storage.ZFSDeleteSnapshot(ctx, req.Snapshot, req.Node)
	if err != nil {
		return &haifypb.DeleteZFSSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.DeleteZFSSnapshotResponse{
		Success: true,
		Message: "ZFS snapshot deleted successfully",
	}, nil
}

func (s *Server) ListZFSSnapshots(ctx context.Context, req *haifypb.ListZFSSnapshotsRequest) (*haifypb.ListZFSSnapshotsResponse, error) {
	snapshots, err := s.storage.ZFSListSnapshots(ctx, req.Dataset, req.Node)
	if err != nil {
		return &haifypb.ListZFSSnapshotsResponse{
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

	return &haifypb.ListZFSSnapshotsResponse{
		Success:   true,
		Message:   "ZFS snapshots listed successfully",
		Snapshots: pbSnapshots,
	}, nil
}

func (s *Server) RestoreZFSSnapshot(ctx context.Context, req *haifypb.RestoreZFSSnapshotRequest) (*haifypb.RestoreZFSSnapshotResponse, error) {
	err := s.storage.ZFSRestoreSnapshot(ctx, req.Dataset, req.SnapshotName, req.Node)
	if err != nil {
		return &haifypb.RestoreZFSSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.RestoreZFSSnapshotResponse{
		Success: true,
		Message: "ZFS snapshot restored successfully",
	}, nil
}

func (s *Server) CloneZFSSnapshot(ctx context.Context, req *haifypb.CloneZFSSnapshotRequest) (*haifypb.CloneZFSSnapshotResponse, error) {
	err := s.storage.ZFSCloneSnapshot(ctx, req.Snapshot, req.ClonePath, req.Node)
	if err != nil {
		return &haifypb.CloneZFSSnapshotResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.CloneZFSSnapshotResponse{
		Success: true,
		Message: "ZFS snapshot cloned successfully",
	}, nil
}
