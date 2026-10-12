package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/deployment"
)

// Storage tiering: an SSD in front of a slow LVM pool, via lvmcache. Reading
// the state of an existing cache lives in poolcachestatus.go; this file is the
// part that changes something.
//
// Three decisions shape the feature.
//
// 1. The cache is per pool, not per volume. Haify already treats a pool as the
//    unit an operator allocates from — resources name a pool, not an LV — and a
//    per-LV cache would have to be re-split by hand every time a resource is
//    added. LVM makes that cheap for a thin pool: caching the pool's data LV
//    puts every thin volume in the group behind the same SSD. It also means
//    only thin pools can be cached, because a thick pool has no single LV that
//    all its volumes pass through; AddPoolCache says so rather than picking a
//    volume on the operator's behalf.
//
// 2. writethrough by default, writeback only when asked for by name. See
//    normalizeCacheMode.
//
// 3. Every guard refuses. A device that already holds something, a pool that
//    already has a cache, a cache that cannot be flushed — none of these are
//    reconfigured into shape, because each one is a sign that the operator and
//    the cluster disagree about what that hardware is for.

const (
	// cacheVolName is the LV the fast device is carved into, mirroring
	// thinPoolName. LVM renames it to "<name>_cvol" once it is attached.
	cacheVolName = "haifycache"

	cacheModeWritethrough = "writethrough"
	cacheModeWriteback    = "writeback"

	// minCacheDeviceBytes is the smallest device worth spending on a cache.
	//
	// Not a kernel limit — dm-cache will happily take less. It is the point
	// below which the cache stops paying for itself: LVM sizes the chunk to
	// keep the block count manageable, so a very small cache holds very few
	// distinct chunks, and a working set that does not fit thrashes at close to
	// the miss rate of no cache at all while still carrying every risk of one.
	minCacheDeviceBytes = 4 * gib
)

// normalizeCacheMode resolves the mode, defaulting to writethrough.
//
// writethrough acknowledges a write only once it has reached the slow device,
// so the SSD holds nothing that is not already durable. writeback acknowledges
// from the SSD and destages later, which means a node whose cache device fails
// — or a node that is destroyed outright — takes with it every write it had
// accepted but not yet written down, and only a replica that happens to hold
// those writes can give them back. DRBD makes that likely but not certain: a
// peer that is resyncing, or a failure that took more than one node, leaves
// nothing to recover from. That is a decision for whoever owns the data, so it
// has to be asked for by name and is never inferred.
func normalizeCacheMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", cacheModeWritethrough:
		zap.L().Debug("normalizeCacheMode: writethrough", zap.String("mode", mode))
		return cacheModeWritethrough, nil
	case cacheModeWriteback:
		zap.L().Debug("normalizeCacheMode: writeback asked for by name", zap.String("mode", mode))
		return cacheModeWriteback, nil
	default:
		zap.L().Debug("normalizeCacheMode: unknown mode", zap.String("mode", mode))
		return "", fmt.Errorf("unknown cache mode %q: use writethrough (default) or writeback", mode)
	}
}

// blockDeviceProbe is what a node says about a candidate cache device.
type blockDeviceProbe struct {
	IsBlock    bool
	SizeBytes  uint64
	FSType     string
	Mountpoint string
	Holders    []string
	VG         string
}

// parseBlockDeviceProbe reads the key=value lines ProbeBlockDevice emits.
func parseBlockDeviceProbe(output string) blockDeviceProbe {
	var p blockDeviceProbe
	for _, line := range strings.Split(output, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "block":
			p.IsBlock = value == "yes"
		case "size":
			p.SizeBytes = parseCacheUint(value)
		case "fstype":
			p.FSType = value
		case "mount":
			p.Mountpoint = value
		case "holders":
			p.Holders = strings.Fields(value)
		case "pvvg":
			p.VG = value
		}
	}
	zap.L().Debug("parseBlockDeviceProbe: parsed", zap.Bool("is_block", p.IsBlock), zap.Uint64("size_bytes", p.SizeBytes),
		zap.String("fstype", p.FSType), zap.String("mountpoint", p.Mountpoint), zap.Strings("holders", p.Holders),
		zap.String("vg", p.VG))
	return p
}

// checkCacheDevice refuses a device that is not free, or is too small to be
// worth the risk of being in the write path.
func checkCacheDevice(device string, p blockDeviceProbe, node string) error {
	log := zap.L()
	log.Debug("checkCacheDevice: start", zap.String("device", device), zap.String("node", node))
	if !p.IsBlock {
		log.Debug("checkCacheDevice: not a block device", zap.String("device", device))
		return fmt.Errorf("%s is not a block device on %s", device, node)
	}
	if p.VG != "" {
		log.Debug("checkCacheDevice: already a physical volume", zap.String("device", device), zap.String("vg", p.VG))
		return fmt.Errorf("%s is already a physical volume of volume group %s on %s; "+
			"pick a device that is not in use", device, p.VG, node)
	}
	if len(p.Holders) > 0 {
		log.Debug("checkCacheDevice: device has holders", zap.String("device", device), zap.Strings("holders", p.Holders))
		return fmt.Errorf("%s has %s layered on it on %s; give the cache a whole, empty device",
			device, strings.Join(p.Holders, ", "), node)
	}
	if p.Mountpoint != "" {
		log.Debug("checkCacheDevice: device is mounted", zap.String("device", device), zap.String("mountpoint", p.Mountpoint))
		return fmt.Errorf("%s is mounted at %s on %s", device, p.Mountpoint, node)
	}
	if p.FSType != "" {
		// Refuse rather than wipe. An unexpected signature means this is not the
		// disk the operator thinks it is far more often than it means a stale
		// label, and lvmcache consumes the device whole.
		log.Debug("checkCacheDevice: device carries a signature", zap.String("device", device), zap.String("fstype", p.FSType))
		return fmt.Errorf("%s already carries a %s signature on %s; "+
			"if it really is free, clear it with `wipefs -a %s` on that node and retry",
			device, p.FSType, node, device)
	}
	if p.SizeBytes < minCacheDeviceBytes {
		log.Debug("checkCacheDevice: device too small", zap.String("device", device), zap.Uint64("size_bytes", p.SizeBytes))
		return fmt.Errorf("%s is %d MiB; a cache smaller than %d GiB holds too little of any real "+
			"working set to earn a place in the write path",
			device, p.SizeBytes/(1<<20), minCacheDeviceBytes/gib)
	}
	log.Debug("checkCacheDevice: device is free", zap.String("device", device), zap.Uint64("size_bytes", p.SizeBytes))
	return nil
}

// AddPoolCache puts a fast device in front of one node's pool.
//
// The cache is local to the node: DRBD replicates the volumes, not the block
// layer beneath them, so each replica is tiered — or not — on its own. Running
// this on one node of a resource is a legitimate end state, not a half-finished
// job.
func (sm *StorageManager) AddPoolCache(ctx context.Context, node, poolName, device, mode string) (*PoolCacheInfo, error) {
	log := loggerOfController(sm.controller)
	poolName = normalizeManagedName(poolName)
	device = strings.TrimSpace(device)
	log.Debug("AddPoolCache: start", zap.String("node", node), zap.String("pool", poolName),
		zap.String("device", device), zap.String("mode", mode))
	if node == "" || poolName == "" || device == "" {
		log.Debug("AddPoolCache: missing node, pool or device")
		return nil, fmt.Errorf("node, pool and device are all required")
	}
	mode, err := normalizeCacheMode(mode)
	if err != nil {
		log.Debug("AddPoolCache: bad mode", zap.Error(err))
		return nil, err
	}
	address := sm.controller.ResolveHost(node)
	if address == "" {
		log.Debug("AddPoolCache: node not resolved", zap.String("node", node))
		return nil, fmt.Errorf("node not found: %s", node)
	}

	// ZFS is out of scope on purpose: a zpool caches through L2ARC and a
	// separate log device, which is a different mechanism with different
	// durability rules, and putting dm-cache under a zpool vdev would hide the
	// device from both.
	if persisted, perr := sm.getPersistedPool(ctx, poolName); perr == nil && isZFSPoolType(persisted.Type) {
		log.Debug("AddPoolCache: ZFS pool refused", zap.String("pool", poolName))
		return nil, fmt.Errorf("%s is a ZFS pool; use an L2ARC/SLOG device with zpool add rather than lvmcache",
			poolName)
	}

	start := time.Now()
	thinPool, err := sm.controller.deployment.LVThinPoolIn(ctx, address, poolName)
	log.Debug("AddPoolCache: looked for a thin pool", zap.String("host", address), zap.String("pool", poolName),
		zap.String("thin_pool", thinPool), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("inspect %s on %s: %w", poolName, node, err)
	}
	if thinPool == "" {
		log.Debug("AddPoolCache: not a thin pool, refused", zap.String("pool", poolName))
		// See the per-pool decision at the top of this file: a thick pool has no
		// single LV to cache.
		return nil, fmt.Errorf("%s on %s is not a thin pool, so it has no single volume to cache; "+
			"convert it first with `haify pool convert-thin --node %s --pool %s`",
			poolName, node, node, poolName)
	}

	existing, err := sm.readPoolCache(ctx, address, poolName)
	if err != nil {
		log.Debug("AddPoolCache: reading existing cache failed", zap.String("pool", poolName), zap.Error(err))
		return nil, err
	}
	if existing != nil {
		log.Debug("AddPoolCache: pool already cached", zap.String("pool", poolName), zap.String("mode", existing.Mode))
		return nil, fmt.Errorf("%s on %s is already cached (%s, %s); remove that cache first",
			poolName, node, existing.Mode, formatCacheSize(existing.SizeBytes))
	}

	start = time.Now()
	probeRes, err := sm.controller.deployment.ProbeBlockDevice(ctx, address, device)
	log.Debug("AddPoolCache: probed device", zap.String("host", address), zap.String("device", device),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("probe %s on %s: %w", device, node, err)
	}
	probe, err := singleHostProbe(probeRes, device, node)
	if err != nil {
		log.Debug("AddPoolCache: probe failed", zap.String("device", device), zap.Error(err))
		return nil, err
	}
	if err := checkCacheDevice(device, probe, node); err != nil {
		log.Debug("AddPoolCache: device refused", zap.String("device", device), zap.Error(err))
		return nil, err
	}

	sm.controller.logger.Info("Attaching pool cache",
		zap.String("node", node), zap.String("pool", poolName),
		zap.String("device", device), zap.String("mode", mode),
		zap.Uint64("device_bytes", probe.SizeBytes))
	if mode == cacheModeWriteback {
		sm.controller.logger.Warn("Cache is writeback: writes are acknowledged from this node's SSD "+
			"and a loss of that device loses everything it has not yet destaged",
			zap.String("node", node), zap.String("pool", poolName), zap.String("device", device))
	}

	return sm.attachPoolCache(ctx, address, node, poolName, thinPool, device, mode)
}

// attachPoolCache carries out an approved attach, undoing its own partial work.
//
// Each step is reversible until the last one lands, and leaving a stray cache
// volume behind would make the obvious next move — try again — fail on a name
// clash rather than on the original problem.
func (sm *StorageManager) attachPoolCache(ctx context.Context, address, node, poolName, thinPool, device, mode string) (*PoolCacheInfo, error) {
	log := loggerOfController(sm.controller)
	log.Debug("attachPoolCache: start", zap.String("node", node), zap.String("host", address),
		zap.String("pool", poolName), zap.String("thin_pool", thinPool), zap.String("device", device),
		zap.String("mode", mode))
	dep := sm.controller.deployment
	hosts := []string{address}

	start := time.Now()
	err := execFailure(dep.PVCreate(ctx, hosts, device))
	log.Debug("attachPoolCache: pvcreate", zap.String("host", address), zap.String("device", device),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("make %s a physical volume on %s: %w", device, node, err)
	}
	start = time.Now()
	err = execFailure(dep.VGExtend(ctx, hosts, poolName, device))
	log.Debug("attachPoolCache: vgextend", zap.String("host", address), zap.String("pool", poolName),
		zap.String("device", device), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		sm.releaseCacheDevice(ctx, hosts, poolName, device)
		return nil, fmt.Errorf("add %s to %s on %s: %w", device, poolName, node, err)
	}
	start = time.Now()
	err = execFailure(dep.LVCreateCacheVol(ctx, hosts, poolName, cacheVolName, device))
	log.Debug("attachPoolCache: create cache volume", zap.String("host", address), zap.String("pool", poolName),
		zap.String("device", device), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		sm.releaseCacheDevice(ctx, hosts, poolName, device)
		return nil, fmt.Errorf("create the cache volume on %s: %w", device, err)
	}
	start = time.Now()
	err = execFailure(dep.LVConvertToCache(ctx, hosts, poolName, thinPool, cacheVolName, mode))
	log.Debug("attachPoolCache: lvconvert to cache", zap.String("host", address), zap.String("pool", poolName),
		zap.String("thin_pool", thinPool), zap.String("mode", mode), zap.Duration("took", time.Since(start)),
		zap.Error(err))
	if err != nil {
		rmErr := execFailure(dep.LVRemove(ctx, hosts, poolName+"/"+cacheVolName))
		log.Debug("attachPoolCache: removed cache volume after failed convert", zap.String("pool", poolName),
			zap.Error(rmErr))
		if rmErr == nil {
			sm.releaseCacheDevice(ctx, hosts, poolName, device)
		}
		return nil, fmt.Errorf("attach the cache to %s/%s: %w", poolName, thinPool, err)
	}

	// Read the cache back rather than reporting the request. The mode is the
	// whole safety argument, and an lvconvert that succeeded while silently
	// applying a different one would leave that argument false.
	info, err := sm.readPoolCache(ctx, address, poolName)
	if err != nil {
		log.Debug("attachPoolCache: read back failed", zap.String("pool", poolName), zap.Error(err))
		return nil, fmt.Errorf("the cache was attached but its state could not be read back: %w", err)
	}
	if info == nil {
		log.Debug("attachPoolCache: no cache after lvconvert", zap.String("pool", poolName))
		return nil, fmt.Errorf("lvconvert reported success but %s on %s has no cache", poolName, node)
	}
	if info.Mode != "" && info.Mode != mode {
		log.Debug("attachPoolCache: cache mode differs from request", zap.String("pool", poolName),
			zap.String("mode", info.Mode), zap.String("requested_mode", mode))
		return nil, fmt.Errorf("the cache on %s is %s, not the requested %s; detach it before using the pool",
			poolName, info.Mode, mode)
	}
	sm.controller.logger.Info("Pool cache attached",
		zap.String("node", node), zap.String("pool", poolName),
		zap.String("mode", info.Mode), zap.Uint64("cache_bytes", info.SizeBytes))
	return info, nil
}

// releaseCacheDevice best-effort undoes the volume-group membership of a device
// that a failed attach had already claimed.
func (sm *StorageManager) releaseCacheDevice(ctx context.Context, hosts []string, poolName, device string) {
	start := time.Now()
	err := execFailure(sm.controller.deployment.VGReduceAndRemovePV(ctx, hosts, poolName, device))
	loggerOfController(sm.controller).Debug("releaseCacheDevice: vgreduce and pvremove", zap.Strings("hosts", hosts),
		zap.String("pool", poolName), zap.String("device", device), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		sm.controller.logger.Warn("Could not take the cache device back out of the pool after a failed attach",
			zap.String("pool", poolName), zap.String("device", device), zap.Error(err))
	}
}

// RemovePoolCache flushes a cache and takes its device back out of the pool.
func (sm *StorageManager) RemovePoolCache(ctx context.Context, node, poolName string) error {
	log := loggerOfController(sm.controller)
	poolName = normalizeManagedName(poolName)
	log.Debug("RemovePoolCache: start", zap.String("node", node), zap.String("pool", poolName))
	if node == "" || poolName == "" {
		log.Debug("RemovePoolCache: missing node or pool")
		return fmt.Errorf("node and pool are both required")
	}
	address := sm.controller.ResolveHost(node)
	if address == "" {
		log.Debug("RemovePoolCache: node not resolved", zap.String("node", node))
		return fmt.Errorf("node not found: %s", node)
	}

	info, err := sm.readPoolCache(ctx, address, poolName)
	if err != nil {
		log.Debug("RemovePoolCache: reading cache failed", zap.String("pool", poolName), zap.Error(err))
		return err
	}
	if info == nil {
		log.Debug("RemovePoolCache: pool has no cache", zap.String("pool", poolName))
		return fmt.Errorf("%s on %s has no cache", poolName, node)
	}
	if info.Degraded {
		log.Debug("RemovePoolCache: cache degraded, refusing", zap.String("pool", poolName),
			zap.Uint32("dirty_percent", info.DirtyPercent))
		// lvconvert --uncache cannot write the dirty blocks down when the device
		// holding them is gone, and the only way past that is --force, which
		// discards them. That is a decision to be taken by hand, with the data
		// loss in view, not one to be taken here on the operator's behalf.
		return fmt.Errorf("the cache on %s on %s is missing a device, so it cannot be flushed; "+
			"%d%% of it is not on the slow disk. Recover the device, or discard those writes "+
			"deliberately with `lvconvert --uncache --force %s/%s` on that node",
			poolName, node, info.DirtyPercent, poolName, info.OriginLV)
	}

	// The LV to detach is the pool, not the internal sub-LV the cache is bolted
	// to: lvconvert takes the pool name and finds "_tdata" itself.
	target := strings.TrimSuffix(info.OriginLV, "_tdata")
	sm.controller.logger.Info("Detaching pool cache",
		zap.String("node", node), zap.String("pool", poolName),
		zap.String("lv", target), zap.String("mode", info.Mode),
		zap.Uint32("dirty_percent", info.DirtyPercent))

	start := time.Now()
	err = execFailure(sm.controller.deployment.LVUncache(ctx, []string{address}, poolName, target))
	log.Debug("RemovePoolCache: lvconvert uncache", zap.String("host", address), zap.String("pool", poolName),
		zap.String("lv", target), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("flush and detach the cache on %s/%s: %w", poolName, target, err)
	}

	// Verify rather than trust. lvconvert flushes a writeback cache before it
	// detaches, so the cache still being there is the same fact as the flush not
	// having finished — and reporting success on that would tell an operator the
	// SSD is free to pull.
	after, err := sm.readPoolCache(ctx, address, poolName)
	if err != nil {
		log.Debug("RemovePoolCache: detach not confirmed", zap.String("pool", poolName), zap.Error(err))
		return fmt.Errorf("the cache detach could not be confirmed on %s: %w", node, err)
	}
	if after != nil {
		log.Debug("RemovePoolCache: cache still attached after uncache", zap.String("pool", poolName),
			zap.Uint32("dirty_percent", after.DirtyPercent))
		return fmt.Errorf("the cache is still attached to %s on %s after lvconvert --uncache; "+
			"%d%% of it has not reached the slow disk, so the device must not be removed",
			poolName, node, after.DirtyPercent)
	}

	// See VGReduceAndRemovePV: an SSD left in the group is free space that a
	// later thin-pool extension would silently allocate pool data onto. Not
	// knowing which device it was is the same problem as failing to remove it,
	// and is reported the same way rather than passed over in silence.
	if info.Device == "" {
		log.Debug("RemovePoolCache: cache device unknown, left in the pool", zap.String("pool", poolName))
		return fmt.Errorf("the cache on %s on %s was flushed and detached, but lvs did not say which device "+
			"it was on, so that device is still a physical volume of the pool; take it out with "+
			"`vgreduce %s <device>` before the pool is grown", poolName, node, poolName)
	}
	start = time.Now()
	err = execFailure(sm.controller.deployment.VGReduceAndRemovePV(ctx,
		[]string{address}, poolName, info.Device))
	log.Debug("RemovePoolCache: vgreduce and pvremove", zap.String("host", address), zap.String("pool", poolName),
		zap.String("device", info.Device), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("the cache was flushed and detached, but %s is still a physical volume of %s "+
			"on %s and must be removed before the pool is grown: %w", info.Device, poolName, node, err)
	}

	sm.controller.logger.Info("Pool cache removed",
		zap.String("node", node), zap.String("pool", poolName), zap.String("device", info.Device))
	return nil
}

func singleHostProbe(res *deployment.ExecResult, device, node string) (blockDeviceProbe, error) {
	for _, r := range res.Hosts {
		if !r.Success {
			zap.L().Debug("singleHostProbe: probe failed on host", zap.String("host", r.Host), zap.String("device", device))
			return blockDeviceProbe{}, fmt.Errorf("probe %s on %s: %s", device, node, strings.TrimSpace(r.Output))
		}
		return parseBlockDeviceProbe(r.Output), nil
	}
	zap.L().Debug("singleHostProbe: no host result", zap.String("device", device), zap.String("node", node))
	return blockDeviceProbe{}, fmt.Errorf("probe %s on %s: no result", device, node)
}

func formatCacheSize(bytes uint64) string {
	if bytes == 0 {
		return "size unknown"
	}
	return fmt.Sprintf("%.1f GiB", float64(bytes)/float64(gib))
}
