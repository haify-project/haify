package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"go.uber.org/zap"
)

// ZFS pools: create, read, list and delete. Datasets, zvols and snapshots
// inside a pool are in storage_zfs_dataset.go.

// CreateZFSPool creates a ZFS storage pool. A zpool has no thin/thick mode;
// thin vs thick provisioning is a per-zvol property applied at volume creation.
//
// compression and dedup are set on the pool's root dataset, so every volume
// inherits them. Both used to be accepted by the deployment layer and dropped
// on the floor, so a pool asked to compress did whatever the OpenZFS default
// was. An empty compression still means that default (on, i.e. lz4, from 2.2).
func (sm *StorageManager) CreateZFSPool(ctx context.Context, name, node string, vdevs []string, compression string, dedup, wipe bool) error {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("CreateZFSPool: creating", zap.String("pool", name), zap.String("node", node), zap.Strings("vdevs", vdevs),
		zap.String("compression", compression), zap.Bool("dedup", dedup), zap.Bool("wipe", wipe))
	name = normalizeManagedName(name)
	if compression != "" && !deployment.ValidZFSCompression(compression) {
		log.Debug("CreateZFSPool: unknown compression", zap.String("compression", compression))
		return fmt.Errorf("unknown ZFS compression %q (on, off, lz4, zstd, zstd-1..19, gzip, gzip-1..9, lzjb, zle)", compression)
	}

	sm.controller.logger.Info("Creating ZFS pool",
		zap.String("name", name),
		zap.String("node", node),
		zap.Strings("vdevs", vdevs))

	// Convert node name to address
	address := sm.controller.ResolveHost(node)
	if address == "" {
		log.Debug("CreateZFSPool: node not found", zap.String("node", node))
		return fmt.Errorf("node not found: %s", node)
	}
	// zpool create -f overwrites whatever is on the devices, so they are
	// checked first like any other pool's.
	if _, err := sm.prepareDisks(ctx, address, node, zfsVdevDevices(vdevs), wipe); err != nil {
		log.Debug("CreateZFSPool: disk preparation failed", zap.Error(err))
		return err
	}

	// Create ZFS pool
	start := time.Now()
	result, err := sm.controller.deployment.ZFSCreatePool(ctx, []string{address}, name, vdevs,
		deployment.WithZFSCompression(compression), deployment.WithZFSDedup(dedup))
	if err != nil {
		log.Debug("CreateZFSPool: zpool create errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to create ZFS pool: %w", err)
	}

	if !result.AllSuccess() {
		log.Debug("CreateZFSPool: zpool create failed", zap.String("host", address), zap.Duration("took", time.Since(start)))
		return fmt.Errorf("failed to create ZFS pool: %s", result.FailureDetails())
	}
	log.Debug("CreateZFSPool: zpool created", zap.String("host", address), zap.String("pool", name), zap.Duration("took", time.Since(start)))

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
		} else {
			log.Debug("CreateZFSPool: pool record saved", zap.String("pool", name))
		}
	}

	return nil
}

// GetZFSPool gets ZFS pool information
func (sm *StorageManager) GetZFSPool(ctx context.Context, poolName, node string) (*PoolInfo, error) {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("GetZFSPool: looking up", zap.String("pool", poolName), zap.String("node", node))
	poolName = normalizeManagedName(poolName)
	address := sm.controller.ResolveHost(node)
	start := time.Now()
	result, err := sm.controller.deployment.Exec(ctx, []string{address},
		fmt.Sprintf("sudo zpool list -Hp -o name,size,free,cap %s", poolName))
	if err != nil {
		log.Debug("GetZFSPool: zpool list errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		if persisted, dbErr := sm.getPersistedPool(ctx, poolName); dbErr == nil {
			log.Debug("GetZFSPool: using the stored record", zap.String("pool", poolName))
			return persisted, nil
		}
		return nil, fmt.Errorf("failed to get ZFS pool: %w", err)
	}

	if !result.AllSuccess() {
		log.Debug("GetZFSPool: zpool list failed", zap.String("host", address), zap.Duration("took", time.Since(start)))
		if persisted, dbErr := sm.getPersistedPool(ctx, poolName); dbErr == nil {
			log.Debug("GetZFSPool: using the stored record", zap.String("pool", poolName))
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
				log.Debug("GetZFSPool: found", zap.String("pool", poolName), zap.Uint64("total_bytes", totalSize),
					zap.Uint64("free_bytes", freeSize), zap.Duration("took", time.Since(start)))
				return info, nil
			}
		}
	}

	if persisted, err := sm.getPersistedPool(ctx, poolName); err == nil {
		log.Debug("GetZFSPool: not on the node, using the stored record", zap.String("pool", poolName))
		return persisted, nil
	}

	log.Debug("GetZFSPool: not found", zap.String("pool", poolName))
	return nil, fmt.Errorf("ZFS pool not found: %s", poolName)
}

// ListZFSpools lists all ZFS pools across all nodes
func (sm *StorageManager) ListZFSpools(ctx context.Context) ([]*PoolInfo, error) {
	log := ctrlLogOrGlobal(sm.controller)
	var pools []*PoolInfo
	seen := make(map[string]bool)

	hosts := sm.poolHosts(ctx)
	log.Debug("ListZFSpools: listing", zap.Strings("hosts", hosts))
	if len(hosts) == 0 {
		if persisted, err := sm.listPersistedPools(ctx); err == nil {
			var zfsPools []*PoolInfo
			for _, pool := range persisted {
				if isZFSPoolType(pool.Type) {
					zfsPools = append(zfsPools, pool)
				}
			}
			log.Debug("ListZFSpools: no hosts, returning stored records", zap.Int("count", len(zfsPools)))
			return zfsPools, nil
		}
		log.Debug("ListZFSpools: no hosts and no records")
		return pools, nil
	}

	start := time.Now()
	result, err := sm.controller.deployment.ZFSListPools(ctx, hosts)
	if err != nil {
		log.Debug("ListZFSpools: zpool list errored", zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, fmt.Errorf("failed to list ZFS pools: %w", err)
	}
	log.Debug("ListZFSpools: zpool list answered", zap.Strings("failed_hosts", result.FailedHosts()), zap.Duration("took", time.Since(start)))

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
					log.Debug("ListZFSpools: pool", zap.String("node", normalizedHost), zap.String("pool", poolName),
						zap.Uint64("total_bytes", totalSize), zap.Uint64("free_bytes", freeSize))
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
			log.Debug("ListZFSpools: none on the nodes, returning stored records", zap.Int("count", len(zfsPools)))
			return zfsPools, nil
		}
	}

	log.Debug("ListZFSpools: listed", zap.Int("count", len(pools)))
	return pools, nil
}

// DeleteZFSPool deletes a ZFS storage pool
func (sm *StorageManager) DeleteZFSPool(ctx context.Context, name, node string) error {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("DeleteZFSPool: deleting", zap.String("pool", name), zap.String("node", node))
	name = normalizeManagedName(name)
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Deleting ZFS pool",
		zap.String("name", name),
		zap.String("node", node))

	// zpool destroy -f takes every dataset and zvol with it; refuse a pool
	// that still holds any, as the LVM path does.
	start := time.Now()
	check, err := sm.controller.deployment.Exec(ctx, []string{address},
		fmt.Sprintf("sudo zfs list -H -o name -r %s 2>/dev/null | tail -n +2 | tr '\n' ' '", name))
	if err == nil {
		for _, h := range check.Hosts {
			if held := strings.TrimSpace(h.Output); h.Success && held != "" {
				log.Debug("DeleteZFSPool: pool still holds datasets, refusing", zap.String("pool", name), zap.String("held", held))
				return fmt.Errorf("pool %s still holds %s— delete or move the resources on it first", name, held)
			}
		}
		log.Debug("DeleteZFSPool: pool is empty", zap.String("pool", name), zap.Duration("took", time.Since(start)))
	} else {
		log.Debug("DeleteZFSPool: emptiness check errored, destroying anyway", zap.String("pool", name), zap.Error(err))
	}

	start = time.Now()
	result, err := sm.controller.deployment.ZFSDestroyPool(ctx, []string{address}, name)
	if err != nil {
		log.Debug("DeleteZFSPool: zpool destroy errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to delete ZFS pool: %w", err)
	}

	if !result.AllSuccess() {
		log.Debug("DeleteZFSPool: zpool destroy failed", zap.String("host", address), zap.Duration("took", time.Since(start)))
		return fmt.Errorf("failed to delete ZFS pool: %s", result.FailureDetails())
	}
	log.Debug("DeleteZFSPool: zpool destroyed", zap.String("pool", name), zap.Duration("took", time.Since(start)))

	sm.controller.logger.Info("ZFS pool deleted successfully",
		zap.String("name", name),
		zap.String("node", node))

	if sm.controller.db != nil {
		if err := sm.controller.db.DeletePool(ctx, name); err != nil {
			sm.controller.logger.Warn("Failed to delete ZFS pool from database",
				zap.String("name", name),
				zap.Error(err))
		} else {
			log.Debug("DeleteZFSPool: pool record deleted", zap.String("pool", name))
		}
	}

	return nil
}
