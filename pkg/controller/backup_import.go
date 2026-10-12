package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/backup"
	"github.com/haify-project/haify/pkg/database"
)

// Importing backups from a target. The controller's database is replicated with
// the cluster, so the disaster backups exist for takes the backup records with
// it. Every backup also leaves a manifest.json beside its images, written only
// once the images are verified; this rebuilds the records, chains included,
// from those manifests alone. It is also how one cluster restores another's
// backups: add the same bucket as a target, import, restore.

// ImportResult says what an import found on the target.
type ImportResult struct {
	// Imported are the backup ids now known to this controller.
	Imported []string
	// Skipped maps a manifest's backup id (or its path when unreadable) to why
	// it was left out.
	Skipped map[string]string
}

// ImportBackups records every complete backup on target that this controller
// does not already know. node names the node that reads the target; empty
// tries the registered nodes in turn until one has rclone.
//
// asOf, when set, imports the target as it was then: the object versions
// current at that time, before any overwrite or delete since — what is left
// to restore from after someone with the target's credentials deleted the
// backups. What it imports is restored from those versions too.
func (bm *BackupManager) ImportBackups(ctx context.Context, targetName, node string, asOf time.Time) (*ImportResult, error) {
	log := bm.controller.traceLogger()
	log.Debug("BackupManager.ImportBackups: importing backups", zap.String("target", targetName),
		zap.String("node", node), zap.Time("as_of", asOf))
	db := bm.controller.db
	if db == nil {
		log.Debug("BackupManager.ImportBackups: no database")
		return nil, fmt.Errorf("database not available")
	}
	start := time.Now()
	dbTarget, err := db.GetBackupTarget(ctx, targetName)
	log.Debug("BackupManager.ImportBackups: read backup target", zap.String("target", targetName),
		zap.Bool("found", err == nil), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	spec := targetSpecFromDB(dbTarget)
	if !asOf.IsZero() && spec.Kind != backup.KindS3 {
		log.Debug("BackupManager.ImportBackups: as-of needs an S3 target", zap.String("kind", string(spec.Kind)))
		return nil, fmt.Errorf("--as-of reads object versions, which only S3 targets keep")
	}
	host, err := bm.importHost(ctx, node, spec)
	if err != nil {
		log.Debug("BackupManager.ImportBackups: no node can read the target", zap.String("target", targetName), zap.Error(err))
		return nil, err
	}
	dep := newBackupDeploymentClient(bm.controller.deployment)
	start = time.Now()
	sess, err := bm.backend.Prepare(ctx, dep, host, spec)
	log.Debug("BackupManager.ImportBackups: prepared target session", zap.String("host", host),
		zap.String("kind", string(spec.Kind)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	sess.SetReadAt(asOf)
	defer func() {
		if err := sess.Close(context.WithoutCancel(ctx)); err != nil {
			bm.controller.logger.Warn("Failed to remove staged backup credentials", zap.String("host", host), zap.Error(err))
		}
	}()

	start = time.Now()
	objects, err := sess.List(ctx)
	log.Debug("BackupManager.ImportBackups: listed target objects", zap.String("host", host),
		zap.Int("count", len(objects)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	sizes := make(map[string]uint64, len(objects))
	var manifests []string
	for _, o := range objects {
		sizes[o.Path] = o.Bytes
		if path.Base(o.Path) == manifestObject {
			manifests = append(manifests, o.Path)
		}
	}
	sort.Strings(manifests)
	log.Debug("BackupManager.ImportBackups: manifests found", zap.Int("count", len(manifests)))

	res := &ImportResult{Skipped: map[string]string{}}
	var found []*database.Backup
	for _, mp := range manifests {
		start := time.Now()
		text, err := sess.GetText(ctx, mp)
		log.Debug("BackupManager.ImportBackups: read manifest", zap.String("manifest", mp),
			zap.Int("size_bytes", len(text)), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			res.Skipped[mp] = err.Error()
			continue
		}
		rec, err := backupFromManifest(text, path.Dir(mp), targetName, sizes)
		if err != nil {
			log.Debug("BackupManager.ImportBackups: manifest skipped", zap.String("manifest", mp), zap.Error(err))
			res.Skipped[mp] = err.Error()
			continue
		}
		if _, err := db.GetBackup(ctx, rec.ID); err == nil {
			log.Debug("BackupManager.ImportBackups: backup already recorded", zap.String("backup", rec.ID))
			res.Skipped[rec.ID] = "already recorded"
			continue
		}
		log.Debug("BackupManager.ImportBackups: backup to import", zap.String("backup", rec.ID),
			zap.String("resource", rec.Resource), zap.String("kind", rec.Kind), zap.String("parent", rec.Parent))
		rec.ReadAt = asOf
		found = append(found, rec)
	}

	bm.adoptBases(ctx, found)
	for _, rec := range found {
		start := time.Now()
		if err := db.SaveBackup(ctx, rec); err != nil {
			log.Debug("BackupManager.ImportBackups: saving imported backup failed", zap.String("backup", rec.ID),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			return res, fmt.Errorf("record imported backup %s: %w", rec.ID, err)
		}
		log.Debug("BackupManager.ImportBackups: recorded imported backup", zap.String("backup", rec.ID),
			zap.Duration("took", time.Since(start)))
		res.Imported = append(res.Imported, rec.ID)
	}
	bm.controller.logger.Info("Imported backups",
		zap.String("target", targetName), zap.Int("imported", len(res.Imported)), zap.Int("skipped", len(res.Skipped)))
	return res, nil
}

// importHost picks the node that reads the target.
func (bm *BackupManager) importHost(ctx context.Context, node string, spec backup.TargetSpec) (string, error) {
	log := bm.controller.traceLogger()
	log.Debug("BackupManager.importHost: picking node to read the target", zap.String("node", node),
		zap.String("kind", string(spec.Kind)))
	dep := newBackupDeploymentClient(bm.controller.deployment)
	if node != "" {
		host := bm.controller.ResolveHost(node)
		start := time.Now()
		err := bm.backend.Preflight(ctx, dep, host, spec)
		log.Debug("BackupManager.importHost: preflight on named node", zap.String("node", node), zap.String("host", host),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return host, err
	}
	nodes, err := bm.controller.db.ListNodes(ctx)
	log.Debug("BackupManager.importHost: listed registered nodes", zap.Int("count", len(nodes)), zap.Error(err))
	if err != nil {
		return "", err
	}
	var last error
	for _, n := range nodes {
		host := bm.controller.ResolveHost(n.Name)
		start := time.Now()
		last = bm.backend.Preflight(ctx, dep, host, spec)
		log.Debug("BackupManager.importHost: preflight", zap.String("node", n.Name), zap.String("host", host),
			zap.Duration("took", time.Since(start)), zap.Error(last))
		if last == nil {
			log.Debug("BackupManager.importHost: node chosen", zap.String("node", n.Name), zap.String("host", host))
			return host, nil
		}
	}
	if last == nil {
		log.Debug("BackupManager.importHost: no node registered")
		return "", fmt.Errorf("no node is registered to read the target from")
	}
	log.Debug("BackupManager.importHost: no node can read the target", zap.Error(last))
	return "", fmt.Errorf("no registered node can read the target: %w", last)
}

// backupFromManifest turns one manifest into a backup record, after checking
// that every object it names is on the target at a plausible size. dir is the
// manifest's directory relative to the target root.
func backupFromManifest(text, dir, target string, sizes map[string]uint64) (*database.Backup, error) {
	log := zap.L()
	log.Debug("backupFromManifest: parsing manifest", zap.String("dir", dir), zap.String("target", target),
		zap.Int("size_bytes", len(text)))
	var m BackupManifest
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &m); err != nil {
		log.Debug("backupFromManifest: not a manifest", zap.String("dir", dir), zap.Error(err))
		return nil, fmt.Errorf("not a backup manifest: %w", err)
	}
	if m.ID == "" || m.Resource == "" || len(m.Volumes) == 0 {
		log.Debug("backupFromManifest: manifest incomplete", zap.String("dir", dir), zap.String("backup", m.ID),
			zap.String("resource", m.Resource), zap.Int("volumes", len(m.Volumes)))
		return nil, fmt.Errorf("manifest names no backup id, resource or volume")
	}
	if m.Version > manifestVersion {
		log.Debug("backupFromManifest: manifest version too new", zap.String("backup", m.ID),
			zap.Int("version", m.Version), zap.Int("supported", manifestVersion))
		return nil, fmt.Errorf("manifest version %d is newer than this controller understands (%d)", m.Version, manifestVersion)
	}
	kind := m.Kind
	if kind == "" {
		log.Debug("backupFromManifest: no kind, treating as full", zap.String("backup", m.ID))
		kind = database.BackupKindFull
	}
	if kind == database.BackupKindIncremental && m.Parent == "" {
		log.Debug("backupFromManifest: incremental without parent", zap.String("backup", m.ID))
		return nil, fmt.Errorf("incremental backup %s names no parent", m.ID)
	}
	started, err := time.Parse(time.RFC3339, m.CreatedAt)
	if err != nil {
		log.Debug("backupFromManifest: bad created_at", zap.String("backup", m.ID), zap.String("created_at", m.CreatedAt))
		return nil, fmt.Errorf("backup %s: bad created_at %q", m.ID, m.CreatedAt)
	}
	finished := started
	if t, err := time.Parse(time.RFC3339, m.FinishedAt); err == nil {
		finished = t
	}
	rec := &database.Backup{
		ID: m.ID, Resource: m.Resource, Target: target, Node: m.Node, Backend: m.Backend,
		State: database.BackupStateCompleted, Prefix: dir, Kind: kind, Parent: m.Parent, Schedule: m.Schedule,
		TotalBytes: m.TotalBytes, StartedAt: started, FinishedAt: finished,
		LockMode: m.LockMode,
	}
	if m.RetainUntil != "" {
		if t, err := time.Parse(time.RFC3339, m.RetainUntil); err == nil {
			rec.RetainUntil = t
		}
	}
	snaps := map[uint32]string{}
	for _, s := range m.Snapshots {
		snaps[s.VolumeID] = s.Name
	}
	for _, v := range m.Volumes {
		log.Debug("backupFromManifest: checking volume image", zap.String("backup", m.ID),
			zap.Uint32("volume", v.VolumeID), zap.String("object", v.Object), zap.String("ranges", v.Ranges))
		if _, ok := sizes[v.Object]; !ok {
			log.Debug("backupFromManifest: image missing", zap.String("backup", m.ID), zap.String("object", v.Object))
			return nil, fmt.Errorf("backup %s: image %s is missing from the target", m.ID, v.Object)
		}
		if v.Ranges != "" {
			if _, ok := sizes[v.Ranges]; !ok {
				log.Debug("backupFromManifest: range list missing", zap.String("backup", m.ID), zap.String("ranges", v.Ranges))
				return nil, fmt.Errorf("backup %s: range list %s is missing from the target", m.ID, v.Ranges)
			}
		}
		rec.Volumes = append(rec.Volumes, database.BackupVolume{
			VolumeID: v.VolumeID, BackingVolume: v.Backing, Pool: v.Pool, Object: v.Object,
			Bytes: v.Bytes, Ranges: v.Ranges, ChangedBytes: v.ChangedBytes, Snapshot: snaps[v.VolumeID],
			ReadThroughLUKS: v.ReadThroughLUKS,
		})
	}
	log.Debug("backupFromManifest: manifest accepted", zap.String("backup", rec.ID), zap.String("resource", rec.Resource),
		zap.String("kind", rec.Kind), zap.Int("volumes", len(rec.Volumes)))
	return rec, nil
}

// adoptBases decides which imported backups keep their base snapshot. Only the
// newest backup of each resource can be the next incremental's base, and only
// when its snapshot is still on a node of this cluster — which it is after a
// lost database, and is not when the backups came from another cluster. Every
// other snapshot name is dropped, so nothing here ever releases (removes) a
// snapshot this cluster did not make.
func (bm *BackupManager) adoptBases(ctx context.Context, found []*database.Backup) {
	log := bm.controller.traceLogger()
	log.Debug("BackupManager.adoptBases: deciding base snapshots", zap.Int("count", len(found)))
	newest := map[string]*database.Backup{}
	for _, b := range found {
		if cur := newest[b.Resource]; cur == nil || b.StartedAt.After(cur.StartedAt) {
			newest[b.Resource] = b
		}
	}
	for _, b := range found {
		isNewest := newest[b.Resource] == b
		keep := isNewest && bm.basesPresent(ctx, b)
		log.Debug("BackupManager.adoptBases: base snapshot decision", zap.String("backup", b.ID),
			zap.String("resource", b.Resource), zap.Bool("newest", isNewest), zap.Bool("keep", keep))
		for i := range b.Volumes {
			if !keep {
				b.Volumes[i].Snapshot = ""
			}
		}
	}
}

// basesPresent reports whether every base snapshot b names is a thin volume on
// b's node right now.
func (bm *BackupManager) basesPresent(ctx context.Context, b *database.Backup) bool {
	log := bm.controller.traceLogger()
	if !bm.registered(ctx, b.Node) {
		log.Debug("BackupManager.basesPresent: backup node not in this cluster", zap.String("backup", b.ID), zap.String("node", b.Node))
		return false
	}
	host := bm.controller.ResolveHost(b.Node)
	for _, v := range b.Volumes {
		if v.Snapshot == "" {
			log.Debug("BackupManager.basesPresent: volume names no snapshot", zap.String("backup", b.ID), zap.Uint32("volume", v.VolumeID))
			return false
		}
		start := time.Now()
		thin, err := bm.controller.deployment.LVIsThin(ctx, host, v.Pool, v.Snapshot)
		log.Debug("BackupManager.basesPresent: checked base snapshot", zap.String("host", host), zap.String("pool", v.Pool),
			zap.String("snapshot", v.Snapshot), zap.Bool("thin", thin), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil || !thin {
			return false
		}
	}
	log.Debug("BackupManager.basesPresent: all base snapshots present", zap.String("backup", b.ID), zap.String("host", host))
	return true
}

// registered reports whether node is a node of this cluster, by name or address.
func (bm *BackupManager) registered(ctx context.Context, node string) bool {
	log := bm.controller.traceLogger()
	nodes, err := bm.controller.db.ListNodes(ctx)
	if err != nil {
		log.Debug("BackupManager.registered: listing nodes failed", zap.String("node", node), zap.Error(err))
		return false
	}
	for _, n := range nodes {
		if n.Name == node || n.Address == node {
			log.Debug("BackupManager.registered: node registered", zap.String("node", node))
			return true
		}
	}
	log.Debug("BackupManager.registered: node not registered", zap.String("node", node), zap.Int("count", len(nodes)))
	return false
}
