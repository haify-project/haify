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
	"github.com/haify-project/haify/pkg/deployment"
)

// dataMoveTimeout is how long a volume transfer may take when the caller set no
// deadline of its own. The CLI defaults to 24h; this matches it so a backup
// started over the API is not cut shorter than one started from the shell.
const dataMoveTimeout = 24 * time.Hour

// execDataMove runs a command that streams a whole volume, bounded by the
// caller's deadline rather than by deployment.Exec's default.
//
// That default is 30 seconds. It is right for the `lvs` and `drbdsetup` queries
// Exec was written for and catastrophic for moving a volume: a 1 GiB image
// takes roughly 40 seconds on a gigabit LAN, so Exec returned while dd and
// rclone were still running. Everything downstream then read that early return
// as a finished upload — the verification found no object yet and failed the
// backup, the cleanup could not delete an object that did not exist yet, and
// the pipeline carried on regardless and eventually left a complete but
// orphaned image on the target. Every symptom traced back to this one line.
//
// It also reports a result that named no host as a failure. deployment.Exec
// returns AllSuccess() == true for an empty host set, so a command that ran
// nowhere is indistinguishable from one that succeeded — which is exactly the
// wrong default for a step whose whole purpose is moving bytes.
func (bm *BackupManager) execDataMove(ctx context.Context, host, cmd string) (*deployment.ExecResult, error) {
	log := bm.controller.traceLogger()
	timeout := dataMoveTimeout
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining > 0 {
			timeout = remaining
		}
	}
	// The command streams through the target's transfer tool and can carry its
	// credentials, so only its length is traced.
	log.Debug("BackupManager.execDataMove: running data move", zap.String("host", host),
		zap.Int("cmd_len", len(cmd)), zap.Duration("timeout", timeout))
	start := time.Now()
	res, err := bm.controller.deployment.Exec(ctx, []string{host}, cmd, deployment.WithExecTimeout(timeout))
	if err != nil {
		log.Debug("BackupManager.execDataMove: exec failed", zap.String("host", host),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, err
	}
	if res == nil || len(res.Hosts) == 0 {
		log.Debug("BackupManager.execDataMove: no result from host", zap.String("host", host),
			zap.Duration("took", time.Since(start)))
		return nil, fmt.Errorf("command produced no result for %s; it may not have run at all", host)
	}
	log.Debug("BackupManager.execDataMove: finished", zap.String("host", host), zap.Bool("success", res.AllSuccess()),
		zap.Duration("took", time.Since(start)))
	return res, nil
}

// uploadVolumes streams each snapshot to the target and verifies the stored
// size. It stops at the first failure: half an image is not a backup.
//
// encrypted names the volumes that are LUKS containers; their snapshots are
// read through a crypt mapping, see backup_luks.go.
func (bm *BackupManager) uploadVolumes(ctx context.Context, sess backup.Session, host string,
	info *ResourceInfo, snaps map[uint32]string, sizes map[uint32]uint64, encrypted map[uint32]bool,
	rec *database.Backup, uploaded *[]string) error {

	log := bm.controller.traceLogger()
	log.Debug("BackupManager.uploadVolumes: start", zap.String("backup", rec.ID), zap.String("host", host),
		zap.Int("volumes", len(info.Volumes)))
	for _, v := range info.Volumes {
		size := sizes[v.VolumeID]
		if size == 0 {
			log.Debug("BackupManager.uploadVolumes: zero-byte device", zap.Uint32("volume", v.VolumeID))
			return fmt.Errorf("volume %d of %q reports a zero-byte DRBD device; refusing to record an empty backup",
				v.VolumeID, info.Name)
		}
		// Compressed on the way out. A thin volume holding 1 GiB of data in a
		// 100 GiB device used to upload — and store, and bill for — all 100
		// GiB, most of it zeros. gzip is on every node Haify supports, so the
		// restoring node never lacks the tool to read it back.
		object := backup.ObjectPath(rec.Prefix, fmt.Sprintf("volume-%d.img.gz", v.VolumeID))
		source, closeSource, err := backupSnapshotSource(v.Pool, v.BackingVolume, snaps[v.VolumeID], encrypted[v.VolumeID])
		if err != nil {
			log.Debug("BackupManager.uploadVolumes: no snapshot source", zap.Uint32("volume", v.VolumeID), zap.Error(err))
			return fmt.Errorf("volume %d of %q: %w", v.VolumeID, info.Name, err)
		}
		log.Debug("BackupManager.uploadVolumes: uploading volume", zap.Uint32("volume", v.VolumeID),
			zap.String("snapshot", snaps[v.VolumeID]), zap.String("object", object), zap.Uint64("size_bytes", size),
			zap.Bool("through_luks", encrypted[v.VolumeID]))

		// pipefail is what makes a truncated read a failed backup: without it
		// the pipeline's exit status is rclone's alone, and rclone happily
		// stores whatever bytes reached it before dd died. The compressed
		// byte count is taken on the way through, so what the target stored
		// can still be checked against what was sent.
		cmd := fmt.Sprintf(`set -e -o pipefail; CNT=$(mktemp); MAP=
cleanup() { %s; rm -f "$CNT"; }
trap cleanup EXIT
%s
sudo dd if="$SRC" bs=4M count=%d iflag=fullblock,count_bytes status=none | gzip -1 -c | tee >(wc -c > "$CNT") | %s
for i in $(seq 1 100); do [ -s "$CNT" ] && break; sleep 0.1; done
echo "HAIFY_SENT=$(cat "$CNT")"`, closeSource, source, size, sess.PushCmd(object, size))
		*uploaded = append(*uploaded, object)
		start := time.Now()
		res, err := bm.execDataMove(ctx, host, "bash -c "+shellSingleQuote(cmd))
		if err != nil {
			log.Debug("BackupManager.uploadVolumes: upload exec failed", zap.Uint32("volume", v.VolumeID),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			return fmt.Errorf("upload volume %d of %q: %w", v.VolumeID, info.Name, err)
		}
		if !res.AllSuccess() {
			log.Debug("BackupManager.uploadVolumes: upload failed on host", zap.Uint32("volume", v.VolumeID),
				zap.String("host", host), zap.Duration("took", time.Since(start)))
			return fmt.Errorf("upload volume %d of %q failed: %s", v.VolumeID, info.Name, res.FailureDetails())
		}
		sent, err := sentBytes(res)
		if err != nil {
			log.Debug("BackupManager.uploadVolumes: sent byte count missing", zap.Uint32("volume", v.VolumeID), zap.Error(err))
			return fmt.Errorf("upload volume %d of %q: %w", v.VolumeID, info.Name, err)
		}
		log.Debug("BackupManager.uploadVolumes: uploaded", zap.Uint32("volume", v.VolumeID), zap.Uint64("sent_bytes", sent),
			zap.Duration("took", time.Since(start)))

		// Ask the far end how much it actually stored. An upload command that
		// exits 0 is not evidence: this is.
		start = time.Now()
		stored, err := sess.SizeBytes(ctx, object)
		log.Debug("BackupManager.uploadVolumes: stored size", zap.String("object", object), zap.Uint64("stored_bytes", stored),
			zap.Uint64("sent_bytes", sent), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return fmt.Errorf("verify volume %d of %q: %w", v.VolumeID, info.Name, err)
		}
		if stored != sent {
			return fmt.Errorf(
				"volume %d of %q uploaded short: sent %d bytes, target holds %d",
				v.VolumeID, info.Name, sent, stored)
		}

		rec.Volumes = append(rec.Volumes, database.BackupVolume{
			VolumeID: v.VolumeID, BackingVolume: v.BackingVolume, Pool: v.Pool,
			Object: object, Bytes: size, ReadThroughLUKS: encrypted[v.VolumeID],
		})
		rec.TotalBytes += size
	}
	log.Debug("BackupManager.uploadVolumes: done", zap.String("backup", rec.ID), zap.Int("volumes", len(rec.Volumes)),
		zap.Uint64("total_bytes", rec.TotalBytes))
	return nil
}

// drbdDeviceBytes returns the size of a resource's DRBD device on host.
//
// It reads sysfs rather than running `blockdev --getsize64` on the device,
// because the node a backup reads from is usually Secondary and a Secondary
// DRBD device cannot be opened. sysfs reports the size without opening
// anything, so the same query works whatever the role happens to be.
func (bm *BackupManager) drbdDeviceBytes(ctx context.Context, host, resource string, volumeID uint32) (uint64, error) {
	cmd := fmt.Sprintf(
		`set -e; DEV=$(readlink -f /dev/drbd/by-res/%s/%d); test -n "$DEV"; cat /sys/class/block/$(basename "$DEV")/size`,
		resource, volumeID)
	log := bm.controller.traceLogger()
	start := time.Now()
	res, err := bm.controller.deployment.Exec(ctx, []string{host}, cmd)
	if err != nil {
		log.Debug("BackupManager.drbdDeviceBytes: exec failed", zap.String("host", host), zap.String("resource", resource),
			zap.Uint32("volume", volumeID), zap.Duration("took", time.Since(start)), zap.Error(err))
		return 0, fmt.Errorf("read DRBD device size for %s/%d on %s: %w", resource, volumeID, host, err)
	}
	if !res.AllSuccess() {
		log.Debug("BackupManager.drbdDeviceBytes: read failed on host", zap.String("host", host), zap.String("resource", resource),
			zap.Uint32("volume", volumeID), zap.Duration("took", time.Since(start)))
		return 0, fmt.Errorf("read DRBD device size for %s/%d on %s failed: %s",
			resource, volumeID, host, res.FailureDetails())
	}
	var out string
	if hr, ok := res.Hosts[host]; ok && hr != nil {
		out = strings.TrimSpace(hr.Output)
	}
	// sysfs reports the size in 512-byte sectors regardless of the device's
	// logical block size; that unit is part of the kernel's ABI.
	fields := strings.Fields(out)
	if len(fields) == 0 {
		log.Debug("BackupManager.drbdDeviceBytes: no size reported", zap.String("host", host), zap.String("resource", resource))
		return 0, fmt.Errorf("no DRBD device size reported for %s/%d on %s", resource, volumeID, host)
	}
	sectors, perr := strconv.ParseUint(fields[len(fields)-1], 10, 64)
	if perr != nil || sectors == 0 {
		log.Debug("BackupManager.drbdDeviceBytes: unparsable size", zap.String("host", host), zap.String("output", out), zap.Error(perr))
		return 0, fmt.Errorf("could not read DRBD device size for %s/%d on %s (got %q)",
			resource, volumeID, host, out)
	}
	log.Debug("BackupManager.drbdDeviceBytes: size", zap.String("host", host), zap.String("resource", resource),
		zap.Uint32("volume", volumeID), zap.Uint64("size_bytes", sectors*512), zap.Duration("took", time.Since(start)))
	return sectors * 512, nil
}

// shellSingleQuote renders s as a single-quoted shell word.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sentBytes reads the compressed byte count an upload reports.
func sentBytes(res *deployment.ExecResult) (uint64, error) {
	for _, h := range res.Hosts {
		for _, line := range strings.Split(h.Output, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "HAIFY_SENT="); ok {
				n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
				if err != nil || n == 0 {
					zap.L().Debug("sentBytes: unparsable sent count", zap.String("host", h.Host), zap.String("value", v))
					return 0, fmt.Errorf("the upload did not report how much it sent (%q)", v)
				}
				zap.L().Debug("sentBytes: sent count", zap.String("host", h.Host), zap.Uint64("bytes", n))
				return n, nil
			}
		}
	}
	zap.L().Debug("sentBytes: no sent count in output", zap.Int("hosts", len(res.Hosts)))
	return 0, fmt.Errorf("the upload did not report how much it sent")
}
