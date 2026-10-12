package csi

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	mount "k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"
)

// Mounter is the node-plugin mount surface; abstracted for testing.
type Mounter interface {
	// FormatAndMount formats source with fsType if needed, then mounts at target.
	FormatAndMount(source, target, fsType string, options []string) error
	// Mount performs a plain mount (used for bind mounts).
	Mount(source, target, fsType string, options []string) error
	// Unmount unmounts target if mounted.
	Unmount(target string) error
	// IsMountPoint reports whether target is currently a mount point.
	IsMountPoint(target string) (bool, error)
	// EnsureDir makes target (and parents) if absent.
	EnsureDir(target string) error
	// ResizeFS expands the filesystem at mountPath to fill the underlying block device.
	ResizeFS(devicePath, mountPath string) error
	// EnsureFile makes an empty regular file at target (and its parent
	// directories). A raw block volume is published by bind-mounting the device
	// node onto a file, not a directory, and the bind target has to exist.
	EnsureFile(target string) error
	// FSStats reports capacity and inode usage of the filesystem mounted at path.
	FSStats(path string) (FSStats, error)
	// BlockSize reports the size in bytes of the block device reachable at path
	// (a device node, or a file a device has been bind-mounted onto).
	BlockSize(path string) (int64, error)
}

// FSStats is one filesystem's usage as statfs reports it, in bytes and inodes.
type FSStats struct {
	TotalBytes, AvailableBytes, UsedBytes    int64
	TotalInodes, AvailableInodes, UsedInodes int64
}

type safeMounter struct {
	m *mount.SafeFormatAndMount
}

// NewMounter returns the production Mounter backed by k8s.io/mount-utils.
func NewMounter() Mounter {
	return &safeMounter{
		m: mount.NewSafeFormatAndMount(mount.New(""), utilexec.New()),
	}
}

func (s *safeMounter) FormatAndMount(source, target, fsType string, options []string) error {
	start := time.Now()
	err := s.m.FormatAndMount(source, target, fsType, options)
	zap.L().Debug("safeMounter.FormatAndMount: done", zap.String("device", source), zap.String("target", target),
		zap.String("fs_type", fsType), zap.Strings("options", options), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

func (s *safeMounter) Mount(source, target, fsType string, options []string) error {
	start := time.Now()
	err := s.m.Mount(source, target, fsType, options)
	zap.L().Debug("safeMounter.Mount: done", zap.String("source", source), zap.String("target", target),
		zap.String("fs_type", fsType), zap.Strings("options", options), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

func (s *safeMounter) Unmount(target string) error {
	mounted, err := s.IsMountPoint(target)
	if err != nil || !mounted {
		zap.L().Debug("safeMounter.Unmount: not mounted, nothing to do", zap.String("target", target),
			zap.Bool("mounted", mounted), zap.Error(err))
		return nil
	}
	start := time.Now()
	err = s.m.Unmount(target)
	zap.L().Debug("safeMounter.Unmount: done", zap.String("target", target), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

func (s *safeMounter) IsMountPoint(target string) (bool, error) {
	notMnt, err := s.m.IsLikelyNotMountPoint(target)
	if os.IsNotExist(err) {
		zap.L().Debug("safeMounter.IsMountPoint: path does not exist", zap.String("target", target))
		return false, nil
	}
	if err != nil {
		zap.L().Debug("safeMounter.IsMountPoint: check failed", zap.String("target", target), zap.Error(err))
		return false, err
	}
	zap.L().Debug("safeMounter.IsMountPoint: checked", zap.String("target", target), zap.Bool("mounted", !notMnt))
	return !notMnt, nil
}

func (s *safeMounter) EnsureDir(target string) error {
	err := os.MkdirAll(target, 0o750)
	zap.L().Debug("safeMounter.EnsureDir: done", zap.String("target", target), zap.Error(err))
	return err
}

func (s *safeMounter) ResizeFS(devicePath, mountPath string) error {
	start := time.Now()
	r := mount.NewResizeFs(utilexec.New())
	resized, err := r.Resize(devicePath, mountPath)
	zap.L().Debug("safeMounter.ResizeFS: done", zap.String("device", devicePath), zap.String("path", mountPath),
		zap.Bool("resized", resized), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

func (s *safeMounter) EnsureFile(target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		zap.L().Debug("safeMounter.EnsureFile: mkdir parent failed", zap.String("target", target), zap.Error(err))
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_RDONLY, 0o640)
	if err != nil {
		zap.L().Debug("safeMounter.EnsureFile: create failed", zap.String("target", target), zap.Error(err))
		return err
	}
	err = f.Close()
	zap.L().Debug("safeMounter.EnsureFile: done", zap.String("target", target), zap.Error(err))
	return err
}

func (s *safeMounter) FSStats(path string) (FSStats, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		zap.L().Debug("safeMounter.FSStats: statfs failed", zap.String("path", path), zap.Error(err))
		return FSStats{}, err
	}
	bs := int64(st.Bsize)
	total := int64(st.Blocks) * bs
	// Bavail, not Bfree: the blocks reserved for root are not space a Pod can
	// use, and reporting them as free is how "80% used" turns into "disk full"
	// with no warning in between.
	avail := int64(st.Bavail) * bs
	used := (int64(st.Blocks) - int64(st.Bfree)) * bs
	return FSStats{
		TotalBytes: total, AvailableBytes: avail, UsedBytes: used,
		TotalInodes: int64(st.Files), AvailableInodes: int64(st.Ffree),
		UsedInodes: int64(st.Files) - int64(st.Ffree),
	}, nil
}

func (s *safeMounter) BlockSize(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		zap.L().Debug("safeMounter.BlockSize: open failed", zap.String("path", path), zap.Error(err))
		return 0, err
	}
	// Opened read-only to seek: nothing was written, so a failed close cannot
	// lose data and has nothing to report.
	defer func() { _ = f.Close() }()
	// Seeking to the end of a block device yields its size; stat reports 0 for
	// one, so it cannot be used here.
	n, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		zap.L().Debug("safeMounter.BlockSize: seek failed", zap.String("path", path), zap.Error(err))
		return 0, fmt.Errorf("seek %s: %w", path, err)
	}
	zap.L().Debug("safeMounter.BlockSize: measured", zap.String("path", path), zap.Int64("size_bytes", n))
	return n, nil
}
