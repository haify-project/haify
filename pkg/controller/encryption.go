package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"
)

// LUKS encryption of a resource's BACKING volume — the layer beneath DRBD.
//
// The stack becomes DRBD → LUKS → LVM, which is where LINSTOR puts its `luks`
// layer too (see assertEncryptableStorage for why ZFS is left out). Two
// consequences follow from that ordering and both of them matter more than the
// code below:
//
//  1. DRBD sits ABOVE the crypt layer, so what travels between nodes is the
//     same plaintext it always was. This is encryption AT REST — it protects a
//     pool disk that leaves the building (RMA, decommission, theft), and it
//     protects nothing on the wire. An operator who turns this on believing the
//     replication link is now confidential has made a security mistake, so the
//     CLI help and both READMEs say so in as many words.
//  2. Every replica encrypts its own copy independently. There is no shared
//     ciphertext, so there is no reason for the replicas to share a key.
//
// # Key management
//
// Each node generates its own 512-bit key, on the node, from /dev/urandom, and
// keeps it at /etc/haify/luks/<container>.key — mode 0400, inside a 0700
// directory, both owned by root. The key never leaves the node it was made on,
// is never sent over SSH, never reaches the controller, and therefore cannot
// appear in the audit log, in the controller's log, or in the database.
//
// That last point is not a stylistic preference. In this codebase the only
// channel to a node is deployment.Exec, which (a) hands the whole command
// string to dispatch, which puts it in the remote sshd session's argv where any
// local user's `ps` can read it, and (b) logs it verbatim at Debug level on the
// controller. DistributeConfig is worse: it base64s the payload into that same
// command string and lands it as a 0644 file. Any design that ships a
// passphrase — a controller master key with per-resource derived subkeys, an
// operator-supplied passphrase, anything — has to travel that channel. Not
// having a secret to transport is the only version of this that is actually
// safe with the primitives that exist here. `cryptsetup` is therefore only ever
// handed --key-file, never a passphrase argument.
//
// What that costs, stated plainly: there is no central escrow. Losing a node's
// root filesystem loses that node's key and makes that node's copy of the
// ciphertext unrecoverable. That is survivable — the peers hold the same data
// under their own keys and DRBD rebuilds the replica — but it does mean this
// scheme cannot be used to recover data from a lone surviving disk whose node
// is gone. It also means the key sits on the same machine as the disks, so the
// threat this defends against is a departing DISK, not a departing SERVER.
//
// On `resource delete` each node's key file is overwritten and removed before
// the backing volume goes away, so the ciphertext left in unallocated pool
// extents has no key anywhere that could still read it.
//
// # Opening the container
//
// The container has to be open before DRBD can attach, on EVERY replica and not
// merely on the one being promoted: a Secondary receives replicated writes and
// therefore needs its backing device just as much as a Primary. A drbd-reactor
// promoter hook would be the wrong hook twice over — it runs only on the node
// being promoted, and it runs after DRBD is already up, which is too late.
//
// So the open belongs where the backing devices are already assembled at boot:
// haify-drbd-up.service, the node-local oneshot that runs `vgchange -ay` and then
// `drbdadm adjust` before drbd-reactor. luksBootOpenSnippet is spliced in
// between those two steps. That covers a reboot and, transitively, a failover:
// after any node comes back its containers are open and its replica reattaches,
// so drbd-reactor can promote it exactly as it would an unencrypted resource.

// luksKeyDir holds one key file and one device pointer per container. 0700 and
// root-owned: the mode is what keeps the keys out of reach of anything but
// root, and it is checked by the provisioning script rather than assumed.
const luksKeyDir = "/etc/haify/luks"

// luksNameRe constrains the pool and volume names that are interpolated into a
// device-mapper name, a file path and a shell command. Everything reaching here
// has already been through normalizeManagedName, so this is a backstop against
// a future caller that has not.
var luksNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.+-]*$`)

// luksHeaderBytes is the space a LUKS2 header plus its keyslot area takes off
// the front of the device, and therefore how much bigger the backing volume has
// to be for the DRBD device to still be the size the user asked for. This is
// the same class of shortfall drbdMetadataBytes exists to absorb.
//
// cryptsetup's LUKS2 default data offset is 16 MiB. We reserve double that, so
// a node whose cryptsetup ships a larger keyslot area does not silently produce
// a volume that is a few MiB short — which DRBD would only report much later,
// as a peer that refuses to attach because it is not the same number of bytes.
const luksHeaderBytes = 32 << 20

// replicaBackingSizes turns the sizes an existing replica's lower-level devices
// report (primaryBackingSizes) into the size of the logical volume a new
// replica needs.
//
// On an encrypted resource DRBD's lower-level device is the crypt container,
// which is its LV less the LUKS header. An LV made at that size opens to a
// container one header smaller than its peers', and DRBD refuses a peer that
// is even one sector short. luksHeaderBytes is at least the header cryptsetup
// writes, so the newcomer's container comes out the same size or a little
// larger, which DRBD accepts.
func replicaBackingSizes(sizes []uint64, encrypted bool) []uint64 {
	if !encrypted {
		return sizes
	}
	zap.L().Debug("replicaBackingSizes: adding LUKS header to each volume", zap.Int("volumes", len(sizes)),
		zap.Uint64("header_bytes", luksHeaderBytes))
	out := make([]uint64, len(sizes))
	for i, s := range sizes {
		out[i] = s + luksHeaderBytes
	}
	return out
}

// luksContainerName is the device-mapper name of one node's container for one
// backing volume. Derived from pool and volume rather than from the resource so
// that it is identical on every node (the DRBD config is shared) and unique
// within a node (pool names are unique per node, LV names within a pool).
func luksContainerName(pool, volume string) string {
	return "haify_" + pool + "_" + volume
}

// luksMapperPath is what DRBD is pointed at instead of the LV or zvol.
func luksMapperPath(pool, volume string) string {
	return "/dev/mapper/" + luksContainerName(pool, volume)
}

// luksIsMapperPath reports whether a disk path recorded in a DRBD config or a
// volume record goes through a crypt container. It is the same trick the ZFS
// branches use with the /dev/zvol/ prefix: the recorded device path is the one
// piece of state that cannot drift out of step with what the node really has.
func luksIsMapperPath(device string) bool {
	return strings.HasPrefix(device, "/dev/mapper/")
}

func luksKeyPath(name string) string { return luksKeyDir + "/" + name + ".key" }
func luksDevPointer(name string) string {
	return luksKeyDir + "/" + name + ".dev"
}

// luksDeviceRe is the shape a backing-device path may have before it is
// interpolated into a shell command. Every caller derives it from names that
// luksNameRe has already accepted; this is the belt to that pair of braces.
var luksDeviceRe = regexp.MustCompile(`^/dev/[a-zA-Z0-9_.+/-]+$`)

// validateLUKSNames rejects names that must not be interpolated into a shell
// command or a device path.
func validateLUKSNames(pool, volume string) error {
	if !luksNameRe.MatchString(pool) {
		zap.L().Debug("validateLUKSNames: pool name refused", zap.String("pool", pool))
		return fmt.Errorf("pool name %q cannot back an encrypted volume: only letters, digits and _.+- are allowed", pool)
	}
	if !luksNameRe.MatchString(volume) {
		zap.L().Debug("validateLUKSNames: volume name refused", zap.String("volume", volume))
		return fmt.Errorf("volume name %q cannot be encrypted: only letters, digits and _.+- are allowed", volume)
	}
	return nil
}

// assertEncryptionSupported refuses before anything is created when a node
// cannot host an encrypted volume.
//
// Checking up front rather than discovering it at luksFormat time is what keeps
// the failure clean: creation walks the nodes one at a time, so a third node
// without cryptsetup would otherwise leave two encrypted volumes and a
// half-built resource to roll back.
func (rm *ResourceManager) assertEncryptionSupported(ctx context.Context, hosts, nodes []string) error {
	log := rm.controller.traceLogger()
	for i, host := range hosts {
		node := host
		if i < len(nodes) && nodes[i] != "" {
			node = nodes[i]
		}
		start := time.Now()
		res, err := rm.deployment.Exec(ctx, []string{host}, cryptsetupProbe)
		if err != nil {
			log.Debug("ResourceManager.assertEncryptionSupported: probe failed", zap.String("node", node),
				zap.String("host", host), zap.Duration("took", time.Since(start)), zap.Error(err))
			return fmt.Errorf("check encryption support on %q: %w", node, err)
		}
		out := ""
		for _, r := range res.Hosts {
			out = strings.TrimSpace(r.Output)
			break
		}
		log.Debug("ResourceManager.assertEncryptionSupported: probe answered", zap.String("node", node),
			zap.String("host", host), zap.String("probe", out), zap.Duration("took", time.Since(start)))
		switch {
		case strings.Contains(out, "nocryptsetup"):
			return fmt.Errorf("node %q has no cryptsetup, so it cannot hold an encrypted replica; install cryptsetup there (apt install cryptsetup / dnf install cryptsetup) or create the resource without --encrypt", node)
		case strings.Contains(out, "nodmcrypt"):
			return fmt.Errorf("node %q has cryptsetup but no dm-crypt target in the running kernel, so a container could be created and never opened; load or install the dm-crypt module there", node)
		case strings.Contains(out, "ok"):
			continue
		default:
			return fmt.Errorf("could not tell whether %q supports encryption (probe said %q); refusing to create an encrypted resource blind", node, out)
		}
	}
	log.Debug("ResourceManager.assertEncryptionSupported: every node can encrypt", zap.Strings("hosts", hosts))
	return nil
}

// assertEncryptableStorage restricts the crypt layer to LVM-backed pools.
//
// dm-crypt would happily sit on a zvol, but every teardown, resize and sweep
// path in this package decides what it is looking at from the recorded device
// path — /dev/zvol/... means ZFS, /dev/mapper/... means a crypt container — and
// an encrypted zvol is both at once. Rather than thread a second discriminator
// through all of them for a combination nobody has asked for (ZFS has its own
// native encryption, which does not need a layer underneath DRBD to exist),
// this says no in one place and means it.
func assertEncryptableStorage(storageType string) error {
	switch storageType {
	case "", "lvm", "lvm-thin":
		zap.L().Debug("assertEncryptableStorage: LVM-backed, encryptable", zap.String("storage_type", storageType))
		return nil
	default:
		zap.L().Debug("assertEncryptableStorage: refused", zap.String("storage_type", storageType))
		return fmt.Errorf(
			"encryption is only supported on LVM-backed pools, not %q; "+
				"ZFS provides its own encryption at the dataset level", storageType)
	}
}

// assertEncryptionNotRetrofitted refuses to change the encryption state of a
// resource that already exists.
//
// Turning encryption on in place would mean, per node: evacuate the replica,
// destroy it, rebuild it as a container and resync the whole thing from a peer
// — the convert-to-thin dance, with the same single-copy window and the same
// preconditions, except that here a failure halfway leaves half the replicas
// encrypted and half not, and no way to tell from the config which is which
// without reading every node. That is a separate operation with its own guards,
// not a flag on create. Until it exists, saying so is better than doing part of
// it: a resource that came back plaintext because the second node failed is a
// security claim that quietly is not true.
func (rm *ResourceManager) assertEncryptionNotRetrofitted(ctx context.Context, name string, want bool) error {
	log := rm.controller.traceLogger()
	if rm.controller.db == nil {
		log.Debug("ResourceManager.assertEncryptionNotRetrofitted: no database, nothing to compare")
		return nil
	}
	start := time.Now()
	existing, err := rm.controller.db.GetResource(ctx, name)
	log.Debug("ResourceManager.assertEncryptionNotRetrofitted: db get resource", zap.String("resource", name),
		zap.Bool("found", existing != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || existing == nil {
		return nil // a name nobody has used yet; nothing to contradict
	}
	if existing.Encrypted == want {
		log.Debug("ResourceManager.assertEncryptionNotRetrofitted: encryption unchanged", zap.Bool("encrypted", want))
		return nil
	}
	log.Debug("ResourceManager.assertEncryptionNotRetrofitted: encryption would change, refused",
		zap.Bool("existing", existing.Encrypted), zap.Bool("want", want))
	if want {
		return fmt.Errorf(
			"resource %q already exists unencrypted; encryption is chosen when a resource is created and cannot be added in place. "+
				"Create a new encrypted resource and copy the data into it", name)
	}
	return fmt.Errorf(
		"resource %q already exists encrypted; it cannot be recreated without encryption in place. "+
			"Delete it (which destroys the data and the keys) or create a new resource", name)
}

// encryptBackingVolumeOn wraps one node's freshly created backing volume in a
// LUKS2 container and leaves it open. Safe only on a volume that was just
// created — see luksProvisionCmd.
func (rm *ResourceManager) encryptBackingVolumeOn(ctx context.Context, host, node, pool, volume, backingDevice string) error {
	log := rm.controller.traceLogger()
	cmd, err := luksProvisionCmd(pool, volume, backingDevice)
	if err != nil {
		return err
	}
	log.Debug("ResourceManager.encryptBackingVolumeOn: provisioning container", zap.String("node", node),
		zap.String("host", host), zap.String("pool", pool), zap.String("volume", volume), zap.String("device", backingDevice))
	start := time.Now()
	if err := execFailure(rm.deployment.Exec(ctx, []string{host}, cmd)); err != nil {
		log.Debug("ResourceManager.encryptBackingVolumeOn: provisioning failed", zap.String("host", host),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("set up the LUKS container for %s/%s on %s: %w", pool, volume, node, err)
	}
	log.Debug("ResourceManager.encryptBackingVolumeOn: container open", zap.String("host", host),
		zap.Duration("took", time.Since(start)))
	rm.controller.logger.Info("Backing volume encrypted",
		zap.String("node", node),
		zap.String("volume", pool+"/"+volume),
		zap.String("container", luksContainerName(pool, volume)))
	return nil
}

// closeBackingVolumeOn closes the container and destroys the keys on the given
// hosts. Best-effort by contract: it runs on teardown paths where the caller is
// already committed to removing the storage.
func (rm *ResourceManager) closeBackingVolumeOn(ctx context.Context, hosts []string, pool, volume string) {
	log := rm.controller.traceLogger()
	if len(hosts) == 0 {
		log.Debug("ResourceManager.closeBackingVolumeOn: no hosts", zap.String("pool", pool), zap.String("volume", volume))
		return
	}
	cmd, err := luksTeardownCmd(pool, volume)
	if err != nil {
		rm.controller.logger.Warn("Skipping LUKS teardown for an unrepresentable volume name",
			zap.String("volume", pool+"/"+volume), zap.Error(err))
		return
	}
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, hosts, cmd)
	if err != nil {
		rm.controller.logger.Warn("Best-effort LUKS teardown failed; the key file may survive on some nodes",
			zap.String("volume", pool+"/"+volume),
			zap.Strings("hosts", hosts),
			zap.Error(err))
		return
	}
	if ce := log.Check(zap.DebugLevel, "ResourceManager.closeBackingVolumeOn: container closed and key destroyed"); ce != nil {
		ce.Write(zap.String("pool", pool), zap.String("volume", volume), zap.Strings("hosts", hosts),
			zap.Strings("failed_hosts", tracedFailedHosts(res)), zap.Duration("took", time.Since(start)))
	}
}

// resourceIsEncrypted reports whether Haify built this resource's backing volumes
// as crypt containers.
//
// The recorded flag is the authority, not the /dev/mapper/ prefix on its own. A
// resource ADOPTED from outside Haify may perfectly well be reached through
// /dev/mapper (multipath, an operator's own dm stack, someone else's LUKS), and
// treating that as ours would have Haify try to resize a mapping it has no key
// for and report an encryption guarantee it did not provide.
func (rm *ResourceManager) resourceIsEncrypted(ctx context.Context, resource string) bool {
	if rm.controller.db == nil {
		rm.controller.traceLogger().Debug("ResourceManager.resourceIsEncrypted: no database, treating as unencrypted",
			zap.String("resource", resource))
		return false
	}
	start := time.Now()
	dbRes, err := rm.controller.db.GetResource(ctx, resource)
	enc := err == nil && dbRes != nil && dbRes.Encrypted
	rm.controller.traceLogger().Debug("ResourceManager.resourceIsEncrypted: db get resource", zap.String("resource", resource),
		zap.Bool("found", dbRes != nil), zap.Bool("encrypted", enc), zap.Duration("took", time.Since(start)), zap.Error(err))
	return enc
}

// backingLVFor resolves the disk path recorded in a DRBD config down to the
// logical volume underneath it, and reports which crypt container (if any) sits
// in between.
//
// LVM commands have to be aimed at the LV, not at the mapping: `lvresize
// /dev/mapper/haify_vg0_data_data` and `lvremove` of the same do not address an
// LV at all. The container name cannot be split back into pool and volume
// unambiguously — both may contain underscores — so the volume record is the
// authority, and a missing one is an error rather than a guess.
func (rm *ResourceManager) backingLVFor(ctx context.Context, resource string, volumeID uint32, diskPath string) (lvPath, pool, volume string, err error) {
	log := rm.controller.traceLogger()
	if !luksIsMapperPath(diskPath) || !rm.resourceIsEncrypted(ctx, resource) {
		log.Debug("ResourceManager.backingLVFor: not a Haify crypt container, path is the LV", zap.String("resource", resource),
			zap.Uint32("volume", volumeID), zap.String("disk", diskPath))
		return diskPath, "", "", nil
	}
	if rm.controller.db == nil {
		log.Debug("ResourceManager.backingLVFor: encrypted but no database", zap.String("resource", resource))
		return "", "", "", fmt.Errorf(
			"volume %d of %q is encrypted and the database is unavailable, so the logical volume beneath %s cannot be identified",
			volumeID, resource, diskPath)
	}
	start := time.Now()
	vols, lerr := rm.controller.db.ListVolumes(ctx, resource)
	log.Debug("ResourceManager.backingLVFor: db list volumes", zap.String("resource", resource),
		zap.Int("count", len(vols)), zap.Duration("took", time.Since(start)), zap.Error(lerr))
	if lerr != nil {
		return "", "", "", fmt.Errorf("list volumes of %q: %w", resource, lerr)
	}
	rec := findVolumeRecord(vols, volumeID)
	if rec == nil || rec.Pool == "" || rec.VolumeName == "" {
		log.Debug("ResourceManager.backingLVFor: no usable volume record", zap.String("resource", resource),
			zap.Uint32("volume", volumeID), zap.Bool("found", rec != nil))
		return "", "", "", fmt.Errorf(
			"volume %d of %q is encrypted but has no usable volume record, so the logical volume beneath %s cannot be identified",
			volumeID, resource, diskPath)
	}
	log.Debug("ResourceManager.backingLVFor: resolved LV beneath container", zap.String("resource", resource),
		zap.Uint32("volume", volumeID), zap.String("pool", rec.Pool), zap.String("lv", rec.VolumeName))
	return fmt.Sprintf("/dev/%s/%s", rec.Pool, rec.VolumeName), rec.Pool, rec.VolumeName, nil
}
