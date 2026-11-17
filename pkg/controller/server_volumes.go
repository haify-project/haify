package controller

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

func (s *Server) AddVolume(ctx context.Context, req *haifypb.AddVolumeRequest) (*haifypb.AddVolumeResponse, error) {
	log := s.serverLog()
	log.Debug("AddVolume: adding", zap.String("resource", req.Resource), zap.Any("volume", req.Volume),
		zap.String("pool", req.Pool), zap.Any("size_gb", req.SizeGb))
	err := s.resources.AddVolume(ctx, req.Resource, req.Volume, req.Pool, req.SizeGb)
	if err != nil {
		log.Debug("AddVolume: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.AddVolumeResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("AddVolume: added", zap.String("resource", req.Resource))
	return &haifypb.AddVolumeResponse{
		Success: true,
		Message: "Volume added successfully",
	}, nil
}

func (s *Server) RemoveVolume(ctx context.Context, req *haifypb.RemoveVolumeRequest) (*haifypb.RemoveVolumeResponse, error) {
	log := s.serverLog()
	log.Debug("RemoveVolume: removing", zap.String("resource", req.Resource), zap.Any("volume", req.VolumeId))
	err := s.resources.RemoveVolume(ctx, req.Resource, req.VolumeId)
	if err != nil {
		log.Debug("RemoveVolume: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.RemoveVolumeResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("RemoveVolume: removed", zap.String("resource", req.Resource), zap.Any("volume", req.VolumeId))
	return &haifypb.RemoveVolumeResponse{
		Success: true,
		Message: "Volume removed successfully",
	}, nil
}

// RenameResource renames a resource (resource_rename.go).
func (s *Server) RenameResource(ctx context.Context, req *haifypb.RenameResourceRequest) (*haifypb.RenameResourceResponse, error) {
	log := s.serverLog()
	log.Debug("RenameResource: renaming", zap.String("resource", req.Name), zap.String("new_name", req.NewName))
	if err := s.resources.RenameResource(ctx, req.Name, req.NewName); err != nil {
		log.Debug("RenameResource: failed", zap.String("resource", req.Name), zap.Error(err))
		return &haifypb.RenameResourceResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("RenameResource: renamed", zap.String("resource", req.Name), zap.String("new_name", req.NewName))
	return &haifypb.RenameResourceResponse{Success: true,
		Message: fmt.Sprintf("resource %s renamed to %s", req.Name, req.NewName)}, nil
}

func (s *Server) ResizeVolume(ctx context.Context, req *haifypb.ResizeVolumeRequest) (*haifypb.ResizeVolumeResponse, error) {
	log := s.serverLog()
	log.Debug("ResizeVolume: resizing", zap.String("resource", req.Resource), zap.Any("volume", req.VolumeId),
		zap.Uint64("size_bytes", req.SizeBytes), zap.Any("size_gb", req.SizeGb), zap.Bool("ignore_free_space", req.IgnoreFreeSpace),
		zap.Bool("grow_filesystem", req.GrowFilesystem))
	var err error
	if req.SizeBytes > 0 {
		log.Debug("ResizeVolume: exact size in bytes", zap.Uint64("size_bytes", req.SizeBytes))
		err = s.resources.ResizeVolumeBytes(ctx, req.Resource, req.VolumeId, req.SizeBytes, req.IgnoreFreeSpace)
	} else {
		log.Debug("ResizeVolume: size in GiB", zap.Any("size_gb", req.SizeGb))
		err = s.resources.ResizeVolumeOptions(ctx, req.Resource, req.VolumeId, uint64(req.SizeGb), req.IgnoreFreeSpace)
	}
	if err != nil {
		log.Debug("ResizeVolume: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.ResizeVolumeResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("ResizeVolume: resized", zap.String("resource", req.Resource), zap.Any("volume", req.VolumeId))
	// The volume is grown; whether its running libvirt guest could be shown
	// the new size only changes what the reply says (resize_guest.go).
	want := uint64(req.SizeGb) << 30
	if req.SizeBytes > 0 {
		want = roundUpSectors(req.SizeBytes)
	}
	// A filesystem a storage node has mounted from the volume is grown with
	// it: always for an NFS or SMB gateway, on request for anything else
	// (resize_filesystem.go). When that could not be done the reply says the
	// resize is unfinished, since the export still shows the old size.
	guestNote := s.resources.TellGuestOfResize(ctx, req.Resource, req.VolumeId, want)
	fsNote, fsErr := s.resources.GrowFilesystem(ctx, req.Resource, req.VolumeId, want, req.GrowFilesystem)
	if fsErr != nil {
		log.Debug("ResizeVolume: volume grown, filesystem not", zap.String("resource", req.Resource), zap.Error(fsErr))
		msg := resizeIncomplete(req.Resource, fsErr)
		if guestNote != "" {
			msg += " (" + guestNote + ")"
		}
		return &haifypb.ResizeVolumeResponse{Success: false, Retryable: true, Message: msg}, nil
	}
	msg := resizedMessage(guestNote)
	if fsNote != "" {
		msg += "; " + fsNote
	}
	return &haifypb.ResizeVolumeResponse{Success: true, Message: msg}, nil
}

func (s *Server) CreateFilesystem(ctx context.Context, req *haifypb.CreateFilesystemRequest) (*haifypb.CreateFilesystemResponse, error) {
	log := s.serverLog()
	log.Debug("CreateFilesystem: creating", zap.String("resource", req.Resource), zap.Any("volume", req.VolumeId),
		zap.String("fstype", req.Fstype), zap.String("node", req.Node))
	// CreateFilesystem is implemented as part of Mount operation
	// This is a convenience wrapper that only creates filesystem
	err := s.resources.CreateFilesystemOnly(ctx, req.Resource, req.VolumeId, req.Fstype, req.Node)
	if err != nil {
		log.Debug("CreateFilesystem: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.CreateFilesystemResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("CreateFilesystem: created", zap.String("resource", req.Resource), zap.String("fstype", req.Fstype))
	return &haifypb.CreateFilesystemResponse{
		Success: true,
		Message: "Filesystem created successfully",
	}, nil
}

func (s *Server) MountResource(ctx context.Context, req *haifypb.MountResourceRequest) (*haifypb.MountResourceResponse, error) {
	log := s.serverLog()
	log.Debug("MountResource: mounting", zap.String("resource", req.Resource), zap.String("path", req.Path),
		zap.Any("volume", req.VolumeId), zap.String("node", req.Node), zap.String("fstype", req.Fstype))
	err := s.resources.Mount(ctx, req.Resource, req.Path, req.VolumeId, req.Node, req.Fstype)
	if err != nil {
		log.Debug("MountResource: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.MountResourceResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("MountResource: mounted", zap.String("resource", req.Resource), zap.String("path", req.Path))
	return &haifypb.MountResourceResponse{
		Success: true,
		Message: "Resource mounted successfully",
	}, nil
}

func (s *Server) UnmountResource(ctx context.Context, req *haifypb.UnmountResourceRequest) (*haifypb.UnmountResourceResponse, error) {
	log := s.serverLog()
	log.Debug("UnmountResource: unmounting", zap.String("resource", req.Resource), zap.Any("volume", req.VolumeId),
		zap.String("node", req.Node))
	err := s.resources.Unmount(ctx, req.Resource, req.VolumeId, req.Node)
	if err != nil {
		log.Debug("UnmountResource: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.UnmountResourceResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}
	log.Debug("UnmountResource: unmounted", zap.String("resource", req.Resource))
	return &haifypb.UnmountResourceResponse{
		Success: true,
		Message: "Resource unmounted successfully",
	}, nil
}
