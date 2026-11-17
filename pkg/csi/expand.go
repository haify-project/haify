package csi

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Volume expansion.
//
// Growing a volume is two things: the DRBD device and its backing volumes,
// which the Haify controller grows on every replica, and the filesystem on
// it, which is grown where it is mounted.
//
// For an ordinary volume that is the node the Pod runs on: the controller
// plugin asks for node expansion, and kubelet has the node plugin grow the
// filesystem there (NodeExpandVolume).
//
// A shared volume's filesystem is mounted on a storage node, by its NFS
// gateway, and the nodes with Pods only mount the export. No node plugin call
// reaches that filesystem, so the Haify controller grows it as part of the
// resize, on whichever node is serving the gateway. ControllerExpandVolume
// has then done all there is to do and asks for no node expansion: an NFS
// client sees the new size by itself.

// retryableResize reports whether a failed resize left the volume grown with
// only a later step outstanding, which repeating the request finishes. The
// Haify client marks such an error; it is recognised by its method so that a
// backend other than that client can mark its own.
func retryableResize(err error) bool {
	var r interface{ Retryable() bool }
	return errors.As(err, &r) && r.Retryable()
}

// ControllerExpandVolume resizes the backing DRBD+LVM/ZFS volume on all
// replica nodes. For an ordinary volume the node plugin follows up with
// NodeExpandVolume to grow the filesystem online; for a shared volume the
// Haify controller has grown it already, on the node serving its NFS gateway.
func (s *controllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	s.log.Debug("controllerServer.ControllerExpandVolume: start", zap.String("resource", req.GetVolumeId()),
		zap.Int64("required_bytes", req.GetCapacityRange().GetRequiredBytes()))
	if req.GetVolumeId() == "" {
		s.log.Debug("controllerServer.ControllerExpandVolume: rejected, no volume id")
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	newSizeGB := bytesToGiB(req.GetCapacityRange().GetRequiredBytes())

	// Whether the volume is shared decides who grows the filesystem. When
	// the controller cannot say, the answer is the one that is safe either
	// way: node expansion is asked for, and the node plugin leaves an NFS
	// mount alone.
	start := time.Now()
	r, gerr := s.backend.GetResource(ctx, req.GetVolumeId())
	shared := gerr == nil && isSharedVolume(r)
	s.log.Debug("controllerServer.ControllerExpandVolume: looked up volume", zap.String("resource", req.GetVolumeId()),
		zap.Bool("shared", shared), zap.Duration("took", time.Since(start)), zap.Error(gerr))

	start = time.Now()
	err := s.backend.ResizeVolume(ctx, req.GetVolumeId(), 0, newSizeGB)
	s.log.Debug("controllerServer.ControllerExpandVolume: resize returned", zap.String("resource", req.GetVolumeId()),
		zap.Uint32("size_gb", newSizeGB), zap.Bool("shared", shared), zap.Bool("retryable", retryableResize(err)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	switch {
	case retryableResize(err):
		// The device is grown and the filesystem is not, because the gateway
		// that has it mounted is failing over or stopped. Reporting success
		// would leave the PVC at a size its Pods never see; the resizer
		// repeats the call, and the repeat grows only the filesystem.
		return nil, status.Errorf(codes.Unavailable, "resize volume: %v", err)
	case err != nil:
		return nil, status.Errorf(codes.Internal, "resize volume: %v", err)
	}

	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         int64(newSizeGB) * giB,
		NodeExpansionRequired: !shared,
	}, nil
}

// NodeExpandVolume resizes the filesystem at the staging path to fill the
// expanded block device. Called by kubelet after ControllerExpandVolume
// succeeds and the volume is re-staged on this node.
func (s *nodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	s.log.Debug("nodeServer.NodeExpandVolume: start", zap.String("resource", req.GetVolumeId()),
		zap.String("path", req.GetVolumePath()), zap.Int64("required_bytes", req.GetCapacityRange().GetRequiredBytes()))
	if req.GetVolumeId() == "" || req.GetVolumePath() == "" {
		s.log.Debug("nodeServer.NodeExpandVolume: rejected, missing volume id or path")
		return nil, status.Error(codes.InvalidArgument, "volume id and volume path are required")
	}

	// A raw block volume has no filesystem to grow: the device itself got
	// bigger in ControllerExpandVolume, and what the Pod sees is that device.
	if req.GetVolumeCapability().GetBlock() != nil {
		s.log.Debug("nodeServer.NodeExpandVolume: raw block, no filesystem to grow", zap.String("resource", req.GetVolumeId()))
		return &csi.NodeExpandVolumeResponse{CapacityBytes: req.GetCapacityRange().GetRequiredBytes()}, nil
	}

	// A shared volume is an NFS mount here: its filesystem is on a storage
	// node and was grown there. This node has nothing of the volume to grow,
	// and no DRBD device of it to look for. The controller plugin does not
	// ask for this call on a shared volume; it is answered all the same,
	// because it is asked for whenever the controller could not tell.
	if s.nfsMounted(req.GetVolumePath(), req.GetStagingTargetPath()) {
		s.log.Debug("nodeServer.NodeExpandVolume: NFS mount of a shared volume, nothing to grow here", zap.String("resource", req.GetVolumeId()))
		return &csi.NodeExpandVolumeResponse{CapacityBytes: req.GetCapacityRange().GetRequiredBytes()}, nil
	}

	// Resolve the block device for this resource so resize2fs / xfs_growfs
	// knows which device backs the mount.
	devicePath, err := s.deviceFor(ctx, req.GetVolumeId())
	if err != nil {
		s.log.Debug("nodeServer.NodeExpandVolume: no device", zap.String("resource", req.GetVolumeId()), zap.Error(err))
		return nil, err
	}

	start := time.Now()
	err = s.mounter.ResizeFS(devicePath, req.GetVolumePath())
	s.log.Debug("nodeServer.NodeExpandVolume: filesystem resize returned", zap.String("device", devicePath),
		zap.String("path", req.GetVolumePath()), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resize filesystem: %v", err)
	}

	newSize := req.GetCapacityRange().GetRequiredBytes()
	return &csi.NodeExpandVolumeResponse{CapacityBytes: newSize}, nil
}

// nfsMounted reports whether any of paths is an NFS mount on this node, as
// the mount table has it. A mounter that cannot tell says no, and the call
// goes on as for an ordinary volume.
func (s *nodeServer) nfsMounted(paths ...string) bool {
	sm, ok := s.mounter.(sharedMounter)
	if !ok {
		return false
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		fsType, err := sm.MountType(p)
		s.log.Debug("nodeServer.nfsMounted: mount type", zap.String("path", p), zap.String("fs_type", fsType), zap.Error(err))
		if err == nil && strings.HasPrefix(fsType, sharedNFS) { // nfs, nfs4
			return true
		}
	}
	return false
}
