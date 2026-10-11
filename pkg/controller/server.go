package controller

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/gateway"
	"go.uber.org/zap"
)

// Server implements the Haify controller gRPC service
type Server struct {
	haifypb.UnimplementedHaifyControllerServer
	ctrl      *Controller
	logger    *zap.Logger
	storage   *StorageManager
	resources *ResourceManager
	snapshots *SnapshotManager
	nodes     *NodeManager
	gateway   *gateway.Manager
}

// NewServer creates a new gRPC server
func NewServer(ctrl *Controller) *Server {
	return &Server{
		ctrl:      ctrl,
		logger:    ctrl.logger,
		storage:   ctrl.storage,
		resources: ctrl.resources,
		snapshots: ctrl.snapshots,
		nodes:     ctrl.nodes,
		gateway:   ctrl.gateway,
	}
}

// ==================== POOL OPERATIONS ====================

func (s *Server) CreatePool(ctx context.Context, req *haifypb.CreatePoolRequest) (*haifypb.CreatePoolResponse, error) {
	err := s.storage.CreatePool(ctx, req.Name, req.Type, req.Node, req.Disks, req.SizeGb, req.Wipe)
	if err != nil {
		return &haifypb.CreatePoolResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.CreatePoolResponse{
		Success: true,
		Message: "Pool created successfully",
	}, nil
}

func (s *Server) DeletePool(ctx context.Context, req *haifypb.DeletePoolRequest) (*haifypb.DeletePoolResponse, error) {
	err := s.storage.DeletePool(ctx, req.Name, req.Node)
	if err != nil {
		return &haifypb.DeletePoolResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.DeletePoolResponse{
		Success: true,
		Message: "Pool deleted successfully",
	}, nil
}

func (s *Server) GetPool(ctx context.Context, req *haifypb.GetPoolRequest) (*haifypb.GetPoolResponse, error) {
	pool, err := s.storage.GetPool(ctx, req.Name, req.Node)
	if err != nil {
		return &haifypb.GetPoolResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.GetPoolResponse{
		Success: true,
		Message: "Pool found",
		Pool:    pbPoolInfo(pool),
	}, nil
}

// pbPoolInfo converts a pool, including its storage tier if it has one.
func pbPoolInfo(p *PoolInfo) *haifypb.PoolInfo {
	out := &haifypb.PoolInfo{
		Name:          p.Name,
		Type:          p.Type,
		Node:          p.Node,
		TotalGb:       p.TotalGB,
		FreeGb:        p.FreeGB,
		TotalBytes:    p.TotalBytes,
		FreeBytes:     p.FreeBytes,
		Devices:       p.Devices,
		Thin:          p.Thin,
		Compression:   p.Compression,
		CompressRatio: p.CompressRatio,
	}
	out.CapacityBytes, out.AvailableBytes = poolAllocatable(p)
	if u := p.ThinUsage; u != nil {
		out.Thin = out.Thin || (u.PoolLV != "" && u.SizeBytes > 0)
		out.ThinPoolLv = u.PoolLV
		out.ThinSizeBytes = u.SizeBytes
		out.ThinDataPercent = u.DataPercent
		out.ThinMetadataPercent = u.MetaPercent
		out.ThinOutOfSpace = u.OutOfSpace
	}
	if v := p.VDO; v != nil {
		out.HasVdo = true
		out.VdoPhysicalPercent = v.PhysicalPercent
		out.VdoSavingPercent = v.SavingPercent
	}
	if c := p.Cache; c != nil {
		out.Cached = true
		out.CacheMode = c.Mode
		out.CacheSizeBytes = c.SizeBytes
		out.CacheUsedPercent = c.UsedPercent
		out.CacheHitPercent = c.HitPercent
		out.CacheDirtyPercent = c.DirtyPercent
		out.CacheDevice = c.Device
		out.CacheDegraded = c.Degraded
	}
	return out
}

// poolAllocatable is the size and the room left of what new volumes on p are
// carved from: its thin pool when it has one, else the pool itself.
func poolAllocatable(p *PoolInfo) (capacity, available uint64) {
	if u := p.ThinUsage; u != nil && u.PoolLV != "" && u.SizeBytes > 0 {
		used := min(max(u.DataPercent, 0), 100)
		return u.SizeBytes, uint64(float64(u.SizeBytes) * (100 - used) / 100)
	}
	return p.TotalBytes, p.FreeBytes
}

func (s *Server) ListPools(ctx context.Context, req *haifypb.ListPoolsRequest) (*haifypb.ListPoolsResponse, error) {
	pools, err := s.storage.ListPools(ctx)
	if err != nil {
		return &haifypb.ListPoolsResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	var pbPools []*haifypb.PoolInfo
	for _, p := range pools {
		pbPools = append(pbPools, pbPoolInfo(p))
	}

	return &haifypb.ListPoolsResponse{
		Success: true,
		Message: "Pools listed successfully",
		Pools:   pbPools,
	}, nil
}

func (s *Server) AddDiskToPool(ctx context.Context, req *haifypb.AddDiskToPoolRequest) (*haifypb.AddDiskToPoolResponse, error) {
	err := s.storage.AddDiskToPool(ctx, req.Pool, req.Disk, req.Node, req.Wipe)
	if err != nil {
		return &haifypb.AddDiskToPoolResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	return &haifypb.AddDiskToPoolResponse{
		Success: true,
		Message: "Disk added to pool successfully",
	}, nil
}

// ConvertPoolToThin rebuilds one node's pool as an LVM thin pool.
func (s *Server) ConvertPoolToThin(ctx context.Context, req *haifypb.ConvertPoolToThinRequest) (*haifypb.ConvertPoolToThinResponse, error) {
	if err := s.ctrl.resources.ConvertPoolToThin(ctx, req.GetNode(), req.GetPool()); err != nil {
		return &haifypb.ConvertPoolToThinResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.ConvertPoolToThinResponse{
		Success: true,
		Message: "pool rebuilt as thin; the volumes are resyncing from their peers",
	}, nil
}

// AddPoolCache puts an SSD in front of one node's pool with lvmcache.
func (s *Server) AddPoolCache(ctx context.Context, req *haifypb.AddPoolCacheRequest) (*haifypb.AddPoolCacheResponse, error) {
	info, err := s.storage.AddPoolCache(ctx, req.GetNode(), req.GetPool(), req.GetDevice(), req.GetMode())
	if err != nil {
		return &haifypb.AddPoolCacheResponse{Success: false, Message: err.Error()}, nil
	}
	msg := fmt.Sprintf("cache attached to %s on %s in %s mode", req.GetPool(), req.GetNode(), info.Mode)
	if info.Mode == cacheModeWriteback {
		msg += "; writes are acknowledged from the SSD, so losing it loses whatever it has not destaged"
	}
	return &haifypb.AddPoolCacheResponse{
		Success:        true,
		Message:        msg,
		Mode:           info.Mode,
		CacheSizeBytes: info.SizeBytes,
	}, nil
}

// RemovePoolCache flushes and detaches a pool's cache.
func (s *Server) RemovePoolCache(ctx context.Context, req *haifypb.RemovePoolCacheRequest) (*haifypb.RemovePoolCacheResponse, error) {
	if err := s.storage.RemovePoolCache(ctx, req.GetNode(), req.GetPool()); err != nil {
		return &haifypb.RemovePoolCacheResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.RemovePoolCacheResponse{
		Success: true,
		Message: "cache flushed, detached, and its device released from the pool",
	}, nil
}
