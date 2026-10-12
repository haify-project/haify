package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

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
	ctrlLogOrGlobal(ctrl).Debug("NewStorageManager: building storage manager")
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

	zap.L().Debug("poolInfoFromDB: pool from record", zap.String("pool", pool.Name), zap.String("type", pool.Type),
		zap.String("node", pool.Node), zap.Strings("devices", devices))
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
	log := ctrlLogOrGlobal(sm.controller)
	if sm.controller.db == nil {
		log.Debug("getPersistedPool: no database", zap.String("pool", name))
		return nil, fmt.Errorf("database not available")
	}

	start := time.Now()
	pool, err := sm.controller.db.GetPool(ctx, name)
	if err != nil {
		log.Debug("getPersistedPool: not found", zap.String("pool", name), zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, err
	}

	log.Debug("getPersistedPool: found", zap.String("pool", name), zap.String("type", pool.Type), zap.Duration("took", time.Since(start)))
	return poolInfoFromDB(pool), nil
}

func (sm *StorageManager) listPersistedPools(ctx context.Context) ([]*PoolInfo, error) {
	log := ctrlLogOrGlobal(sm.controller)
	if sm.controller.db == nil {
		log.Debug("listPersistedPools: no database")
		return nil, fmt.Errorf("database not available")
	}

	start := time.Now()
	pools, err := sm.controller.db.ListPools(ctx)
	if err != nil {
		log.Debug("listPersistedPools: database read failed", zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, err
	}

	result := make([]*PoolInfo, 0, len(pools))
	for _, pool := range pools {
		result = append(result, poolInfoFromDB(pool))
	}
	log.Debug("listPersistedPools: read", zap.Int("count", len(result)), zap.Duration("took", time.Since(start)))
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
	zap.L().Debug("normalizeDeviceList: normalized", zap.Int("given", len(devices)), zap.Strings("devices", result))
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
		zap.L().Debug("defaultedPoolType: type given", zap.String("type", poolType))
		return poolType
	}
	if sm.controller == nil || sm.controller.config == nil {
		zap.L().Debug("defaultedPoolType: no type and no config, left empty")
		return poolType
	}
	zap.L().Debug("defaultedPoolType: using configured default", zap.String("type", sm.controller.config.Storage.DefaultPoolType))
	return sm.controller.config.Storage.DefaultPoolType
}

// CreatePool creates a pool from disks on node. A disk holding old data is
// refused unless wipe is set.
func (sm *StorageManager) CreatePool(ctx context.Context, name, poolType, node string, disks []string, sizeGB uint64, wipe bool) error {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("CreatePool: creating", zap.String("pool", name), zap.String("type", poolType), zap.String("node", node),
		zap.Strings("disks", disks), zap.Uint64("size_gb", sizeGB), zap.Bool("wipe", wipe))
	normalizedType, err := normalizeLVMPoolType(sm.defaultedPoolType(poolType))
	if err != nil {
		log.Debug("CreatePool: bad pool type", zap.String("type", poolType), zap.Error(err))
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
		log.Debug("CreatePool: node not found", zap.String("node", node))
		return fmt.Errorf("node not found: %s", node)
	}

	if normalizedType == vdoPoolType {
		log.Debug("CreatePool: VDO pool, checking support", zap.String("host", address))
		if err := sm.assertVDOSupported(ctx, address, node); err != nil {
			log.Debug("CreatePool: VDO not supported", zap.String("node", node), zap.Error(err))
			return err
		}
	}

	// Disks are checked before anything is touched; file: devices become
	// loop devices (pool_file.go), and are recorded as given so delete can
	// remove them.
	var real, files []string
	for _, d := range disks {
		if isFileDevice(d) {
			log.Debug("CreatePool: file-backed device", zap.String("device", d))
			files = append(files, d)
		} else {
			log.Debug("CreatePool: block device", zap.String("device", d))
			real = append(real, d)
		}
	}
	if len(real) > 0 {
		if real, err = sm.prepareDisks(ctx, address, node, real, wipe); err != nil {
			log.Debug("CreatePool: disk preparation failed", zap.Error(err))
			return err
		}
	}
	loops, err := sm.attachFileDevices(ctx, address, node, name, files, isThinPoolType(normalizedType))
	if err != nil {
		log.Debug("CreatePool: file devices not attached", zap.Error(err))
		return err
	}
	recorded := append(append([]string(nil), real...), files...)
	disks = append(real, loops...)
	log.Debug("CreatePool: devices ready", zap.Strings("disks", disks), zap.Strings("recorded", recorded))

	// Create PVs first
	for _, disk := range disks {
		start := time.Now()
		result, err := sm.controller.deployment.PVCreate(ctx, []string{address}, disk)
		if err != nil {
			log.Debug("CreatePool: pvcreate errored", zap.String("host", address), zap.String("disk", disk),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			return fmt.Errorf("failed to create PV on %s: %w", disk, err)
		}
		if !result.AllSuccess() {
			log.Debug("CreatePool: pvcreate failed", zap.String("host", address), zap.String("disk", disk),
				zap.Duration("took", time.Since(start)))
			return fmt.Errorf("PV creation failed on %s for disk %s: %s", node, disk, result.FailureDetails())
		}
		log.Debug("CreatePool: PV created", zap.String("host", address), zap.String("disk", disk), zap.Duration("took", time.Since(start)))
	}

	// Create VG
	start := time.Now()
	result, err := sm.controller.deployment.VGCreate(ctx, []string{address}, name, disks)
	if err != nil {
		log.Debug("CreatePool: vgcreate errored", zap.String("host", address), zap.String("pool", name),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to create pool: %w", err)
	}

	if !result.AllSuccess() {
		log.Debug("CreatePool: vgcreate failed", zap.String("host", address), zap.String("pool", name), zap.Duration("took", time.Since(start)))
		return fmt.Errorf("failed to create pool: %s", result.FailureDetails())
	}
	log.Debug("CreatePool: VG created", zap.String("host", address), zap.String("pool", name), zap.Duration("took", time.Since(start)))

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
		log.Debug("CreatePool: creating thin pool", zap.String("thin_lv", thinPoolName), zap.String("size", thinSize),
			zap.Bool("vdo", normalizedType == vdoPoolType))

		start := time.Now()
		if normalizedType == vdoPoolType {
			if err := sm.createVDOThinPool(ctx, address, name, thinSize); err != nil {
				log.Debug("CreatePool: VDO thin pool failed", zap.Error(err))
				return err
			}
		} else if tpResult, err := sm.controller.deployment.LVCreateThinPool(ctx, []string{address}, name, thinPoolName, thinSize); err != nil {
			log.Debug("CreatePool: thin pool lvcreate errored", zap.Duration("took", time.Since(start)), zap.Error(err))
			return fmt.Errorf("failed to create thin pool: %w", err)
		} else if !tpResult.AllSuccess() {
			log.Debug("CreatePool: thin pool lvcreate failed", zap.Duration("took", time.Since(start)))
			return fmt.Errorf("failed to create thin pool: %s", tpResult.FailureDetails())
		}
		log.Debug("CreatePool: thin pool created", zap.String("thin_lv", thinPoolName), zap.Duration("took", time.Since(start)))
	}

	sm.controller.logger.Info("Pool created successfully",
		zap.String("name", name),
		zap.String("node", node))

	if sm.controller.db != nil {
		dbPool := &database.Pool{
			Name:    name,
			Type:    normalizedType,
			Node:    node,
			Devices: strings.Join(recorded, ","),
		}
		if err := sm.controller.db.SavePool(ctx, dbPool); err != nil {
			sm.controller.logger.Warn("Failed to save pool to database",
				zap.String("name", name),
				zap.Error(err))
		} else {
			log.Debug("CreatePool: pool record saved", zap.String("pool", name))
		}
	} else {
		log.Debug("CreatePool: no database, pool record not saved", zap.String("pool", name))
	}

	return nil
}

// DeletePool deletes a storage pool
func (sm *StorageManager) DeletePool(ctx context.Context, name, node string) error {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("DeletePool: deleting", zap.String("pool", name), zap.String("node", node))
	name = normalizeManagedName(name)
	if persisted, err := sm.getPersistedPool(ctx, name); err == nil && isZFSPoolType(persisted.Type) {
		log.Debug("DeletePool: recorded as ZFS, deleting the zpool", zap.String("pool", name))
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
  %[2]s
else
  exit 4
fi`, name, fileTeardownScript(name))) + " | base64 -d | sudo /bin/bash"
	start := time.Now()
	result, err := sm.controller.deployment.Exec(ctx, []string{address}, cmd)
	log.Debug("DeletePool: vgremove script ran", zap.String("host", address), zap.String("pool", name),
		zap.Bool("all_success", err == nil && result.AllSuccess()), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err == nil && !result.AllSuccess() && strings.Contains(result.FailureDetails(), "still holds volumes") {
		log.Debug("DeletePool: pool still holds volumes, refusing", zap.String("pool", name))
		return errors.New(result.FailureDetails())
	}
	if err != nil {
		if zfsErr := sm.DeleteZFSPool(ctx, name, node); zfsErr == nil {
			log.Debug("DeletePool: exec failed but it was a zpool, deleted that", zap.String("pool", name))
			return nil
		}
		return fmt.Errorf("failed to delete pool: %w", err)
	}

	if !result.AllSuccess() {
		if zfsErr := sm.DeleteZFSPool(ctx, name, node); zfsErr == nil {
			log.Debug("DeletePool: not a VG but a zpool, deleted that", zap.String("pool", name))
			return nil
		}
		log.Debug("DeletePool: failed", zap.String("pool", name), zap.Strings("failed_hosts", result.FailedHosts()))
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
		} else {
			log.Debug("DeletePool: pool record deleted", zap.String("pool", name))
		}
	}

	return nil
}
