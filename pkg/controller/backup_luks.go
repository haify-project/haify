package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
)

// Backups of encrypted resources.
//
// An encrypted volume's backing LV holds a LUKS2 header followed by ciphertext;
// DRBD sits above the crypt mapping and only ever sees plaintext. A backup
// snapshots the LV, so what it snapshots is the ciphertext. Reading that
// snapshot directly — which is what backups did until this file existed —
// stored the LUKS header plus the first DRBD-device-size bytes of ciphertext,
// and a restore wrote those bytes into the DRBD device, the plaintext layer.
// The result was a volume full of noise. Nor could the image be decrypted some
// other way: the key never leaves the node it was made on (encryption.go), and
// the image is short by the header's length.
//
// So the snapshot is read THROUGH a crypt mapping of its own: the snapshot of
// a LUKS volume carries the same header, so the node's key for that volume
// opens it, read-only, under a temporary name. The image is then the same
// plaintext DRBD holds, and it restores onto any replica — encrypted or not —
// because restores write through the DRBD device.
//
// The consequence, which the user guide states: an encrypted resource's backup
// is plaintext on the target. Encryption here protects departing pool disks;
// protecting the backup target is the target's job (bucket encryption, access
// policy). A ciphertext backup would need a key that outlives the node, which
// is precisely what this design refuses to create.

// backupSnapshotSource returns the shell that makes a backup snapshot readable,
// and the shell that undoes it on exit (":" when there is nothing to undo). The
// caller's script starts with MAP empty, so the cleanup is harmless if setup
// never got as far as opening anything.
//
// setup leaves $SRC naming the device to read and $OFF holding the byte offset,
// in the snapshot's own address space, of $SRC's first byte. For a plain volume
// that is the snapshot itself at offset 0. For an encrypted one it is a
// read-only crypt mapping of the snapshot, and $OFF is the LUKS data offset:
// thin_delta reports changed ranges of the LV, and every one of them has to be
// shifted by the header to address the same bytes in the plaintext.
func backupSnapshotSource(pool, backing, snap string, encrypted bool) (setup, cleanup string, err error) {
	log := zap.L()
	log.Debug("backupSnapshotSource: building snapshot source", zap.String("pool", pool), zap.String("backing", backing),
		zap.String("snapshot", snap), zap.Bool("encrypted", encrypted))
	if !encrypted {
		log.Debug("backupSnapshotSource: plain volume, reading the snapshot directly", zap.String("snapshot", snap))
		return fmt.Sprintf("SRC=%s; OFF=0", shellSingleQuote("/dev/"+pool+"/"+snap)), ":", nil
	}
	if err := validateLUKSNames(pool, backing); err != nil {
		log.Debug("backupSnapshotSource: invalid LUKS names", zap.String("pool", pool), zap.String("backing", backing), zap.Error(err))
		return "", "", err
	}
	if !luksNameRe.MatchString(snap) {
		log.Debug("backupSnapshotSource: snapshot name cannot be a LUKS container", zap.String("snapshot", snap))
		return "", "", fmt.Errorf("snapshot name %q cannot be opened as a LUKS container", snap)
	}
	key := luksKeyPath(luksContainerName(pool, backing))
	// The mapping is named from a hash rather than from pool and snapshot: a
	// device-mapper name is capped at 127 bytes and an LV name is not, and a
	// temporary name needs to be unique, not legible.
	sum := sha256.Sum256([]byte(pool + "/" + snap))
	mapping := "haifybk_" + hex.EncodeToString(sum[:8])

	// The key is only ever handed to cryptsetup as a file, as everywhere else.
	// dmsetup masks the key in the table it prints, and only the offset field
	// is kept from it. udev briefly opens every new mapping, which can make an
	// immediate close fail as busy; the deferred close then removes it as soon
	// as the last opener lets go, so the snapshot itself can still be removed.
	setup = fmt.Sprintf(`MAP=%s
sudo test -f %s || { echo "this node holds no LUKS key for %s/%s, so its snapshot cannot be read" >&2; exit 1; }
sudo cryptsetup open --type luks --readonly --key-file %s %s "$MAP"
SRC=/dev/mapper/$MAP
OFF=$(sudo dmsetup table "$MAP" | awk '$3 == "crypt" {print $8; exit}')
case "$OFF" in ''|*[!0-9]*) echo "could not read the LUKS data offset of $MAP (got '$OFF')" >&2; exit 1;; esac
OFF=$((OFF * 512))`,
		mapping, key, pool, backing, key, shellSingleQuote("/dev/"+pool+"/"+snap))
	cleanup = `[ -n "$MAP" ] && { sudo udevadm settle >/dev/null 2>&1; sudo cryptsetup close "$MAP" 2>/dev/null || sudo cryptsetup close --deferred "$MAP" 2>/dev/null; }`
	log.Debug("backupSnapshotSource: encrypted volume, reading through a crypt mapping", zap.String("snapshot", snap),
		zap.String("mapping", mapping))
	return setup, cleanup, nil
}

// encryptedVolumes reports which of a resource's volumes are LUKS containers,
// keyed by volume id, from the backing device paths backingDevices returned.
//
// The resource's recorded flag is the authority and the /dev/mapper path is
// the per-volume confirmation, exactly as backingLVFor decides it: an adopted
// resource reached through someone else's device-mapper stack is not ours to
// open, and a volume added without a container has nothing to open.
func (bm *BackupManager) encryptedVolumes(ctx context.Context, resource string, backing map[uint32]string) map[uint32]bool {
	log := bm.controller.traceLogger()
	out := map[uint32]bool{}
	if !bm.controller.resources.resourceIsEncrypted(ctx, resource) {
		log.Debug("BackupManager.encryptedVolumes: resource not encrypted", zap.String("resource", resource))
		return out
	}
	for id, dev := range backing {
		mapped := luksIsMapperPath(dev)
		log.Debug("BackupManager.encryptedVolumes: volume", zap.String("resource", resource), zap.Uint32("volume", id),
			zap.String("device", dev), zap.Bool("luks", mapped))
		if mapped {
			out[id] = true
		}
	}
	return out
}

// assertNotCiphertext refuses to restore a backup whose images are ciphertext.
//
// Backups taken before snapshots were read through the crypt mapping copied an
// encrypted volume's LUKS header and ciphertext, and recorded no marker saying
// so. Written into the DRBD device they would replace a volume's data with
// noise and report success. They are recognised by what they lack: a volume
// that is a LUKS container today, of the resource the backup was taken from,
// whose image was not marked as read through one.
//
// A backup whose source resource has since been deleted cannot be checked this
// way, and is let through: there is nothing left to compare it with.
func (bm *BackupManager) assertNotCiphertext(ctx context.Context, chain []*database.Backup) error {
	log := bm.controller.traceLogger()
	log.Debug("BackupManager.assertNotCiphertext: checking chain", zap.Int("count", len(chain)))
	for _, b := range chain {
		backing, err := bm.backingDevices(ctx, b.Resource)
		if err != nil {
			log.Debug("BackupManager.assertNotCiphertext: source resource gone, not checked", zap.String("backup", b.ID),
				zap.String("resource", b.Resource), zap.Error(err))
			continue // the source is gone; see above
		}
		enc := bm.encryptedVolumes(ctx, b.Resource, backing)
		var bad []string
		for _, v := range b.Volumes {
			if enc[v.VolumeID] && !v.ReadThroughLUKS {
				bad = append(bad, fmt.Sprint(v.VolumeID))
			}
		}
		log.Debug("BackupManager.assertNotCiphertext: backup checked", zap.String("backup", b.ID),
			zap.Int("encrypted_volumes", len(enc)), zap.Strings("ciphertext_volumes", bad))
		if len(bad) > 0 {
			return fmt.Errorf(
				"backup %q of encrypted resource %q holds the ciphertext of volume %s: it was taken before backups read through the LUKS container, "+
					"and its images cannot be decrypted (the key never left the node) or restored; take a new backup",
				b.ID, b.Resource, strings.Join(bad, ", "))
		}
	}
	return nil
}
