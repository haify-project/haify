package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/backup"
	"github.com/haify-project/haify/pkg/database"
)

// Off-cluster backup shipping. See pkg/backup for the backend trade-off; this
// file is the orchestration: snapshot, read the snapshot, push, verify, record.
//
// Two properties drive every decision below.
//
// Crash consistency. A backup is taken from a storage-native snapshot, never
// from the live volume. Reading a mounted, actively-written volume produces an
// image whose blocks come from different instants, which is not a filesystem —
// it is something that usually mounts and occasionally does not. The snapshot
// path reused here is the same one `resource snapshot` and the GFS scheduler
// use, so there is one implementation of "freeze this volume", not two.
//
// Size. With `meta-disk internal` the DRBD device is SMALLER than the volume
// backing it: the tail of that volume holds DRBD's own metadata. A snapshot of
// the backing volume therefore contains [filesystem][DRBD metadata], and
// archiving all of it would ship one resource's DRBD metadata into another
// resource's data area on restore. Every image is bounded by the DRBD device's
// size instead, which is exactly the filesystem region. PopulateVolume carries
// the same warning for the same reason.

const (
	// backupSnapMarker separates a backing volume name from the UTC timestamp
	// in a backup snapshot's name. It is deliberately different from
	// schedSnapMarker: GFS retention must never prune a snapshot a backup is
	// mid-flight on, and a backup must never prune a scheduled one.
	backupSnapMarker = "_bk_"

	// backupIDLayout is the UTC timestamp in a backup id and snapshot name. It
	// uses only characters valid in an LVM LV name and in an object key.
	backupIDLayout = "20060102T150405Z"

	// manifestObject is the self-describing index written alongside the images.
	// It exists for the disaster the backups are for: when the Haify database is
	// gone too, `haify backup import` rebuilds the backup records, chains
	// included, from these files alone (backup_import.go).
	manifestObject = "manifest.json"

	// manifestVersion is bumped when the on-target layout changes.
	manifestVersion = 2
)

// BackupManifest is the JSON index stored next to a backup's volume images.
type BackupManifest struct {
	Version    int                      `json:"version"`
	ID         string                   `json:"id"`
	Resource   string                   `json:"resource"`
	Node       string                   `json:"node"`
	Backend    string                   `json:"backend"`
	Kind       string                   `json:"kind"`
	Parent     string                   `json:"parent,omitempty"`
	Schedule   string                   `json:"schedule,omitempty"`
	CreatedAt  string                   `json:"created_at"`
	FinishedAt string                   `json:"finished_at,omitempty"`
	TotalBytes uint64                   `json:"total_bytes"`
	Volumes    []BackupManifestVolume   `json:"volumes"`
	Note       string                   `json:"note"`
	Snapshots  []BackupManifestSnapshot `json:"snapshots"`
	// LockMode and RetainUntil (RFC3339) are the Object Lock the objects
	// carry, so an import knows a backup cannot be deleted before then.
	LockMode    string `json:"lock_mode,omitempty"`
	RetainUntil string `json:"retain_until,omitempty"`
}

// BackupManifestVolume describes one raw image in the manifest.
type BackupManifestVolume struct {
	VolumeID uint32 `json:"volume_id"`
	Object   string `json:"object"`
	Bytes    uint64 `json:"bytes"`
	Pool     string `json:"pool"`
	Backing  string `json:"backing_volume"`
	// Ranges lists, one "offset length" pair per line, where each run of
	// bytes in an incremental Object belongs. Empty for a full image.
	Ranges string `json:"ranges,omitempty"`
	// ChangedBytes is how much of the volume an incremental image carries.
	ChangedBytes uint64 `json:"changed_bytes,omitempty"`
	// ReadThroughLUKS marks an encrypted volume's image as plaintext, read
	// through its LUKS container (backup_luks.go).
	ReadThroughLUKS bool `json:"read_through_luks,omitempty"`
}

// BackupManifestSnapshot records the snapshot each image was read from.
type BackupManifestSnapshot struct {
	VolumeID uint32 `json:"volume_id"`
	Name     string `json:"name"`
}

// BackupManager ships point-in-time copies of resources to off-cluster targets
// and restores them.
type BackupManager struct {
	controller *Controller
	backend    backup.Backend
}

// NewBackupManager creates a backup manager over the rclone backend.
func NewBackupManager(c *Controller) *BackupManager {
	return &BackupManager{controller: c, backend: backup.NewRclone()}
}

// ==================== BACKUP ====================

// ListBackups returns backup records, newest first.
func (bm *BackupManager) ListBackups(ctx context.Context, resource, target string) ([]*database.Backup, error) {
	log := bm.controller.traceLogger()
	if bm.controller.db == nil {
		log.Debug("BackupManager.ListBackups: no database")
		return nil, fmt.Errorf("database not available")
	}
	start := time.Now()
	out, err := bm.controller.db.ListBackups(ctx, resource, target)
	log.Debug("BackupManager.ListBackups: db list", zap.String("resource", resource), zap.String("target", target),
		zap.Int("count", len(out)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return out, err
}

// GetBackup returns one backup record.
func (bm *BackupManager) GetBackup(ctx context.Context, id string) (*database.Backup, error) {
	log := bm.controller.traceLogger()
	if bm.controller.db == nil {
		log.Debug("BackupManager.GetBackup: no database")
		return nil, fmt.Errorf("database not available")
	}
	start := time.Now()
	b, err := bm.controller.db.GetBackup(ctx, id)
	log.Debug("BackupManager.GetBackup: db get", zap.String("backup", id), zap.Bool("found", b != nil),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return b, err
}

// ReconcileInterrupted marks backups left in the "running" state as failed.
//
// Only the active controller runs backups, so on startup nothing can still be
// in flight: a record in that state belongs to a run whose controller died. It
// is turned into an explicit failure rather than left ambiguous, because the
// one thing a backup system must never do is let an incomplete copy look
// available.
func (bm *BackupManager) ReconcileInterrupted(ctx context.Context) error {
	log := bm.controller.traceLogger()
	if bm.controller.db == nil {
		log.Debug("BackupManager.ReconcileInterrupted: no database, nothing to reconcile")
		return nil
	}
	start := time.Now()
	backups, err := bm.controller.db.ListBackups(ctx, "", "")
	log.Debug("BackupManager.ReconcileInterrupted: listed backups", zap.Int("count", len(backups)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return err
	}
	for _, b := range backups {
		if b.State != database.BackupStateRunning {
			continue
		}
		log.Debug("BackupManager.ReconcileInterrupted: marking running backup failed", zap.String("backup", b.ID),
			zap.String("resource", b.Resource))
		b.State = database.BackupStateFailed
		b.Error = "interrupted: the controller restarted while this backup was running"
		b.FinishedAt = time.Now()
		if err := bm.controller.db.SaveBackup(ctx, b); err != nil {
			log.Debug("BackupManager.ReconcileInterrupted: db save failed", zap.String("backup", b.ID), zap.Error(err))
			return err
		}
		bm.controller.logger.Warn("Marked interrupted backup as failed", zap.String("backup", b.ID))
	}
	return nil
}

// CreateBackup ships a crash-consistent copy of every volume of resource to
// target, and returns the recorded backup. It is incremental when the last
// backup to the same target left a base snapshot on the node it reads (see
// backup_incremental.go), and full otherwise or when full is set.
//
// The record only reaches "completed" once every image has been uploaded AND
// the target has confirmed it holds exactly as many bytes as were sent. A run
// that fails anywhere in between is recorded as failed and its objects are
// removed; it is never listed as something that can be restored.
//
// node selects which replica to read. Empty picks one automatically, preferring
// a Secondary so the workload's node is left alone.
func (bm *BackupManager) CreateBackup(ctx context.Context, resource, targetName, node string, full bool) (*database.Backup, error) {
	return bm.createBackup(ctx, resource, targetName, node, full, "", "")
}

// createBackup is CreateBackup on behalf of schedule, which is recorded on the
// backup so that schedule's retention can tell its own backups from others.
// from, when set, is an existing snapshot to read instead of taking one
// (CreateBackupFromSnapshot).
func (bm *BackupManager) createBackup(ctx context.Context, resource, targetName, node string, full bool, schedule, from string) (*database.Backup, error) {
	log := bm.controller.traceLogger()
	log.Debug("BackupManager.createBackup: start", zap.String("resource", resource), zap.String("target", targetName),
		zap.String("node", node), zap.Bool("full", full), zap.String("schedule", schedule), zap.String("from_snapshot", from))
	if bm.controller.db == nil {
		log.Debug("BackupManager.createBackup: no database")
		return nil, fmt.Errorf("database not available")
	}
	start := time.Now()
	dbTarget, err := bm.controller.db.GetBackupTarget(ctx, targetName)
	log.Debug("BackupManager.createBackup: db get target", zap.String("target", targetName),
		zap.Bool("found", dbTarget != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	spec := targetSpecFromDB(dbTarget)

	info, err := bm.controller.resources.GetResource(ctx, resource)
	if err != nil {
		log.Debug("BackupManager.createBackup: reading resource failed", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	if len(info.Volumes) == 0 {
		log.Debug("BackupManager.createBackup: resource has no volumes", zap.String("resource", resource))
		return nil, fmt.Errorf("resource %q has no volumes to back up", resource)
	}
	// The backing device path comes from the database, not from ResourceInfo:
	// the live-status parse reports a volume's DRBD device (/dev/drbdN), which
	// says nothing about what is underneath it. The stored path is the backing
	// LV or zvol, which is what actually has to be snapshotted and read.
	backing, err := bm.backingDevices(ctx, resource)
	if err != nil {
		log.Debug("BackupManager.createBackup: reading backing devices failed", zap.Error(err))
		return nil, err
	}
	// An encrypted volume's snapshot is LUKS ciphertext; it is read through a
	// crypt mapping of its own so the image holds what DRBD holds.
	encrypted := bm.encryptedVolumes(ctx, resource, backing)
	log.Debug("BackupManager.createBackup: volumes", zap.String("resource", resource),
		zap.Int("volumes", len(info.Volumes)), zap.Int("encrypted", len(encrypted)))
	for _, v := range info.Volumes {
		log.Debug("BackupManager.createBackup: checking volume", zap.Uint32("volume", v.VolumeID),
			zap.String("pool", v.Pool), zap.String("backing_volume", v.BackingVolume), zap.String("device", backing[v.VolumeID]))
		if isZFSDevice(backing[v.VolumeID]) {
			log.Debug("BackupManager.createBackup: ZFS-backed volume refused", zap.Uint32("volume", v.VolumeID))
			// A zvol snapshot has no block device unless snapdev=visible is set
			// or the snapshot is cloned first, so the read path here simply does
			// not apply. Refusing is the honest answer; a ZFS-native `zfs send`
			// pipeline is a separate change.
			return nil, fmt.Errorf(
				"resource %q volume %d is ZFS-backed; backup shipping currently supports LVM-backed volumes only",
				resource, v.VolumeID)
		}
		if v.Pool == "" || v.BackingVolume == "" {
			log.Debug("BackupManager.createBackup: volume has no backing volume", zap.Uint32("volume", v.VolumeID))
			return nil, fmt.Errorf("resource %q volume %d has no known backing volume to snapshot", resource, v.VolumeID)
		}
	}
	if from != "" {
		if err := checkFromSnapshot(info, node, from); err != nil {
			log.Debug("BackupManager.createBackup: snapshot source refused", zap.String("snapshot", from), zap.Error(err))
			return nil, err
		}
		// Every backup of a snapshot is self-contained: see backup_from_snapshot.go.
		full = true
	}

	// The node holding the last backup's base snapshot is the only one an
	// incremental can be read on, so it wins when it is healthy.
	if node == "" && !full {
		if last := bm.latestCompleted(ctx, resource, targetName); last != nil && bm.readable(info, last.Node) {
			log.Debug("BackupManager.createBackup: reading on the last backup's node", zap.String("node", last.Node),
				zap.String("last_backup", last.ID))
			node = last.Node
		}
	}
	if node == "" {
		node = bm.nodeWithSnapshotRoom(ctx, info)
		log.Debug("BackupManager.createBackup: node with snapshot room", zap.String("node", node))
	}
	node, err = bm.pickBackupNode(info, node)
	if err != nil {
		log.Debug("BackupManager.createBackup: no node to read on", zap.Error(err))
		return nil, err
	}
	host := bm.controller.ResolveHost(node)
	dep := newBackupDeploymentClient(bm.controller.deployment)
	log.Debug("BackupManager.createBackup: chose node", zap.String("node", node), zap.String("host", host))

	// Everything that can be checked without side effects happens before the
	// first snapshot, so a node missing rclone costs a round trip rather than a
	// snapshot that then has to be cleaned up.
	start = time.Now()
	if err := bm.backend.Preflight(ctx, dep, host, spec); err != nil {
		log.Debug("BackupManager.createBackup: preflight failed", zap.String("host", host),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, err
	}
	log.Debug("BackupManager.createBackup: preflight passed", zap.String("host", host), zap.Duration("took", time.Since(start)))

	started := time.Now().UTC()
	id := backupID(resource, started, from)
	if err := bm.assertBackupIDFree(ctx, resource, id, from); err != nil {
		log.Debug("BackupManager.createBackup: backup id already taken", zap.String("backup", id), zap.Error(err))
		return nil, err
	}

	// The size of each DRBD device — NOT of its backing volume. See the file
	// header for what happens if those are confused.
	sizes := make(map[uint32]uint64, len(info.Volumes))
	for _, v := range info.Volumes {
		n, err := bm.drbdDeviceBytes(ctx, host, resource, v.VolumeID)
		if err != nil {
			log.Debug("BackupManager.createBackup: reading DRBD device size failed", zap.Uint32("volume", v.VolumeID), zap.Error(err))
			return nil, err
		}
		log.Debug("BackupManager.createBackup: DRBD device size", zap.Uint32("volume", v.VolumeID), zap.Uint64("size_bytes", n))
		sizes[v.VolumeID] = n
	}

	start = time.Now()
	sess, err := bm.backend.Prepare(ctx, dep, host, spec)
	if err != nil {
		log.Debug("BackupManager.createBackup: preparing transfer session failed", zap.String("host", host),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, err
	}
	log.Debug("BackupManager.createBackup: transfer session ready", zap.String("host", host),
		zap.String("backend", bm.backend.Name()), zap.Duration("took", time.Since(start)))
	defer func() {
		if err := sess.Close(context.WithoutCancel(ctx)); err != nil {
			bm.controller.logger.Warn("Failed to remove staged backup credentials",
				zap.String("node", node), zap.Error(err))
		}
	}()

	var parent *database.Backup
	if !full {
		var why string
		if parent, why = bm.incrementalBase(ctx, info, targetName, node, host, sizes, encrypted); parent == nil {
			bm.controller.logger.Info("Taking a full backup", zap.String("resource", resource), zap.String("reason", why))
		} else if why = lockedChainRefusal(spec, parent, started); why != "" {
			parent = nil
			bm.controller.logger.Info("Taking a full backup", zap.String("resource", resource), zap.String("reason", why))
		} else {
			log.Debug("BackupManager.createBackup: incremental on parent", zap.String("parent", parent.ID))
		}
	} else {
		log.Debug("BackupManager.createBackup: full backup requested")
	}

	// Snapshot every volume before uploading any of them, so the images are as
	// close to a single instant as the storage layer allows. Snapshots kept as
	// the next incremental's base are left out of the cleanup.
	keep := map[uint32]bool{}
	var snaps map[uint32]string
	if from != "" {
		// The caller's snapshot is the caller's: never removed here.
		snaps, err = bm.useSnapshot(ctx, host, info, from)
		for id := range snaps {
			keep[id] = true
		}
	} else {
		snaps, err = bm.snapshotVolumes(ctx, host, info, started)
	}
	defer func() {
		drop := make(map[uint32]string, len(snaps))
		for id, name := range snaps {
			if !keep[id] {
				drop[id] = name
			}
		}
		log.Debug("BackupManager.createBackup: dropping snapshots not kept as base", zap.Int("drop", len(drop)),
			zap.Int("keep", len(keep)))
		bm.removeSnapshots(context.WithoutCancel(ctx), host, info, drop)
	}()
	if err != nil {
		log.Debug("BackupManager.createBackup: snapshotting failed", zap.Int("taken", len(snaps)), zap.Error(err))
		return nil, err
	}

	rec := &database.Backup{
		ID: id, Resource: resource, Target: targetName, Node: node,
		Backend: bm.backend.Name(), State: database.BackupStateRunning,
		Prefix: backup.ObjectPath(resource, id), StartedAt: started,
		Kind: database.BackupKindFull, Schedule: schedule,
	}
	if parent != nil {
		rec.Kind, rec.Parent = database.BackupKindIncremental, parent.ID
	}
	if spec.Locked() {
		rec.LockMode, rec.RetainUntil = string(spec.LockMode), chainRetainUntil(spec, parent, started)
		sess.SetLock(spec.LockMode, rec.RetainUntil)
		log.Debug("BackupManager.createBackup: objects will be locked", zap.String("lock_mode", rec.LockMode),
			zap.Time("retain_until", rec.RetainUntil))
	}
	start = time.Now()
	if err := bm.controller.db.SaveBackup(ctx, rec); err != nil {
		log.Debug("BackupManager.createBackup: db save failed", zap.String("backup", rec.ID),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, fmt.Errorf("record backup: %w", err)
	}
	log.Debug("BackupManager.createBackup: recorded as running", zap.String("backup", rec.ID), zap.String("kind", rec.Kind),
		zap.Duration("took", time.Since(start)))

	// uploaded tracks every object the node was asked to write, whether or not
	// it then verified. Cleanup has to work from this and not from rec.Volumes:
	// the volume that failed verification is precisely the one whose remains
	// are on the target, and it is the one rec.Volumes does not list.
	var uploaded []string
	start = time.Now()
	if parent != nil {
		err = bm.uploadDeltas(ctx, sess, host, info, parent, snaps, sizes, encrypted, rec, &uploaded)
	} else {
		err = bm.uploadVolumes(ctx, sess, host, info, snaps, sizes, encrypted, rec, &uploaded)
	}
	log.Debug("BackupManager.createBackup: upload finished", zap.String("backup", rec.ID), zap.Bool("incremental", parent != nil),
		zap.Int("objects", len(uploaded)), zap.Uint64("bytes", rec.TotalBytes), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		bm.failBackup(ctx, sess, rec, uploaded, err)
		return nil, err
	}

	manifestPath := backup.ObjectPath(rec.Prefix, manifestObject)
	manifest, err := bm.renderManifest(rec, info, snaps)
	if err != nil {
		log.Debug("BackupManager.createBackup: rendering manifest failed", zap.Error(err))
		bm.failBackup(ctx, sess, rec, uploaded, err)
		return nil, err
	}
	start = time.Now()
	if err := sess.PutText(ctx, manifestPath, manifest); err != nil {
		log.Debug("BackupManager.createBackup: uploading manifest failed", zap.String("object", manifestPath),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		bm.failBackup(ctx, sess, rec, append(uploaded, manifestPath), err)
		return nil, err
	}
	log.Debug("BackupManager.createBackup: manifest uploaded", zap.String("object", manifestPath),
		zap.Int("bytes", len(manifest)), zap.Duration("took", time.Since(start)))
	if err := verifyLocks(ctx, sess, rec, append(uploaded, manifestPath)); err != nil {
		log.Debug("BackupManager.createBackup: lock verification failed", zap.Error(err))
		bm.failBackup(ctx, sess, rec, append(uploaded, manifestPath), err)
		return nil, err
	}

	if from == "" {
		keep = bm.keepBases(ctx, host, rec, snaps)
		log.Debug("BackupManager.createBackup: snapshots kept as next base", zap.Int("count", len(keep)))
	}
	rec.State = database.BackupStateCompleted
	rec.FinishedAt = time.Now()
	start = time.Now()
	if err := bm.controller.db.SaveBackup(ctx, rec); err != nil {
		log.Debug("BackupManager.createBackup: db save of completed backup failed", zap.String("backup", rec.ID),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, fmt.Errorf("record completed backup: %w", err)
	}
	// The new snapshots are the base now; the old ones would only hold space.
	// A backup of a caller's snapshot keeps no base, so the base of the
	// target's incremental chain stays where it is.
	if from == "" {
		bm.releaseOlderBases(context.WithoutCancel(ctx), rec)
	}
	bm.controller.logger.Info("Backup completed",
		zap.String("backup", rec.ID), zap.String("resource", resource),
		zap.String("target", targetName), zap.String("kind", rec.Kind), zap.Uint64("bytes", rec.TotalBytes))
	return rec, nil
}

// failBackup records the failure and removes whatever was uploaded, so a failed
// run leaves neither a restorable-looking record nor paid-for garbage.
func (bm *BackupManager) failBackup(ctx context.Context, sess backup.Session, rec *database.Backup, objects []string, cause error) {
	log := bm.controller.traceLogger()
	log.Debug("BackupManager.failBackup: cleaning up failed backup", zap.String("backup", rec.ID),
		zap.Int("objects", len(objects)), zap.NamedError("cause", cause))
	cleanup := context.WithoutCancel(ctx)
	for _, obj := range objects {
		start := time.Now()
		if err := sess.Remove(cleanup, obj); err != nil {
			bm.controller.logger.Warn("Failed to remove object of a failed backup",
				zap.String("backup", rec.ID), zap.String("object", obj), zap.Error(err))
			continue
		}
		log.Debug("BackupManager.failBackup: object removed", zap.String("backup", rec.ID), zap.String("object", obj),
			zap.Duration("took", time.Since(start)))
	}
	rec.State = database.BackupStateFailed
	rec.Error = cause.Error()
	rec.FinishedAt = time.Now()
	if err := bm.controller.db.SaveBackup(cleanup, rec); err != nil {
		bm.controller.logger.Error("Failed to record backup failure",
			zap.String("backup", rec.ID), zap.Error(err))
	}
	bm.controller.logger.Warn("Backup failed",
		zap.String("backup", rec.ID), zap.String("resource", rec.Resource), zap.Error(cause))
}

// renderManifest builds the JSON index stored beside the images.
func (bm *BackupManager) renderManifest(rec *database.Backup, info *ResourceInfo, snaps map[uint32]string) (string, error) {
	bm.controller.traceLogger().Debug("BackupManager.renderManifest: rendering", zap.String("backup", rec.ID),
		zap.Int("volumes", len(rec.Volumes)), zap.Int("snapshots", len(snaps)))
	m := BackupManifest{
		Version: manifestVersion, ID: rec.ID, Resource: rec.Resource, Node: rec.Node,
		Backend: rec.Backend, Kind: rec.Kind, Parent: rec.Parent, Schedule: rec.Schedule,
		CreatedAt:  rec.StartedAt.UTC().Format(time.RFC3339),
		FinishedAt: time.Now().UTC().Format(time.RFC3339),
		TotalBytes: rec.TotalBytes,
		LockMode:   rec.LockMode,
		Note: "Raw full images of each DRBD volume, bounded by the DRBD device size " +
			"(smaller than the backing LV, whose tail holds DRBD metadata). Restore with " +
			"`haify backup restore`, or by writing each image to a device of at least that size. " +
			"An incremental holds only changed ranges: restore its parent chain down to the full " +
			"backup first, then write each run of its gunzipped image at the offset its ranges list gives.",
	}
	if !rec.RetainUntil.IsZero() {
		m.RetainUntil = rec.RetainUntil.UTC().Format(time.RFC3339)
	}
	for _, v := range rec.Volumes {
		m.Volumes = append(m.Volumes, BackupManifestVolume{
			VolumeID: v.VolumeID, Object: v.Object, Bytes: v.Bytes,
			Pool: v.Pool, Backing: v.BackingVolume, Ranges: v.Ranges, ChangedBytes: v.ChangedBytes,
			ReadThroughLUKS: v.ReadThroughLUKS,
		})
	}
	for _, v := range info.Volumes {
		if name, ok := snaps[v.VolumeID]; ok {
			m.Snapshots = append(m.Snapshots, BackupManifestSnapshot{VolumeID: v.VolumeID, Name: name})
		}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		bm.controller.traceLogger().Debug("BackupManager.renderManifest: marshal failed", zap.Error(err))
		return "", fmt.Errorf("render backup manifest: %w", err)
	}
	return string(data), nil
}
