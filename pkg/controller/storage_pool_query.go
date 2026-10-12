package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Reading pools back from the nodes: one pool (GetPool) and every pool on
// every node (ListPools), LVM first and ZFS after, with the controller's own
// records as the fallback when the nodes cannot be asked.

// GetPool gets pool information
func (sm *StorageManager) GetPool(ctx context.Context, poolName, node string) (*PoolInfo, error) {
	log := sm.controller.traceLogger()
	log.Debug("GetPool: looking up", zap.String("pool", poolName), zap.String("node", node))
	poolName = normalizeManagedName(poolName)
	persisted, persistedErr := sm.getPersistedPool(ctx, poolName)
	if persistedErr == nil && isZFSPoolType(persisted.Type) {
		log.Debug("GetPool: recorded as ZFS", zap.String("pool", poolName))
		return sm.GetZFSPool(ctx, poolName, node)
	}

	address := sm.controller.ResolveHost(node)
	start := time.Now()
	result, err := sm.controller.deployment.Exec(ctx, []string{address}, "sudo vgs --noheadings --units b --separator '|' -o vg_name,vg_size,vg_free")
	if err != nil {
		log.Debug("GetPool: vgs errored, trying ZFS then the record", zap.String("host", address),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if zfsPool, zfsErr := sm.GetZFSPool(ctx, poolName, node); zfsErr == nil {
			return zfsPool, nil
		}
		if persistedErr == nil {
			log.Debug("GetPool: using the stored record", zap.String("pool", poolName))
			return persisted, nil
		}
		return nil, fmt.Errorf("failed to get pool: %w", err)
	}

	if !result.AllSuccess() {
		log.Debug("GetPool: vgs failed, trying ZFS then the record", zap.String("host", address),
			zap.Duration("took", time.Since(start)))
		if zfsPool, zfsErr := sm.GetZFSPool(ctx, poolName, node); zfsErr == nil {
			return zfsPool, nil
		}
		if persistedErr == nil {
			log.Debug("GetPool: using the stored record", zap.String("pool", poolName))
			return persisted, nil
		}
		return nil, fmt.Errorf("failed to get pool: %s", result.FailureDetails())
	}
	log.Debug("GetPool: vgs answered", zap.String("host", address), zap.Duration("took", time.Since(start)))

	// Parse VGS output
	for _, r := range result.Hosts {
		if r.Success {
			lines := strings.Split(strings.TrimSpace(r.Output), "\n")
			for _, line := range lines {
				name, totalSize, freeSize, _, ok := parseLVMPoolLine(line)
				if ok && name == poolName {
					log.Debug("GetPool: found volume group", zap.String("pool", poolName), zap.Uint64("total_bytes", totalSize),
						zap.Uint64("free_bytes", freeSize))
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
					log.Debug("GetPool: done", zap.String("pool", poolName), zap.Bool("cached", info.Cache != nil),
						zap.Bool("thin", info.ThinUsage != nil))
					return info, nil
				}
			}
		}
	}

	log.Debug("GetPool: no such volume group, trying ZFS", zap.String("pool", poolName))
	if zfsPool, zfsErr := sm.GetZFSPool(ctx, poolName, node); zfsErr == nil {
		return zfsPool, nil
	}

	if persistedErr == nil {
		log.Debug("GetPool: using the stored record", zap.String("pool", poolName))
		return persisted, nil
	}

	log.Debug("GetPool: not found anywhere", zap.String("pool", poolName))
	return nil, fmt.Errorf("pool not found: %s", poolName)
}

// ListPools lists all pools across all nodes (LVM and ZFS)
func (sm *StorageManager) ListPools(ctx context.Context) ([]*PoolInfo, error) {
	log := sm.controller.traceLogger()
	var pools []*PoolInfo
	// Use map to deduplicate by normalized node name
	seen := make(map[string]bool)

	hosts := sm.poolHosts(ctx)
	log.Debug("ListPools: listing", zap.Strings("hosts", hosts))
	if len(hosts) == 0 {
		if persisted, err := sm.listPersistedPools(ctx); err == nil {
			log.Debug("ListPools: no hosts, returning stored records", zap.Int("count", len(persisted)))
			return persisted, nil
		}
		log.Debug("ListPools: no hosts and no records")
		return pools, nil
	}

	// 1. Get LVM pools. Adding pv_name to the vgs output makes vgs emit one row
	// per physical volume (vg_name/size/free repeated), so a single SSH round
	// yields both the capacity AND the devices backing each VG — no separate
	// pvs call. Rows for the same VG are folded into one PoolInfo, collecting
	// its devices.
	poolByKey := make(map[string]*PoolInfo)
	start := time.Now()
	result, err := sm.controller.deployment.Exec(ctx, hosts, "sudo vgs --noheadings --units b --separator '|' -o vg_name,vg_size,vg_free,pv_name")
	if err != nil {
		// Log error but continue to try ZFS
		sm.controller.logger.Warn("Failed to list LVM pools", zap.Error(err))
	} else {
		log.Debug("ListPools: vgs answered", zap.Strings("failed_hosts", result.FailedHosts()), zap.Duration("took", time.Since(start)))
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
						log.Debug("ListPools: LVM pool", zap.String("node", normalizedHost), zap.String("pool", vgName),
							zap.Uint64("total_bytes", totalSize), zap.Uint64("free_bytes", freeSize))
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
			} else {
				log.Debug("ListPools: vgs failed on host", zap.String("host", host))
			}
		}
		for _, pool := range poolByKey {
			slices.Sort(pool.Devices)
		}

		// Two lvs calls for every host, folded into the rows above by the same
		// normalized host name they were keyed under.
		if len(poolByKey) > 0 {
			log.Debug("ListPools: reading caches and thin usage", zap.Int("lvm_pools", len(poolByKey)))
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
		log.Debug("ListPools: ZFS pools", zap.Int("count", len(zfsPools)))
		pools = append(pools, zfsPools...)
	}

	if len(pools) == 0 {
		if persisted, err := sm.listPersistedPools(ctx); err == nil {
			log.Debug("ListPools: nothing found on the nodes, returning stored records", zap.Int("count", len(persisted)))
			return persisted, nil
		}
	}

	log.Debug("ListPools: listed", zap.Int("count", len(pools)), zap.Int("lvm", len(poolByKey)))
	return pools, nil
}
