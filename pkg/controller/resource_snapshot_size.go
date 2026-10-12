package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// A volume grown after a snapshot of it was taken.
//
// Rolling back merges the snapshot into the backing LV, which takes the LV
// back to the snapshot's size, with DRBD's internal metadata at its end. The
// resource's config still carries the grown size (`size` in the volume's disk
// section, for a volume of an exact size), and DRBD refuses to attach a
// backing device smaller than that: the resource stays down on every replica.
//
// So for such a volume the size is taken out of the config before the
// resource comes back up, DRBD attaches at the size the snapshot's metadata
// recorded, and the volume is then grown back the way a resize grows it. The
// volume keeps the size it had, with the snapshot's content, which is what a
// hypervisor that sized the disk in its own config expects.

// grownVolume is a volume to grow back to its size after a rollback.
type grownVolume struct {
	volumeID uint32
	sizeGB   uint64
	exact    uint64
}

// volumesGrownSinceSnapshot lists the LVM volumes whose backing LV on host is
// larger than snapshot name of it. A size that cannot be read counts as not
// grown: the rollback then behaves as it always did.
func (rm *ResourceManager) volumesGrownSinceSnapshot(ctx context.Context, targets []snapTarget, host, name string) []grownVolume {
	log := rm.resourcesLog()
	var out []grownVolume
	for _, t := range targets {
		if t.zfs {
			log.Debug("volumesGrownSinceSnapshot: zvol; not checked", zap.String("volume", t.vol.VolumeName))
			continue
		}
		cur, err1 := rm.lvSizeBytes(ctx, host, t.vol.Pool+"/"+t.vol.VolumeName)
		snap, err2 := rm.lvSizeBytes(ctx, host, t.vol.Pool+"/"+t.lvName(name))
		grown := err1 == nil && err2 == nil && snap < cur
		log.Debug("volumesGrownSinceSnapshot: compared sizes", zap.String("volume", t.vol.VolumeName), zap.String("snapshot", name),
			zap.Uint64("size_bytes", cur), zap.Uint64("snapshot_bytes", snap), zap.Bool("grown", grown),
			zap.NamedError("volume_error", err1), zap.NamedError("snapshot_error", err2))
		if !grown {
			continue
		}
		out = append(out, grownVolume{
			volumeID: uint32(max(t.vol.VolumeID, 0)),
			sizeGB:   uint64(max(t.vol.SizeGB, 0)),
			exact:    uint64(max(t.vol.SizeBytes, 0)),
		})
	}
	return out
}

func (rm *ResourceManager) lvSizeBytes(ctx context.Context, host, lv string) (uint64, error) {
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, []string{host}, "sudo lvs --noheadings --units b --nosuffix -o lv_size "+lv)
	rm.resourcesLog().Debug("lvSizeBytes: read LV size", zap.String("host", host), zap.String("lv", lv),
		zap.Array("failed_hosts", resourcesFailedHosts{res}), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return 0, err
	}
	if !res.AllSuccess() {
		return 0, fmt.Errorf("lvs %s: %s", lv, res.FailureDetails())
	}
	for _, hr := range res.Hosts {
		if hr != nil {
			return strconv.ParseUint(strings.TrimSpace(hr.Output), 10, 64)
		}
	}
	return 0, fmt.Errorf("lvs %s: no output", lv)
}

// clearGrownSizes takes the exact size of every grown volume out of the
// resource's config, so the merged, smaller backing LV attaches.
func (rm *ResourceManager) clearGrownSizes(ctx context.Context, resource string, grown []grownVolume) error {
	exact := false
	for _, g := range grown {
		exact = exact || g.exact > 0
	}
	log := rm.resourcesLog()
	if !exact {
		log.Debug("clearGrownSizes: no exact sizes to clear", zap.String("resource", resource), zap.Int("grown", len(grown)))
		return nil
	}
	_, _, err := rm.stageResourceConfig(ctx, resource, func(conf string) (string, error) {
		for _, g := range grown {
			if g.exact == 0 {
				continue
			}
			var err error
			if conf, err = clearVolumeSizeInConfig(conf, int(g.volumeID)); err != nil {
				return "", err
			}
		}
		return conf, nil
	})
	log.Debug("clearGrownSizes: cleared exact sizes from config", zap.String("resource", resource),
		zap.Int("grown", len(grown)), zap.Error(err))
	return err
}

// clearVolumeSizeInConfig removes the `size` line of a volume's disk section.
func clearVolumeSizeInConfig(conf string, volumeID int) (string, error) {
	// setVolumeSizeInConfig finds the line; a size no device has marks it.
	const marker = uint64(1) << 62
	marked, err := setVolumeSizeInConfig(conf, volumeID, marker)
	if err != nil {
		return "", err
	}
	entry := "size " + drbdSizeSectors(marker) + ";"
	lines := strings.Split(marked, "\n")
	out := lines[:0]
	for _, line := range lines {
		if strings.TrimSpace(line) != entry {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n"), nil
}

// growBack grows the rolled-back volumes to the sizes they had, once every
// replica is connected again: DRBD settles a resize between connected peers.
func (rm *ResourceManager) growBack(ctx context.Context, resource string, hosts []string, grown []grownVolume) error {
	log := rm.resourcesLog()
	if len(grown) == 0 {
		log.Debug("growBack: nothing to grow back", zap.String("resource", resource))
		return nil
	}
	start := time.Now()
	if err := rm.execAllSuccess(ctx, hosts, "sudo drbdsetup wait-connect-resource --wfc-timeout=60 "+resource,
		"wait for the replicas of "+resource+" to connect"); err != nil {
		log.Debug("growBack: replicas did not connect", zap.String("resource", resource), zap.Duration("took", time.Since(start)), zap.Error(err))
		return err
	}
	log.Debug("growBack: replicas connected", zap.String("resource", resource), zap.Duration("took", time.Since(start)))
	for _, g := range grown {
		log.Debug("growBack: growing volume", zap.String("resource", resource), zap.Uint32("volume_id", g.volumeID),
			zap.Uint64("size_gb", g.sizeGB), zap.Uint64("size_bytes", g.exact))
		if err := rm.resizeVolume(ctx, resource, g.volumeID, g.sizeGB, g.exact, true); err != nil {
			return fmt.Errorf("%s/%d is back at its snapshot's size; growing it to %d GiB again failed (haify "+
				"resource resize grows it): %w", resource, g.volumeID, g.sizeGB, err)
		}
		rm.controller.logger.Info("Grew a rolled-back volume to the size it had",
			zap.String("resource", resource), zap.Uint32("volume_id", g.volumeID), zap.Uint64("size_gb", g.sizeGB))
	}
	return nil
}
