package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// Giving disks to pools: `pool create`, `pool add`, `pool replace-disk` and
// ZFS pool create all go through prepareDisks, which refuses a disk that is
// in use and erases one that only holds old data when, and only when, the
// caller asked for it. Before this, pvcreate -y and zpool create -f wiped
// whatever device they were handed, so one mistyped letter in a device name
// was enough to destroy a disk full of somebody's data.

// nodeInventory probes the disks on one node.
func (sm *StorageManager) nodeInventory(ctx context.Context, address, node string) (*blockInventory, error) {
	res, err := sm.controller.deployment.ListBlockDevices(ctx, address)
	if err != nil {
		return nil, fmt.Errorf("list the disks on %s: %w", node, err)
	}
	for _, r := range res.Hosts {
		if !r.Success {
			return nil, fmt.Errorf("list the disks on %s: %s", node, strings.TrimSpace(r.Output))
		}
		return parseBlockInventory(r.Output), nil
	}
	return nil, fmt.Errorf("list the disks on %s: no result", node)
}

// ListNodeDisks lists the disks of node, or of every online node when node
// is empty, and says which of them a pool can use.
func (sm *StorageManager) ListNodeDisks(ctx context.Context, node string) ([]NodeDisk, error) {
	nodes, err := sm.controller.nodes.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var targets []*NodeInfo
	for _, n := range nodes {
		if node != "" && n.Name != node && n.Address != node {
			continue
		}
		if node == "" && n.State != NodeStateOnline {
			continue
		}
		targets = append(targets, n)
	}
	if len(targets) == 0 {
		if node != "" {
			return nil, fmt.Errorf("node %q is not registered", node)
		}
		return nil, nil
	}

	results := make([][]NodeDisk, len(targets))
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, n := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inv, err := sm.nodeInventory(ctx, n.Address, n.Name)
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = inv.disks(n.Name)
		}()
	}
	wg.Wait()

	var out []NodeDisk
	for i := range targets {
		if errs[i] != nil {
			// One unreachable node must not hide the disks of the others;
			// asked about that node alone, the caller gets the error.
			if node != "" {
				return nil, errs[i]
			}
			sm.controller.logger.Warn("Could not list a node's disks", zap.String("node", targets[i].Name), zap.Error(errs[i]))
			continue
		}
		out = append(out, results[i]...)
	}
	return out, nil
}

// prepareDisks checks every device before touching any of them, then erases
// the ones that hold old data — only when wipe is set; without it such a
// device is refused. The returned paths are the devices as the node names
// them, with by-id links resolved.
func (sm *StorageManager) prepareDisks(ctx context.Context, address, node string, devices []string, wipe bool) ([]string, error) {
	inv, err := sm.nodeInventory(ctx, address, node)
	if err != nil {
		return nil, err
	}
	return sm.prepareInventoried(ctx, inv, address, node, devices, wipe)
}

func (sm *StorageManager) prepareInventoried(ctx context.Context, inv *blockInventory, address, node string, devices []string, wipe bool) ([]string, error) {
	checked := make([]NodeDisk, 0, len(devices))
	seen := map[string]string{}
	for _, dev := range devices {
		d, err := inv.checkPoolDisk(node, dev, wipe)
		if err != nil {
			return nil, err
		}
		if first, dup := seen[d.Device]; dup {
			return nil, fmt.Errorf("%s and %s are the same disk on %s", first, dev, node)
		}
		seen[d.Device] = dev
		checked = append(checked, d)
	}
	paths := make([]string, 0, len(checked))
	for _, d := range checked {
		if d.State == DiskWipe {
			sm.controller.logger.Warn("Erasing a disk before it joins a pool",
				zap.String("node", node), zap.String("device", d.Device), zap.String("held", d.Detail))
			if err := execFailure(sm.controller.deployment.WipeDevice(ctx, address, d.Device, d.partitions)); err != nil {
				return nil, fmt.Errorf("erase %s on %s: %w", d.Device, node, err)
			}
		}
		paths = append(paths, d.Device)
	}
	return paths, nil
}

// zfsVdevDevices is the devices among zpool create's vdev arguments; the
// rest are layout keywords (mirror, raidz, log, cache, spare).
func zfsVdevDevices(vdevs []string) []string {
	var devs []string
	for _, v := range vdevs {
		if strings.HasPrefix(v, "/") {
			devs = append(devs, v)
		}
	}
	return devs
}

// AddDiskToPool gives disk on node to pool. A disk holding old data is
// refused unless wipe is set.
func (sm *StorageManager) AddDiskToPool(ctx context.Context, pool, disk, node string, wipe bool) error {
	pool = normalizeManagedName(pool)
	address := sm.controller.ResolveHost(node)
	if address == "" {
		return fmt.Errorf("node not found: %s", node)
	}
	inv, err := sm.nodeInventory(ctx, address, node)
	if err != nil {
		return err
	}
	// The disk already joined this pool on an earlier attempt whose thin pool
	// growth failed: finish that rather than refuse, so retrying the same
	// command is how an operator recovers.
	if r := inv.find(disk); r != nil {
		if d := inv.classify(node, r); d.State == DiskPool && d.Pool == pool && !sm.vdoPoolNames(ctx)[pool] {
			if err := sm.growThinPoolAfterExtend(ctx, address, pool); err != nil {
				return fmt.Errorf("%s is already in %s on %s, but its thin pool was not grown into it: %w",
					disk, pool, node, err)
			}
			return nil
		}
	}
	paths, err := sm.prepareInventoried(ctx, inv, address, node, []string{disk}, wipe)
	if err != nil {
		return err
	}
	disk = paths[0]

	if persisted, perr := sm.getPersistedPool(ctx, pool); perr == nil && isZFSPoolType(persisted.Type) {
		if err := execFailure(sm.controller.deployment.ZFSAddVdev(ctx, address, pool, disk)); err != nil {
			return fmt.Errorf("add %s to ZFS pool %s on %s: %w", disk, pool, node, err)
		}
		sm.logDiskAdded(pool, disk, node)
		sm.recordPoolDevice(ctx, pool, disk)
		return nil
	}

	if err := execFailure(sm.controller.deployment.PVCreate(ctx, []string{address}, disk)); err != nil {
		return fmt.Errorf("failed to create PV: %w", err)
	}
	if err := execFailure(sm.controller.deployment.VGExtend(ctx, []string{address}, pool, disk)); err != nil {
		return fmt.Errorf("failed to add disk: %w", err)
	}
	sm.logDiskAdded(pool, disk, node)

	// The disk is part of the group now whatever happens to the thin pool, so
	// record it before trying to grow the pool.
	sm.recordPoolDevice(ctx, pool, disk)

	if sm.vdoPoolNames(ctx)[pool] {
		return errVDONotGrown(pool, disk, node)
	}
	if err := sm.growThinPoolAfterExtend(ctx, address, pool); err != nil {
		return fmt.Errorf("disk %s added to %s on %s, but its thin pool was not grown into it: %w",
			disk, pool, node, err)
	}
	return nil
}

func (sm *StorageManager) logDiskAdded(pool, disk, node string) {
	sm.controller.logger.Info("Disk added to pool",
		zap.String("pool", pool),
		zap.String("disk", disk),
		zap.String("node", node))
}

// recordPoolDevice appends disk to the pool's persisted device list.
func (sm *StorageManager) recordPoolDevice(ctx context.Context, pool, disk string) {
	if sm.controller.db != nil {
		if dbPool, err := sm.controller.db.GetPool(ctx, pool); err == nil {
			devices := strings.Split(strings.Trim(dbPool.Devices, ","), ",")
			devices = append(devices, disk)
			dbPool.Devices = strings.Join(normalizeDeviceList(devices), ",")
			if err := sm.controller.db.SavePool(ctx, dbPool); err != nil {
				sm.controller.logger.Warn("Failed to update pool devices in database",
					zap.String("pool", pool),
					zap.Error(err))
			}
		}
	}
}
