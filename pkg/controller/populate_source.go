package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
)

// Where PopulateVolume reads a snapshot from.
//
// The clients that make a volume from a snapshot (CSI, the Cinder driver, the
// XCP-ng SR) used to name the device to copy themselves, as /dev/<vg>/<snap>.
// That is an LVM path. On a ZFS pool it does not exist, and a snapshot of a
// zvol has no block device at all unless the zvol has snapdev=visible. The
// controller knows the pool type from the volume's record, so it takes the
// snapshot by name and finds the device itself.
//
// A ZFS snapshot is read through a temporary read-only clone rather than by
// setting snapdev=visible on the source zvol. The property would turn every
// snapshot of the zvol into a device node at once, and two copies from one
// zvol would race to set it back, pulling the device from under the other.

// SnapshotSource is a snapshot of one volume of a resource, as PopulateVolume
// takes it.
type SnapshotSource struct {
	Resource string
	VolumeID uint32
	Name     string
	// Replicated is true for a snapshot taken on every replica
	// (CreateResourceSnapshot), false for one taken on one node
	// (CreateSnapshot). The two are named differently on disk.
	Replicated bool
}

// snapshotSourceNameRe is what a snapshot name may look like before it goes
// into a shell command: what LVM and ZFS both accept, minus anything a shell
// would act on.
var snapshotSourceNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+:-]*$`)

// snapshotBackingName is the name the storage gives a snapshot of vol:
// "<vg>/<lv>" on LVM, "<pool>/<zvol>@<snap>" on ZFS.
func snapshotBackingName(vol *database.Volume, name string, replicated bool) string {
	t := snapTarget{vol: vol, zfs: isZFSDevice(vol.Device)}
	switch {
	case t.zfs && replicated:
		return t.zfsName(name)
	case t.zfs:
		return vol.Pool + "/" + vol.VolumeName + "@" + name
	case replicated:
		return vol.Pool + "/" + t.lvName(name)
	default:
		return vol.Pool + "/" + name
	}
}

// populateCloneName is the temporary clone a ZFS snapshot is read through
// while it fills volume volumeID of target. A clone lives in the pool of its
// snapshot; the name is the target's, so two copies never share one.
func populateCloneName(pool, target string, volumeID uint32) string {
	return fmt.Sprintf("%s/haify-populate-%s-%d", pool, target, volumeID)
}

// zfsCloneForReadCmd clones snapshot read-only to clone and waits for the
// clone's device node, which udev creates after zfs returns. A clone left by
// a copy that died half-way has the same name and goes first.
func zfsCloneForReadCmd(snapshot, clone string) string {
	dev := "/dev/zvol/" + clone
	return fmt.Sprintf("set -e; sudo zfs destroy %s >/dev/null 2>&1 || true; "+
		"sudo zfs clone -o readonly=on %s %s; sudo udevadm settle >/dev/null 2>&1 || true; "+
		"for i in $(seq 100); do [ -e %s ] && break; sleep 0.1; done; [ -e %s ]",
		clone, snapshot, clone, dev, dev)
}

// zfsDestroyCloneCmd removes the clone again. udev may still be probing the
// device the copy just closed, which makes zfs call it busy, so it tries twice.
func zfsDestroyCloneCmd(clone string) string {
	return fmt.Sprintf("sudo udevadm settle >/dev/null 2>&1; sudo zfs destroy %s || { sleep 2; sudo zfs destroy %s; }",
		clone, clone)
}

// resolveSnapshotSource returns the device to read src from on address, and
// a function that removes whatever was made to expose it. target and
// volumeID are the volume being filled.
func (sm *SnapshotManager) resolveSnapshotSource(ctx context.Context, address, node, target string, volumeID uint32,
	src SnapshotSource) (string, func(), error) {
	if strings.TrimSpace(src.Resource) == "" {
		return "", nil, fmt.Errorf("the source snapshot's resource is required")
	}
	if !snapshotSourceNameRe.MatchString(src.Name) {
		return "", nil, fmt.Errorf("%q is not a snapshot name", src.Name)
	}
	if sm.controller.db == nil {
		return "", nil, fmt.Errorf("database not available")
	}
	vol, err := sm.controller.resources.dbVolume(ctx, src.Resource, int(src.VolumeID))
	if err != nil {
		return "", nil, fmt.Errorf("source snapshot %s of %s: %w", src.Name, src.Resource, err)
	}
	backing := snapshotBackingName(vol, src.Name, src.Replicated)
	dep := sm.controller.deployment

	if !isZFSDevice(vol.Device) {
		res, err := dep.Exec(ctx, []string{address}, "sudo lvs --noheadings -o lv_name "+backing)
		if err != nil {
			return "", nil, fmt.Errorf("look for snapshot %s of %s on %s: %w", src.Name, src.Resource, node, err)
		}
		if !res.AllSuccess() {
			return "", nil, fmt.Errorf("snapshot %s of %s (%s) is not on %s", src.Name, src.Resource, backing, node)
		}
		// A thin snapshot has no device node until it is activated;
		// PopulateVolume activates it before reading.
		return "/dev/" + backing, func() {}, nil
	}

	clone := populateCloneName(vol.Pool, target, volumeID)
	release := func() {
		if err := execFailure(dep.Exec(context.WithoutCancel(ctx), []string{address}, zfsDestroyCloneCmd(clone))); err != nil {
			sm.controller.logger.Warn("Failed to remove the temporary clone a snapshot was copied from; remove it with zfs destroy",
				zap.String("clone", clone), zap.String("node", node), zap.Error(err))
		}
	}
	if err := execFailure(dep.Exec(ctx, []string{address}, zfsCloneForReadCmd(backing, clone))); err != nil {
		release()
		return "", nil, fmt.Errorf("expose snapshot %s of %s (%s) on %s for reading: %w", src.Name, src.Resource, backing, node, err)
	}
	return "/dev/zvol/" + clone, release, nil
}

// PopulateVolumeFromSnapshot fills an already-created, still-empty resource
// from a snapshot of another resource's volume, on node, which must hold both.
// It is PopulateVolume with the device found by the controller, on LVM and ZFS
// pools alike.
func (sm *SnapshotManager) PopulateVolumeFromSnapshot(ctx context.Context, resource string, volumeID uint32,
	src SnapshotSource, node string) (uint64, error) {
	if strings.TrimSpace(resource) == "" {
		return 0, fmt.Errorf("resource is required")
	}
	address := sm.controller.ResolveHost(node)
	if address == "" {
		return 0, fmt.Errorf("node not found: %s", node)
	}
	device, release, err := sm.resolveSnapshotSource(ctx, address, node, resource, volumeID, src)
	if err != nil {
		return 0, err
	}
	defer release()
	return sm.PopulateVolume(ctx, resource, volumeID, device, node)
}
