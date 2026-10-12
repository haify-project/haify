package csi

import (
	"context"
	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// volumeSource is where a new volume's initial contents come from: a snapshot
// on a specific node, which the new volume's placement must therefore include
// so the copy can run locally.
type volumeSource struct {
	// snapshot names what to copy from. The controller finds its device, which
	// differs between LVM and ZFS pools.
	snapshot *haifypb.PopulateSnapshotSource
	// node holds the snapshot locally and must end up holding a replica of the
	// new volume, since PopulateVolume runs the copy there.
	node string
	// cleanup removes anything created purely to serve this copy (the temporary
	// snapshot taken when cloning a live volume). Nil when there is nothing to
	// undo.
	cleanup func()
}

// resolveVolumeSource turns a CSI VolumeContentSource into the snapshot to
// copy from.
//
// Cloning an in-use volume by reading its backing store directly would capture a
// torn, mid-write image, so a clone first takes a snapshot of the source and
// copies from that instead: the snapshot is a single point in time, and the
// caller drops it again once the copy is done.
//
// wantGiB is the size the new volume was requested at, checked here rather than
// by the caller because this is the last point before the first side effect: a
// clone takes a snapshot of its source, and a request that can only end in a
// failed copy should not have created one.
func (s *controllerServer) resolveVolumeSource(ctx context.Context, cs *csi.VolumeContentSource, wantGiB uint32) (*volumeSource, error) {
	switch src := cs.GetType().(type) {
	case *csi.VolumeContentSource_Snapshot:
		id := src.Snapshot.GetSnapshotId()
		resource, node, snapName, err := parseSnapshotID(id)
		if err != nil {
			return nil, status.Errorf(codes.NotFound, "snapshot %q: %v", id, err)
		}
		res, gerr := s.backend.GetResource(ctx, resource)
		if gerr != nil || res == nil {
			return nil, status.Errorf(codes.NotFound, "source volume %q of snapshot %q no longer exists", resource, id)
		}
		if _, _, perr := snapshotSource(res); perr != nil {
			return nil, status.Error(codes.FailedPrecondition, perr.Error())
		}
		if err := checkSourceFits(res, wantGiB); err != nil {
			return nil, err
		}
		return &volumeSource{snapshot: oneNodeSnapshot(res, snapName), node: node}, nil

	case *csi.VolumeContentSource_Volume:
		srcID := src.Volume.GetVolumeId()
		res, gerr := s.backend.GetResource(ctx, srcID)
		if gerr != nil || res == nil {
			return nil, status.Errorf(codes.NotFound, "source volume %q not found", srcID)
		}
		volumePath, node, perr := snapshotSource(res)
		if perr != nil {
			return nil, status.Error(codes.FailedPrecondition, perr.Error())
		}

		if err := checkSourceFits(res, wantGiB); err != nil {
			return nil, err
		}

		// Snapshot the source so the clone copies a consistent point in time
		// even if the source is mounted and being written to.
		tmp := sanitizeSnapshotName("clone-src-" + srcID)
		if err := s.backend.CreateSnapshot(ctx, volumePath, tmp, node); err != nil {
			return nil, status.Errorf(codes.Internal, "snapshot source %q for clone: %v", srcID, err)
		}
		return &volumeSource{
			snapshot: oneNodeSnapshot(res, tmp),
			node:     node,
			cleanup: func() {
				// Best effort: a leaked temporary snapshot wastes COW space but
				// does not corrupt anything, and must not fail the clone.
				_ = s.backend.DeleteSnapshot(context.WithoutCancel(ctx), volumePath, tmp, node)
			},
		}, nil

	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported volume content source")
	}
}

// oneNodeSnapshot names a CSI snapshot of res for PopulateVolume: a snapshot
// of the volume snapshotSource picks, taken on one node (CreateSnapshot), not
// on every replica.
func oneNodeSnapshot(res *haifypb.ResourceInfo, snapName string) *haifypb.PopulateSnapshotSource {
	return &haifypb.PopulateSnapshotSource{Resource: res.GetName(), VolumeId: res.GetVolumes()[0].GetVolumeId(), Name: snapName}
}

// describeSource names a volume source in an error message.
func describeSource(src *volumeSource) string {
	return fmt.Sprintf("snapshot %s of %s on %s", src.snapshot.GetName(), src.snapshot.GetResource(), src.node)
}

// contentSourceOf echoes the request's content source back in the response, as
// the CSI spec requires for a volume created from a snapshot or another volume.
func contentSourceOf(cs *csi.VolumeContentSource) *csi.VolumeContentSource {
	if cs == nil {
		return nil
	}
	switch src := cs.GetType().(type) {
	case *csi.VolumeContentSource_Snapshot:
		return &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
			Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: src.Snapshot.GetSnapshotId()},
		}}
	case *csi.VolumeContentSource_Volume:
		return &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
			Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: src.Volume.GetVolumeId()},
		}}
	}
	return nil
}

// sourceSizeGiB reports the source resource's size in whole GiB, used to reject
// a restore into a volume smaller than the data it must hold.
func sourceSizeGiB(res *haifypb.ResourceInfo) uint32 {
	if vols := res.GetVolumes(); len(vols) > 0 {
		return uint32(vols[0].GetSizeGb())
	}
	return 0
}

// checkSourceFits rejects a restore or clone into a volume smaller than its
// source.
//
// The copy is a whole-device read of the source onto the new volume, so a
// smaller target cannot succeed: it fails inside PopulateVolume, after the
// resource has been created on every replica and after a clone has snapshotted
// its source, and the new volume is then torn down again. Kubernetes retries
// the PVC forever, repeating that cycle, and the operator sees only a
// dd/copy error. Refusing the request instead puts the real reason —
// requested smaller than the source — on the PVC's events immediately.
//
// wantGiB of 0 means the caller stated no capacity; there is nothing to compare
// against, so the copy is left to decide.
func checkSourceFits(res *haifypb.ResourceInfo, wantGiB uint32) error {
	srcGiB := sourceSizeGiB(res)
	if wantGiB == 0 || srcGiB == 0 || srcGiB <= wantGiB {
		return nil
	}
	return status.Errorf(codes.InvalidArgument,
		"requested size %d GiB is smaller than source volume %q (%d GiB)", wantGiB, res.GetName(), srcGiB)
}
