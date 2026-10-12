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
	ctrlLogOrGlobal(ctrl).Debug("NewServer: building gRPC server", zap.Bool("has_controller", ctrl != nil))
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

// ctrlLogOrGlobal is the logger traces use when they reach the controller
// through a manager: the controller's own, or zap's global one when the
// controller (or the manager holding it) was built bare, as tests do.
func ctrlLogOrGlobal(c *Controller) *zap.Logger {
	if c != nil && c.logger != nil {
		return c.logger
	}
	return zap.L()
}

// serverLog is the logger the gRPC handlers trace with: the server's own,
// else the controller's, else the global one.
func (s *Server) serverLog() *zap.Logger {
	if s != nil && s.logger != nil {
		return s.logger
	}
	if s != nil {
		return ctrlLogOrGlobal(s.ctrl)
	}
	return zap.L()
}

// ==================== POOL OPERATIONS ====================

func (s *Server) CreatePool(ctx context.Context, req *haifypb.CreatePoolRequest) (*haifypb.CreatePoolResponse, error) {
	log := s.serverLog()
	log.Debug("CreatePool: creating pool", zap.String("pool", req.Name), zap.String("type", req.Type),
		zap.String("node", req.Node), zap.Strings("disks", req.Disks), zap.Uint64("size_gb", req.SizeGb), zap.Bool("wipe", req.Wipe))
	err := s.storage.CreatePool(ctx, req.Name, req.Type, req.Node, req.Disks, req.SizeGb, req.Wipe)
	if err != nil {
		log.Debug("CreatePool: failed", zap.String("pool", req.Name), zap.Error(err))
		return &haifypb.CreatePoolResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("CreatePool: created", zap.String("pool", req.Name))
	return &haifypb.CreatePoolResponse{
		Success: true,
		Message: "Pool created successfully",
	}, nil
}

func (s *Server) DeletePool(ctx context.Context, req *haifypb.DeletePoolRequest) (*haifypb.DeletePoolResponse, error) {
	log := s.serverLog()
	log.Debug("DeletePool: deleting pool", zap.String("pool", req.Name), zap.String("node", req.Node))
	err := s.storage.DeletePool(ctx, req.Name, req.Node)
	if err != nil {
		log.Debug("DeletePool: failed", zap.String("pool", req.Name), zap.Error(err))
		return &haifypb.DeletePoolResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("DeletePool: deleted", zap.String("pool", req.Name))
	return &haifypb.DeletePoolResponse{
		Success: true,
		Message: "Pool deleted successfully",
	}, nil
}

func (s *Server) GetPool(ctx context.Context, req *haifypb.GetPoolRequest) (*haifypb.GetPoolResponse, error) {
	log := s.serverLog()
	log.Debug("GetPool: looking up pool", zap.String("pool", req.Name), zap.String("node", req.Node))
	pool, err := s.storage.GetPool(ctx, req.Name, req.Node)
	if err != nil {
		log.Debug("GetPool: failed", zap.String("pool", req.Name), zap.Error(err))
		return &haifypb.GetPoolResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("GetPool: found", zap.String("pool", pool.Name), zap.String("type", pool.Type),
		zap.String("node", pool.Node), zap.Uint64("free_bytes", pool.FreeBytes))
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
	zap.L().Debug("pbPoolInfo: converted pool", zap.String("pool", p.Name), zap.String("node", p.Node),
		zap.Bool("thin", out.Thin), zap.Bool("vdo", out.HasVdo), zap.Bool("cached", out.Cached),
		zap.Uint64("capacity_bytes", out.CapacityBytes), zap.Uint64("available_bytes", out.AvailableBytes))
	return out
}

// poolAllocatable is the size and the room left of what new volumes on p are
// carved from: its thin pool when it has one, else the pool itself.
func poolAllocatable(p *PoolInfo) (capacity, available uint64) {
	if u := p.ThinUsage; u != nil && u.PoolLV != "" && u.SizeBytes > 0 {
		used := min(max(u.DataPercent, 0), 100)
		zap.L().Debug("poolAllocatable: using thin pool", zap.String("pool", p.Name), zap.String("thin_lv", u.PoolLV),
			zap.Uint64("size_bytes", u.SizeBytes), zap.Float64("data_percent", used))
		return u.SizeBytes, uint64(float64(u.SizeBytes) * (100 - used) / 100)
	}
	zap.L().Debug("poolAllocatable: using pool totals", zap.String("pool", p.Name),
		zap.Uint64("total_bytes", p.TotalBytes), zap.Uint64("free_bytes", p.FreeBytes))
	return p.TotalBytes, p.FreeBytes
}

func (s *Server) ListPools(ctx context.Context, req *haifypb.ListPoolsRequest) (*haifypb.ListPoolsResponse, error) {
	log := s.serverLog()
	log.Debug("ListPools: listing pools")
	pools, err := s.storage.ListPools(ctx)
	if err != nil {
		log.Debug("ListPools: failed", zap.Error(err))
		return &haifypb.ListPoolsResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	var pbPools []*haifypb.PoolInfo
	for _, p := range pools {
		log.Debug("ListPools: pool", zap.String("pool", p.Name), zap.String("node", p.Node), zap.String("type", p.Type))
		pbPools = append(pbPools, pbPoolInfo(p))
	}

	log.Debug("ListPools: listed", zap.Int("count", len(pbPools)))
	return &haifypb.ListPoolsResponse{
		Success: true,
		Message: "Pools listed successfully",
		Pools:   pbPools,
	}, nil
}

func (s *Server) AddDiskToPool(ctx context.Context, req *haifypb.AddDiskToPoolRequest) (*haifypb.AddDiskToPoolResponse, error) {
	log := s.serverLog()
	log.Debug("AddDiskToPool: adding disk", zap.String("pool", req.Pool), zap.String("disk", req.Disk),
		zap.String("node", req.Node), zap.Bool("wipe", req.Wipe))
	err := s.storage.AddDiskToPool(ctx, req.Pool, req.Disk, req.Node, req.Wipe)
	if err != nil {
		log.Debug("AddDiskToPool: failed", zap.String("pool", req.Pool), zap.Error(err))
		return &haifypb.AddDiskToPoolResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("AddDiskToPool: added", zap.String("pool", req.Pool), zap.String("disk", req.Disk))
	return &haifypb.AddDiskToPoolResponse{
		Success: true,
		Message: "Disk added to pool successfully",
	}, nil
}

// ConvertPoolToThin rebuilds one node's pool as an LVM thin pool.
func (s *Server) ConvertPoolToThin(ctx context.Context, req *haifypb.ConvertPoolToThinRequest) (*haifypb.ConvertPoolToThinResponse, error) {
	log := s.serverLog()
	log.Debug("ConvertPoolToThin: converting", zap.String("pool", req.GetPool()), zap.String("node", req.GetNode()))
	if err := s.ctrl.resources.ConvertPoolToThin(ctx, req.GetNode(), req.GetPool()); err != nil {
		log.Debug("ConvertPoolToThin: failed", zap.String("pool", req.GetPool()), zap.Error(err))
		return &haifypb.ConvertPoolToThinResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("ConvertPoolToThin: converted", zap.String("pool", req.GetPool()), zap.String("node", req.GetNode()))
	return &haifypb.ConvertPoolToThinResponse{
		Success: true,
		Message: "pool rebuilt as thin; the volumes are resyncing from their peers",
	}, nil
}

// AddPoolCache puts an SSD in front of one node's pool with lvmcache.
func (s *Server) AddPoolCache(ctx context.Context, req *haifypb.AddPoolCacheRequest) (*haifypb.AddPoolCacheResponse, error) {
	log := s.serverLog()
	log.Debug("AddPoolCache: attaching cache", zap.String("pool", req.GetPool()), zap.String("node", req.GetNode()),
		zap.String("device", req.GetDevice()), zap.String("mode", req.GetMode()))
	info, err := s.storage.AddPoolCache(ctx, req.GetNode(), req.GetPool(), req.GetDevice(), req.GetMode())
	if err != nil {
		log.Debug("AddPoolCache: failed", zap.String("pool", req.GetPool()), zap.Error(err))
		return &haifypb.AddPoolCacheResponse{Success: false, Message: err.Error()}, nil
	}
	msg := fmt.Sprintf("cache attached to %s on %s in %s mode", req.GetPool(), req.GetNode(), info.Mode)
	if info.Mode == cacheModeWriteback {
		log.Debug("AddPoolCache: writeback mode, warning about SSD loss", zap.String("pool", req.GetPool()))
		msg += "; writes are acknowledged from the SSD, so losing it loses whatever it has not destaged"
	}
	log.Debug("AddPoolCache: attached", zap.String("pool", req.GetPool()), zap.String("mode", info.Mode),
		zap.Uint64("size_bytes", info.SizeBytes))
	return &haifypb.AddPoolCacheResponse{
		Success:        true,
		Message:        msg,
		Mode:           info.Mode,
		CacheSizeBytes: info.SizeBytes,
	}, nil
}

// RemovePoolCache flushes and detaches a pool's cache.
func (s *Server) RemovePoolCache(ctx context.Context, req *haifypb.RemovePoolCacheRequest) (*haifypb.RemovePoolCacheResponse, error) {
	log := s.serverLog()
	log.Debug("RemovePoolCache: detaching cache", zap.String("pool", req.GetPool()), zap.String("node", req.GetNode()))
	if err := s.storage.RemovePoolCache(ctx, req.GetNode(), req.GetPool()); err != nil {
		log.Debug("RemovePoolCache: failed", zap.String("pool", req.GetPool()), zap.Error(err))
		return &haifypb.RemovePoolCacheResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("RemovePoolCache: detached", zap.String("pool", req.GetPool()), zap.String("node", req.GetNode()))
	return &haifypb.RemovePoolCacheResponse{
		Success: true,
		Message: "cache flushed, detached, and its device released from the pool",
	}, nil
}
