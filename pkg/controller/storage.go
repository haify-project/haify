package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
)

// PoolInfo represents pool information
type PoolInfo struct {
	Name string `json:"name"`
	Type string `json:"type"` // "vg" or "zfs"
	Node string `json:"node"`
	// TotalGB and FreeGB are rounded, not truncated. A 20 GiB disk carries PV
	// metadata and leaves the group at 19.996 GiB; truncating reported that as
	// 19 and made a freshly created pool look like it had lost a gigabyte.
	// TotalBytes and FreeBytes carry the exact figures for anything that needs
	// to do arithmetic or render a precise size.
	TotalGB     uint64   `json:"total_gb"`
	FreeGB      uint64   `json:"free_gb"`
	TotalBytes  uint64   `json:"total_bytes"`
	FreeBytes   uint64   `json:"free_bytes"`
	Devices     []string `json:"devices"`
	Thin        bool     `json:"thin"`
	Compression string   `json:"compression,omitempty"`
	// CompressRatio is what compression saves on a ZFS pool, as ZFS reports
	// it (1.85 = 1.85x); 0 when not known.
	CompressRatio float64 `json:"compress_ratio,omitempty"`
	// Cache describes the pool's fast tier, or is nil when it has none. See
	// poolcache.go — this is what makes a tiered pool distinguishable from a
	// plain one without a second call.
	Cache *PoolCacheInfo `json:"cache,omitempty"`
	// ThinUsage is the utilisation of the thin pool inside this group, or nil
	// when the group holds none.
	//
	// It is the only capacity figure here that reflects whether writes will
	// succeed. TotalGB and FreeGB describe the *volume group*, almost all of
	// which belongs to the thin pool (95% of it from `pool create`, all of it
	// after convert-thin), so FreeGB stays near zero however empty the pool
	// is. Prefer this when it is present; see poolthin.go.
	ThinUsage *PoolThinInfo `json:"thin_usage,omitempty"`
	// VDO is the physical side of a VDO-backed thin pool, nil for any other
	// pool; see storage_vdo.go.
	VDO *PoolVDOInfo `json:"vdo,omitempty"`
}

// StorageManager manages all storage operations
type StorageManager struct {
	controller *Controller
}

// NewStorageManager creates a new storage manager
func NewStorageManager(ctrl *Controller) *StorageManager {
	return &StorageManager{
		controller: ctrl,
	}
}

func poolInfoFromDB(pool *database.Pool) *PoolInfo {
	var devices []string
	if pool.Devices != "" {
		for _, device := range strings.Split(pool.Devices, ",") {
			device = strings.TrimSpace(device)
			if device != "" {
				devices = append(devices, device)
			}
		}
	}

	return &PoolInfo{
		Name:    pool.Name,
		Type:    pool.Type,
		Node:    pool.Node,
		TotalGB: uint64(max(pool.TotalGB, 0)),
		FreeGB:  uint64(max(pool.FreeGB, 0)),
		Devices: devices,
		Thin:    isThinPoolType(pool.Type),
	}
}

func isZFSPoolType(poolType string) bool {
	return poolType == "zfs"
}

// bytesToGB converts to whole gibibytes by rounding to nearest rather than
// truncating. See the comment on PoolInfo.TotalGB.
func bytesToGB(b uint64) uint64 {
	const giB = 1024 * 1024 * 1024
	return (b + giB/2) / giB
}

func (sm *StorageManager) getPersistedPool(ctx context.Context, name string) (*PoolInfo, error) {
	if sm.controller.db == nil {
		return nil, fmt.Errorf("database not available")
	}

	pool, err := sm.controller.db.GetPool(ctx, name)
	if err != nil {
		return nil, err
	}

	return poolInfoFromDB(pool), nil
}

func (sm *StorageManager) listPersistedPools(ctx context.Context) ([]*PoolInfo, error) {
	if sm.controller.db == nil {
		return nil, fmt.Errorf("database not available")
	}

	pools, err := sm.controller.db.ListPools(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]*PoolInfo, 0, len(pools))
	for _, pool := range pools {
		result = append(result, poolInfoFromDB(pool))
	}
	return result, nil
}

func normalizeDeviceList(devices []string) []string {
	seen := make(map[string]struct{})
	var result []string
	for _, device := range devices {
		device = strings.TrimSpace(device)
		if device == "" {
			continue
		}
		if _, exists := seen[device]; exists {
			continue
		}
		seen[device] = struct{}{}
		result = append(result, device)
	}
	slices.Sort(result)
	return result
}

// ==================== POOL OPERATIONS ====================

// defaultedPoolType resolves an omitted pool type from the controller's
// configuration.
//
// The policy belongs here rather than in a client. It used to live in haify,
// which substituted "lvm-thin" before the request ever left the process — so the
// CLI created thin pools while every other caller that omitted the type (the
// REST gateway, MCP, the web UI) reached normalizeLVMPoolType with an empty
// string and got a thick group instead. Two entry points, two silently
// different defaults, and storage.default_pool_type — the setting that exists to
// say which one — read by nobody.
//
// An unset configuration falls through to normalizeLVMPoolType's own handling of
// "", which is how a Controller built without config (tests, embedded uses)
// keeps working.
func (sm *StorageManager) defaultedPoolType(poolType string) string {
	if strings.TrimSpace(poolType) != "" {
		return poolType
	}
	if sm.controller == nil || sm.controller.config == nil {
		return poolType
	}
	return sm.controller.config.Storage.DefaultPoolType
}

// CreatePool creates a pool from disks on node. A disk holding old data is
// refused unless wipe is set.
func (sm *StorageManager) CreatePool(ctx context.Context, name, poolType, node string, disks []string, sizeGB uint64, wipe bool) error {
	normalizedType, err := normalizeLVMPoolType(sm.defaultedPoolType(poolType))
	if err != nil {
		return err
	}
	name = normalizeManagedName(name)

	sm.controller.logger.Info("Creating pool",
		zap.String("name", name),
		zap.String("type", normalizedType),
		zap.String("node", node),
		zap.Strings("disks", disks))

	address := sm.controller.ResolveHost(node)
	if address == "" {
		return fmt.Errorf("node not found: %s", node)
	}

	if normalizedType == vdoPoolType {
		if err := sm.assertVDOSupported(ctx, address, node); err != nil {
			return err
		}
	}

	disks, err = sm.prepareDisks(ctx, address, node, disks, wipe)
	if err != nil {
		return err
	}

	// Create PVs first
	for _, disk := range disks {
		result, err := sm.controller.deployment.PVCreate(ctx, []string{address}, disk)
		if err != nil {
			return fmt.Errorf("failed to create PV on %s: %w", disk, err)
		}
		if !result.AllSuccess() {
			return fmt.Errorf("PV creation failed on %s for disk %s: %s", node, disk, result.FailureDetails())
		}
	}

	// Create VG
	result, err := sm.controller.deployment.VGCreate(ctx, []string{address}, name, disks)
	if err != nil {
		return fmt.Errorf("failed to create pool: %w", err)
	}

	if !result.AllSuccess() {
		return fmt.Errorf("failed to create pool: %s", result.FailureDetails())
	}

	// If type is thin_pool, create a thin pool LV
	if isThinPoolType(normalizedType) {
		// An explicit size is honoured; otherwise the pool takes 95% of the
		// group's free extents less [storage.thin] reserve_percent. What is
		// left is room for the metadata area to grow and for the pool to be
		// grown into before it fills. AddDiskToPool keeps the same ratio.
		thinPoolName := name + "_thin"
		thinSize := fmt.Sprintf("%d%%FREE", sm.controller.thinPoolPercentFree())
		if sizeGB > 0 {
			thinSize = fmt.Sprintf("%dG", sizeGB)
		}

		if normalizedType == vdoPoolType {
			if err := sm.createVDOThinPool(ctx, address, name, thinSize); err != nil {
				return err
			}
		} else if tpResult, err := sm.controller.deployment.LVCreateThinPool(ctx, []string{address}, name, thinPoolName, thinSize); err != nil {
			return fmt.Errorf("failed to create thin pool: %w", err)
		} else if !tpResult.AllSuccess() {
			return fmt.Errorf("failed to create thin pool: %s", tpResult.FailureDetails())
		}
	}

	sm.controller.logger.Info("Pool created successfully",
		zap.String("name", name),
		zap.String("node", node))

	if sm.controller.db != nil {
		dbPool := &database.Pool{
			Name:    name,
			Type:    normalizedType,
			Node:    node,
			Devices: strings.Join(disks, ","),
		}
		if err := sm.controller.db.SavePool(ctx, dbPool); err != nil {
			sm.controller.logger.Warn("Failed to save pool to database",
				zap.String("name", name),
				zap.Error(err))
		}
	}

	return nil
}

// GetPool gets pool information
func (sm *StorageManager) GetPool(ctx context.Context, poolName, node string) (*PoolInfo, error) {
	poolName = normalizeManagedName(poolName)
	persisted, persistedErr := sm.getPersistedPool(ctx, poolName)
	if persistedErr == nil && isZFSPoolType(persisted.Type) {
		return sm.GetZFSPool(ctx, poolName, node)
	}

	address := sm.controller.ResolveHost(node)
	result, err := sm.controller.deployment.Exec(ctx, []string{address}, "sudo vgs --noheadings --units b --separator '|' -o vg_name,vg_size,vg_free")
	if err != nil {
		if zfsPool, zfsErr := sm.GetZFSPool(ctx, poolName, node); zfsErr == nil {
			return zfsPool, nil
		}
		if persistedErr == nil {
			return persisted, nil
		}
		return nil, fmt.Errorf("failed to get pool: %w", err)
	}

	if !result.AllSuccess() {
		if zfsPool, zfsErr := sm.GetZFSPool(ctx, poolName, node); zfsErr == nil {
			return zfsPool, nil
		}
		if persistedErr == nil {
			return persisted, nil
		}
		return nil, fmt.Errorf("failed to get pool: %s", result.FailureDetails())
	}

	// Parse VGS output
	for _, r := range result.Hosts {
		if r.Success {
			lines := strings.Split(strings.TrimSpace(r.Output), "\n")
			for _, line := range lines {
				name, totalSize, freeSize, _, ok := parseLVMPoolLine(line)
				if ok && name == poolName {
					info := &PoolInfo{
						Name:       poolName,
						Type:       "vg",
						Node:       node,
						TotalGB:    bytesToGB(totalSize),
						FreeGB:     bytesToGB(freeSize),
						TotalBytes: totalSize,
						FreeBytes:  freeSize,
						Devices:    []string{},
					}
					// A pool that cannot report its cache is still a pool; the
					// capacity figures above are the reason this call exists.
					if cache, cerr := sm.readPoolCache(ctx, address, poolName); cerr == nil {
						info.Cache = cache
					} else {
						sm.controller.logger.Warn("Failed to read pool cache state",
							zap.String("pool", poolName), zap.Error(cerr))
					}
					// Same tolerance for utilisation, and the same reason it is
					// worth a second call: the vgs figures above cannot say
					// whether a thin pool is about to refuse writes.
					if usage, uerr := sm.readThinUsage(ctx, address, poolName); uerr == nil {
						info.ThinUsage = usage
					} else {
						sm.controller.logger.Warn("Failed to read thin pool usage",
							zap.String("pool", poolName), zap.Error(uerr))
					}
					return info, nil
				}
			}
		}
	}

	if zfsPool, zfsErr := sm.GetZFSPool(ctx, poolName, node); zfsErr == nil {
		return zfsPool, nil
	}

	if persistedErr == nil {
		return persisted, nil
	}

	return nil, fmt.Errorf("pool not found: %s", poolName)
}

// ListPools lists all pools across all nodes (LVM and ZFS)
func (sm *StorageManager) ListPools(ctx context.Context) ([]*PoolInfo, error) {
	var pools []*PoolInfo
	// Use map to deduplicate by normalized node name
	seen := make(map[string]bool)

	hosts := sm.poolHosts(ctx)
	if len(hosts) == 0 {
		if persisted, err := sm.listPersistedPools(ctx); err == nil {
			return persisted, nil
		}
		return pools, nil
	}

	// 1. Get LVM pools. Adding pv_name to the vgs output makes vgs emit one row
	// per physical volume (vg_name/size/free repeated), so a single SSH round
	// yields both the capacity AND the devices backing each VG — no separate
	// pvs call. Rows for the same VG are folded into one PoolInfo, collecting
	// its devices.
	poolByKey := make(map[string]*PoolInfo)
	result, err := sm.controller.deployment.Exec(ctx, hosts, "sudo vgs --noheadings --units b --separator '|' -o vg_name,vg_size,vg_free,pv_name")
	if err != nil {
		// Log error but continue to try ZFS
		sm.controller.logger.Warn("Failed to list LVM pools", zap.Error(err))
	} else {
		for host, r := range result.Hosts {
			if r.Success {
				normalizedHost := sm.controller.NormalizeHost(host)
				if normalizedHost == "" {
					normalizedHost = host
				}

				lines := strings.Split(strings.TrimSpace(r.Output), "\n")
				for _, line := range lines {
					line = strings.TrimSpace(line)
					if line == "" {
						continue
					}
					vgName, totalSize, freeSize, pv, ok := parseLVMPoolLine(line)
					if !ok || !strings.HasPrefix(vgName, managedNamePrefix) {
						continue
					}
					key := normalizedHost + "/lvm/" + vgName
					pool, exists := poolByKey[key]
					if !exists {
						pool = &PoolInfo{
							Name:       vgName,
							Type:       "vg",
							Node:       normalizedHost,
							TotalGB:    bytesToGB(totalSize),
							FreeGB:     bytesToGB(freeSize),
							TotalBytes: totalSize,
							FreeBytes:  freeSize,
						}
						poolByKey[key] = pool
						seen[key] = true
						pools = append(pools, pool)
					}
					if pv != "" {
						pool.Devices = append(pool.Devices, pv)
					}
				}
			}
		}
		for _, pool := range poolByKey {
			slices.Sort(pool.Devices)
		}

		// Two lvs calls for every host, folded into the rows above by the same
		// normalized host name they were keyed under.
		if len(poolByKey) > 0 {
			caches := sm.cacheByPool(ctx, hosts)
			usage := sm.thinUsageByPool(ctx, hosts)
			for _, pool := range poolByKey {
				if byVG, ok := caches[pool.Node]; ok {
					pool.Cache = byVG[pool.Name]
				}
				if byVG, ok := usage[pool.Node]; ok {
					pool.ThinUsage = byVG[pool.Name]
				}
			}
			sm.attachVDOUsage(ctx, poolByKey)
		}
	}

	// 2. Get ZFS pools
	zfsPools, err := sm.ListZFSpools(ctx)
	if err != nil {
		sm.controller.logger.Warn("Failed to list ZFS pools", zap.Error(err))
	} else {
		pools = append(pools, zfsPools...)
	}

	if len(pools) == 0 {
		if persisted, err := sm.listPersistedPools(ctx); err == nil {
			return persisted, nil
		}
	}

	return pools, nil
}

// DeletePool deletes a storage pool
func (sm *StorageManager) DeletePool(ctx context.Context, name, node string) error {
	name = normalizeManagedName(name)
	if persisted, err := sm.getPersistedPool(ctx, name); err == nil && isZFSPoolType(persisted.Type) {
		return sm.DeleteZFSPool(ctx, name, node)
	}

	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Deleting pool",
		zap.String("name", name),
		zap.String("node", node))

	// Remove the VG — but only an empty one. `vgremove -f` takes every LV
	// with it without asking, so deleting a pool that still held replicas
	// used to destroy them on the spot. The thin pool LV itself is the pool,
	// not a volume in it, and does not count. The PVs are wiped afterwards:
	// a disk left with a PV label is still claimed by LVM and cannot be
	// given to another pool without being cleared by hand.
	cmd := "echo " + base64Std(fmt.Sprintf(`set -e
vg=%[1]s
if vgs "$vg" >/dev/null 2>&1; then
  used=$(lvs --noheadings -o lv_name,lv_attr "$vg" | awk '$2 !~ /^t/ {print $1}' | tr '\n' ' ')
  if [ -n "$used" ]; then
    echo "pool $vg still holds volumes: $used— delete or move the resources on it first" >&2
    exit 3
  fi
  pvs=$(pvs --noheadings -o pv_name -S vg_name="$vg")
  vgremove -f "$vg"
  for pv in $pvs; do pvremove -y "$pv" >/dev/null; done
else
  exit 4
fi`, name)) + " | base64 -d | sudo /bin/bash"
	result, err := sm.controller.deployment.Exec(ctx, []string{address}, cmd)
	if err == nil && !result.AllSuccess() && strings.Contains(result.FailureDetails(), "still holds volumes") {
		return errors.New(result.FailureDetails())
	}
	if err != nil {
		if zfsErr := sm.DeleteZFSPool(ctx, name, node); zfsErr == nil {
			return nil
		}
		return fmt.Errorf("failed to delete pool: %w", err)
	}

	if !result.AllSuccess() {
		if zfsErr := sm.DeleteZFSPool(ctx, name, node); zfsErr == nil {
			return nil
		}
		return fmt.Errorf("failed to delete pool: %s", result.FailureDetails())
	}

	sm.controller.logger.Info("Pool deleted successfully",
		zap.String("name", name),
		zap.String("node", node))

	if sm.controller.db != nil {
		if err := sm.controller.db.DeletePool(ctx, name); err != nil {
			sm.controller.logger.Warn("Failed to delete pool from database",
				zap.String("name", name),
				zap.Error(err))
		}
	}

	return nil
}
