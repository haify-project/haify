package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// How much of a source PopulateVolume copies, and how big the target must be
// to take it.
//
// A source is usually a DRBD backing volume (a template's, for a Proxmox
// clone) or a snapshot of one (CSI restore). Its data region is what its DRBD
// device held, and that is not the size its volume was asked for: the backing
// volume carries a metadata allowance for more peers than it has, so the
// device comes out a few MiB larger, and a guest that grows its partition to
// the end of the disk uses those MiB. A clone made at the asked-for size and
// filled "up to the target's size" lost them — its partition table pointed
// past the end of the disk, and it did not boot.
//
// DRBD records the device's size in its own metadata at the end of the
// backing volume (la-size), snapshots included, so the data region is read
// from there rather than guessed.

// unusedDRBDMinor is the minor drbdmeta is handed to read metadata from a
// device DRBD is not running on. It only names the device in drbdmeta's
// checks; the highest minor DRBD accepts is the one no resource uses.
const unusedDRBDMinor = 1048575

// sourceDataBytesCmd prints the sector count of the DRBD device whose
// metadata sits at the end of device, or nothing when there is none.
func sourceDataBytesCmd(device string) string {
	return fmt.Sprintf(
		"%s sudo drbdmeta --force %d v09 %s internal dump-md 2>/dev/null | sed -n 's/^la-size-sect \\([0-9]*\\);.*/\\1/p' | head -1",
		activateSnapshotCmd(device), unusedDRBDMinor, device)
}

// parseLaSizeSectors reads sourceDataBytesCmd's output as bytes; 0 when the
// device holds no DRBD metadata.
func parseLaSizeSectors(out string) uint64 {
	sectors, err := strconv.ParseUint(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0
	}
	return sectors * sectorBytes
}

// fitTargetToSource returns how many bytes of sourceDevice to copy (0: no
// DRBD metadata on it, so the target's own size bounds the copy), growing the
// still-empty target first when the source's data region is larger.
func (sm *SnapshotManager) fitTargetToSource(ctx context.Context, resource string, volumeID uint32, address, target, sourceDevice string) (uint64, error) {
	log := loggerOfController(sm.controller)
	log.Debug("fitTargetToSource: reading source data size", zap.String("resource", resource), zap.Uint32("volume", volumeID),
		zap.String("host", address), zap.String("target", target), zap.String("source", sourceDevice))
	dep := sm.controller.deployment
	start := time.Now()
	res, err := dep.Exec(ctx, []string{address}, sourceDataBytesCmd(sourceDevice))
	log.Debug("fitTargetToSource: drbdmeta dump-md la-size", zap.String("host", address), zap.String("source", sourceDevice),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || res.Hosts[address] == nil {
		log.Debug("fitTargetToSource: source size unknown, target size bounds the copy")
		return 0, nil
	}
	data := parseLaSizeSectors(res.Hosts[address].Output)
	if data == 0 {
		log.Debug("fitTargetToSource: source holds no DRBD metadata", zap.String("source", sourceDevice))
		return 0, nil
	}
	start = time.Now()
	res, err = dep.Exec(ctx, []string{address}, "sudo blockdev --getsize64 "+target)
	log.Debug("fitTargetToSource: blockdev --getsize64", zap.String("host", address), zap.String("target", target),
		zap.Bool("success", err == nil && res.AllSuccess()), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || !res.AllSuccess() {
		return 0, fmt.Errorf("read the size of %s on %s: %v", target, address, err)
	}
	have, err := strconv.ParseUint(strings.TrimSpace(res.Hosts[address].Output), 10, 64)
	if err != nil {
		log.Debug("fitTargetToSource: target size unparsable", zap.String("target", target), zap.Error(err))
		return 0, fmt.Errorf("read the size of %s on %s: %q", target, address, res.Hosts[address].Output)
	}
	log.Debug("fitTargetToSource: sizes", zap.Uint64("source_bytes", data), zap.Uint64("target_bytes", have),
		zap.Bool("grow", data > have))
	if data > have {
		sm.controller.logger.Info("Growing the target to hold the whole source",
			zap.String("resource", resource), zap.Uint64("source_bytes", data), zap.Uint64("target_bytes", have))
		start = time.Now()
		err := sm.controller.resources.ResizeVolumeBytes(ctx, resource, volumeID, data, false)
		log.Debug("fitTargetToSource: grew target", zap.String("resource", resource), zap.Uint64("size_bytes", data),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return 0, fmt.Errorf("%s holds %d bytes but %s is %d; growing it failed: %w", sourceDevice, data, resource, have, err)
		}
	}
	return data, nil
}
