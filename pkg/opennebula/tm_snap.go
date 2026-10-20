package opennebula

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"go.uber.org/zap"
)

// TMSnapshot takes, reverts or deletes a disk snapshot: a Haify snapshot of
// the disk's resource on every replica, named one-snap-<snap_id>. A snapshot
// of a running guest is crash-consistent. Reverting needs the guest off
// (OpenNebula reverts in POWEROFF), so the disk is Primary nowhere.
//
//	snap_create|snap_create_live|snap_revert|snap_delete host:remote_system_ds/disk.i snap_id vm_id ds_id
func (d *Driver) TMSnapshot(ctx context.Context, action string, args []string) error {
	log := zap.L()
	if len(args) < 3 {
		return fmt.Errorf("expected host:disk_path snap_id vm_id ds_id, got %d arguments", len(args))
	}
	loc := ParseLocation(args[0])
	vmID, err := strconv.Atoi(args[2])
	if err != nil {
		return fmt.Errorf("vm id %q: %w", args[2], err)
	}
	td, ok, err := d.haifyDisk(ctx, loc, vmID)
	if err != nil {
		return err
	}
	if !ok {
		return d.Delegate(ctx, "ssh", action, args, nil)
	}
	snap := snapshotName(args[1])
	start := time.Now()
	switch action {
	case "snap_create", "snap_create_live":
		err = td.b.CreateResourceSnapshot(ctx, td.resource, snap)
	case "snap_revert":
		err = td.b.RollbackResourceSnapshot(ctx, td.resource, snap)
	case "snap_delete":
		err = td.b.DeleteResourceSnapshot(ctx, td.resource, snap)
	default:
		err = fmt.Errorf("unknown snapshot action %q", action)
	}
	log.Debug("Driver.TMSnapshot: done", zap.String("action", action), zap.String("resource", td.resource),
		zap.String("snapshot", snap), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("%s %s of %s: %w", action, snap, td.resource, err)
	}
	return nil
}

// TMCpds saves a disk, or one of its snapshots, as the image SOURCE
// (onevm disk-saveas). The image becomes a resource of its own on the
// disk's nodes.
//
//	cpds host:remote_system_ds/disk.i fe:SOURCE snap_id vm_id ds_id
func (d *Driver) TMCpds(ctx context.Context, args []string) error {
	log := zap.L()
	if len(args) < 5 {
		return fmt.Errorf("expected host:disk_path fe:SOURCE snap_id vm_id ds_id, got %d arguments", len(args))
	}
	loc, dst := ParseLocation(args[0]), ParseLocation(args[1])
	vmID, err := strconv.Atoi(args[3])
	if err != nil {
		return fmt.Errorf("vm id %q: %w", args[3], err)
	}
	td, ok, err := d.haifyDisk(ctx, loc, vmID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("disk %s of VM %d is not on a Haify datastore", loc.Path, vmID)
	}
	m := imageResourceRE.FindStringSubmatch(dst.Path)
	if m == nil {
		return fmt.Errorf("%q is not a Haify image source", dst.Path)
	}
	src, err := findResource(ctx, td.b, td.resource)
	if err != nil {
		return err
	}
	if src == nil {
		return fmt.Errorf("disk resource %s does not exist", td.resource)
	}
	existing, err := findResource(ctx, td.b, dst.Path)
	if err != nil {
		return err
	}
	if existing != nil {
		log.Debug("Driver.TMCpds: replacing the empty target", zap.String("resource", dst.Path))
		if err := td.b.DeleteResource(ctx, dst.Path); err != nil {
			return fmt.Errorf("replacing %s: %w", dst.Path, err)
		}
	}
	snap := ""
	if args[2] != "-1" {
		snap = snapshotName(args[2])
	}
	labels := map[string]string{LabelManaged: managedValue, LabelImage: imageIDOf(dst.Path),
		LabelDatastore: args[4]}
	log.Debug("Driver.TMCpds: saving", zap.String("resource", td.resource), zap.String("snapshot", snap),
		zap.String("target", dst.Path))
	return copyResource(ctx, td.b, src, snap, dst.Path, resourceBytes(src), labels)
}

// imageIDOf is the image id in an image resource's name.
func imageIDOf(resource string) string {
	m := imageIDRE.FindStringSubmatch(resource)
	if m == nil {
		return ""
	}
	return m[1]
}
