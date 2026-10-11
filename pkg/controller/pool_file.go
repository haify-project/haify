package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	"github.com/haify-project/haify/pkg/util"
)

// File-backed pools: a pool built on an image file instead of a disk, for a
// node with no spare disk, a laptop or a test cluster. `pool create --devices
// file:/var/lib/haify-pools/data.img:50G` creates the file, attaches it as a
// loop device and builds the pool on that like on any disk, so thin pools,
// snapshots and everything else work unchanged. A systemd unit attaches the
// file again at boot, before LVM activation and DRBD, so the pool is there
// when the replicas come up. It costs a filesystem's overhead on every write;
// for anything that matters, give the pool a disk.

const fileDevicePrefix = "file:"

// fileSpec is a parsed file:PATH:SIZE device.
type fileSpec struct {
	Path string
	Size uint64
}

func isFileDevice(dev string) bool { return strings.HasPrefix(dev, fileDevicePrefix) }

// parseFileDevice reads file:PATH:SIZE. The size is needed to create the file;
// an existing file of another size is refused rather than resized.
func parseFileDevice(dev string) (fileSpec, error) {
	rest := strings.TrimPrefix(dev, fileDevicePrefix)
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		return fileSpec{}, fmt.Errorf("%q: give file:PATH:SIZE, e.g. file:/var/lib/haify-pools/data.img:50G", dev)
	}
	p, sizeStr := rest[:i], rest[i+1:]
	if !path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, " '\"$`\\;&|") {
		return fileSpec{}, fmt.Errorf("%q is not a plain absolute path", p)
	}
	if strings.HasPrefix(p, "/dev/") || strings.HasPrefix(p, "/proc/") || strings.HasPrefix(p, "/sys/") {
		return fileSpec{}, fmt.Errorf("%s is not a place for an image file", p)
	}
	// /var/lib/haify is where Self-HA mounts the controller's own replicated
	// volume on whichever node runs it: a pool file there would live on that
	// volume on one node and on the root filesystem, under the mount point,
	// on the others.
	if p == "/var/lib/haify" || strings.HasPrefix(p, "/var/lib/haify/") {
		return fileSpec{}, fmt.Errorf("%s is under /var/lib/haify, where the controller's own volume is mounted; use /var/lib/haify-pools", p)
	}
	size, err := util.ParseSize(sizeStr)
	if err != nil {
		return fileSpec{}, fmt.Errorf("size of %s: %w", p, err)
	}
	if size < 1<<30 {
		return fileSpec{}, fmt.Errorf("%s: a pool file must be at least 1 GiB", p)
	}
	return fileSpec{Path: p, Size: size}, nil
}

// fileUnitName is the systemd unit that attaches the file at boot.
func fileUnitName(p string) string {
	sum := sha256.Sum256([]byte(p))
	return "haify-pool-file-" + hex.EncodeToString(sum[:6]) + ".service"
}

// fileUnit attaches the file at boot and activates the group on it.
func fileUnit(p, vg string) string {
	return fmt.Sprintf(`[Unit]
Description=Haify pool %[2]s on %[1]s
DefaultDependencies=no
RequiresMountsFor=%[1]s
After=local-fs.target systemd-udev-settle.service
Before=drbd.service drbd-reactor.service haify-controller.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'losetup -j %[1]s | grep -q . || losetup --direct-io=on -f %[1]s'
ExecStartPost=-/sbin/vgchange -ay %[2]s

[Install]
WantedBy=multi-user.target
`, p, vg)
}

// fileSetupScript creates the file (sparse for a thin pool, allocated for a
// thick one, whose promise to its volumes is that the space exists), installs
// and starts the unit, and prints the loop device.
func fileSetupScript(f fileSpec, vg string, thin bool) string {
	alloc := fmt.Sprintf("fallocate -l %d %s", f.Size, f.Path)
	if thin {
		alloc = fmt.Sprintf("truncate -s %d %s", f.Size, f.Path)
	}
	unit := fileUnitName(f.Path)
	return fmt.Sprintf(`set -e
f=%[1]s
if [ -e "$f" ]; then
  [ "$(stat -c %%s "$f")" = %[2]d ] || { echo "$f exists with another size; remove it or pick another path" >&2; exit 1; }
  if [ -z "$(losetup -j "$f")" ] && [ "$(head -c 4096 "$f" | tr -d '\0' | wc -c)" != 0 ]; then
    echo "$f exists and is not empty; remove it or pick another path" >&2; exit 1
  fi
else
  # A pool on a file on a DRBD volume would replicate twice and vanish with
  # that volume's mount; it belongs on the node's own disk.
  d=$(dirname "$f"); while [ ! -e "$d" ]; do d=$(dirname "$d"); done
  case "$(findmnt -T "$d" -no SOURCE)" in /dev/drbd*) echo "$(dirname "$f") is on a DRBD volume; put the pool file on the node's own disk" >&2; exit 1;; esac
  mkdir -p "$(dirname "$f")"
  %[3]s
fi
cat > /etc/systemd/system/%[4]s <<'UNIT'
%[5]sUNIT
systemctl daemon-reload
systemctl enable --now %[4]s >/dev/null 2>&1
losetup -j "$f" | head -1 | cut -d: -f1
`, f.Path, f.Size, alloc, unit, fileUnit(f.Path, vg))
}

// fileTeardownScript removes the files a group was built on, found from the
// units on the node itself: the pool record names one node's devices, and a
// pool of the same name on several nodes has one record.
func fileTeardownScript(vg string) string {
	return fmt.Sprintf(`for u in /etc/systemd/system/haify-pool-file-*.service; do
  [ -e "$u" ] || continue
  grep -qx 'ExecStartPost=-/sbin/vgchange -ay %[1]s' "$u" || continue
  f=$(sed -n 's/^Description=Haify pool [^ ]* on //p' "$u")
  systemctl disable --now "$(basename "$u")" >/dev/null 2>&1
  rm -f "$u"
  for l in $(losetup -j "$f" | cut -d: -f1); do losetup -d "$l"; done
  [ -n "$f" ] && rm -f "$f"
done
systemctl daemon-reload
true
`, vg)
}

// attachFileDevices turns every file: device into its loop device, leaving
// disks as they are.
func (sm *StorageManager) attachFileDevices(ctx context.Context, address, node, vg string, devices []string, thin bool) ([]string, error) {
	out := make([]string, 0, len(devices))
	for _, d := range devices {
		if !isFileDevice(d) {
			out = append(out, d)
			continue
		}
		f, err := parseFileDevice(d)
		if err != nil {
			return nil, err
		}
		res, err := sm.controller.deployment.Exec(ctx, []string{address},
			"echo "+base64Std(fileSetupScript(f, vg, thin))+" | base64 -d | sudo /bin/bash")
		if err := execFailure(res, err); err != nil {
			return nil, fmt.Errorf("set up %s on %s: %w", f.Path, node, err)
		}
		// The device is the script's last line; stderr shares the output.
		loop := ""
		for _, h := range res.Hosts {
			for _, l := range strings.Split(h.Output, "\n") {
				if l = strings.TrimSpace(l); strings.HasPrefix(l, "/dev/loop") {
					loop = l
				}
			}
		}
		if !strings.HasPrefix(loop, "/dev/loop") {
			return nil, fmt.Errorf("set up %s on %s: no loop device came back (%q)", f.Path, node, loop)
		}
		out = append(out, loop)
	}
	return out, nil
}
