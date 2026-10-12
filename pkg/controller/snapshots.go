package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// SnapshotInfo represents snapshot information
type SnapshotInfo struct {
	Name   string
	Volume string
	SizeGB uint64
	// CreatedAt is LVM's own lv_time, passed through as it renders it.
	CreatedAt string
	// Origin is the logical volume the snapshot was taken from. It is what
	// attributes a snapshot to a DRBD resource: a pool holds the snapshots of
	// every resource on that node, and the name alone does not say which.
	Origin string
}

// SnapshotManager manages volume snapshots
type SnapshotManager struct {
	controller *Controller
}

// NewSnapshotManager creates a new snapshot manager
func NewSnapshotManager(ctrl *Controller) *SnapshotManager {
	return &SnapshotManager{
		controller: ctrl,
	}
}

// zfsVolumeDataset returns volume's dataset when its record says it is a
// zvol. The one-node snapshot calls address a volume as "<pool>/<backing>",
// which reads the same on both pool types, so the record decides.
func (sm *SnapshotManager) zfsVolumeDataset(ctx context.Context, volume string) (string, bool) {
	db := sm.controller.db
	if db == nil {
		return "", false
	}
	pool, backing := parseVolumePath(volume)
	resources, err := db.ListResources(ctx)
	if err != nil {
		return "", false
	}
	for _, r := range resources {
		vols, err := db.ListVolumes(ctx, r.Name)
		if err != nil {
			continue
		}
		for _, v := range vols {
			if v.Pool == pool && v.VolumeName == backing {
				return pool + "/" + backing, isZFSDevice(v.Device)
			}
		}
	}
	return "", false
}

// CreateSnapshot creates a snapshot of volume on node: an LVM snapshot, or a
// ZFS one ("<dataset>@<name>") when the volume is a zvol.
func (sm *SnapshotManager) CreateSnapshot(ctx context.Context, volume, snapshotName, node string) error {
	if dataset, ok := sm.zfsVolumeDataset(ctx, volume); ok {
		return sm.controller.storage.ZFSSnapshot(ctx, dataset, snapshotName, node)
	}
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Creating snapshot",
		zap.String("volume", volume),
		zap.String("snapshot", snapshotName),
		zap.String("node", node))

	// Parse volume path (e.g., "ubuntu-vg/lv0" -> vg="ubuntu-vg", lv="lv0")
	vg, lv := parseVolumePath(volume)
	originPath := fmt.Sprintf("/dev/%s/%s", vg, lv)

	// Create snapshot using lvcreate. Thick LVM snapshots require a COW
	// size or they are rejected outright ("Please specify either size or
	// extents"); 20% of the origin is a sane default for the
	// backup-then-delete workflow this API serves. Callers needing exact
	// sizing use the LVM-specific RPC, which takes an explicit size.
	cmd := fmt.Sprintf("sudo lvcreate -s -l 20%%ORIGIN -n %s %s", snapshotName, originPath)
	result, err := sm.controller.deployment.Exec(ctx, []string{address}, cmd)
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	if !result.AllSuccess() {
		for host, hr := range result.Hosts {
			if !hr.Success {
				return fmt.Errorf("failed to create snapshot on %s: %s", host, strings.TrimSpace(hr.Output))
			}
		}
		return fmt.Errorf("failed to create snapshot: %s", result.FailureDetails())
	}

	sm.controller.logger.Info("Snapshot created successfully",
		zap.String("volume", volume),
		zap.String("snapshot", snapshotName))

	return nil
}

// DeleteSnapshot deletes a snapshot
func (sm *SnapshotManager) DeleteSnapshot(ctx context.Context, volume, snapshotName, node string) error {
	if err := sm.controller.assertSnapshotUnlocked(ctx, snapshotName); err != nil {
		return err
	}
	if dataset, ok := sm.zfsVolumeDataset(ctx, volume); ok {
		return sm.controller.storage.ZFSDeleteSnapshot(ctx, dataset+"@"+snapshotName, node)
	}
	address := sm.controller.ResolveHost(node)

	sm.controller.logger.Info("Deleting snapshot",
		zap.String("volume", volume),
		zap.String("snapshot", snapshotName),
		zap.String("node", node))

	// Parse volume path and build snapshot LV path
	vg, _ := parseVolumePath(volume)
	snapshotPath := fmt.Sprintf("/dev/%s/%s", vg, snapshotName)

	// Remove snapshot
	cmd := fmt.Sprintf("sudo lvremove -f %s", snapshotPath)
	result, err := sm.controller.deployment.Exec(ctx, []string{address}, cmd)
	if err != nil {
		return fmt.Errorf("failed to delete snapshot: %w", err)
	}

	if !result.AllSuccess() {
		return fmt.Errorf("failed to delete snapshot: %s", result.FailureDetails())
	}

	sm.controller.logger.Info("Snapshot deleted successfully",
		zap.String("snapshot", snapshotName))

	return nil
}

// ListSnapshots lists snapshots for a volume
func (sm *SnapshotManager) ListSnapshots(ctx context.Context, volume, node string) ([]*SnapshotInfo, error) {
	if dataset, ok := sm.zfsVolumeDataset(ctx, volume); ok {
		snaps, err := sm.controller.storage.ZFSListSnapshots(ctx, dataset, node)
		for _, s := range snaps {
			s.Volume = volume
		}
		return snaps, err
	}
	// Parse volume path
	vg, lv := parseVolumePath(volume)
	address := sm.controller.ResolveHost(node)

	// List snapshots using lvs
	cmd := fmt.Sprintf("sudo lvs --noheadings --separator '|' -o lv_name,lv_size,origin %s", vg)
	result, err := sm.controller.deployment.Exec(ctx, []string{address}, cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to list snapshots: %w", err)
	}

	if !result.AllSuccess() {
		return nil, fmt.Errorf("failed to list snapshots: %s", result.FailureDetails())
	}

	var snapshots []*SnapshotInfo
	for _, r := range result.Hosts {
		if r.Success {
			lines := strings.Split(strings.TrimSpace(r.Output), "\n")
			for _, line := range lines {
				fields := strings.Split(line, "|")
				if len(fields) >= 3 {
					lvName := strings.TrimSpace(fields[0])
					origin := strings.TrimSpace(fields[2])
					// Check if this is a snapshot of the requested LV
					if origin == lv {
						sizeStr := strings.TrimSpace(fields[1])
						// Parse size (e.g., "4.00g" or "4.00G")
						sizeStr = strings.TrimSuffix(sizeStr, "g")
						sizeStr = strings.TrimSuffix(sizeStr, "G")
						sizeFloat, _ := strconv.ParseFloat(sizeStr, 64)
						snapshots = append(snapshots, &SnapshotInfo{
							Name:      lvName,
							Volume:    volume,
							SizeGB:    uint64(sizeFloat),
							CreatedAt: "",
						})
					}
				}
			}
		}
	}

	return snapshots, nil
}

// PopulateVolume copies sourceDevice into an already-created, still-empty DRBD
// resource. It is how a CSI restore-from-snapshot (and volume clone) gets data
// into a brand-new volume; RestoreSnapshot, by contrast, merges a snapshot back
// into its own origin in place.
//
// The copy is written to the target's DRBD device rather than its backing LV, so
// DRBD replicates every block to the peers as part of the write path — no
// separate per-replica copy, and no risk of replicas silently diverging.
//
// The caller MUST ensure the resource is newly created and not yet in use: this
// overwrites it from byte zero. node must hold a diskful replica of resource and
// have sourceDevice locally.
func (sm *SnapshotManager) PopulateVolume(ctx context.Context, resource string, volumeID uint32, sourceDevice, node string) (uint64, error) {
	if strings.TrimSpace(resource) == "" {
		return 0, fmt.Errorf("resource is required")
	}
	if strings.TrimSpace(sourceDevice) == "" {
		return 0, fmt.Errorf("source device is required")
	}
	address := sm.controller.ResolveHost(node)
	if address == "" {
		return 0, fmt.Errorf("node not found: %s", node)
	}

	target := fmt.Sprintf("/dev/drbd/by-res/%s/%d", resource, volumeID)

	sm.controller.logger.Info("Populating volume from source device",
		zap.String("resource", resource),
		zap.Uint32("volume_id", volumeID),
		zap.String("source", sourceDevice),
		zap.String("target", target),
		zap.String("node", node))

	// The target must be Primary to be writable. Promote before the copy and
	// demote afterwards so the new volume is left exactly as a freshly created
	// one would be (Secondary everywhere, ready for the node plugin to promote).
	if err := sm.controller.resources.SetPrimary(ctx, resource, node, false); err != nil {
		return 0, fmt.Errorf("promote %s on %s for populate: %w", resource, node, err)
	}
	defer func() {
		if err := sm.controller.resources.SetSecondary(ctx, resource, node); err != nil {
			sm.controller.logger.Warn("Failed to demote after populate; the volume is still usable but stays Primary",
				zap.String("resource", resource), zap.String("node", node), zap.Error(err))
		}
	}()

	// Copy exactly as many bytes as the DRBD device holds, not the whole source.
	//
	// With `meta-disk internal` a DRBD device is SMALLER than the backing volume
	// it sits on — the tail of that volume holds DRBD's own metadata. A snapshot
	// of the backing volume therefore contains [filesystem][DRBD metadata], and
	// blindly copying all of it into the (smaller) target device both overflows
	// it and would drag the source's metadata into the target's data area.
	// Bounding the copy by the target's size takes precisely the filesystem
	// region and leaves the target's own metadata untouched.
	//
	// conv=fsync forces the copy to reach stable storage (and, through DRBD, the
	// peers) before dd exits, so a later promote elsewhere cannot read stale
	// data. Errors are fatal: a partial copy must never look like success.
	//
	// When the source is a DRBD backing volume, its own data region bounds the
	// copy instead, and the target is grown first if it is the smaller of the
	// two (populate_size.go).
	copyBytes, err := sm.fitTargetToSource(ctx, resource, volumeID, address, target, sourceDevice)
	if err != nil {
		return 0, err
	}
	size := fmt.Sprintf("$(sudo blockdev --getsize64 %s)", target)
	if copyBytes > 0 {
		size = strconv.FormatUint(copyBytes, 10)
	}
	cmd := fmt.Sprintf(
		"set -e; %s SZ=%s; %s"+
			"sudo dd if=%s of=%s bs=4M count=$SZ iflag=fullblock,count_bytes oflag=direct conv=$CONV status=none; "+
			"sudo blockdev --flushbufs %s; echo $SZ",
		activateSnapshotCmd(sourceDevice), size,
		sparseWriteSetup(sm.controller.resources.zeroReadingReplicas(ctx, resource), target),
		sourceDevice, target, target)
	result, err := sm.controller.deployment.Exec(ctx, []string{address}, cmd)
	if err != nil {
		return 0, fmt.Errorf("copy %s -> %s on %s: %w", sourceDevice, target, node, err)
	}
	if !result.AllSuccess() {
		return 0, fmt.Errorf("copy %s -> %s on %s failed: %s", sourceDevice, target, node, result.FailureDetails())
	}

	var copied uint64
	if hr, ok := result.Hosts[address]; ok && hr != nil {
		if n, perr := strconv.ParseUint(strings.TrimSpace(hr.Output), 10, 64); perr == nil {
			copied = n
		}
	}

	sm.controller.logger.Info("Volume populated",
		zap.String("resource", resource),
		zap.String("source", sourceDevice),
		zap.Uint64("bytes", copied))

	return copied, nil
}

func parseVolumePath(volume string) (vg, lv string) {
	parts := strings.Split(volume, "/")
	if len(parts) >= 2 {
		return parts[0], parts[1]
	}
	return "", volume
}

// activateSnapshotCmd makes an LVM snapshot's device node exist before it is
// read. LVM creates thin snapshots with the activation-skip flag, so a thin
// snapshot has no /dev node until activated with -K; reading it failed with
// "No such file or directory". That made every backup, and every CSI restore
// from a snapshot, fail on thin pools. A no-op for a snapshot that is already
// active and for anything that is not an LV path.
func activateSnapshotCmd(device string) string {
	if !strings.HasPrefix(device, "/dev/") || strings.HasPrefix(device, "/dev/zvol/") ||
		strings.HasPrefix(device, "/dev/mapper/") || strings.HasPrefix(device, "/dev/drbd") {
		return ""
	}
	lv := strings.TrimPrefix(device, "/dev/")
	if strings.Count(lv, "/") != 1 {
		return ""
	}
	// Only when the node is missing: activating an already-active (thick)
	// snapshot makes lvchange ask whether to change its origin too, and the
	// unanswered prompt fails the command.
	return fmt.Sprintf("[ -e %s ] || sudo lvchange -ay -K %s;", device, lv)
}
