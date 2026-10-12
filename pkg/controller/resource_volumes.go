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

// AddVolume adds a volume to an existing DRBD resource
func (rm *ResourceManager) AddVolume(ctx context.Context, resource, volume, pool string, sizeGB uint32) error {
	rm.controller.logger.Info("Adding volume to resource",
		zap.String("resource", resource),
		zap.String("volume", volume),
		zap.String("pool", pool),
		zap.Uint32("size_gb", sizeGB))

	log := rm.resourcesLog()
	if rm.deployment == nil {
		log.Debug("AddVolume: no deployment client", zap.String("resource", resource))
		return fmt.Errorf("deployment client not set")
	}

	// Auto-select the pool when none was given: with exactly one registered
	// pool name the choice is unambiguous; otherwise the caller must decide.
	// (A hardcoded fallback name here used to send lvcreate at a volume
	// group that doesn't exist.)
	if pool == "" {
		selected, err := rm.autoSelectPool(ctx)
		if err != nil {
			log.Debug("AddVolume: pool auto-selection failed", zap.String("resource", resource), zap.Error(err))
			return err
		}
		pool = selected
	}
	pool = normalizeManagedName(pool)

	// A volume added to an encrypted resource is encrypted too. Anything else
	// would put plaintext on the pool disks of a resource whose whole point is
	// that it does not — and would do it invisibly, since nothing in the DRBD
	// config makes one volume's crypt layer more visible than another's.
	encrypt := false
	if rm.controller.db != nil {
		dbRes, derr := rm.controller.db.GetResource(ctx, resource)
		if derr == nil && dbRes != nil {
			encrypt = dbRes.Encrypted
		}
	}
	log.Debug("AddVolume: resolved pool and encryption", zap.String("resource", resource), zap.String("pool", pool), zap.Bool("encrypt", encrypt))
	if encrypt {
		if err := validateLUKSNames(pool, volume); err != nil {
			log.Debug("AddVolume: names unusable for LUKS", zap.String("volume", volume), zap.Error(err))
			return err
		}
		if err := rm.assertEncryptableVDO(ctx, pool); err != nil {
			log.Debug("AddVolume: pool not encryptable", zap.String("pool", pool), zap.Error(err))
			return err
		}
	}

	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		log.Debug("AddVolume: no hosts", zap.String("resource", resource), zap.Error(err))
		return err
	}
	if err := rm.assertQuotaForResource(ctx, resource, pool, hosts, uint64(sizeGB)); err != nil {
		log.Debug("AddVolume: quota refuses", zap.String("resource", resource), zap.Error(err))
		return err
	}
	// Tiebreakers and diskless clients carry the resource config too. They get
	// no LV and no metadata, but the new volume has to appear in their copy —
	// as `disk none` — or DRBD refuses their connection. See
	// addDisklessVolumeOverrides.
	allHosts := append(append([]string(nil), hosts...), rm.disklessParticipantHosts(ctx, resource)...)

	// Get current config to find next volume number and minor
	t0 := time.Now()
	result, err := rm.deployment.Exec(ctx, []string{hosts[0]}, fmt.Sprintf("cat /etc/drbd.d/%s.res", resource))
	log.Debug("AddVolume: read config", zap.String("resource", resource), zap.String("host", hosts[0]),
		zap.Duration("took", time.Since(t0)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	var hostResult *deployment.HostResult
	for _, r := range result.Hosts {
		hostResult = r
		break
	}

	if hostResult == nil || !hostResult.Success {
		log.Debug("AddVolume: config read failed", zap.String("resource", resource))
		return fmt.Errorf("failed to get config")
	}

	// Volume numbers are scoped to this resource's config.
	maxVolNum := -1
	lines := strings.Split(hostResult.Output, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "volume ") {
			parts := strings.Fields(trimmed)
			if len(parts) >= 2 {
				if volNum, err := strconv.Atoi(strings.TrimSuffix(parts[1], "{")); err == nil {
					if volNum > maxVolNum {
						maxVolNum = volNum
					}
				}
			}
		}
	}
	newVolNum := maxVolNum + 1

	// Idempotency guard: if a volume already references this backing LV in the
	// resource config, a previous add already created it. Appending a SECOND
	// volume block for the same disk makes drbdadm reject the whole config with
	// "conflicting use of disk ... first used here" and every create-md on the
	// duplicate minor fails. This is exactly what a retried gateway state-volume
	// provision used to do — each attempt appended another volume N pointing at
	// the same <res>_state1 LV. Treat an already-referenced disk as done.
	backingDevice := fmt.Sprintf("/dev/%s/%s", pool, volume)
	drbdDisk := backingDevice
	if encrypt {
		drbdDisk = luksMapperPath(pool, volume)
	}
	diskRef := drbdDisk + ";"
	if drbdConfigReferencesDisk(hostResult.Output, diskRef) {
		rm.controller.logger.Info("Volume already present in resource config; skipping duplicate add",
			zap.String("resource", resource),
			zap.String("volume", volume),
			zap.String("disk", diskRef))
		return nil
	}

	// Device minors are GLOBAL on a node: scanning only this resource's
	// config hands out minors already claimed by other resources and
	// drbdadm rejects the whole config with "conflicting use of
	// device-minor". Collect minors across every resource file instead.
	// (The previous in-file scan was additionally broken — it required 4
	// fields on a 3-field line and always allocated minor 0.)
	newMinor, err := rm.nextGlobalMinor(ctx, allHosts)
	if err != nil {
		log.Debug("AddVolume: minor allocation failed", zap.String("resource", resource), zap.Error(err))
		return fmt.Errorf("failed to allocate device minor: %w", err)
	}
	log.Debug("AddVolume: new volume", zap.String("resource", resource), zap.Int("volume_id", newVolNum),
		zap.Int("minor", newMinor), zap.String("disk", drbdDisk), zap.Strings("hosts", allHosts))

	// Extend the synchronized DRBD resource config with the new volume block.
	// LINBIT recommends updating the config identically on all nodes and then
	// calling `drbdadm adjust <resource>` to let DRBD enable the new volume.
	volumeBlock := fmt.Sprintf("    volume %d {\n        device    minor %d;\n        disk      %s;\n        meta-disk internal;\n    }",
		newVolNum, newMinor, drbdDisk)

	// Roll back partial state if a later step fails. Without this, a retry of a
	// failed add (e.g. create-md errored) re-reads the .res that still carries
	// the half-added volume block and appends ANOTHER block for the same LV,
	// which DRBD then rejects for "conflicting use of disk". On failure restore
	// the pre-add config on every node and remove the LV we created here.
	originalConfig := hostResult.Output
	committed := false
	defer func() {
		if committed {
			return
		}
		rm.controller.logger.Warn("Volume add failed; rolling back appended volume block and backing LV",
			zap.String("resource", resource),
			zap.String("volume", volume))
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, cerr := rm.deployment.DistributeConfig(cleanupCtx, allHosts, originalConfig, fmt.Sprintf("/etc/drbd.d/%s.res", resource))
		if encrypt {
			// Close before removing: the open container holds the LV, and a key
			// left behind outlives the volume it was protecting.
			rm.closeBackingVolumeOn(cleanupCtx, hosts, pool, volume)
		}
		_, lerr := rm.deployment.LVRemove(cleanupCtx, hosts, backingDevice)
		log.Debug("AddVolume: rolled back", zap.String("resource", resource), zap.String("volume", volume),
			zap.NamedError("config_error", cerr), zap.NamedError("lvremove_error", lerr))
	}()

	// Create the backing volume on every diskful node, shaped by the node's
	// pool the way resource creation and add-replica do it. This used to be a
	// thick lvcreate whose per-host result was never looked at: on a thin
	// pool — which leaves the volume group next to nothing — it failed on
	// every node, and the add went on to create-md against a device that did
	// not exist. Sized like every other backing volume, so a volume added
	// later has the same usable capacity as one the resource was created with.
	sizeBytes := backingVolumeSizeBytes(sizeGB, len(hosts)-1, encrypt)
	for _, host := range hosts {
		if err := rm.createBackingVolumeOn(ctx, host, pool, volume, sizeBytes); err != nil {
			log.Debug("AddVolume: backing volume failed", zap.String("host", host), zap.String("volume", volume), zap.Error(err))
			return fmt.Errorf("failed to create volume %s/%s on %s: %w", pool, volume, host, err)
		}
		if encrypt {
			if err := rm.encryptBackingVolumeOn(ctx, host, host, pool, volume, backingDevice); err != nil {
				log.Debug("AddVolume: encryption failed", zap.String("host", host), zap.String("volume", volume), zap.Error(err))
				return err
			}
		}
		log.Debug("AddVolume: backing volume created", zap.String("host", host), zap.String("volume", volume),
			zap.Uint64("size_bytes", sizeBytes))
	}

	lines = strings.Split(hostResult.Output, "\n")
	insertIdx := len(lines)
	for idx := len(lines) - 1; idx >= 0; idx-- {
		if strings.TrimSpace(lines[idx]) == "}" {
			insertIdx = idx
			break
		}
	}
	updatedLines := append([]string{}, lines[:insertIdx]...)
	updatedLines = append(updatedLines, volumeBlock)
	updatedLines = append(updatedLines, lines[insertIdx:]...)
	updatedConfig := addDisklessVolumeOverrides(strings.Join(updatedLines, "\n"), newVolNum, newMinor)

	t0 = time.Now()
	_, err = rm.deployment.DistributeConfig(ctx, allHosts, updatedConfig, fmt.Sprintf("/etc/drbd.d/%s.res", resource))
	log.Debug("AddVolume: distributed config", zap.String("resource", resource), zap.Strings("hosts", allHosts),
		zap.Duration("took", time.Since(t0)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("failed to distribute updated config: %w", err)
	}

	// The new volume's backing device has no DRBD metadata yet; without
	// create-md the subsequent adjust attaches it Diskless.
	createMDCmd := fmt.Sprintf("sudo drbdadm create-md --force %s/%d", resource, newVolNum)
	t0 = time.Now()
	mdResult, err := rm.deployment.Exec(ctx, hosts, createMDCmd)
	log.Debug("AddVolume: created metadata", zap.String("resource", resource), zap.Int("volume_id", newVolNum),
		zap.Array("failed_hosts", resourcesFailedHosts{mdResult}), zap.Duration("took", time.Since(t0)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("failed to create metadata for new volume: %w", err)
	}
	if !mdResult.AllSuccess() {
		return fmt.Errorf("metadata creation for new volume failed: %s", mdResult.FailureDetails())
	}

	adjustCmd := fmt.Sprintf("sudo drbdadm adjust %s", resource)
	t0 = time.Now()
	adjustResult, err := rm.deployment.Exec(ctx, allHosts, adjustCmd)
	log.Debug("AddVolume: adjusted", zap.String("resource", resource), zap.Strings("hosts", allHosts),
		zap.Array("failed_hosts", resourcesFailedHosts{adjustResult}), zap.Duration("took", time.Since(t0)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("failed to adjust resource after volume add: %w", err)
	}
	if !adjustResult.AllSuccess() {
		return fmt.Errorf("resource adjust failed on hosts: %s", adjustResult.FailureDetails())
	}

	// A brand-new volume is Inconsistent on every node with no UpToDate
	// peer to sync from, so DRBD refuses to open it ("Could not open")
	// until an initial sync source exists. The volume is empty, so skip
	// the pointless full sync the LINSTOR way: declare a new current UUID
	// with a cleared bitmap on one node, which marks all replicas UpToDate
	// — once every peer's disk has finished its handshake (volume_settle.go).
	if err := rm.settleNewVolume(ctx, hosts[0], resource, newVolNum); err != nil {
		log.Debug("AddVolume: settling new volume failed", zap.String("resource", resource), zap.Int("volume_id", newVolNum), zap.Error(err))
		return err
	}

	// The volume is now fully attached and UpToDate. Persisting to the database
	// is best-effort below, so a save failure must NOT roll back the working
	// volume — mark the add committed here.
	committed = true

	rm.controller.logger.Info("Volume added successfully",
		zap.String("resource", resource),
		zap.String("volume", volume))

	if rm.controller.db != nil {
		err := rm.controller.db.SaveVolume(ctx, &database.Volume{
			ResourceName: resource,
			VolumeName:   volume,
			VolumeID:     newVolNum,
			Pool:         pool,
			SizeGB:       int(sizeGB),
			Device:       drbdDisk,
		})
		log.Debug("AddVolume: saved volume record", zap.String("resource", resource), zap.String("volume", volume), zap.Error(err))
		if err != nil {
			rm.controller.logger.Warn("Failed to save added volume to database",
				zap.String("resource", resource),
				zap.String("volume", volume),
				zap.Error(err))
		}
	}

	return nil
}

// RemoveVolume removes a volume from a DRBD resource
func (rm *ResourceManager) RemoveVolume(ctx context.Context, resource string, volumeID uint32) error {
	rm.controller.logger.Info("Removing volume from resource",
		zap.String("resource", resource),
		zap.Uint32("volume_id", volumeID))

	log := rm.resourcesLog()
	if rm.deployment == nil {
		log.Debug("RemoveVolume: no deployment client", zap.String("resource", resource))
		return fmt.Errorf("deployment client not set")
	}

	if volumeID == 0 {
		log.Debug("RemoveVolume: volume 0 refused", zap.String("resource", resource))
		return fmt.Errorf("removing volume 0 is not supported")
	}
	if err := rm.controller.assertResourceUnlocked(ctx, resource, "removing a volume"); err != nil {
		log.Debug("RemoveVolume: resource is locked", zap.String("resource", resource), zap.Error(err))
		return err
	}

	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		log.Debug("RemoveVolume: no hosts", zap.String("resource", resource), zap.Error(err))
		return err
	}
	// The diskless participants' copies of the config lose the volume too.
	allHosts := append(append([]string(nil), hosts...), rm.disklessParticipantHosts(ctx, resource)...)

	t0 := time.Now()
	result, err := rm.deployment.Exec(ctx, []string{hosts[0]}, fmt.Sprintf("cat /etc/drbd.d/%s.res", resource))
	log.Debug("RemoveVolume: read config", zap.String("resource", resource), zap.String("host", hosts[0]),
		zap.Duration("took", time.Since(t0)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	var hostResult *deployment.HostResult
	for _, r := range result.Hosts {
		hostResult = r
		break
	}
	if hostResult == nil || !hostResult.Success {
		log.Debug("RemoveVolume: config read failed", zap.String("resource", resource))
		return fmt.Errorf("failed to get config")
	}

	configContent := hostResult.Output
	volumes := parseResourceConfigVolumes(configContent)
	var target *resourceConfigVolume
	for i := range volumes {
		if volumes[i].VolumeID == int(volumeID) {
			target = &volumes[i]
			break
		}
	}
	if target == nil {
		log.Debug("RemoveVolume: volume not in config", zap.String("resource", resource), zap.Uint32("volume_id", volumeID))
		return fmt.Errorf("volume %d not found", volumeID)
	}
	log.Debug("RemoveVolume: target volume", zap.String("resource", resource), zap.Uint32("volume_id", volumeID),
		zap.String("disk", target.DiskPath), zap.Strings("hosts", allHosts))

	lines := strings.Split(configContent, "\n")
	start := target.StartLine
	if start > 0 && strings.TrimSpace(lines[start-1]) == "" {
		start--
	}
	updatedLines := append([]string{}, lines[:start]...)
	updatedLines = append(updatedLines, lines[target.EndLine+1:]...)
	newConfig := removeDisklessVolumeOverrides(strings.Join(updatedLines, "\n"), int(volumeID))

	t0 = time.Now()
	_, err = rm.deployment.DistributeConfig(ctx, allHosts, newConfig, fmt.Sprintf("/etc/drbd.d/%s.res", resource))
	log.Debug("RemoveVolume: distributed config", zap.String("resource", resource), zap.Duration("took", time.Since(t0)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("failed to distribute updated config: %w", err)
	}

	t0 = time.Now()
	adjustRes, err := rm.deployment.Exec(ctx, allHosts, fmt.Sprintf("sudo drbdadm adjust %s", resource))
	log.Debug("RemoveVolume: adjusted", zap.String("resource", resource), zap.Array("failed_hosts", resourcesFailedHosts{adjustRes}),
		zap.Duration("took", time.Since(t0)), zap.Error(err))
	if err == nil && !adjustRes.AllSuccess() {
		err = fmt.Errorf("drbdadm adjust failed after removing volume %d: %s", volumeID, adjustRes.FailureDetails())
	}
	if err != nil {
		log.Debug("RemoveVolume: adjust failed; restoring config", zap.String("resource", resource), zap.Error(err))
		// Nothing has been deleted yet, so put every node's config back the
		// way it was. Left alone, the nodes keep a config the kernel refused,
		// and the next adjust of this resource — for any reason — fails the
		// same way.
		restoreCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, rerr := rm.deployment.DistributeConfig(restoreCtx, allHosts, configContent,
			fmt.Sprintf("/etc/drbd.d/%s.res", resource)); rerr != nil {
			rm.controller.logger.Error("Could not restore the config after a failed volume removal",
				zap.String("resource", resource), zap.Error(rerr))
		} else {
			_, _ = rm.deployment.Exec(restoreCtx, allHosts, fmt.Sprintf("sudo drbdadm adjust %s", resource))
		}
		return fmt.Errorf("failed to adjust resource after config update (config restored): %w", err)
	}

	if strings.HasPrefix(target.DiskPath, "/dev/zvol/") {
		dataset := strings.TrimPrefix(target.DiskPath, "/dev/zvol/")
		t0 := time.Now()
		zfsRes, err := rm.deployment.ZFSDestroyDataset(ctx, hosts, dataset)
		log.Debug("RemoveVolume: destroyed zvol", zap.String("dataset", dataset), zap.Array("failed_hosts", resourcesFailedHosts{zfsRes}),
			zap.Duration("took", time.Since(t0)), zap.Error(err))
		if err != nil {
			return fmt.Errorf("failed to delete ZFS backing volume: %w", err)
		}
		if !zfsRes.AllSuccess() {
			return fmt.Errorf("ZFS backing volume removal failed: %s", zfsRes.FailureDetails())
		}
	} else {
		// An encrypted volume is addressed as /dev/mapper/... in the config but
		// LVM only understands the LV beneath it, so resolve one to the other
		// and close the container first — it holds the LV open.
		lvPath, cryptPool, cryptVolume, rerr := rm.backingLVFor(ctx, resource, volumeID, target.DiskPath)
		if rerr != nil {
			log.Debug("RemoveVolume: backing LV unknown", zap.String("resource", resource), zap.Error(rerr))
			return rerr
		}
		if cryptPool != "" {
			log.Debug("RemoveVolume: closing LUKS container first", zap.String("pool", cryptPool), zap.String("volume", cryptVolume))
			rm.closeBackingVolumeOn(ctx, hosts, cryptPool, cryptVolume)
		}

		// A failed lvremove on any node leaves an orphan and a lopsided DRBD
		// resource, so surface per-host failures instead of only transport
		// errors. Detaching the just-removed volume's minor releases the LV if
		// the kernel still holds it after the adjust.
		removeCmd := fmt.Sprintf("sudo lvremove -f %s || { sudo drbdsetup detach %s/%d 2>/dev/null; sudo lvremove -f %s; }",
			lvPath, resource, volumeID, lvPath)
		t0 := time.Now()
		rmRes, err := rm.deployment.Exec(ctx, hosts, removeCmd)
		log.Debug("RemoveVolume: removed LV", zap.String("lv", lvPath), zap.Array("failed_hosts", resourcesFailedHosts{rmRes}),
			zap.Duration("took", time.Since(t0)), zap.Error(err))
		if err != nil {
			return fmt.Errorf("failed to delete LVM backing volume: %w", err)
		}
		if !rmRes.AllSuccess() {
			return fmt.Errorf("LVM backing volume removal failed: %s", rmRes.FailureDetails())
		}
	}

	if rm.controller.db != nil {
		dbVolumes, err := rm.controller.db.ListVolumes(ctx, resource)
		log.Debug("RemoveVolume: listed volume records", zap.String("resource", resource), zap.Int("count", len(dbVolumes)), zap.Error(err))
		if err == nil {
			if volume := findVolumeRecord(dbVolumes, volumeID); volume != nil {
				err := rm.controller.db.DeleteVolume(ctx, resource, volume.VolumeName)
				log.Debug("RemoveVolume: deleted volume record", zap.String("volume", volume.VolumeName), zap.Error(err))
				if err != nil {
					rm.controller.logger.Warn("Failed to delete volume metadata",
						zap.String("resource", resource),
						zap.String("volume", volume.VolumeName),
						zap.Error(err))
				}
			}
		}
	}

	return nil
}
