package opennebula

import (
	"context"
	"fmt"
	"strconv"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// tmDisk is a VM disk on Haify, as a transfer action addresses it.
type tmDisk struct {
	vm       *VM
	disk     *Disk
	resource string
	ds       *Datastore // the disk's image datastore
	b        Backend
}

// haifyDisk resolves the disk at loc of VM vmID. ok is false when the disk is
// not a Haify disk (a volatile disk or one from another datastore, which the
// system datastore's files driver handles).
func (d *Driver) haifyDisk(ctx context.Context, loc Location, vmID int) (*tmDisk, bool, error) {
	id := loc.DiskID()
	if id < 0 {
		return nil, false, nil
	}
	vm, err := ShowVM(ctx, d.One, vmID)
	if err != nil {
		return nil, false, err
	}
	disk := vm.Disk(id)
	if disk == nil || disk.TMMad != "haify" || disk.Source == "" {
		zap.L().Debug("Driver.haifyDisk: not a Haify disk", zap.Int("vm_id", vmID), zap.Int("disk_id", id),
			zap.Bool("found", disk != nil))
		return nil, false, nil
	}
	return d.resolveDisk(ctx, vm, disk)
}

func (d *Driver) resolveDisk(ctx context.Context, vm *VM, disk *Disk) (*tmDisk, bool, error) {
	res, err := diskResource(vm.ID, disk)
	if err != nil {
		return nil, false, err
	}
	ds, err := ShowDatastore(ctx, d.One, disk.DatastoreID)
	if err != nil {
		return nil, false, err
	}
	b, err := d.backendFor(ds)
	if err != nil {
		return nil, false, err
	}
	zap.L().Debug("Driver.resolveDisk: resolved", zap.Int("vm_id", vm.ID), zap.Int("disk_id", disk.ID),
		zap.String("resource", res), zap.String("ds_id", disk.DatastoreID))
	return &tmDisk{vm: vm, disk: disk, resource: res, ds: ds, b: b}, true, nil
}

// TMLink gives host a disk and links it into the VM directory: ln for a
// persistent image (the image itself), clone for a non-persistent one (a
// private copy of it, made first).
//
//	ln|clone fe:SOURCE host:remote_system_ds/disk.i vm_id ds_id
func (d *Driver) TMLink(ctx context.Context, clone bool, args []string) error {
	log := zap.L()
	if len(args) < 4 {
		return fmt.Errorf("expected fe:SOURCE host:disk_path vm_id ds_id, got %d arguments", len(args))
	}
	dst := ParseLocation(args[1])
	vmID, err := strconv.Atoi(args[2])
	if err != nil {
		return fmt.Errorf("vm id %q: %w", args[2], err)
	}
	log.Debug("Driver.TMLink: start", zap.Bool("clone", clone), zap.String("src", args[0]),
		zap.String("host", dst.Host), zap.String("path", dst.Path), zap.Int("vm_id", vmID))
	td, ok, err := d.haifyDisk(ctx, dst, vmID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("disk %s of VM %d is not on a Haify datastore", dst.Path, vmID)
	}
	if clone {
		if err := d.cloneDisk(ctx, td); err != nil {
			return err
		}
	}
	node, err := nodeFor(ctx, td.b, dst.Host)
	if err != nil {
		return err
	}
	if err := ensureMember(ctx, td.b, td.resource, node); err != nil {
		return err
	}
	labels := map[string]string{LabelVM: strconv.Itoa(vmID)}
	_, lerr := td.b.SetResourceLabels(ctx, td.resource, labels, nil)
	log.Debug("Driver.TMLink: labelled", zap.String("resource", td.resource), zap.Error(lerr))
	if err := d.linkDisk(ctx, dst.Host, td.resource, dst.Path); err != nil {
		return err
	}
	if hotplug(td.vm.LCMState) {
		err := td.b.PromoteForNode(ctx, td.resource, node)
		log.Debug("Driver.TMLink: promoted for a hot attach", zap.String("resource", td.resource),
			zap.String("node", node), zap.Error(err))
		if err != nil {
			return fmt.Errorf("making %s Primary on %s for the running guest: %w", td.resource, node, err)
		}
	}
	return nil
}

// release lets host go of a disk the VM no longer uses: Secondary again
// after a hot detach (the hook demotes everything else when the guest
// stops), the link removed, and a diskless client detached.
func (d *Driver) release(ctx context.Context, td *tmDisk, loc Location) error {
	node, nerr := nodeFor(ctx, td.b, loc.Host)
	if nerr == nil && hotplug(td.vm.LCMState) {
		err := td.b.SetSecondary(ctx, td.resource, node)
		zap.L().Debug("Driver.release: demoted after a hot detach", zap.String("resource", td.resource),
			zap.String("node", node), zap.Error(err))
		if err != nil {
			return fmt.Errorf("releasing %s on %s: %w", td.resource, node, err)
		}
	}
	d.unlinkDisk(ctx, loc.Host, loc.Path)
	if nerr == nil {
		leave(ctx, td.b, td.resource, node)
	}
	return nil
}

// cloneDisk makes a non-persistent disk's private copy of its image, as
// large as the disk asks, on the image's nodes.
func (d *Driver) cloneDisk(ctx context.Context, td *tmDisk) error {
	log := zap.L()
	src, err := findResource(ctx, td.b, td.disk.Source)
	if err != nil {
		return err
	}
	if src == nil {
		return fmt.Errorf("image resource %s does not exist", td.disk.Source)
	}
	existing, err := findResource(ctx, td.b, td.resource)
	if err != nil {
		return err
	}
	if existing != nil {
		// A retried prolog: the copy may be partial, and nothing has used it.
		log.Debug("Driver.cloneDisk: dropping a leftover copy", zap.String("resource", td.resource))
		if err := td.b.DeleteResource(ctx, td.resource); err != nil {
			return fmt.Errorf("dropping the leftover copy %s: %w", td.resource, err)
		}
	}
	size := uint64(td.disk.Size) * uint64(mib)
	if s := resourceBytes(src); s > size {
		size = s
	}
	labels := map[string]string{LabelManaged: managedValue, LabelVM: strconv.Itoa(td.vm.ID),
		LabelDatastore: td.disk.DatastoreID}
	return copyResource(ctx, td.b, src, "", td.resource, size, labels)
}

// copyResource fills a new resource name, of size bytes, on src's nodes,
// with src's snapshot snap, or, with snap empty, with src as it is now: a
// snapshot taken on every replica at one instant, copied, then dropped.
func copyResource(ctx context.Context, b Backend, src *haifypb.ResourceInfo, snap, name string, size uint64,
	labels map[string]string) error {
	log := zap.L()
	nodes := src.GetNodes()
	if len(nodes) == 0 {
		return fmt.Errorf("%s has no replica to copy from", src.GetName())
	}
	start := time.Now()
	if snap == "" {
		snap = fmt.Sprintf("one-copy-%d", time.Now().UnixNano())
		if err := b.CreateResourceSnapshot(ctx, src.GetName(), snap); err != nil {
			log.Debug("copyResource: snapshot failed", zap.String("source", src.GetName()), zap.Error(err))
			return fmt.Errorf("snapshotting %s: %w", src.GetName(), err)
		}
		defer func() {
			err := b.DeleteResourceSnapshot(ctx, src.GetName(), snap)
			log.Debug("copyResource: dropped snapshot", zap.String("source", src.GetName()), zap.String("snapshot", snap),
				zap.Error(err))
		}()
	}
	req := createRequest(name, size, src.GetVolumes(), labels)
	req.Nodes = nodes
	if err := b.CreateResourceRequest(ctx, req); err != nil {
		log.Debug("copyResource: create failed", zap.String("resource", name), zap.Error(err))
		return fmt.Errorf("creating %s: %w", name, err)
	}
	copied, err := b.PopulateVolumeFromSnapshot(ctx, name, 0,
		&haifypb.PopulateSnapshotSource{Resource: src.GetName(), VolumeId: 0, Name: snap, Replicated: true}, nodes[0])
	log.Debug("copyResource: populated", zap.String("resource", name), zap.String("source", src.GetName()),
		zap.Uint64("bytes", copied), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		if derr := b.DeleteResource(ctx, name); derr != nil {
			log.Debug("copyResource: dropping the partial copy failed", zap.String("resource", name), zap.Error(derr))
		}
		return fmt.Errorf("copying %s into %s: %w", src.GetName(), name, err)
	}
	// The copy is Primary nowhere until a host starts a VM on it; the
	// controller only warns when its own demote after the copy fails.
	prim, err := primaries(ctx, b, name)
	if err != nil {
		return err
	}
	for _, node := range prim {
		if err := b.SetSecondary(ctx, name, node); err != nil {
			return fmt.Errorf("%s stayed Primary on %s after the copy: %w", name, node, err)
		}
	}
	return nil
}

// createRequest is a new single-volume resource as the driver makes them.
func createRequest(name string, size uint64, like []*haifypb.VolumeInfo, labels map[string]string) *haifypb.CreateResourceRequest {
	req := &haifypb.CreateResourceRequest{
		Name: name, Protocol: "C", SizeBytes: size, Labels: labels,
		// A guest whose disk loses quorum waits instead of seeing I/O errors,
		// and carries on when quorum returns.
		DrbdOptions: map[string]string{"on-no-quorum": "suspend-io", "on-no-data-accessible": "suspend-io"},
	}
	if len(like) > 0 {
		req.Pool = like[0].GetPool()
	}
	return req
}

// resourceBytes is the size of a resource's first volume.
func resourceBytes(r *haifypb.ResourceInfo) uint64 {
	vols := r.GetVolumes()
	if len(vols) == 0 {
		return 0
	}
	if b := vols[0].GetSizeBytes(); b > 0 {
		return b
	}
	return vols[0].GetSizeGb() << 30
}

// TMDelete removes a non-persistent disk's copy, or the VM directory.
//
//	delete host:remote_system_ds/disk.i|host:remote_system_ds/ vm_id ds_id
func (d *Driver) TMDelete(ctx context.Context, args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("expected host:path vm_id ds_id, got %d arguments", len(args))
	}
	loc := ParseLocation(args[0])
	vmID, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("vm id %q: %w", args[1], err)
	}
	td, ok, err := d.haifyDisk(ctx, loc, vmID)
	if err != nil {
		return err
	}
	if !ok {
		return d.Delegate(ctx, "ssh", "delete", args, nil)
	}
	zap.L().Debug("Driver.TMDelete: deleting disk", zap.String("host", loc.Host), zap.String("resource", td.resource),
		zap.Bool("persistent", yes(td.disk.Persistent)))
	if err := d.release(ctx, td, loc); err != nil {
		return err
	}
	if yes(td.disk.Persistent) || !yes(td.disk.Clone) {
		return nil
	}
	r, err := findResource(ctx, td.b, td.resource)
	if err != nil || r == nil {
		return err
	}
	if err := td.b.DeleteResource(ctx, td.resource); err != nil {
		return fmt.Errorf("deleting %s: %w", td.resource, err)
	}
	return nil
}

// TMMvds returns a persistent disk to its datastore when the VM is done
// with it: the host lets go of it. The data is the image already.
//
//	mvds host:remote_system_ds/disk.i fe:SOURCE vm_id ds_id
func (d *Driver) TMMvds(ctx context.Context, args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("expected host:disk_path fe:SOURCE vm_id ds_id, got %d arguments", len(args))
	}
	loc := ParseLocation(args[0])
	vmID, err := strconv.Atoi(args[2])
	if err != nil {
		return fmt.Errorf("vm id %q: %w", args[2], err)
	}
	td, ok, err := d.haifyDisk(ctx, loc, vmID)
	if err != nil || !ok {
		return err
	}
	if err := d.release(ctx, td, loc); err != nil {
		return err
	}
	_, lerr := td.b.SetResourceLabels(ctx, td.resource, nil, []string{LabelVM})
	zap.L().Debug("Driver.TMMvds: released", zap.String("resource", td.resource), zap.Error(lerr))
	return nil
}

// TMMv moves a VM's disks between hosts, or between a host and the frontend
// (stop, undeploy, resume, cold migration, host failure). The disk's data
// does not move: the destination host joins the disk, and the system
// datastore's mv carries the link with the rest of the VM directory.
//
//	mv hostA:system_ds/disk.i|hostA:system_ds/ hostB:system_ds/disk.i|hostB:system_ds/ vm_id ds_id
func (d *Driver) TMMv(ctx context.Context, args []string) error {
	if len(args) < 4 {
		return fmt.Errorf("expected src dst vm_id ds_id, got %d arguments", len(args))
	}
	src, dst := ParseLocation(args[0]), ParseLocation(args[1])
	vmID, err := strconv.Atoi(args[2])
	if err != nil {
		return fmt.Errorf("vm id %q: %w", args[2], err)
	}
	td, ok, err := d.haifyDisk(ctx, src, vmID)
	if err != nil {
		return err
	}
	if !ok {
		return d.Delegate(ctx, "ssh", "mv", args, nil)
	}
	lcm := td.vm.LCMState
	zap.L().Debug("Driver.TMMv: moving disk", zap.String("resource", td.resource), zap.String("src", src.Host),
		zap.String("dst", dst.Host), zap.Int("lcm_state", lcm))
	if !leavingHost(lcm) {
		node, err := nodeFor(ctx, td.b, dst.Host)
		if err != nil {
			return err
		}
		if err := ensureMember(ctx, td.b, td.resource, node); err != nil {
			return err
		}
		if hostFailed(lcm) || sourceGone(lcm) {
			// Nothing will copy the VM directory from the source.
			if err := d.linkDisk(ctx, dst.Host, td.resource, dst.Path); err != nil {
				return err
			}
		}
	}
	if !sourceGone(lcm) && src.Host != dst.Host {
		if node, err := nodeFor(ctx, td.b, src.Host); err == nil {
			leave(ctx, td.b, td.resource, node)
		}
	}
	return nil
}

// TMResize grows a disk; OpenNebula then tells a running guest.
//
//	resize host:remote_system_ds/disk.i size_mb vm_id ds_id
func (d *Driver) TMResize(ctx context.Context, args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("expected host:disk_path size vm_id ds_id, got %d arguments", len(args))
	}
	loc := ParseLocation(args[0])
	sizeMB, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || sizeMB <= 0 {
		return fmt.Errorf("size %q is not a number of MiB", args[1])
	}
	vmID, err := strconv.Atoi(args[2])
	if err != nil {
		return fmt.Errorf("vm id %q: %w", args[2], err)
	}
	td, ok, err := d.haifyDisk(ctx, loc, vmID)
	if err != nil {
		return err
	}
	if !ok {
		return d.Delegate(ctx, "ssh", "resize", args, nil)
	}
	start := time.Now()
	err = td.b.ResizeVolumeBytes(ctx, td.resource, 0, uint64(sizeMB*mib))
	zap.L().Debug("Driver.TMResize: resized", zap.String("resource", td.resource), zap.Int64("size_mb", sizeMB),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("growing %s to %d MiB: %w", td.resource, sizeMB, err)
	}
	return nil
}
