package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
)

// Filling a volume from a snapshot of an ENCRYPTED volume.
//
// An encrypted volume's backing LV holds a LUKS2 header followed by ciphertext,
// and a snapshot of that LV is a snapshot of the ciphertext. PopulateVolume
// writes into the target's DRBD device, the plaintext layer, so copying the
// raw snapshot there would fill the new volume with the source's header and
// ciphertext: noise, under a key that exists only on the source node.
//
// So the snapshot is opened THROUGH a crypt mapping of its own, as a backup
// does (backup_luks.go): it carries the same header as its origin, so the
// node's key for the origin opens it, read-only, under a temporary name. The
// copy then reads the plaintext DRBD saw, including the DRBD metadata at the
// end of the mapping that fitTargetToSource sizes the copy from, and writes it
// through the target's DRBD device, which on an encrypted target is encrypted
// again under the target node's own key. The key itself is only ever handed to
// cryptsetup as a file on the node; nothing about it crosses SSH.
//
// The copy of an encrypted volume has to be encrypted too. A target that is
// not would hold the plaintext on its pool disks, which is exactly what the
// source's encryption exists to prevent, so assertCopyStaysEncrypted refuses
// that before anything is promoted or read.

// populateLUKSMapping names the temporary crypt mapping a snapshot is read
// through while it fills volume volumeID of target. Hashed rather than spelled
// out because a device-mapper name is capped at 127 bytes and an LV name is
// not; unique per target volume, so two copies never share one.
func populateLUKSMapping(target string, volumeID uint32) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", target, volumeID)))
	return "haifypop_" + hex.EncodeToString(sum[:8])
}

// populateLUKSOpenCmd activates the snapshot if it is thin, and opens it
// read-only through a crypt mapping with the key the node keeps for the
// snapshot's origin. A mapping left by a copy that died half-way has the same
// name and is closed first. Every name is inlined: no shell variables, so the
// command means the same however dispatch quotes it.
func populateLUKSOpenCmd(pool, volume, snapshotDevice, mapping string) string {
	key := luksKeyPath(luksContainerName(pool, volume))
	return fmt.Sprintf("set -e; %s "+
		"sudo test -f %s || { echo 'this node holds no LUKS key for %s/%s, so its snapshot cannot be read' >&2; exit 1; }; "+
		"sudo cryptsetup close %s >/dev/null 2>&1 || true; "+
		"sudo cryptsetup open --type luks --readonly --key-file %s %s %s",
		activateSnapshotCmd(snapshotDevice), key, pool, volume, mapping, key, snapshotDevice, mapping)
}

// populateLUKSCloseCmd removes the mapping again. udev briefly opens every new
// mapping, which can make an immediate close fail as busy; the deferred close
// then removes it as soon as the last opener lets go.
func populateLUKSCloseCmd(mapping string) string {
	return fmt.Sprintf("sudo udevadm settle >/dev/null 2>&1; sudo cryptsetup close %s 2>/dev/null || sudo cryptsetup close --deferred %s",
		mapping, mapping)
}

// sourceIsEncrypted reports whether vol, a volume of resource, is one of
// Haify's crypt containers: the resource's recorded flag is the authority and
// the /dev/mapper path the per-volume confirmation, as backingLVFor decides it.
func (sm *SnapshotManager) sourceIsEncrypted(ctx context.Context, resource string, vol *database.Volume) bool {
	enc := luksIsMapperPath(vol.Device) && sm.controller.resources.resourceIsEncrypted(ctx, resource)
	sm.controller.traceLogger().Debug("sourceIsEncrypted: checked", zap.String("resource", resource),
		zap.String("device", vol.Device), zap.Bool("encrypted", enc))
	return enc
}

// openEncryptedSnapshot exposes the LVM snapshot at snapshotDevice, of the
// encrypted volume vol, as plaintext on address, and returns the mapping's
// device and a function that closes it.
func (sm *SnapshotManager) openEncryptedSnapshot(ctx context.Context, address, node, target string, volumeID uint32,
	vol *database.Volume, snapshotDevice string) (string, func(), error) {
	log := sm.controller.traceLogger()
	log.Debug("openEncryptedSnapshot: opening", zap.String("host", address), zap.String("node", node),
		zap.String("target", target), zap.Uint32("volume", volumeID), zap.String("pool", vol.Pool),
		zap.String("backing", vol.VolumeName), zap.String("snapshot", snapshotDevice))
	if err := validateLUKSNames(vol.Pool, vol.VolumeName); err != nil {
		log.Debug("openEncryptedSnapshot: names unusable", zap.Error(err))
		return "", nil, err
	}
	if !luksDeviceRe.MatchString(snapshotDevice) {
		log.Debug("openEncryptedSnapshot: snapshot device refused", zap.String("snapshot", snapshotDevice))
		return "", nil, fmt.Errorf("snapshot device %q cannot be opened as a LUKS container", snapshotDevice)
	}
	mapping := populateLUKSMapping(target, volumeID)
	dep := sm.controller.deployment
	release := func() {
		start := time.Now()
		err := execFailure(dep.Exec(context.WithoutCancel(ctx), []string{address}, populateLUKSCloseCmd(mapping)))
		log.Debug("openEncryptedSnapshot: closed temporary mapping", zap.String("mapping", mapping), zap.String("host", address),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			sm.controller.logger.Warn("Failed to close the crypt mapping a snapshot was copied through; close it with cryptsetup close",
				zap.String("mapping", mapping), zap.String("node", node), zap.Error(err))
		}
	}
	start := time.Now()
	err := execFailure(dep.Exec(ctx, []string{address}, populateLUKSOpenCmd(vol.Pool, vol.VolumeName, snapshotDevice, mapping)))
	log.Debug("openEncryptedSnapshot: cryptsetup open", zap.String("snapshot", snapshotDevice), zap.String("mapping", mapping),
		zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		release()
		return "", nil, fmt.Errorf("open encrypted snapshot %s on %s for reading: %w", snapshotDevice, node, err)
	}
	return "/dev/mapper/" + mapping, release, nil
}

// assertCopyStaysEncrypted refuses to fill an unencrypted resource from a
// snapshot of an encrypted one: the copy would land on the target's pool disks
// as plaintext.
func (sm *SnapshotManager) assertCopyStaysEncrypted(ctx context.Context, target string, src SnapshotSource) error {
	log := sm.controller.traceLogger()
	srcEnc := sm.controller.resources.resourceIsEncrypted(ctx, src.Resource)
	if !srcEnc {
		log.Debug("assertCopyStaysEncrypted: source not encrypted", zap.String("source_resource", src.Resource))
		return nil
	}
	if sm.controller.resources.resourceIsEncrypted(ctx, target) {
		log.Debug("assertCopyStaysEncrypted: both encrypted", zap.String("source_resource", src.Resource), zap.String("target", target))
		return nil
	}
	log.Debug("assertCopyStaysEncrypted: plaintext copy of an encrypted source refused",
		zap.String("source_resource", src.Resource), zap.String("target", target))
	return fmt.Errorf("%s is encrypted and %s is not: copying snapshot %s into it would leave the data unencrypted on %s's disks; "+
		"create %s with encryption", src.Resource, target, src.Name, target, target)
}
