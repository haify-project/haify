package opennebula

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"go.uber.org/zap"
)

// Live migration. The libvirt hook on the two hosts does the DRBD side: the
// destination opens the dual-primary window and promotes when the guest
// arrives, the source demotes and closes it when the guest has left, and the
// destination demotes if the guest never started there. The transfer driver
// makes sure the destination holds every disk before that, and afterwards
// that exactly one host is left holding each disk Primary.
//
// As the system datastore's driver (TM_MAD=haify on it), an action first runs
// the ssh driver's, which handles the VM directory and calls this driver
// back, with the system driver's name as a sixth argument, for the disks.
// The ssh driver's failmigrate does not call back, so it is done here.
//
//	premigrate|postmigrate|failmigrate src_host dst_host remote_system_dir vm_id ds_id [tm_mad]
//	stdin: the VM template, base64

// TMMigrate runs premigrate, postmigrate or failmigrate.
func (d *Driver) TMMigrate(ctx context.Context, action string, args []string, stdin []byte) error {
	log := zap.L()
	if len(args) < 5 {
		return fmt.Errorf("expected src_host dst_host system_dir vm_id ds_id, got %d arguments", len(args))
	}
	srcHost, dstHost := args[0], args[1]
	callback := len(args) > 5
	log.Debug("Driver.TMMigrate: start", zap.String("action", action), zap.String("src", srcHost),
		zap.String("dst", dstHost), zap.Bool("callback", callback))
	if !callback {
		if err := d.Delegate(ctx, "ssh", action, args, stdin); err != nil {
			return err
		}
		if action != "failmigrate" {
			return nil // the disks came back through the callback
		}
	}
	if srcHost == dstHost {
		return nil
	}
	vm, err := ParseVM64(stdin)
	if err != nil {
		return err
	}
	var errs []error
	for i := range vm.Disks {
		disk := &vm.Disks[i]
		if disk.TMMad != "haify" || disk.Source == "" {
			continue
		}
		td, _, err := d.resolveDisk(ctx, vm, disk)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		switch action {
		case "premigrate":
			err = d.premigrateDisk(ctx, td, dstHost)
		case "postmigrate":
			err = d.settleDisk(ctx, td, dstHost, srcHost)
		case "failmigrate":
			err = d.settleDisk(ctx, td, srcHost, dstHost)
		default:
			err = fmt.Errorf("unknown migrate action %q", action)
		}
		log.Debug("Driver.TMMigrate: disk done", zap.String("action", action), zap.String("resource", td.resource),
			zap.Error(err))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", td.resource, err))
		}
	}
	return errors.Join(errs...)
}

// premigrateDisk makes the destination hold the disk before the guest moves.
func (d *Driver) premigrateDisk(ctx context.Context, td *tmDisk, dstHost string) error {
	node, err := nodeFor(ctx, td.b, dstHost)
	if err != nil {
		return err
	}
	return ensureMember(ctx, td.b, td.resource, node)
}

// settleDisk leaves the disk Primary on keepHost only, closes the
// dual-primary window, and lets goneHost go of it. After a migration
// keepHost is the destination; after a failed one, the source.
func (d *Driver) settleDisk(ctx context.Context, td *tmDisk, keepHost, goneHost string) error {
	log := zap.L()
	keep, err := nodeFor(ctx, td.b, keepHost)
	if err != nil {
		return err
	}
	gone, err := nodeFor(ctx, td.b, goneHost)
	if err != nil {
		return err
	}
	prim, err := primaries(ctx, td.b, td.resource)
	if err != nil {
		return err
	}
	var errs []error
	if slices.Contains(prim, gone) {
		if len(prim) == 1 {
			// The guest is not where OpenNebula thinks: leave it be.
			log.Debug("Driver.settleDisk: only the other host is Primary, leaving it", zap.String("resource", td.resource),
				zap.String("primary", gone))
			return fmt.Errorf("%s is Primary on %s only, not on %s", td.resource, gone, keep)
		}
		err := td.b.SetSecondary(ctx, td.resource, gone)
		log.Debug("Driver.settleDisk: demoted the host the guest left", zap.String("resource", td.resource),
			zap.String("node", gone), zap.Error(err))
		if err != nil {
			errs = append(errs, fmt.Errorf("demoting on %s: %w", gone, err))
		}
	}
	err = td.b.SetDualPrimary(ctx, td.resource, false)
	log.Debug("Driver.settleDisk: closed the dual-primary window", zap.String("resource", td.resource), zap.Error(err))
	if err != nil {
		errs = append(errs, fmt.Errorf("closing the dual-primary window: %w", err))
	}
	leave(ctx, td.b, td.resource, gone)
	return errors.Join(errs...)
}
