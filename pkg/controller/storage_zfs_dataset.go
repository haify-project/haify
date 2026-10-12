package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// What lives inside a ZFS pool: datasets, thin zvols, snapshots and clones,
// and zvol resizing. The pools themselves are in storage_zfs.go.

// CreateZFSDataset creates a ZFS dataset
func (sm *StorageManager) CreateZFSDataset(ctx context.Context, datasetPath, node string) error {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("CreateZFSDataset: creating", zap.String("dataset", datasetPath), zap.String("node", node))
	datasetPath = normalizeManagedZFSPath(datasetPath)
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Creating ZFS dataset",
		zap.String("dataset", datasetPath),
		zap.String("node", node))

	start := time.Now()
	result, err := sm.controller.deployment.ZFSCreateDataset(ctx, []string{address}, datasetPath)
	if err != nil {
		log.Debug("CreateZFSDataset: zfs create errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to create ZFS dataset: %w", err)
	}

	if !result.AllSuccess() {
		log.Debug("CreateZFSDataset: zfs create failed", zap.String("host", address), zap.Duration("took", time.Since(start)))
		return fmt.Errorf("failed to create ZFS dataset: %s", result.FailureDetails())
	}

	log.Debug("CreateZFSDataset: created", zap.String("dataset", datasetPath), zap.Duration("took", time.Since(start)))
	return nil
}

// ZFSDeleteDataset destroys a ZFS dataset or volume
func (sm *StorageManager) ZFSDeleteDataset(ctx context.Context, datasetPath, node string) error {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("ZFSDeleteDataset: destroying", zap.String("dataset", datasetPath), zap.String("node", node))
	datasetPath = normalizeManagedZFSPath(datasetPath)
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Deleting ZFS dataset",
		zap.String("dataset", datasetPath),
		zap.String("node", node))

	start := time.Now()
	result, err := sm.controller.deployment.ZFSDestroyDataset(ctx, []string{address}, datasetPath)
	if err != nil {
		log.Debug("ZFSDeleteDataset: zfs destroy errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to delete ZFS dataset: %w", err)
	}

	if !result.AllSuccess() {
		for host, hr := range result.Hosts {
			if !hr.Success {
				log.Debug("ZFSDeleteDataset: zfs destroy failed", zap.String("host", host), zap.Duration("took", time.Since(start)))
				return fmt.Errorf("failed to delete ZFS dataset on %s: %s", host, strings.TrimSpace(hr.Output+hr.Error.Error()))
			}
		}
	}

	log.Debug("ZFSDeleteDataset: destroyed", zap.String("dataset", datasetPath), zap.Duration("took", time.Since(start)))
	return nil
}

// CreateZFSThinVolume creates a thin-provisioned ZFS volume
func (sm *StorageManager) CreateZFSThinVolume(ctx context.Context, poolName, volumeName, size, node string) error {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("CreateZFSThinVolume: creating", zap.String("pool", poolName), zap.String("volume", volumeName),
		zap.String("size", size), zap.String("node", node))
	poolName = normalizeManagedName(poolName)
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Creating ZFS thin volume",
		zap.String("pool", poolName),
		zap.String("volume", volumeName),
		zap.String("size", size),
		zap.String("node", node))

	volumePath := fmt.Sprintf("%s/%s", poolName, volumeName)
	start := time.Now()
	result, err := sm.controller.deployment.ZFSCreateThinDataset(ctx, []string{address}, poolName, volumeName, size)
	if err != nil {
		log.Debug("CreateZFSThinVolume: zfs create errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to create ZFS thin volume: %w", err)
	}

	if !result.AllSuccess() {
		log.Debug("CreateZFSThinVolume: zfs create failed", zap.String("host", address), zap.Duration("took", time.Since(start)))
		return fmt.Errorf("failed to create ZFS thin volume: %s", result.FailureDetails())
	}

	// Set quota for thin provisioning
	_, qerr := sm.controller.deployment.ZFSSetQuota(ctx, []string{address}, volumePath, size)
	log.Debug("CreateZFSThinVolume: created, quota set", zap.String("volume", volumePath), zap.NamedError("quota_error", qerr),
		zap.Duration("took", time.Since(start)))

	return nil
}

// ZFSSnapshot creates a ZFS snapshot
func (sm *StorageManager) ZFSSnapshot(ctx context.Context, dataset, snapshotName, node string) error {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("ZFSSnapshot: taking", zap.String("dataset", dataset), zap.String("snapshot", snapshotName), zap.String("node", node))
	dataset = normalizeManagedZFSPath(dataset)
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Creating ZFS snapshot",
		zap.String("dataset", dataset),
		zap.String("snapshot", snapshotName),
		zap.String("node", node))

	start := time.Now()
	result, err := sm.controller.deployment.ZFSSnapshot(ctx, []string{address}, dataset, snapshotName)
	if err != nil {
		log.Debug("ZFSSnapshot: zfs snapshot errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to create ZFS snapshot: %w", err)
	}

	if !result.AllSuccess() {
		log.Debug("ZFSSnapshot: zfs snapshot failed", zap.String("host", address), zap.Duration("took", time.Since(start)))
		return fmt.Errorf("failed to create ZFS snapshot: %s", result.FailureDetails())
	}

	log.Debug("ZFSSnapshot: taken", zap.String("dataset", dataset), zap.String("snapshot", snapshotName), zap.Duration("took", time.Since(start)))
	return nil
}

// ZFSListSnapshots lists ZFS snapshots for a dataset
func (sm *StorageManager) ZFSListSnapshots(ctx context.Context, dataset, node string) ([]*SnapshotInfo, error) {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("ZFSListSnapshots: listing", zap.String("dataset", dataset), zap.String("node", node))
	// Resolve node name to address
	dataset = normalizeManagedZFSPath(dataset)
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Listing ZFS snapshots",
		zap.String("dataset", dataset),
		zap.String("node", node),
		zap.String("address", address))

	start := time.Now()
	result, err := sm.controller.deployment.ZFSListSnapshots(ctx, []string{address}, dataset)
	if err != nil {
		log.Debug("ZFSListSnapshots: zfs list errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
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

	log.Debug("ZFSListSnapshots: listed", zap.String("dataset", dataset), zap.Int("count", len(snapshots)),
		zap.Duration("took", time.Since(start)))
	return snapshots, nil
}

// ZFSDeleteSnapshot deletes a ZFS snapshot
func (sm *StorageManager) ZFSDeleteSnapshot(ctx context.Context, snapshot, node string) error {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("ZFSDeleteSnapshot: destroying", zap.String("snapshot", snapshot), zap.String("node", node))
	snapshot = normalizeManagedZFSPath(snapshot)
	if _, name, ok := strings.Cut(snapshot, "@"); ok {
		if err := sm.controller.assertSnapshotUnlocked(ctx, name); err != nil {
			log.Debug("ZFSDeleteSnapshot: snapshot is locked", zap.String("snapshot", name), zap.Error(err))
			return err
		}
	}
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Deleting ZFS snapshot",
		zap.String("snapshot", snapshot),
		zap.String("node", node))

	sm.controller.releaseZFSLockHold(ctx, address, snapshot)
	start := time.Now()
	result, err := sm.controller.deployment.ZFSDestroySnapshot(ctx, []string{address}, snapshot)
	if err != nil {
		log.Debug("ZFSDeleteSnapshot: zfs destroy errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to delete ZFS snapshot: %w", err)
	}

	// execFailure, not hr.Error: a command that ran and failed has output but
	// may carry no error value.
	if err := execFailure(result, nil); err != nil {
		log.Debug("ZFSDeleteSnapshot: zfs destroy failed", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to delete ZFS snapshot: %w", err)
	}

	log.Debug("ZFSDeleteSnapshot: destroyed", zap.String("snapshot", snapshot), zap.Duration("took", time.Since(start)))
	return nil
}

// ZFSRestoreSnapshot restores a ZFS snapshot (rollback)
func (sm *StorageManager) ZFSRestoreSnapshot(ctx context.Context, dataset, snapshotName, node string) error {
	dataset = normalizeManagedZFSPath(dataset)
	ctrlLogOrGlobal(sm.controller).Debug("ZFSRestoreSnapshot: handing to the snapshot manager", zap.String("dataset", dataset),
		zap.String("snapshot", snapshotName), zap.String("node", node))
	sm.controller.logger.Info("Restoring ZFS snapshot",
		zap.String("dataset", dataset),
		zap.String("snapshot", snapshotName),
		zap.String("node", node))
	return sm.controller.snapshots.RestoreZFSSnapshot(ctx, dataset, snapshotName, node)
}

// ZFSCloneSnapshot creates a clone from a ZFS snapshot
func (sm *StorageManager) ZFSCloneSnapshot(ctx context.Context, snapshot, clonePath, node string) error {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("ZFSCloneSnapshot: cloning", zap.String("snapshot", snapshot), zap.String("clone", clonePath), zap.String("node", node))
	snapshot = normalizeManagedZFSPath(snapshot)
	clonePath = normalizeManagedZFSPath(clonePath)
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Cloning ZFS snapshot",
		zap.String("snapshot", snapshot),
		zap.String("clone", clonePath),
		zap.String("node", node))

	start := time.Now()
	result, err := sm.controller.deployment.ZFSClone(ctx, []string{address}, snapshot, clonePath)
	if err != nil {
		log.Debug("ZFSCloneSnapshot: zfs clone errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to clone ZFS snapshot: %w", err)
	}

	if !result.AllSuccess() {
		log.Debug("ZFSCloneSnapshot: zfs clone failed", zap.String("host", address), zap.Duration("took", time.Since(start)))
		return fmt.Errorf("failed to clone ZFS snapshot: %s", result.FailureDetails())
	}

	log.Debug("ZFSCloneSnapshot: cloned", zap.String("clone", clonePath), zap.Duration("took", time.Since(start)))
	return nil
}

// ZFSResizeVolume resizes a ZFS volume
func (sm *StorageManager) ZFSResizeVolume(ctx context.Context, volumePath, newSize, node string) error {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("ZFSResizeVolume: resizing", zap.String("volume", volumePath), zap.String("size", newSize), zap.String("node", node))
	volumePath = normalizeManagedZFSPath(volumePath)
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Resizing ZFS volume",
		zap.String("volume", volumePath),
		zap.String("size", newSize),
		zap.String("node", node))

	start := time.Now()
	result, err := sm.controller.deployment.ZFSResizeVolume(ctx, []string{address}, volumePath, newSize)
	if err != nil {
		log.Debug("ZFSResizeVolume: zfs set volsize errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to resize ZFS volume: %w", err)
	}

	if !result.AllSuccess() {
		log.Debug("ZFSResizeVolume: zfs set volsize failed", zap.String("host", address), zap.Duration("took", time.Since(start)))
		return fmt.Errorf("failed to resize ZFS volume: %s", result.FailureDetails())
	}

	log.Debug("ZFSResizeVolume: resized", zap.String("volume", volumePath), zap.String("size", newSize), zap.Duration("took", time.Since(start)))
	return nil
}
