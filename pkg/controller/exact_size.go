package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Byte-exact volume sizes.
//
// Volumes are allocated in whole GiB, which is fine for a new disk and wrong
// for one that has to match another byte for byte: PVE's online Move Disk
// and mirroring a disk image both refuse a target that is not exactly the
// source's size. So a volume can be given an exact size. Its backing volume
// is still allocated in whole GiB (plus DRBD's metadata), and the DRBD device
// is capped at the exact size with the `size` disk option — the guest sees
// exactly what was asked for and the slack stays unused under it.
//
// The size is kept in the resource's config file on every participant, not
// only applied at runtime: `drbdadm adjust` brings a device to what the file
// says, and would undo a runtime-only size on the next repair.

const sectorBytes = 512

// volumeSize resolves a VolumeSpec into the backing size in GiB and the exact
// device size in bytes (zero for a whole-GiB volume).
func volumeSize(v VolumeSpec) (uint32, uint64, error) {
	log := zap.L()
	if v.SizeBytes == 0 {
		if v.SizeGB == 0 {
			log.Debug("volumeSize: no size given")
			return 0, 0, fmt.Errorf("size must be greater than 0 GB")
		}
		log.Debug("volumeSize: whole-GiB volume", zap.Uint32("size_gb", v.SizeGB))
		return v.SizeGB, 0, nil
	}
	exact := roundUpSectors(v.SizeBytes)
	gb := uint32((exact + 1<<30 - 1) >> 30)
	if v.SizeGB != 0 && uint64(v.SizeGB)<<30 < exact {
		log.Debug("volumeSize: exact size does not fit the given GiB", zap.Uint64("size_bytes", v.SizeBytes),
			zap.Uint32("size_gb", v.SizeGB))
		return 0, 0, fmt.Errorf("size of %d bytes does not fit in %d GB", v.SizeBytes, v.SizeGB)
	}
	if v.SizeGB > gb {
		gb = v.SizeGB
	}
	log.Debug("volumeSize: exact-size volume", zap.Uint64("size_bytes", exact), zap.Uint32("backing_gb", gb))
	return gb, exact, nil
}

func roundUpSectors(b uint64) uint64 {
	return (b + sectorBytes - 1) / sectorBytes * sectorBytes
}

// drbdSizeSectors renders a size for DRBD's `size` option and --size, in
// 512-byte sectors.
func drbdSizeSectors(b uint64) string {
	return fmt.Sprintf("%ds", roundUpSectors(b)/sectorBytes)
}

var volumeBlockStart = regexp.MustCompile(`^\s*volume\s+(\d+)\s*\{`)

// setVolumeSizeInConfig sets the `size` disk option of the resource-level
// volume block volumeID, adding a disk section when it has none. Volume
// blocks inside `on <node>` sections (a diskless node's override) are left
// alone: the size belongs to the data, not to one node.
func setVolumeSizeInConfig(conf string, volumeID int, b uint64) (string, error) {
	lines := strings.Split(conf, "\n")
	depth, volStart, volDepth := 0, -1, 0
	diskOpen, sizeLine := -1, -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if volStart < 0 && depth == 1 {
			if m := volumeBlockStart.FindStringSubmatch(line); m != nil && m[1] == fmt.Sprint(volumeID) {
				volStart, volDepth = i, depth+1
			}
		}
		if volStart >= 0 && depth == volDepth {
			if strings.HasPrefix(trimmed, "disk") && strings.HasSuffix(trimmed, "{") {
				diskOpen = i
			}
		}
		if volStart >= 0 && diskOpen >= 0 && depth == volDepth+1 && strings.HasPrefix(trimmed, "size ") {
			sizeLine = i
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if volStart >= 0 && i > volStart && depth < volDepth {
			// The closing brace of the volume block.
			entry := "            size " + drbdSizeSectors(b) + ";"
			switch {
			case sizeLine >= 0:
				zap.L().Debug("setVolumeSizeInConfig: replacing size line", zap.Int("volume", volumeID), zap.Uint64("size_bytes", b))
				lines[sizeLine] = entry
			case diskOpen >= 0:
				zap.L().Debug("setVolumeSizeInConfig: adding size to disk section", zap.Int("volume", volumeID), zap.Uint64("size_bytes", b))
				lines = append(lines[:diskOpen+1], append([]string{entry}, lines[diskOpen+1:]...)...)
			default:
				zap.L().Debug("setVolumeSizeInConfig: adding disk section with size", zap.Int("volume", volumeID), zap.Uint64("size_bytes", b))
				block := []string{"        disk {", entry, "        }"}
				lines = append(lines[:i], append(block, lines[i:]...)...)
			}
			return strings.Join(lines, "\n"), nil
		}
	}
	zap.L().Debug("setVolumeSizeInConfig: volume block not found", zap.Int("volume", volumeID))
	return "", fmt.Errorf("volume %d not found in the resource config", volumeID)
}

// exactResizeBytes decides the exact size a resize sets: the one asked for,
// or — for a volume that already has one and was resized in whole GiB — that
// many GiB exactly, so the config's size never caps the device below what was
// grown. A shrink is refused.
func (rm *ResourceManager) exactResizeBytes(ctx context.Context, resource string, volumeID uint32, newSizeGB, exact uint64) (uint64, error) {
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.exactResizeBytes: deciding", zap.String("resource", resource), zap.Uint32("volume", volumeID),
		zap.Uint64("new_size_gb", newSizeGB), zap.Uint64("exact_bytes", exact))
	var current uint64
	if rm.controller.db != nil {
		start := time.Now()
		vols, err := rm.controller.db.ListVolumes(ctx, resource)
		log.Debug("ResourceManager.exactResizeBytes: db list volumes", zap.String("resource", resource),
			zap.Int("count", len(vols)), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err == nil {
			if v := findVolumeRecord(vols, volumeID); v != nil {
				current = uint64(max(v.SizeBytes, 0))
				if current == 0 {
					current = uint64(max(v.SizeGB, 0)) << 30
				}
				if exact == 0 && v.SizeBytes > 0 {
					log.Debug("ResourceManager.exactResizeBytes: exact-size volume grown in GiB, keeping it exact",
						zap.Uint64("exact_bytes", newSizeGB<<30))
					exact = newSizeGB << 30
				}
			}
		}
	}
	if exact == 0 {
		log.Debug("ResourceManager.exactResizeBytes: whole-GiB resize, no exact size")
		return 0, nil
	}
	exact = roundUpSectors(exact)
	if exact > newSizeGB<<30 {
		log.Debug("ResourceManager.exactResizeBytes: exact size does not fit", zap.Uint64("exact_bytes", exact))
		return 0, fmt.Errorf("%d bytes does not fit in %d GB", exact, newSizeGB)
	}
	if exact < current {
		log.Debug("ResourceManager.exactResizeBytes: shrink refused", zap.Uint64("current_bytes", current), zap.Uint64("exact_bytes", exact))
		return 0, fmt.Errorf("%s/%d is %d bytes; shrinking it to %d is not supported", resource, volumeID, current, exact)
	}
	log.Debug("ResourceManager.exactResizeBytes: decided", zap.Uint64("current_bytes", current), zap.Uint64("exact_bytes", exact))
	return exact, nil
}

// ResizeVolumeBytes grows a volume to exactly sizeBytes.
func (rm *ResourceManager) ResizeVolumeBytes(ctx context.Context, resource string, volumeID uint32, sizeBytes uint64, ignoreFreeSpace bool) error {
	exact := roundUpSectors(sizeBytes)
	gb := (exact + 1<<30 - 1) >> 30
	rm.controller.traceLogger().Debug("ResourceManager.ResizeVolumeBytes: resizing to exact size", zap.String("resource", resource),
		zap.Uint32("volume", volumeID), zap.Uint64("size_bytes", exact), zap.Uint64("size_gb", gb),
		zap.Bool("ignore_free_space", ignoreFreeSpace))
	return rm.resizeVolume(ctx, resource, volumeID, gb, exact, ignoreFreeSpace)
}
