package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"go.uber.org/zap"
)

// CreateZFSPool creates a ZFS storage pool. A zpool has no thin/thick mode;
// thin vs thick provisioning is a per-zvol property applied at volume creation.
//
// compression and dedup are set on the pool's root dataset, so every volume
// inherits them. Both used to be accepted by the deployment layer and dropped
// on the floor, so a pool asked to compress did whatever the OpenZFS default
// was. An empty compression still means that default (on, i.e. lz4, from 2.2).
func (sm *StorageManager) CreateZFSPool(ctx context.Context, name, node string, vdevs []string, compression string, dedup, wipe bool) error {
	name = normalizeManagedName(name)
	if compression != "" && !deployment.ValidZFSCompression(compression) {
		return fmt.Errorf("unknown ZFS compression %q (on, off, lz4, zstd, zstd-1..19, gzip, gzip-1..9, lzjb, zle)", compression)
	}

	sm.controller.logger.Info("Creating ZFS pool",
		zap.String("name", name),
		zap.String("node", node),
		zap.Strings("vdevs", vdevs))

	// Convert node name to address
	address := sm.controller.ResolveHost(node)
	if address == "" {
		return fmt.Errorf("node not found: %s", node)
	}
	// zpool create -f overwrites whatever is on the devices, so they are
	// checked first like any other pool's.
	if _, err := sm.prepareDisks(ctx, address, node, zfsVdevDevices(vdevs), wipe); err != nil {
		return err
	}

	// Create ZFS pool
	result, err := sm.controller.deployment.ZFSCreatePool(ctx, []string{address}, name, vdevs,
		deployment.WithZFSCompression(compression), deployment.WithZFSDedup(dedup))
	if err != nil {
		return fmt.Errorf("failed to create ZFS pool: %w", err)
	}

	if !result.AllSuccess() {
		return fmt.Errorf("failed to create ZFS pool: %s", result.FailureDetails())
	}

	sm.controller.logger.Info("ZFS pool created successfully",
		zap.String("name", name),
		zap.String("node", node))

	if sm.controller.db != nil {
		dbPool := &database.Pool{
			Name:    name,
			Type:    "zfs",
			Node:    node,
			Devices: strings.Join(vdevs, ","),
		}
		if err := sm.controller.db.SavePool(ctx, dbPool); err != nil {
			sm.controller.logger.Warn("Failed to save ZFS pool to database",
				zap.String("name", name),
				zap.Error(err))
		}
	}

	return nil
}

// GetZFSPool gets ZFS pool information
func (sm *StorageManager) GetZFSPool(ctx context.Context, poolName, node string) (*PoolInfo, error) {
	poolName = normalizeManagedName(poolName)
	address := sm.controller.ResolveHost(node)
	result, err := sm.controller.deployment.Exec(ctx, []string{address},
		fmt.Sprintf("sudo zpool list -Hp -o name,size,free,cap %s", poolName))
	if err != nil {
		if persisted, dbErr := sm.getPersistedPool(ctx, poolName); dbErr == nil {
			return persisted, nil
		}
		return nil, fmt.Errorf("failed to get ZFS pool: %w", err)
	}

	if !result.AllSuccess() {
		if persisted, dbErr := sm.getPersistedPool(ctx, poolName); dbErr == nil {
			return persisted, nil
		}
		return nil, fmt.Errorf("failed to get ZFS pool: %s", result.FailureDetails())
	}

	for _, r := range result.Hosts {
		if r.Success && r.Output != "" {
			fields := strings.Fields(r.Output)
			if len(fields) >= 4 {
				totalSize, _ := strconv.ParseUint(fields[1], 10, 64)
				freeSize, _ := strconv.ParseUint(fields[2], 10, 64)
				info := &PoolInfo{
					Name:       poolName,
					Type:       "zfs",
					Node:       node,
					TotalGB:    bytesToGB(totalSize),
					FreeGB:     bytesToGB(freeSize),
					TotalBytes: totalSize,
					FreeBytes:  freeSize,
					Devices:    []string{},
				}
				sm.fillZFSCompression(ctx, []*PoolInfo{info}, map[string]string{node: address})
				return info, nil
			}
		}
	}

	if persisted, err := sm.getPersistedPool(ctx, poolName); err == nil {
		return persisted, nil
	}

	return nil, fmt.Errorf("ZFS pool not found: %s", poolName)
}

// ListZFSpools lists all ZFS pools across all nodes
func (sm *StorageManager) ListZFSpools(ctx context.Context) ([]*PoolInfo, error) {
	var pools []*PoolInfo
	seen := make(map[string]bool)

	hosts := sm.poolHosts(ctx)
	if len(hosts) == 0 {
		if persisted, err := sm.listPersistedPools(ctx); err == nil {
			var zfsPools []*PoolInfo
			for _, pool := range persisted {
				if isZFSPoolType(pool.Type) {
					zfsPools = append(zfsPools, pool)
				}
			}
			return zfsPools, nil
		}
		return pools, nil
	}

	result, err := sm.controller.deployment.ZFSListPools(ctx, hosts)
	if err != nil {
		return nil, fmt.Errorf("failed to list ZFS pools: %w", err)
	}

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
				fields := strings.Fields(line)
				if len(fields) >= 4 {
					poolName := fields[0]
					// Only show Haify-managed pools (with haify_ prefix)
					if !strings.HasPrefix(poolName, "haify_") {
						continue
					}
					key := normalizedHost + "/" + poolName
					if seen[key] {
						continue
					}
					seen[key] = true

					totalSize, _ := strconv.ParseUint(fields[1], 10, 64)
					freeSize, _ := strconv.ParseUint(fields[2], 10, 64)
					pools = append(pools, &PoolInfo{
						Name:       poolName,
						Type:       "zfs",
						Node:       normalizedHost,
						TotalGB:    bytesToGB(totalSize),
						FreeGB:     bytesToGB(freeSize),
						TotalBytes: totalSize,
						FreeBytes:  freeSize,
					})
				}
			}
		}
	}

	addrs := make(map[string]string, len(result.Hosts))
	for host := range result.Hosts {
		if n := sm.controller.NormalizeHost(host); n != "" {
			addrs[n] = host
		} else {
			addrs[host] = host
		}
	}
	sm.fillZFSCompression(ctx, pools, addrs)

	if len(pools) == 0 {
		if persisted, err := sm.listPersistedPools(ctx); err == nil {
			var zfsPools []*PoolInfo
			for _, pool := range persisted {
				if isZFSPoolType(pool.Type) {
					zfsPools = append(zfsPools, pool)
				}
			}
			return zfsPools, nil
		}
	}

	return pools, nil
}

// DeleteZFSPool deletes a ZFS storage pool
func (sm *StorageManager) DeleteZFSPool(ctx context.Context, name, node string) error {
	name = normalizeManagedName(name)
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Deleting ZFS pool",
		zap.String("name", name),
		zap.String("node", node))

	// zpool destroy -f takes every dataset and zvol with it; refuse a pool
	// that still holds any, as the LVM path does.
	check, err := sm.controller.deployment.Exec(ctx, []string{address},
		fmt.Sprintf("sudo zfs list -H -o name -r %s 2>/dev/null | tail -n +2 | tr '\n' ' '", name))
	if err == nil {
		for _, h := range check.Hosts {
			if held := strings.TrimSpace(h.Output); h.Success && held != "" {
				return fmt.Errorf("pool %s still holds %s— delete or move the resources on it first", name, held)
			}
		}
	}

	result, err := sm.controller.deployment.ZFSDestroyPool(ctx, []string{address}, name)
	if err != nil {
		return fmt.Errorf("failed to delete ZFS pool: %w", err)
	}

	if !result.AllSuccess() {
		return fmt.Errorf("failed to delete ZFS pool: %s", result.FailureDetails())
	}

	sm.controller.logger.Info("ZFS pool deleted successfully",
		zap.String("name", name),
		zap.String("node", node))

	if sm.controller.db != nil {
		if err := sm.controller.db.DeletePool(ctx, name); err != nil {
			sm.controller.logger.Warn("Failed to delete ZFS pool from database",
				zap.String("name", name),
				zap.Error(err))
		}
	}

	return nil
}

// CreateZFSDataset creates a ZFS dataset
func (sm *StorageManager) CreateZFSDataset(ctx context.Context, datasetPath, node string) error {
	datasetPath = normalizeManagedZFSPath(datasetPath)
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Creating ZFS dataset",
		zap.String("dataset", datasetPath),
		zap.String("node", node))

	result, err := sm.controller.deployment.ZFSCreateDataset(ctx, []string{address}, datasetPath)
	if err != nil {
		return fmt.Errorf("failed to create ZFS dataset: %w", err)
	}

	if !result.AllSuccess() {
		return fmt.Errorf("failed to create ZFS dataset: %s", result.FailureDetails())
	}

	return nil
}

// ZFSDeleteDataset destroys a ZFS dataset or volume
func (sm *StorageManager) ZFSDeleteDataset(ctx context.Context, datasetPath, node string) error {
	datasetPath = normalizeManagedZFSPath(datasetPath)
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Deleting ZFS dataset",
		zap.String("dataset", datasetPath),
		zap.String("node", node))

	result, err := sm.controller.deployment.ZFSDestroyDataset(ctx, []string{address}, datasetPath)
	if err != nil {
		return fmt.Errorf("failed to delete ZFS dataset: %w", err)
	}

	if !result.AllSuccess() {
		for host, hr := range result.Hosts {
			if !hr.Success {
				return fmt.Errorf("failed to delete ZFS dataset on %s: %s", host, strings.TrimSpace(hr.Output+hr.Error.Error()))
			}
		}
	}

	return nil
}

// CreateZFSThinVolume creates a thin-provisioned ZFS volume
func (sm *StorageManager) CreateZFSThinVolume(ctx context.Context, poolName, volumeName, size, node string) error {
	poolName = normalizeManagedName(poolName)
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Creating ZFS thin volume",
		zap.String("pool", poolName),
		zap.String("volume", volumeName),
		zap.String("size", size),
		zap.String("node", node))

	volumePath := fmt.Sprintf("%s/%s", poolName, volumeName)
	result, err := sm.controller.deployment.ZFSCreateThinDataset(ctx, []string{address}, poolName, volumeName, size)
	if err != nil {
		return fmt.Errorf("failed to create ZFS thin volume: %w", err)
	}

	if !result.AllSuccess() {
		return fmt.Errorf("failed to create ZFS thin volume: %s", result.FailureDetails())
	}

	// Set quota for thin provisioning
	_, _ = sm.controller.deployment.ZFSSetQuota(ctx, []string{address}, volumePath, size)

	return nil
}

// ZFSSnapshot creates a ZFS snapshot
func (sm *StorageManager) ZFSSnapshot(ctx context.Context, dataset, snapshotName, node string) error {
	dataset = normalizeManagedZFSPath(dataset)
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Creating ZFS snapshot",
		zap.String("dataset", dataset),
		zap.String("snapshot", snapshotName),
		zap.String("node", node))

	result, err := sm.controller.deployment.ZFSSnapshot(ctx, []string{address}, dataset, snapshotName)
	if err != nil {
		return fmt.Errorf("failed to create ZFS snapshot: %w", err)
	}

	if !result.AllSuccess() {
		return fmt.Errorf("failed to create ZFS snapshot: %s", result.FailureDetails())
	}

	return nil
}

// ZFSListSnapshots lists ZFS snapshots for a dataset
func (sm *StorageManager) ZFSListSnapshots(ctx context.Context, dataset, node string) ([]*SnapshotInfo, error) {
	// Resolve node name to address
	dataset = normalizeManagedZFSPath(dataset)
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Listing ZFS snapshots",
		zap.String("dataset", dataset),
		zap.String("node", node),
		zap.String("address", address))

	result, err := sm.controller.deployment.ZFSListSnapshots(ctx, []string{address}, dataset)
	if err != nil {
		return nil, fmt.Errorf("failed to list ZFS snapshots: %w", err)
	}

	var snapshots []*SnapshotInfo
	for host, r := range result.Hosts {
		if r.Success {
			sm.controller.logger.Info("ZFS snapshot list output",
				zap.String("host", host),
				zap.String("output", r.Output))

			lines := strings.Split(strings.TrimSpace(r.Output), "\n")
			for _, line := range lines {
				if line == "" {
					continue
				}
				name, volume, createdAt, ok := parseZFSSnapshotLine(line)
				if !ok {
					continue
				}
				snapshots = append(snapshots, &SnapshotInfo{
					Name:      name,
					Volume:    volume,
					CreatedAt: createdAt,
				})
			}
		} else {
			sm.controller.logger.Warn("Failed to list ZFS snapshots on host",
				zap.String("host", host),
				zap.String("error", r.Output))
		}
	}

	return snapshots, nil
}

// ZFSDeleteSnapshot deletes a ZFS snapshot
func (sm *StorageManager) ZFSDeleteSnapshot(ctx context.Context, snapshot, node string) error {
	snapshot = normalizeManagedZFSPath(snapshot)
	if _, name, ok := strings.Cut(snapshot, "@"); ok {
		if err := sm.controller.assertSnapshotUnlocked(ctx, name); err != nil {
			return err
		}
	}
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Deleting ZFS snapshot",
		zap.String("snapshot", snapshot),
		zap.String("node", node))

	sm.controller.releaseZFSLockHold(ctx, address, snapshot)
	result, err := sm.controller.deployment.ZFSDestroySnapshot(ctx, []string{address}, snapshot)
	if err != nil {
		return fmt.Errorf("failed to delete ZFS snapshot: %w", err)
	}

	if !result.AllSuccess() {
		for host, hr := range result.Hosts {
			if !hr.Success {
				return fmt.Errorf("failed to delete ZFS snapshot on %s: %s", host, strings.TrimSpace(hr.Output+hr.Error.Error()))
			}
		}
	}

	return nil
}

// ZFSRestoreSnapshot restores a ZFS snapshot (rollback)
func (sm *StorageManager) ZFSRestoreSnapshot(ctx context.Context, dataset, snapshotName, node string) error {
	dataset = normalizeManagedZFSPath(dataset)
	sm.controller.logger.Info("Restoring ZFS snapshot",
		zap.String("dataset", dataset),
		zap.String("snapshot", snapshotName),
		zap.String("node", node))
	return sm.controller.snapshots.RestoreZFSSnapshot(ctx, dataset, snapshotName, node)
}

// ZFSCloneSnapshot creates a clone from a ZFS snapshot
func (sm *StorageManager) ZFSCloneSnapshot(ctx context.Context, snapshot, clonePath, node string) error {
	snapshot = normalizeManagedZFSPath(snapshot)
	clonePath = normalizeManagedZFSPath(clonePath)
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Cloning ZFS snapshot",
		zap.String("snapshot", snapshot),
		zap.String("clone", clonePath),
		zap.String("node", node))

	result, err := sm.controller.deployment.ZFSClone(ctx, []string{address}, snapshot, clonePath)
	if err != nil {
		return fmt.Errorf("failed to clone ZFS snapshot: %w", err)
	}

	if !result.AllSuccess() {
		return fmt.Errorf("failed to clone ZFS snapshot: %s", result.FailureDetails())
	}

	return nil
}

// ZFSResizeVolume resizes a ZFS volume
func (sm *StorageManager) ZFSResizeVolume(ctx context.Context, volumePath, newSize, node string) error {
	volumePath = normalizeManagedZFSPath(volumePath)
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Resizing ZFS volume",
		zap.String("volume", volumePath),
		zap.String("size", newSize),
		zap.String("node", node))

	result, err := sm.controller.deployment.ZFSResizeVolume(ctx, []string{address}, volumePath, newSize)
	if err != nil {
		return fmt.Errorf("failed to resize ZFS volume: %w", err)
	}

	if !result.AllSuccess() {
		return fmt.Errorf("failed to resize ZFS volume: %s", result.FailureDetails())
	}

	return nil
}
