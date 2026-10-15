package controller

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
)

// Backups of a snapshot the caller already took.
//
// A backup normally takes its own snapshot. That is wrong for a caller whose
// snapshot IS the point in time — the CSI driver hands Kubernetes a
// VolumeSnapshot the moment its local snapshot exists, perhaps between an
// application freeze and thaw, and the off-cluster copy has to hold that
// instant, not one taken seconds later after the thaw. So the image is read
// from the caller's snapshot instead.
//
// Such a backup is always full and keeps no base for an incremental. An
// incremental chain ties its links together — a backup another is built on
// cannot be deleted — and the caller deletes these backups one by one, in any
// order, when their snapshots go away. A full image per snapshot keeps each
// one deletable on its own. The snapshot is the caller's to delete, never the
// backup's.

// snapshotNamePattern is what an LVM logical volume name may contain. The name
// is put into a device path and a shell command, so anything else is refused.
var snapshotNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+-]*$`)

// CreateBackupFromSnapshot ships the existing snapshot `snapshot` of
// resource's volume, held on node, to target. The backup's id is
// "<resource>_<snapshot>", so the caller knows it before the upload finishes.
func (bm *BackupManager) CreateBackupFromSnapshot(ctx context.Context, resource, targetName, node, snapshot string) (*database.Backup, error) {
	bm.controller.traceLogger().Debug("BackupManager.CreateBackupFromSnapshot: start", zap.String("resource", resource),
		zap.String("target", targetName), zap.String("node", node), zap.String("snapshot", snapshot))
	return bm.createBackup(ctx, resource, targetName, node, true, "", snapshot)
}

// checkFromSnapshot refuses a snapshot source that cannot be read as asked: no
// node to find it on, a name that is not a volume name, or a resource with
// more than one volume, of which the snapshot can only be one.
func checkFromSnapshot(info *ResourceInfo, node, snapshot string) error {
	log := zap.L()
	log.Debug("checkFromSnapshot: checking", zap.String("resource", info.Name), zap.String("node", node),
		zap.String("snapshot", snapshot), zap.Int("volumes", len(info.Volumes)))
	if node == "" {
		log.Debug("checkFromSnapshot: no node")
		return fmt.Errorf("a backup of snapshot %q needs the node that holds it", snapshot)
	}
	if !snapshotNamePattern.MatchString(snapshot) {
		log.Debug("checkFromSnapshot: invalid snapshot name", zap.String("snapshot", snapshot))
		return fmt.Errorf("invalid snapshot name %q", snapshot)
	}
	if len(info.Volumes) != 1 {
		log.Debug("checkFromSnapshot: not a single-volume resource", zap.Int("volumes", len(info.Volumes)))
		return fmt.Errorf("resource %q has %d volumes; a backup of one snapshot covers single-volume resources only",
			info.Name, len(info.Volumes))
	}
	return nil
}

// backupID names a backup: by the snapshot it reads when it reads the
// caller's, by its start time otherwise.
func backupID(resource string, started time.Time, from string) string {
	if from != "" {
		return resource + "_" + from
	}
	return resource + "_" + started.Format(backupIDLayout)
}

// assertBackupIDFree refuses an id that already names a backup. A failed one
// does not count: it is no backup, its objects are already removed, and its
// record is overwritten by the new attempt. That is what lets a caller retry a
// backup of the same snapshot under the same id.
func (bm *BackupManager) assertBackupIDFree(ctx context.Context, resource, id, from string) error {
	log := bm.controller.traceLogger()
	start := time.Now()
	existing, err := bm.controller.db.GetBackup(ctx, id)
	log.Debug("BackupManager.assertBackupIDFree: db get backup", zap.String("backup", id), zap.Bool("found", err == nil && existing != nil),
		zap.Duration("took", time.Since(start)))
	if err != nil || existing == nil {
		return nil
	}
	if existing.State == database.BackupStateFailed {
		log.Debug("BackupManager.assertBackupIDFree: replacing a failed backup", zap.String("backup", id))
		return nil
	}
	if from != "" {
		return fmt.Errorf("a backup of snapshot %q of %q already exists (%s)", from, resource, existing.State)
	}
	return fmt.Errorf("a backup of %q with id %q already exists; retry in a second", resource, id)
}

// useSnapshot makes the caller's snapshot readable on host and returns it as
// the snapshot of the resource's one volume. A thin snapshot is created with
// activation skipped and has no device to read until it is activated; a
// snapshot that does not exist fails here, before anything is recorded.
func (bm *BackupManager) useSnapshot(ctx context.Context, host string, info *ResourceInfo, snapshot string) (map[uint32]string, error) {
	log := bm.controller.traceLogger()
	v := info.Volumes[0]
	device := fmt.Sprintf("/dev/%s/%s", v.Pool, snapshot)
	log.Debug("BackupManager.useSnapshot: start", zap.String("resource", info.Name), zap.String("host", host),
		zap.String("device", device))
	if cmd := activateSnapshotCmd(device); cmd != "" {
		start := time.Now()
		err := bm.controller.resources.execAllSuccess(ctx, []string{host}, cmd,
			fmt.Sprintf("find snapshot %s/%s of %q", v.Pool, snapshot, info.Name))
		log.Debug("BackupManager.useSnapshot: activated", zap.String("host", host), zap.String("device", device),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return nil, err
		}
	}
	return map[uint32]string{v.VolumeID: snapshot}, nil
}
