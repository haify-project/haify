package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/deployment"
)

// The storage side of a backup: the snapshots each image is read from, and
// the backing devices they are taken of.

// snapshotVolumes takes one storage-native snapshot per volume and returns the
// snapshot name per volume id. A partial result is returned alongside the error
// so the caller's deferred cleanup removes what did get created.
func (bm *BackupManager) snapshotVolumes(ctx context.Context, host string, info *ResourceInfo, ts time.Time) (map[uint32]string, error) {
	log := bm.controller.traceLogger()
	log.Debug("BackupManager.snapshotVolumes: start", zap.String("resource", info.Name), zap.String("host", host),
		zap.Int("volumes", len(info.Volumes)))
	snaps := make(map[uint32]string, len(info.Volumes))
	dep := bm.controller.deployment
	for _, v := range info.Volumes {
		name := v.BackingVolume + backupSnapMarker + ts.Format(backupIDLayout)
		start := time.Now()
		thin, err := dep.LVIsThin(ctx, host, v.Pool, v.BackingVolume)
		if err != nil {
			bm.controller.logger.Warn("Backup: LVIsThin failed; assuming thick",
				zap.String("volume", v.BackingVolume), zap.Error(err))
		}
		log.Debug("BackupManager.snapshotVolumes: snapshotting volume", zap.Uint32("volume", v.VolumeID),
			zap.String("pool", v.Pool), zap.String("backing_volume", v.BackingVolume), zap.String("snapshot", name),
			zap.Bool("thin", thin))
		var res *deployment.ExecResult
		var execErr error
		if thin {
			res, execErr = dep.LVCreateThinSnapshot(ctx, []string{host}, v.Pool, v.BackingVolume, name)
		} else {
			// Thick LVM snapshots need an explicit copy-on-write reservation;
			// reuse the scheduler's sizing so both paths behave the same.
			res, execErr = dep.LVCreateSnapshot(ctx, []string{host}, v.Pool, v.BackingVolume, name, cowSize(v.SizeGB))
		}
		if execErr != nil {
			log.Debug("BackupManager.snapshotVolumes: snapshot command failed", zap.String("host", host),
				zap.String("snapshot", name), zap.Duration("took", time.Since(start)), zap.Error(execErr))
			return snaps, fmt.Errorf("snapshot volume %d of %q: %w", v.VolumeID, info.Name, execErr)
		}
		if res == nil || !res.AllSuccess() {
			details := ""
			if res != nil {
				details = ": " + res.FailureDetails()
			}
			log.Debug("BackupManager.snapshotVolumes: snapshot failed on host", zap.String("host", host),
				zap.String("snapshot", name), zap.Duration("took", time.Since(start)))
			return snaps, fmt.Errorf("snapshot volume %d of %q failed on %s%s", v.VolumeID, info.Name, host, details)
		}
		snaps[v.VolumeID] = name
		log.Debug("BackupManager.snapshotVolumes: snapshot taken", zap.String("host", host),
			zap.String("snapshot", name), zap.Duration("took", time.Since(start)))
		// A thin snapshot is created with activation skipped and has no device
		// node to read until it is activated; see activateSnapshotCmd.
		if cmd := activateSnapshotCmd(fmt.Sprintf("/dev/%s/%s", v.Pool, name)); cmd != "" {
			log.Debug("BackupManager.snapshotVolumes: activating snapshot", zap.String("host", host), zap.String("snapshot", name))
			if err := bm.controller.resources.execAllSuccess(ctx, []string{host}, cmd,
				fmt.Sprintf("activate the backup snapshot %s/%s", v.Pool, name)); err != nil {
				log.Debug("BackupManager.snapshotVolumes: activation failed", zap.String("snapshot", name), zap.Error(err))
				return snaps, err
			}
		}
	}
	log.Debug("BackupManager.snapshotVolumes: done", zap.String("resource", info.Name), zap.Int("count", len(snaps)))
	return snaps, nil
}

// removeSnapshots drops the temporary backup snapshots. Failures are logged
// rather than returned: the backup's own outcome is already decided, and a
// leftover snapshot is a capacity problem, not a correctness one.
func (bm *BackupManager) removeSnapshots(ctx context.Context, host string, info *ResourceInfo, snaps map[uint32]string) {
	log := bm.controller.traceLogger()
	log.Debug("BackupManager.removeSnapshots: start", zap.String("resource", info.Name), zap.String("host", host),
		zap.Int("count", len(snaps)))
	for _, v := range info.Volumes {
		name, ok := snaps[v.VolumeID]
		if !ok {
			continue
		}
		start := time.Now()
		if _, err := bm.controller.deployment.LVRemoveSnapshot(ctx, []string{host}, v.Pool, name); err != nil {
			bm.controller.logger.Warn("Failed to remove backup snapshot",
				zap.String("node", host), zap.String("snapshot", name), zap.Error(err))
			continue
		}
		log.Debug("BackupManager.removeSnapshots: snapshot removed", zap.String("host", host),
			zap.String("snapshot", name), zap.Duration("took", time.Since(start)))
	}
}

// backingDevices returns each volume's stored backing device path, keyed by
// volume id. A volume with no record maps to the empty string, which reads as
// "not ZFS" — the LVM path then fails loudly on a missing pool/backing name
// rather than quietly treating an unknown volume as something it is not.
func (bm *BackupManager) backingDevices(ctx context.Context, resource string) (map[uint32]string, error) {
	log := bm.controller.traceLogger()
	start := time.Now()
	vols, err := bm.controller.db.ListVolumes(ctx, resource)
	log.Debug("BackupManager.backingDevices: db list volumes", zap.String("resource", resource),
		zap.Int("count", len(vols)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("list volumes of %q: %w", resource, err)
	}
	out := make(map[uint32]string, len(vols))
	for _, v := range vols {
		log.Debug("BackupManager.backingDevices: volume device", zap.String("resource", resource),
			zap.Int("volume", v.VolumeID), zap.String("device", v.Device))
		out[uint32(v.VolumeID)] = v.Device
	}
	return out, nil
}
