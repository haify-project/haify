package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/backup"
	"github.com/haify-project/haify/pkg/database"
)

// Restore and delete. A backup you cannot restore is not a backup, and a
// restore that silently overwrites a volume something is using is worse than no
// restore at all — so the safety checks here are the point of the file, not
// decoration around it.

// RestoreBackup writes a completed backup's images into resource.
//
// The target resource must already exist, must have at least as many volumes as
// the backup, each at least as large, and must not be in use. It is restored in
// place: every block of every volume is overwritten from byte zero.
//
// node selects which replica to write through. The write goes to the DRBD
// device rather than the backing LV, so DRBD replicates it to the peers as part
// of the write path — one copy over the network, and no chance of replicas
// silently diverging. PopulateVolume takes the same approach for the same
// reason.
func (bm *BackupManager) RestoreBackup(ctx context.Context, backupID, resource, node string) (*database.Backup, error) {
	log := bm.controller.traceLogger()
	log.Debug("BackupManager.RestoreBackup: start", zap.String("backup", backupID), zap.String("resource", resource),
		zap.String("node", node))
	if bm.controller.db == nil {
		log.Debug("BackupManager.RestoreBackup: no database")
		return nil, fmt.Errorf("database not available")
	}
	start := time.Now()
	rec, err := bm.controller.db.GetBackup(ctx, backupID)
	log.Debug("BackupManager.RestoreBackup: db get backup", zap.String("backup", backupID), zap.Bool("found", rec != nil),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	if rec.State != database.BackupStateCompleted {
		log.Debug("BackupManager.RestoreBackup: backup not completed", zap.String("state", rec.State))
		return nil, fmt.Errorf(
			"backup %q is in state %q and cannot be restored; only a verified, completed backup is restorable",
			backupID, rec.State)
	}
	if len(rec.Volumes) == 0 {
		log.Debug("BackupManager.RestoreBackup: backup has no volume images")
		return nil, fmt.Errorf("backup %q records no volume images", backupID)
	}
	if resource == "" {
		log.Debug("BackupManager.RestoreBackup: restoring into the backed-up resource", zap.String("resource", rec.Resource))
		resource = rec.Resource
	}
	chain, err := bm.restoreChain(ctx, rec)
	if err != nil {
		log.Debug("BackupManager.RestoreBackup: building restore chain failed", zap.Error(err))
		return nil, err
	}
	log.Debug("BackupManager.RestoreBackup: restore chain", zap.Int("length", len(chain)))
	if err := bm.assertNotCiphertext(ctx, chain); err != nil {
		log.Debug("BackupManager.RestoreBackup: chain holds ciphertext images", zap.Error(err))
		return nil, err
	}

	start = time.Now()
	dbTarget, err := bm.controller.db.GetBackupTarget(ctx, rec.Target)
	log.Debug("BackupManager.RestoreBackup: db get target", zap.String("target", rec.Target),
		zap.Bool("found", dbTarget != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("backup %q refers to target %q: %w", backupID, rec.Target, err)
	}

	info, err := bm.controller.resources.GetResource(ctx, resource)
	if err != nil {
		log.Debug("BackupManager.RestoreBackup: reading resource failed", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	if err := bm.assertRestorable(ctx, info); err != nil {
		log.Debug("BackupManager.RestoreBackup: resource not restorable", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}

	node, err = bm.pickRestoreNode(info, node)
	if err != nil {
		log.Debug("BackupManager.RestoreBackup: no node to restore through", zap.Error(err))
		return nil, err
	}
	host := bm.controller.ResolveHost(node)
	dep := newBackupDeploymentClient(bm.controller.deployment)
	log.Debug("BackupManager.RestoreBackup: restoring through node", zap.String("node", node), zap.String("host", host))
	start = time.Now()
	if err := bm.backend.Preflight(ctx, dep, host, targetSpecFromDB(dbTarget)); err != nil {
		log.Debug("BackupManager.RestoreBackup: preflight failed", zap.String("host", host),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, err
	}
	log.Debug("BackupManager.RestoreBackup: preflight passed", zap.String("host", host), zap.Duration("took", time.Since(start)))

	// Size check before anything is promoted or written. A target device
	// smaller than the image would take a silently truncated copy — a
	// filesystem that mounts and is missing its tail, which is the worst
	// possible way for a restore to fail.
	for _, v := range rec.Volumes {
		have, err := bm.drbdDeviceBytes(ctx, host, resource, v.VolumeID)
		if err != nil {
			log.Debug("BackupManager.RestoreBackup: reading device size failed", zap.Uint32("volume", v.VolumeID), zap.Error(err))
			return nil, err
		}
		log.Debug("BackupManager.RestoreBackup: size check", zap.Uint32("volume", v.VolumeID),
			zap.Uint64("device_bytes", have), zap.Uint64("image_bytes", v.Bytes))
		if have < v.Bytes {
			return nil, fmt.Errorf(
				"volume %d of %q is %d bytes but the backup image is %d; restore into a resource at least as large",
				v.VolumeID, resource, have, v.Bytes)
		}
	}

	start = time.Now()
	sess, err := bm.backend.Prepare(ctx, dep, host, targetSpecFromDB(dbTarget))
	if err != nil {
		log.Debug("BackupManager.RestoreBackup: preparing transfer session failed", zap.String("host", host),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, err
	}
	log.Debug("BackupManager.RestoreBackup: transfer session ready", zap.String("host", host), zap.Duration("took", time.Since(start)))
	defer func() {
		if err := sess.Close(context.WithoutCancel(ctx)); err != nil {
			bm.controller.logger.Warn("Failed to remove staged backup credentials",
				zap.String("node", node), zap.Error(err))
		}
	}()

	// The device must be Primary to be writable. Demote afterwards so the
	// resource is left exactly as it was found.
	if err := bm.controller.resources.SetPrimary(ctx, resource, node, false); err != nil {
		log.Debug("BackupManager.RestoreBackup: promote failed", zap.String("node", node), zap.Error(err))
		return nil, fmt.Errorf("promote %s on %s for restore: %w", resource, node, err)
	}
	log.Debug("BackupManager.RestoreBackup: promoted for restore", zap.String("resource", resource), zap.String("node", node))
	defer func() {
		if err := bm.controller.resources.SetSecondary(context.WithoutCancel(ctx), resource, node); err != nil {
			bm.controller.logger.Warn("Failed to demote after restore; the data is written but the resource stays Primary",
				zap.String("resource", resource), zap.String("node", node), zap.Error(err))
		}
	}()

	bm.controller.logger.Info("Restoring backup",
		zap.String("backup", backupID), zap.String("resource", resource), zap.String("node", node))

	// An incremental is the full backup it is built on plus every change
	// since, applied oldest first.
	for _, b := range chain {
		// A locked backup is read as it was written: a version written over
		// it or a delete marker put on top since changes nothing here.
		sess.SetReadAt(readAtFor(b))
		log.Debug("BackupManager.RestoreBackup: applying chain link", zap.String("backup", b.ID),
			zap.String("kind", b.Kind), zap.Int("volumes", len(b.Volumes)))
		for _, v := range b.Volumes {
			start := time.Now()
			if v.Ranges == "" {
				err = bm.restoreVolume(ctx, sess, host, resource, v)
			} else {
				err = bm.applyDelta(ctx, sess, host, resource, b.ID, v)
			}
			log.Debug("BackupManager.RestoreBackup: volume written", zap.String("backup", b.ID), zap.Uint32("volume", v.VolumeID),
				zap.Bool("delta", v.Ranges != ""), zap.Duration("took", time.Since(start)), zap.Error(err))
			if err != nil {
				return nil, err
			}
		}
	}

	bm.controller.logger.Info("Backup restored",
		zap.String("backup", backupID), zap.String("resource", resource),
		zap.Uint64("bytes", rec.TotalBytes))
	return rec, nil
}

// restoreVolume streams one image onto its DRBD device.
func (bm *BackupManager) restoreVolume(ctx context.Context, sess backup.Session, host, resource string, v database.BackupVolume) error {
	target := fmt.Sprintf("/dev/drbd/by-res/%s/%d", resource, v.VolumeID)

	// count is bounded by the image's recorded size, not by the device's: the
	// device may be larger, and writing past the image would be writing
	// whatever the pipe produced after it ended. conv=fsync forces the copy
	// through to stable storage — and, via DRBD, to the peers — before dd
	// exits, so a promote elsewhere afterwards cannot read stale blocks.
	// pipefail makes a failed download fail the whole restore instead of
	// leaving dd to write a short stream and exit 0; `set -e` is what stops the
	// trailing flushbufs from overwriting that failure with its own exit 0.
	// On all-thin, unencrypted replicas the zero runs are skipped rather than
	// written; see sparse_write.go.
	cmd := fmt.Sprintf(
		"set -e -o pipefail; %s%s%s | sudo dd of=%s bs=4M count=%d iflag=fullblock,count_bytes oflag=direct conv=$CONV status=none; sudo blockdev --flushbufs %s",
		sparseWriteSetup(bm.controller.resources.zeroReadingReplicas(ctx, resource), target),
		sess.PullCmd(v.Object), decompressFor(v.Object), target, v.Bytes, target)
	// Same 30-second default, same consequence in the other direction: a
	// truncated restore that reports success. See execDataMove.
	log := bm.controller.traceLogger()
	log.Debug("BackupManager.restoreVolume: streaming image onto device", zap.String("host", host),
		zap.String("resource", resource), zap.Uint32("volume", v.VolumeID), zap.String("object", v.Object),
		zap.Uint64("bytes", v.Bytes), zap.String("device", target))
	start := time.Now()
	res, err := bm.execDataMove(ctx, host, "bash -c "+shellSingleQuote(cmd))
	if err != nil {
		log.Debug("BackupManager.restoreVolume: exec failed", zap.String("host", host),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("restore volume %d of %q: %w", v.VolumeID, resource, err)
	}
	if !res.AllSuccess() {
		log.Debug("BackupManager.restoreVolume: write failed on host", zap.String("host", host),
			zap.Duration("took", time.Since(start)))
		return fmt.Errorf("restore volume %d of %q failed: %s", v.VolumeID, resource, res.FailureDetails())
	}
	log.Debug("BackupManager.restoreVolume: written", zap.String("host", host), zap.Uint32("volume", v.VolumeID),
		zap.Duration("took", time.Since(start)))
	return nil
}

// applyDelta writes one incremental image's changed ranges onto its volume.
func (bm *BackupManager) applyDelta(ctx context.Context, sess backup.Session, host, resource, id string, v database.BackupVolume) error {
	target := fmt.Sprintf("/dev/drbd/by-res/%s/%d", resource, v.VolumeID)
	log := bm.controller.traceLogger()
	cmd := applyDeltaCmd(sess.PullCmd(v.Ranges), sess.PullCmd(v.Object), target)
	log.Debug("BackupManager.applyDelta: applying changed ranges", zap.String("host", host), zap.String("backup", id),
		zap.String("resource", resource), zap.Uint32("volume", v.VolumeID), zap.String("object", v.Object),
		zap.Uint64("changed_bytes", v.ChangedBytes))
	start := time.Now()
	res, err := bm.execDataMove(ctx, host, "bash -c "+shellSingleQuote(cmd))
	if err != nil {
		log.Debug("BackupManager.applyDelta: exec failed", zap.String("host", host),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("apply %s to volume %d of %q: %w", id, v.VolumeID, resource, err)
	}
	if !res.AllSuccess() {
		log.Debug("BackupManager.applyDelta: write failed on host", zap.String("host", host),
			zap.Duration("took", time.Since(start)))
		return fmt.Errorf("apply %s to volume %d of %q failed: %s", id, v.VolumeID, resource, res.FailureDetails())
	}
	log.Debug("BackupManager.applyDelta: applied", zap.String("host", host), zap.Uint32("volume", v.VolumeID),
		zap.Duration("took", time.Since(start)))
	return nil
}

// assertRestorable refuses to overwrite a resource anything is using.
//
// "In use" is defined by DRBD's own rule: only a Primary can be open, so a
// resource that is Secondary everywhere has no writer and no mount anywhere in
// the cluster. A gateway or a drbd-reactor promoter config is treated the same
// way even when nothing is Primary right now, because either of them can
// promote the resource at any moment — including in the middle of the restore.
func (bm *BackupManager) assertRestorable(ctx context.Context, info *ResourceInfo) error {
	log := bm.controller.traceLogger()
	log.Debug("BackupManager.assertRestorable: checking", zap.String("resource", info.Name), zap.Int("volumes", len(info.Volumes)))
	if len(info.Volumes) == 0 {
		log.Debug("BackupManager.assertRestorable: no volumes", zap.String("resource", info.Name))
		return fmt.Errorf("resource %q has no volumes to restore into", info.Name)
	}
	var primaries []string
	for node, st := range info.NodeStates {
		if st != nil && strings.EqualFold(st.Role, "Primary") {
			primaries = append(primaries, node)
		}
	}
	if len(primaries) > 0 {
		log.Debug("BackupManager.assertRestorable: resource is Primary somewhere", zap.Strings("primaries", primaries))
		return fmt.Errorf(
			"resource %q is Primary on %s and is therefore in use; stop the workload and demote it before restoring over it",
			info.Name, strings.Join(primaries, ", "))
	}
	if hosts := bm.promoterHosts(ctx, info); len(hosts) > 0 {
		log.Debug("BackupManager.assertRestorable: promoter config present", zap.Strings("nodes", hosts))
		return fmt.Errorf(
			"resource %q has a drbd-reactor promoter config on %s, which can promote it at any moment; remove it (ha delete, gateway delete) or disable it before restoring over it",
			info.Name, strings.Join(hosts, ", "))
	}
	if bm.controller.db != nil {
		if gw, err := bm.controller.db.GetGatewayByResource(ctx, info.Name); err == nil && gw != nil {
			log.Debug("BackupManager.assertRestorable: exported by gateway", zap.String("gateway", gw.Name))
			return fmt.Errorf(
				"resource %q is exported by gateway %q, which can promote it at any moment; delete or stop the gateway before restoring over it",
				info.Name, gw.Name)
		}
	}
	log.Debug("BackupManager.assertRestorable: restorable", zap.String("resource", info.Name))
	return nil
}

// pickRestoreNode chooses the replica to write through: the named one, or any
// node with an UpToDate copy.
func (bm *BackupManager) pickRestoreNode(info *ResourceInfo, requested string) (string, error) {
	log := bm.controller.traceLogger()
	if requested != "" {
		for _, n := range info.Nodes {
			if n == requested {
				log.Debug("BackupManager.pickRestoreNode: requested node holds a replica", zap.String("node", requested))
				return requested, nil
			}
		}
		log.Debug("BackupManager.pickRestoreNode: requested node holds no replica", zap.String("node", requested))
		return "", fmt.Errorf("node %q holds no replica of %q", requested, info.Name)
	}
	for _, n := range info.Nodes {
		if info.WANMode && n == info.DRNode {
			log.Debug("BackupManager.pickRestoreNode: skipping DR node", zap.String("node", n))
			continue
		}
		if st := info.NodeStates[n]; st != nil && st.DiskState == "UpToDate" {
			log.Debug("BackupManager.pickRestoreNode: chose UpToDate node", zap.String("node", n))
			return n, nil
		}
		log.Debug("BackupManager.pickRestoreNode: node not UpToDate", zap.String("node", n))
	}
	log.Debug("BackupManager.pickRestoreNode: no UpToDate replica", zap.String("resource", info.Name))
	return "", fmt.Errorf(
		"no node holds an UpToDate replica of %q to restore through; repair the resource first", info.Name)
}

// DeleteBackup removes a backup's objects from its target and then its record.
//
// The record is only dropped once the objects are gone, so a failure leaves the
// backup listed and retryable rather than turning it into storage nobody knows
// about. force drops the record regardless, which is the escape hatch for a
// target that no longer exists.
func (bm *BackupManager) DeleteBackup(ctx context.Context, backupID, node string, force bool) error {
	log := bm.controller.traceLogger()
	log.Debug("BackupManager.DeleteBackup: start", zap.String("backup", backupID), zap.String("node", node), zap.Bool("force", force))
	if bm.controller.db == nil {
		log.Debug("BackupManager.DeleteBackup: no database")
		return fmt.Errorf("database not available")
	}
	start := time.Now()
	rec, err := bm.controller.db.GetBackup(ctx, backupID)
	log.Debug("BackupManager.DeleteBackup: db get backup", zap.String("backup", backupID), zap.Bool("found", rec != nil),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return err
	}
	// Deleting a link would leave every later incremental unrestorable, and
	// --force is no way out of that: the data really would be gone.
	deps, err := bm.dependents(ctx, rec)
	if err != nil {
		log.Debug("BackupManager.DeleteBackup: listing dependents failed", zap.Error(err))
		return err
	}
	if len(deps) > 0 {
		log.Debug("BackupManager.DeleteBackup: backup has dependents", zap.Strings("dependents", deps))
		return fmt.Errorf("backup %q is the base of %s; delete those first, newest first",
			backupID, strings.Join(deps, ", "))
	}

	touchObjects, err := assertDeletable(rec, force, lockNow())
	if err != nil {
		log.Debug("BackupManager.DeleteBackup: not deletable", zap.Error(err))
		return err
	}
	log.Debug("BackupManager.DeleteBackup: deletable", zap.Bool("remove_objects", touchObjects))
	if !touchObjects {
		bm.controller.logger.Warn("Dropping the record of a locked backup; its objects stay on the target until the lock expires",
			zap.String("backup", backupID), zap.Time("retain_until", rec.RetainUntil))
	} else if err := bm.removeBackupObjects(ctx, rec, node); err != nil {
		if !force {
			return fmt.Errorf("%w (pass --force to drop the record anyway and leave the objects behind)", err)
		}
		bm.controller.logger.Warn("Dropping backup record despite object removal failure",
			zap.String("backup", backupID), zap.Error(err))
	}

	bm.releaseBases(ctx, rec)
	start = time.Now()
	if err := bm.controller.db.DeleteBackup(ctx, backupID); err != nil {
		log.Debug("BackupManager.DeleteBackup: db delete failed", zap.String("backup", backupID),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("delete backup record: %w", err)
	}
	log.Debug("BackupManager.DeleteBackup: record deleted", zap.String("backup", backupID), zap.Duration("took", time.Since(start)))
	bm.controller.logger.Info("Backup deleted", zap.String("backup", backupID), zap.Bool("force", force))
	return nil
}

// removeBackupObjects deletes a backup's images and manifest from its target.
func (bm *BackupManager) removeBackupObjects(ctx context.Context, rec *database.Backup, node string) error {
	log := bm.controller.traceLogger()
	start := time.Now()
	dbTarget, err := bm.controller.db.GetBackupTarget(ctx, rec.Target)
	log.Debug("BackupManager.removeBackupObjects: db get target", zap.String("backup", rec.ID), zap.String("target", rec.Target),
		zap.Bool("found", dbTarget != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("backup %q refers to target %q: %w", rec.ID, rec.Target, err)
	}
	if node == "" {
		log.Debug("BackupManager.removeBackupObjects: using the node that wrote the backup", zap.String("node", rec.Node))
		// Any node can reach the target; the one that wrote the backup is the
		// one already known to have working credentials and connectivity.
		node = rec.Node
	}
	host := bm.controller.ResolveHost(node)
	if host == "" {
		log.Debug("BackupManager.removeBackupObjects: no host to reach target", zap.String("node", node))
		return fmt.Errorf("no node available to reach target %q", rec.Target)
	}

	dep := newBackupDeploymentClient(bm.controller.deployment)
	start = time.Now()
	sess, err := bm.backend.Prepare(ctx, dep, host, targetSpecFromDB(dbTarget))
	if err != nil {
		log.Debug("BackupManager.removeBackupObjects: preparing transfer session failed", zap.String("host", host),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return err
	}
	defer func() {
		if err := sess.Close(context.WithoutCancel(ctx)); err != nil {
			bm.controller.logger.Warn("Failed to remove staged backup credentials",
				zap.String("node", node), zap.Error(err))
		}
	}()

	objects := make([]string, 0, 2*len(rec.Volumes)+1)
	for _, v := range rec.Volumes {
		objects = append(objects, v.Object)
		if v.Ranges != "" {
			objects = append(objects, v.Ranges)
		}
	}
	objects = append(objects, backup.ObjectPath(rec.Prefix, manifestObject))
	for _, obj := range objects {
		start := time.Now()
		if err := sess.Remove(ctx, obj); err != nil {
			log.Debug("BackupManager.removeBackupObjects: removing object failed", zap.String("object", obj),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			return err
		}
		log.Debug("BackupManager.removeBackupObjects: object removed", zap.String("object", obj),
			zap.Duration("took", time.Since(start)))
	}
	return nil
}

// formatBytes renders a byte count for the CLI and API summaries.
func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatUint(n, 10) + " B"
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// decompressFor is the pipeline stage that turns a stored image back into raw
// blocks: backups taken before images were compressed have no suffix and are
// written as they are.
func decompressFor(object string) string {
	if strings.HasSuffix(object, ".gz") {
		return " | gzip -dc"
	}
	return ""
}

// promoterHosts names the nodes of info holding an enabled drbd-reactor
// promoter config for it: an `ha create` config or a gateway's. The database
// knows about gateways, but `ha create` configs live only on the nodes.
func (bm *BackupManager) promoterHosts(ctx context.Context, info *ResourceInfo) []string {
	var found []string
	cmd := fmt.Sprintf("ls /etc/drbd-reactor.d/ 2>/dev/null | grep -Eq '^haify-[a-z]+-%s\\.toml$' && echo HAIFY_PROMOTER=yes; true",
		regexpQuoteForGrep(info.Name))
	log := bm.controller.traceLogger()
	for _, n := range info.Nodes {
		host := bm.controller.ResolveHost(n)
		start := time.Now()
		res, err := bm.controller.deployment.Exec(ctx, []string{host}, cmd)
		has := err == nil && res != nil && strings.Contains(hostOutput(res, host), "HAIFY_PROMOTER=yes")
		log.Debug("BackupManager.promoterHosts: checked node for promoter config", zap.String("resource", info.Name),
			zap.String("node", n), zap.String("host", host), zap.Bool("has_promoter", has),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if has {
			found = append(found, n)
		}
	}
	return found
}

// regexpQuoteForGrep escapes the characters of a resource name that mean
// something in an extended regular expression.
func regexpQuoteForGrep(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`.+*?()[]{}|^$\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
