package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"go.uber.org/zap"
)

// drbdMetadataBytes returns the space DRBD's INTERNAL metadata takes off the end
// of a backing volume, so a caller can add it on top of the size the user asked
// for. Internal metadata lives on the same device as the data, so without this
// allowance a "4 GiB" volume exports a device slightly SMALLER than 4 GiB.
//
// DRBD's layout is a fixed superblock/activity-log area plus one bitmap per
// peer, at one bit per 4 KiB of data:
//
//	metadata = 36 KiB + (dataBytes / 32768) * peers
//
// Verified against a live 4 GiB 3-node resource: 36 KiB + 2 x 128 KiB = 292 KiB,
// matching the measured shortfall exactly.
//
// peers is sized generously (see minMetadataPeers) because bitmap slots are
// fixed at create-md time, while diskless clients may attach later.
func drbdMetadataBytes(dataBytes uint64, peers int) uint64 {
	if peers < minMetadataPeers {
		peers = minMetadataPeers
	}
	const (
		fixedOverhead = 36 * 1024 // superblock + activity log
		bitmapDivisor = 32768     // 1 bit per 4 KiB of data
	)
	meta := uint64(fixedOverhead) + (dataBytes/bitmapDivisor)*uint64(peers)

	// Round up to a 1 MiB boundary. LVM allocates in extents (4 MiB by default)
	// anyway, so the rounding is free in practice and absorbs any difference
	// between DRBD versions rather than leaving the volume a few KiB short.
	const mib = 1024 * 1024
	return ((meta + mib - 1) / mib) * mib
}

// minMetadataPeers is the floor for the bitmap-slot count used when sizing
// metadata. DRBD fixes the number of bitmap slots when metadata is created, but
// diskless clients (Proxmox/CSI hosts) attach to a resource later — so the
// allowance assumes room to grow instead of exactly today's peer count. The
// cost of being wrong upward is a few MiB per volume; being wrong downward
// means the volume is short again.
const minMetadataPeers = 7

// backingVolumeSizeArg returns the size to hand lvcreate/zfs for one volume:
// the requested size plus DRBD's internal-metadata allowance, so the DRBD
// device the guest actually sees is at least as big as the user asked for.
func backingVolumeSizeArg(sizeGB uint32, peers int) string {
	return fmt.Sprintf("%dB", backingVolumeSizeBytes(sizeGB, peers, false))
}

// backingVolumeSizeBytes is the same calculation with the crypt layer folded
// in. A LUKS2 header sits at the FRONT of the device and is not part of the
// mapping DRBD sees, so an encrypted volume has to be that much larger for the
// DRBD device to come out the size that was asked for — the same shortfall
// drbdMetadataBytes exists to absorb, from the other end of the device.
func backingVolumeSizeBytes(sizeGB uint32, peers int, encrypted bool) uint64 {
	data := uint64(sizeGB) * 1024 * 1024 * 1024
	// LVM/ZFS accept byte suffixes; using bytes avoids rounding the allowance
	// away by expressing the total in whole gigabytes.
	total := data + drbdMetadataBytes(data, peers)
	if encrypted {
		total += luksHeaderBytes
	}
	zap.L().Debug("backingVolumeSizeBytes: sized backing volume", zap.Uint32("size_gb", sizeGB), zap.Int("peers", peers),
		zap.Bool("encrypted", encrypted), zap.Uint64("size_bytes", total))
	return total
}

// createBackingVolume creates one volume's backing storage (ZFS zvol, LVM thin
// LV or plain LVM LV per storageType) on every diskful node. nodeIPs and nodes
// are parallel (IP for the command, name for error messages).
//
// It is IDEMPOTENT: a volume that already exists at a sufficient size counts as
// success. That matters because creation is not atomic — it walks the nodes one
// at a time, so a failure on the third node leaves volumes behind on the first
// two. Without this, the rollback's best-effort lvremove missing even one node
// would make every later attempt at the same name fail with "already exists",
// permanently blocking that volume (the same class of trap as a
// partially-applied resize).
//
// When encrypt is set each node's volume is wrapped in its own LUKS2 container
// and left open, so what the caller ends up with is /dev/mapper/<container>
// rather than the LV itself. The container is built per node, immediately after
// that node's volume, so a failure anywhere leaves at most one half-encrypted
// volume for the rollback to collect rather than a set of them.
func (rm *ResourceManager) createBackingVolume(ctx context.Context, nodeIPs, nodes []string, storageType, pool, volumeName string, sizeGB uint32, encrypt bool) error {
	log := rm.resourcesLog()
	size := fmt.Sprintf("%dB", backingVolumeSizeBytes(sizeGB, len(nodeIPs)-1, encrypt))
	log.Debug("createBackingVolume: start", zap.Strings("hosts", nodeIPs), zap.Strings("nodes", nodes),
		zap.String("storage_type", storageType), zap.String("pool", pool), zap.String("volume", volumeName),
		zap.Uint32("size_gb", sizeGB), zap.String("size", size), zap.Bool("encrypt", encrypt))
	for i, nodeIP := range nodeIPs {
		var result *deployment.ExecResult
		var err error
		start := time.Now()
		switch storageType {
		case "zfs", "zfs-thin":
			result, err = rm.deployment.ZFSCreateThinDataset(ctx, []string{nodeIP}, pool, volumeName, size)
			log.Debug("createBackingVolume: created zvol", zap.String("node", nodes[i]), zap.String("host", nodeIP),
				zap.String("pool", pool), zap.String("volume", volumeName), zap.Array("failed_hosts", resourcesFailedHosts{result}),
				zap.Duration("took", time.Since(start)), zap.Error(err))
		default:
			// The shape comes from the node, not from storageType.
			//
			// --storage-type defaults to "lvm" and nothing reconciles it with the
			// pool it names, so a resource created in a thin pool without the flag
			// used to attempt a thick lvcreate in a volume group the thin pool had
			// already consumed. That fails on free space — an error naming the
			// symptom and not the cause — and it is unfixable by construction: a
			// full-VG thin pool leaves nothing for a thick LV to take. add-replica
			// and add-dr already ask the node (see createBackingVolumeOn); this is
			// the same question, so that a replica added later has the same shape
			// as the one creation made.
			//
			// The thin pool's name is asked for too, not assumed: `pool create`
			// builds "<pool>_thin" but converting a thick pool in place builds a
			// differently named one, and guessing fails on those nodes.
			thinPool, perr := rm.deployment.LVThinPoolIn(ctx, nodeIP, pool)
			log.Debug("createBackingVolume: looked for thin pool", zap.String("node", nodes[i]), zap.String("pool", pool),
				zap.String("thin_pool", thinPool), zap.Duration("took", time.Since(start)), zap.Error(perr))
			if perr != nil {
				return fmt.Errorf("look for a thin pool in %s on %s: %w", pool, nodes[i], perr)
			}
			start = time.Now()
			if thinPool == "" {
				if storageType == "lvm-thin" {
					log.Debug("createBackingVolume: lvm-thin pool has no thin pool", zap.String("node", nodes[i]), zap.String("pool", pool))
					return fmt.Errorf("pool %s on %s is recorded as lvm-thin but has no thin pool", pool, nodes[i])
				}
				result, err = rm.deployment.LVCreate(ctx, []string{nodeIP}, pool, volumeName, size)
				log.Debug("createBackingVolume: created thick LV", zap.String("node", nodes[i]), zap.String("host", nodeIP),
					zap.String("pool", pool), zap.String("volume", volumeName), zap.Array("failed_hosts", resourcesFailedHosts{result}),
					zap.Duration("took", time.Since(start)), zap.Error(err))
				break
			}
			result, err = rm.deployment.LVCreateThinVolume(ctx, []string{nodeIP}, pool, thinPool, volumeName, size)
			log.Debug("createBackingVolume: created thin LV", zap.String("node", nodes[i]), zap.String("host", nodeIP),
				zap.String("pool", pool), zap.String("thin_pool", thinPool), zap.String("volume", volumeName),
				zap.Array("failed_hosts", resourcesFailedHosts{result}), zap.Duration("took", time.Since(start)), zap.Error(err))
		}
		if err != nil {
			return fmt.Errorf("failed to create backing volume %s/%s on %s: %w", pool, volumeName, nodes[i], err)
		}
		if !result.AllSuccess() {
			for host, hres := range result.Hosts {
				if hres.Success {
					continue
				}
				log.Debug("createBackingVolume: creation failed on host", zap.String("host", host), zap.String("volume", volumeName),
					zap.String("output", strings.TrimSpace(hres.Output)))
				// Tolerate a leftover from a previous failed attempt, but only
				// once we have confirmed it is actually big enough to hold what
				// was asked for — a stale SMALLER volume must still be an error
				// rather than silently handing the caller a short device.
				if storageType != "zfs" && storageType != "zfs-thin" &&
					strings.Contains(strings.ToLower(hres.Output), "already exists") {
					ok, verr := rm.backingVolumeAtLeast(ctx, host, pool, volumeName, sizeGB)
					if verr == nil && ok {
						rm.controller.logger.Info("Reusing existing backing volume from a previous attempt",
							zap.String("volume", pool+"/"+volumeName),
							zap.String("host", host))
						continue
					}
					return fmt.Errorf("backing volume %s/%s already exists on %s but is too small to reuse; remove it and retry",
						pool, volumeName, host)
				}
				return fmt.Errorf("backing volume %s/%s creation failed on %s: %s", pool, volumeName, host, hres.Output)
			}
		}
		if encrypt {
			log.Debug("createBackingVolume: encrypting backing volume", zap.String("node", nodes[i]), zap.String("volume", volumeName))
			if err := rm.encryptBackingVolumeOn(ctx, nodeIP, nodes[i], pool, volumeName,
				backingPathForVolume(pool, volumeName, storageType)); err != nil {
				log.Debug("createBackingVolume: encryption failed", zap.String("node", nodes[i]), zap.String("volume", volumeName), zap.Error(err))
				return err
			}
		}
	}
	log.Debug("createBackingVolume: created on every node", zap.String("pool", pool), zap.String("volume", volumeName),
		zap.Int("count", len(nodeIPs)))
	return nil
}

// backingVolumeAtLeast reports whether an existing logical volume is big enough
// to back a volume of sizeGB (data plus DRBD metadata).
func (rm *ResourceManager) backingVolumeAtLeast(ctx context.Context, host, pool, volumeName string, sizeGB uint32) (bool, error) {
	log := rm.resourcesLog()
	cmd := fmt.Sprintf("sudo lvs --noheadings --nosuffix --units b -o lv_size %s/%s", pool, volumeName)
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, []string{host}, cmd)
	log.Debug("backingVolumeAtLeast: read LV size", zap.String("host", host), zap.String("pool", pool), zap.String("volume", volumeName),
		zap.Array("failed_hosts", resourcesFailedHosts{res}), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return false, err
	}
	hres := res.Hosts[host]
	if hres == nil || !hres.Success {
		return false, fmt.Errorf("could not read size of %s/%s on %s", pool, volumeName, host)
	}
	have, perr := strconv.ParseUint(strings.TrimSpace(hres.Output), 10, 64)
	if perr != nil {
		log.Debug("backingVolumeAtLeast: unparseable size", zap.String("host", host), zap.Error(perr))
		return false, perr
	}
	ok := have >= uint64(sizeGB)*1024*1024*1024
	log.Debug("backingVolumeAtLeast: compared size", zap.String("host", host), zap.String("volume", volumeName),
		zap.Uint64("size_bytes", have), zap.Uint32("want_gb", sizeGB), zap.Bool("big_enough", ok))
	return ok, nil
}

// autoSelectPool returns the single registered pool name, or an error when
// the choice would be ambiguous (zero or multiple distinct pool names).
func (rm *ResourceManager) autoSelectPool(ctx context.Context) (string, error) {
	log := rm.resourcesLog()
	if rm.controller.db == nil {
		log.Debug("autoSelectPool: no database")
		return "", fmt.Errorf("no pool specified and database not available for auto-selection")
	}
	pools, err := rm.controller.db.ListPools(ctx)
	log.Debug("autoSelectPool: listed pools", zap.Int("count", len(pools)), zap.Error(err))
	if err != nil {
		return "", fmt.Errorf("no pool specified and pool lookup failed: %w", err)
	}
	names := make(map[string]bool)
	for _, p := range pools {
		names[p.Name] = true
	}
	if len(names) == 1 {
		for name := range names {
			log.Debug("autoSelectPool: single pool chosen", zap.String("pool", name))
			return name, nil
		}
	}
	if len(names) == 0 {
		log.Debug("autoSelectPool: no pools registered")
		return "", fmt.Errorf("no pool specified and no pools are registered; create one with 'pool create'")
	}
	choices := make([]string, 0, len(names))
	for name := range names {
		choices = append(choices, name)
	}
	log.Debug("autoSelectPool: ambiguous", zap.Strings("pools", choices))
	return "", fmt.Errorf("no pool specified and multiple pools exist (%v); pass --pool", choices)
}

// deleteBackingVolume removes a volume's backing LV or zvol on all hosts.
// The device path recorded at creation time identifies the storage type:
// "/dev/zvol/<pool>/<vol>" is ZFS, "/dev/<pool>/<vol>" is LVM.
func (rm *ResourceManager) deleteBackingVolume(ctx context.Context, hosts []string, volume *database.Volume) error {
	log := rm.resourcesLog()
	log.Debug("deleteBackingVolume: start", zap.String("resource", volume.ResourceName), zap.String("pool", volume.Pool),
		zap.String("volume", volume.VolumeName), zap.String("device", volume.Device), zap.Strings("hosts", hosts))
	if volume.Pool == "" || volume.VolumeName == "" {
		log.Debug("deleteBackingVolume: record incomplete", zap.String("resource", volume.ResourceName))
		return fmt.Errorf("volume record incomplete (pool=%q, volume=%q)", volume.Pool, volume.VolumeName)
	}
	// An encrypted volume's container has to go first, for two reasons: an open
	// container holds the LV so lvremove refuses, and the key is the only thing
	// making the ciphertext left in the freed extents unreadable. Destroying it
	// before the storage is released is what turns "deleted" into "gone".
	if luksIsMapperPath(volume.Device) && rm.resourceIsEncrypted(ctx, volume.ResourceName) {
		log.Debug("deleteBackingVolume: closing encrypted container first", zap.String("volume", volume.VolumeName))
		rm.closeBackingVolumeOn(ctx, hosts, volume.Pool, volume.VolumeName)
	}

	var cmd string
	if strings.HasPrefix(volume.Device, "/dev/zvol/") {
		// -r takes the zvol's own snapshots with it, as the LVM branch removes
		// an LV's; without it a zvol with any snapshot outlives its resource.
		// Not -R: that would also destroy datasets cloned from those
		// snapshots, which can belong to other resources.
		cmd = fmt.Sprintf("sudo zfs destroy -r %s/%s", volume.Pool, volume.VolumeName)
	} else {
		// Two things block removal of the origin LV: (1) any LVM snapshot of it
		// (e.g. scheduled snapshots) must go first, or lvremove reports the
		// origin "is used by another device"; (2) a DRBD minor may still hold
		// it, and by this point the config may be gone so drbdadm cannot help —
		// drbdsetup operates on kernel state directly. Remove snapshots, then
		// the origin, releasing the minor on the retry.
		cmd = fmt.Sprintf("for s in $(sudo lvs --noheadings -o lv_name -S origin=%s %s 2>/dev/null); do sudo lvremove -f %s/$s; done; "+
			"sudo lvremove -f %s/%s || { sudo drbdsetup down %s 2>/dev/null; sudo lvremove -f %s/%s; }",
			volume.VolumeName, volume.Pool, volume.Pool,
			volume.Pool, volume.VolumeName, volume.ResourceName, volume.Pool, volume.VolumeName)
	}
	start := time.Now()
	result, err := rm.deployment.Exec(ctx, hosts, cmd)
	log.Debug("deleteBackingVolume: removed storage", zap.String("volume", volume.VolumeName),
		zap.Bool("zfs", strings.HasPrefix(volume.Device, "/dev/zvol/")), zap.Strings("hosts", hosts),
		zap.Array("failed_hosts", resourcesFailedHosts{result}), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return err
	}
	if !result.AllSuccess() {
		// Tolerate hosts where the volume is already gone.
		for host, hr := range result.Hosts {
			if !hr.Success &&
				!strings.Contains(hr.Output, "not found") &&
				!strings.Contains(hr.Output, "does not exist") {
				log.Debug("deleteBackingVolume: removal failed", zap.String("host", host), zap.String("volume", volume.VolumeName))
				return fmt.Errorf("removal failed on %s: %s", host, strings.TrimSpace(hr.Output))
			}
			if !hr.Success {
				log.Debug("deleteBackingVolume: already gone", zap.String("host", host), zap.String("volume", volume.VolumeName))
			}
		}
	}
	return nil
}
