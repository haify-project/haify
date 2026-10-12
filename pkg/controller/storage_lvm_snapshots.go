package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/deployment"
	"go.uber.org/zap"
)

// CreateLvmSnapshot creates an LVM snapshot
func (sm *StorageManager) CreateLvmSnapshot(ctx context.Context, vgName, lvName, snapshotName, node, size string) error {
	log := sm.controller.traceLogger()
	log.Debug("CreateLvmSnapshot: taking", zap.String("pool", vgName), zap.String("lv", lvName),
		zap.String("snapshot", snapshotName), zap.String("node", node), zap.String("size", size))
	vgName = normalizeManagedName(vgName)

	sm.controller.logger.Info("Creating LVM snapshot",
		zap.String("vg_name", vgName),
		zap.String("lv_name", lvName),
		zap.String("snapshot", snapshotName),
		zap.String("node", node),
		zap.String("size", size))

	// Resolve node address
	address := sm.controller.ResolveHost(node)

	// Check if LV is thin
	start := time.Now()
	isThin, err := sm.controller.deployment.LVIsThin(ctx, address, vgName, lvName)
	if err != nil {
		sm.controller.logger.Warn("Failed to check if LV is thin", zap.Error(err))
		// Fallback to standard snapshot if check fails (safest default?) or error?
		// Default to standard
	}
	log.Debug("CreateLvmSnapshot: origin checked", zap.String("host", address), zap.Bool("thin", isThin),
		zap.Duration("took", time.Since(start)))

	var result *deployment.ExecResult
	start = time.Now()
	if isThin {
		sm.controller.logger.Info("Creating Thin Snapshot", zap.String("origin", lvName))
		result, err = sm.controller.deployment.LVCreateThinSnapshot(ctx, []string{address}, vgName, lvName, snapshotName)
	} else {
		sm.controller.logger.Info("Creating Standard Snapshot", zap.String("origin", lvName))
		result, err = sm.controller.deployment.LVCreateSnapshot(ctx, []string{address}, vgName, lvName, snapshotName, size)
	}

	if err != nil {
		log.Debug("CreateLvmSnapshot: lvcreate errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to create LVM snapshot: %w", err)
	}

	if !result.AllSuccess() {
		log.Debug("CreateLvmSnapshot: lvcreate failed", zap.String("host", address), zap.Duration("took", time.Since(start)))
		return fmt.Errorf("failed to create LVM snapshot: %s", result.FailureDetails())
	}

	log.Debug("CreateLvmSnapshot: taken", zap.String("snapshot", snapshotName), zap.Duration("took", time.Since(start)))
	return nil
}

// ListLvmSnapshots lists LVM snapshots in a volume group.
//
// resource narrows the result to that DRBD resource's volumes; empty lists the
// whole group.
func (sm *StorageManager) ListLvmSnapshots(ctx context.Context, vgName, node, resource string) ([]*SnapshotInfo, error) {
	log := sm.controller.traceLogger()
	log.Debug("ListLvmSnapshots: listing", zap.String("pool", vgName), zap.String("node", node), zap.String("resource", resource))
	vgName = normalizeManagedName(vgName)

	// Resolve node address
	address := sm.controller.ResolveHost(node)

	start := time.Now()
	result, err := sm.controller.deployment.LVListSnapshots(ctx, []string{address}, vgName)
	if err != nil {
		log.Debug("ListLvmSnapshots: lvs errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, fmt.Errorf("failed to list LVM snapshots: %w", err)
	}
	if !result.AllSuccess() {
		// Reporting an empty list on a failed command is how this went unnoticed
		// for so long: a pool holding 27 snapshots read as a pool holding none,
		// and nothing said why.
		log.Debug("ListLvmSnapshots: lvs failed", zap.String("host", address), zap.Duration("took", time.Since(start)))
		return nil, fmt.Errorf("failed to list LVM snapshots in %s: %s", vgName, result.FailureDetails())
	}

	// An empty resource lists the whole group. Otherwise only snapshots whose
	// origin is one of the resource's backing volumes are returned — the
	// `--resource` flag used to be accepted, printed in the heading, and then
	// ignored, so `snapshot list --resource a` and `--resource b` returned
	// identical lists of everything in the pool.
	var wanted map[string]bool
	if resource != "" {
		wanted, err = sm.backingVolumesOf(ctx, resource)
		if err != nil {
			log.Debug("ListLvmSnapshots: resource volumes not resolved", zap.String("resource", resource), zap.Error(err))
			return nil, err
		}
	}

	var snapshots []*SnapshotInfo
	skipped := 0
	for _, r := range result.Hosts {
		for _, line := range strings.Split(strings.TrimSpace(r.Output), "\n") {
			snap, ok := parseSnapshotLine(line, vgName)
			if !ok {
				continue
			}
			if wanted != nil && !wanted[snap.Origin] {
				skipped++
				continue
			}
			snapshots = append(snapshots, snap)
		}
	}

	log.Debug("ListLvmSnapshots: listed", zap.String("pool", vgName), zap.Int("count", len(snapshots)),
		zap.Int("other_resources", skipped), zap.Duration("took", time.Since(start)))
	return snapshots, nil
}

// parseSnapshotLine reads one row of LVListSnapshots output:
// "name|size_bytes|time|origin".
func parseSnapshotLine(line, vgName string) (*SnapshotInfo, bool) {
	fields := strings.Split(strings.TrimSpace(line), "|")
	if len(fields) < 4 {
		return nil, false
	}
	name := strings.TrimSpace(fields[0])
	if name == "" {
		return nil, false
	}
	return &SnapshotInfo{
		Name:      name,
		Volume:    vgName,
		SizeGB:    bytesToGB(parseThinUint(fields[1])),
		CreatedAt: strings.TrimSpace(fields[2]),
		Origin:    strings.TrimSpace(fields[3]),
	}, true
}

// backingVolumesOf resolves a resource to the set of LV names its snapshots can
// have as an origin.
//
// Read from the database rather than rebuilt from the "<name>_data" /
// "<name>_vol<K>" convention: the convention lives in resource creation, and a
// second copy of it here would keep working right up until the day the first
// one changed.
func (sm *StorageManager) backingVolumesOf(ctx context.Context, resource string) (map[string]bool, error) {
	log := sm.controller.traceLogger()
	if sm.controller.db == nil {
		log.Debug("backingVolumesOf: no database", zap.String("resource", resource))
		return nil, fmt.Errorf("database not available, cannot resolve volumes of resource %s", resource)
	}
	volumes, err := sm.controller.db.ListVolumes(ctx, resource)
	if err != nil {
		log.Debug("backingVolumesOf: volume list failed", zap.String("resource", resource), zap.Error(err))
		return nil, fmt.Errorf("list volumes of resource %s: %w", resource, err)
	}
	if len(volumes) == 0 {
		log.Debug("backingVolumesOf: no volumes", zap.String("resource", resource))
		return nil, fmt.Errorf("resource %s has no volumes, or does not exist", resource)
	}
	wanted := make(map[string]bool, len(volumes))
	for _, v := range volumes {
		if v != nil && v.VolumeName != "" {
			wanted[v.VolumeName] = true
		}
	}
	log.Debug("backingVolumesOf: resolved", zap.String("resource", resource), zap.Int("volumes", len(wanted)))
	return wanted, nil
}

// DeleteLvmSnapshot deletes an LVM snapshot
func (sm *StorageManager) DeleteLvmSnapshot(ctx context.Context, vgName, snapshotName, node string) error {
	log := sm.controller.traceLogger()
	log.Debug("DeleteLvmSnapshot: deleting", zap.String("pool", vgName), zap.String("snapshot", snapshotName), zap.String("node", node))
	vgName = normalizeManagedName(vgName)
	if err := sm.controller.assertSnapshotUnlocked(ctx, snapshotName); err != nil {
		log.Debug("DeleteLvmSnapshot: snapshot is locked", zap.String("snapshot", snapshotName), zap.Error(err))
		return err
	}

	sm.controller.logger.Info("Deleting LVM snapshot",
		zap.String("vg_name", vgName),
		zap.String("snapshot", snapshotName),
		zap.String("node", node))

	// Resolve node address
	address := sm.controller.ResolveHost(node)

	start := time.Now()
	result, err := sm.controller.deployment.LVRemoveSnapshot(ctx, []string{address}, vgName, snapshotName)
	if err != nil {
		log.Debug("DeleteLvmSnapshot: lvremove errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to delete LVM snapshot: %w", err)
	}

	if !result.AllSuccess() {
		log.Debug("DeleteLvmSnapshot: lvremove failed", zap.String("host", address), zap.Duration("took", time.Since(start)))
		return fmt.Errorf("failed to delete LVM snapshot: %s", result.FailureDetails())
	}

	log.Debug("DeleteLvmSnapshot: deleted", zap.String("snapshot", snapshotName), zap.Duration("took", time.Since(start)))
	return nil
}

// RestoreLvmSnapshot restores an LVM snapshot (merges it back to the origin).
// A snapshot of a DRBD backing volume is restored the way that keeps the
// resource's replicas in step; see snapshot_restore.go.
func (sm *StorageManager) RestoreLvmSnapshot(ctx context.Context, vgName, snapshotName, node string) error {
	vgName = normalizeManagedName(vgName)
	sm.controller.traceLogger().Debug("RestoreLvmSnapshot: handing to the snapshot manager", zap.String("pool", vgName),
		zap.String("snapshot", snapshotName), zap.String("node", node))
	sm.controller.logger.Info("Restoring LVM snapshot",
		zap.String("vg_name", vgName),
		zap.String("snapshot", snapshotName),
		zap.String("node", node))
	return sm.controller.snapshots.RestoreLVMSnapshotByName(ctx, vgName, snapshotName, node)
}
