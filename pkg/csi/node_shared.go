package csi

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	mount "k8s.io/mount-utils"
)

// The node side of a shared volume (shared.go): the volume is mounted from
// its NFS gateway, on as many nodes as have Pods using it. DRBD is not
// involved on the Pod's node at all: nothing is promoted, no diskless client
// is attached, and the node need not be one Haify knows.
//
// Everything needed to mount is in the volume context, so staging a shared
// volume asks the Haify controller nothing and keeps working while the
// controller is moving or down.

// nfsUnmountTimeout is how long an unmount may wait on the server before it
// is forced. An NFS unmount flushes to the server first, and with the server
// gone that wait has no end, which would leave the Pod stuck Terminating.
const nfsUnmountTimeout = 30 * time.Second

// nfsMountHelpers is where mount.nfs lives when it is not on the PATH: the
// plugin can be started with a PATH that lacks the sbin directories.
var nfsMountHelpers = []string{"/sbin/mount.nfs", "/usr/sbin/mount.nfs"}

// sharedMounter is what a Mounter may offer for NFS mounts. The production
// mounter has both; one without them falls back to a plain unmount and to
// asking the controller what kind of volume a staging path belongs to.
type sharedMounter interface {
	// MountType returns the filesystem type mounted at target, "" when
	// nothing is mounted there.
	MountType(target string) (string, error)
	// UnmountNFS unmounts an NFS mount, forcing it when the server does not
	// answer within the timeout.
	UnmountNFS(target string, timeout time.Duration) error
}

func (s *safeMounter) MountType(target string) (string, error) {
	start := time.Now()
	mounts, err := s.m.List()
	if err != nil {
		zap.L().Debug("safeMounter.MountType: listing mounts failed", zap.String("target", target), zap.Error(err))
		return "", err
	}
	fsType := ""
	for _, mp := range mounts {
		// The last entry for a path is the mount on top, the one in effect.
		if mp.Path == target {
			fsType = mp.Type
		}
	}
	zap.L().Debug("safeMounter.MountType: read", zap.String("target", target), zap.String("fs_type", fsType),
		zap.Int("mounts", len(mounts)), zap.Duration("took", time.Since(start)))
	return fsType, nil
}

func (s *safeMounter) UnmountNFS(target string, timeout time.Duration) error {
	mounted, err := s.IsMountPoint(target)
	if err != nil || !mounted {
		zap.L().Debug("safeMounter.UnmountNFS: not mounted, nothing to do", zap.String("target", target),
			zap.Bool("mounted", mounted), zap.Error(err))
		return nil
	}
	start := time.Now()
	if forcer, ok := s.m.Interface.(mount.MounterForceUnmounter); ok {
		err = forcer.UnmountWithForce(target, timeout)
		zap.L().Debug("safeMounter.UnmountNFS: unmounted, forcing after the timeout", zap.String("target", target),
			zap.Duration("timeout", timeout), zap.Duration("took", time.Since(start)), zap.Error(err))
		return err
	}
	err = s.m.Unmount(target)
	zap.L().Debug("safeMounter.UnmountNFS: unmounted", zap.String("target", target), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// findNFSClient reports whether this node can mount NFS: mount(8) hands an
// NFS mount to the mount.nfs helper, and without the helper the mount fails
// with "bad option; for several filesystems you might need a helper program",
// which names neither NFS nor the package.
func findNFSClient() error {
	if p, err := exec.LookPath("mount.nfs"); err == nil {
		zap.L().Debug("findNFSClient: found on the PATH", zap.String("path", p))
		return nil
	}
	for _, p := range nfsMountHelpers {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			zap.L().Debug("findNFSClient: found", zap.String("path", p))
			return nil
		}
	}
	zap.L().Debug("findNFSClient: mount.nfs not found", zap.Strings("looked_in", nfsMountHelpers))
	return fmt.Errorf("the NFS client is missing: mount.nfs was not found where the node plugin runs " +
		"(package nfs-common on Debian and Ubuntu, nfs-utils on RHEL, Rocky and openSUSE)")
}

// nfsSource writes the server and export as mount(8) wants them. An IPv6
// address goes in brackets, or its colons are read as the separator.
func nfsSource(server, export string) string {
	if addr, err := netip.ParseAddr(server); err == nil && addr.Is6() {
		return "[" + server + "]:" + export
	}
	return server + ":" + export
}

// nfsMountOptions are the PV's mount options, with NFSv4 asked for unless
// they pick a version themselves. Left to itself mount.nfs settles on NFSv3
// when the server turns v4 down, and v3 needs rpc.statd running where the
// mount is made, which the node plugin does not provide: the mount would
// fail on locking, far from its cause.
func nfsMountOptions(flags []string) []string {
	out := make([]string, 0, len(flags)+1)
	versioned := false
	for _, f := range flags {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		for _, key := range []string{"vers=", "nfsvers=", "minorversion="} {
			if strings.HasPrefix(f, key) {
				versioned = true
			}
		}
		out = append(out, f)
	}
	if !versioned {
		out = append(out, "vers=4")
	}
	return out
}

// stageShared mounts a shared volume's NFS export at the staging path.
// NodePublishVolume then bind-mounts the staging path into each Pod, exactly
// as it does for a volume staged from a DRBD device.
func (s *nodeServer) stageShared(_ context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	res, staging := req.GetVolumeId(), req.GetStagingTargetPath()
	server, export := req.GetVolumeContext()[ctxNFSServer], req.GetVolumeContext()[ctxNFSExport]
	s.log.Debug("nodeServer.stageShared: start", zap.String("resource", res), zap.String("staging", staging),
		zap.String("node", s.nodeName), zap.String("server", server), zap.String("export", export))
	if req.GetVolumeCapability().GetBlock() != nil {
		s.log.Debug("nodeServer.stageShared: rejected, block capability", zap.String("resource", res))
		return nil, status.Errorf(codes.InvalidArgument, "shared volume %q is a filesystem served over NFS and cannot be used as a raw block device", res)
	}
	if server == "" || export == "" {
		s.log.Debug("nodeServer.stageShared: rejected, volume context incomplete", zap.String("resource", res))
		return nil, status.Errorf(codes.InvalidArgument,
			"shared volume %q: its volume context lacks %s or %s, so there is nothing to mount it from", res, ctxNFSServer, ctxNFSExport)
	}
	check := s.nfsClient
	if check == nil {
		check = findNFSClient
	}
	if err := check(); err != nil {
		s.log.Debug("nodeServer.stageShared: no NFS client", zap.String("resource", res), zap.String("node", s.nodeName), zap.Error(err))
		return nil, status.Errorf(codes.FailedPrecondition, "node %q cannot mount shared volume %q: %v", s.nodeName, res, err)
	}
	if err := s.mounter.EnsureDir(staging); err != nil {
		s.log.Debug("nodeServer.stageShared: mkdir staging failed", zap.String("staging", staging), zap.Error(err))
		return nil, status.Errorf(codes.Internal, "mkdir staging: %v", err)
	}
	mounted, err := s.mounter.IsMountPoint(staging)
	if err != nil {
		s.log.Debug("nodeServer.stageShared: mount check failed", zap.String("staging", staging), zap.Error(err))
		return nil, status.Errorf(codes.Internal, "check mount: %v", err)
	}
	if mounted {
		s.log.Debug("nodeServer.stageShared: already mounted (idempotent)", zap.String("staging", staging))
		return &csi.NodeStageVolumeResponse{}, nil
	}
	source := nfsSource(server, export)
	opts := nfsMountOptions(req.GetVolumeCapability().GetMount().GetMountFlags())
	start := time.Now()
	err = s.mounter.Mount(source, staging, sharedNFS, opts)
	s.log.Debug("nodeServer.stageShared: mount returned", zap.String("resource", res), zap.String("source", source),
		zap.String("staging", staging), zap.Strings("options", opts), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "mount %s (NFS) for shared volume %q: %v", source, res, err)
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

// unstageShared unmounts the staging path if it belongs to a shared volume,
// and reports whether it did: false sends the caller down the path for a
// volume staged from a DRBD device.
//
// NodeUnstageVolume gets no volume context, so what kind of volume this is
// has to be found out. The mount itself says so, and asks nobody. Only when
// nothing is mounted there, as on a retry after the unmount already
// happened, is the controller asked. Getting this wrong matters in one
// direction: the other path demotes the resource on this node, and on a node
// that happens to be serving the shared volume's gateway that is the service
// every other node is using.
func (s *nodeServer) unstageShared(ctx context.Context, res, staging string) (bool, error) {
	sm, hasTypes := s.mounter.(sharedMounter)
	fsType := ""
	if hasTypes {
		var err error
		if fsType, err = sm.MountType(staging); err != nil {
			s.log.Debug("nodeServer.unstageShared: cannot read the mount type", zap.String("staging", staging), zap.Error(err))
			fsType = ""
		}
	}
	shared := strings.HasPrefix(fsType, sharedNFS) // nfs, nfs4
	s.log.Debug("nodeServer.unstageShared: mount type", zap.String("resource", res), zap.String("staging", staging),
		zap.String("fs_type", fsType), zap.Bool("typed_mounter", hasTypes))
	if fsType == "" {
		start := time.Now()
		r, err := s.backend.GetResource(ctx, res)
		s.log.Debug("nodeServer.unstageShared: asked the controller", zap.String("resource", res),
			zap.Bool("shared", err == nil && isSharedVolume(r)), zap.Duration("took", time.Since(start)), zap.Error(err))
		shared = err == nil && isSharedVolume(r)
	}
	if !shared {
		return false, nil
	}
	start := time.Now()
	var err error
	if hasTypes {
		err = sm.UnmountNFS(staging, nfsUnmountTimeout)
	} else {
		err = s.mounter.Unmount(staging)
	}
	s.log.Debug("nodeServer.unstageShared: unmounted", zap.String("resource", res), zap.String("staging", staging),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return true, status.Errorf(codes.Internal, "unmount staging of shared volume %q: %v", res, err)
	}
	return true, nil
}
