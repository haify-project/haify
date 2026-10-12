package deployment

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// blockDeviceScript prints everything needed to say, for every disk on a node,
// whether it can be given to a pool: one line per fact, tagged by its first
// field, so the controller can classify the disks without a second round trip.
//
//	D|name|parent|size|type|ro|rota|tran|mountpoint|model|serial|parttype  (lsblk -r)
//	B|device|fstype|pttype     low-level signature probe (blkid -p)
//	P|pv|vg                    LVM physical volumes
//	Z|device|pool              devices of imported ZFS pools
//	K|device                   DRBD backing and metadata disks
//	I|link|target              /dev/disk/by-id links
//
// lsblk's raw mode escapes spaces as \x20, so its fields split on single
// spaces; empty columns stay as empty fields. blkid -p reads the device itself
// rather than the udev cache, which is what makes a stale or missing udev
// database (containers, early boot) unable to hide a filesystem. DRBD devices,
// loop devices, LVs and other mapper devices are never probed: reading a
// Secondary DRBD device can go over the network, and none of them is a disk.
const blockDeviceScript = `export LC_ALL=C
cols=NAME,PKNAME,SIZE,TYPE,RO,ROTA,TRAN,MOUNTPOINT,MODEL,SERIAL,PARTTYPE
lsblk -o "$cols" >/dev/null 2>&1 || cols=${cols%,PARTTYPE}
lsblk -nrbp -o "$cols" 2>/dev/null | while IFS= read -r line; do
  echo "D|$(echo "$line" | tr ' ' '|')"
  dev=${line%% *}
  t=$(echo "$line" | cut -d' ' -f4)
  case "$dev" in /dev/drbd*|/dev/loop*|/dev/zram*|/dev/ram*|/dev/sr*|/dev/nbd*) continue;; esac
  case "$t" in disk|part|mpath) ;; *) continue;; esac
  fs=$(timeout 5 blkid -p -o value -s TYPE "$dev" 2>/dev/null | head -1)
  pt=$(timeout 5 blkid -p -o value -s PTTYPE "$dev" 2>/dev/null | head -1)
  echo "B|$dev|$fs|$pt"
done
pvs --noheadings --separator '|' -o pv_name,vg_name 2>/dev/null | while IFS='|' read -r pv vg; do
  echo "P|$(echo $pv)|$(echo $vg)"
done
if command -v zpool >/dev/null 2>&1; then
  zpool status -LP 2>/dev/null | awk '$1=="pool:"{p=$2} $1 ~ /^\/dev\//{print "Z|"$1"|"p}'
fi
if command -v drbdsetup >/dev/null 2>&1; then
  drbdsetup show 2>/dev/null | awk '$1=="disk"||$1=="meta-disk"{gsub(/[";]/,"",$2); if ($2 ~ /^\//) print $2}' |
    while read -r d; do echo "K|$(readlink -f "$d")"; done
fi
for l in /dev/disk/by-id/*; do
  [ -e "$l" ] && echo "I|$l|$(readlink -f "$l")"
done
true`

// ListBlockDevices reports the disks on host, their signatures and who holds
// them; see blockDeviceScript for the format.
func (c *Client) ListBlockDevices(ctx context.Context, host string) (*ExecResult, error) {
	c.log().Debug("ListBlockDevices: probing disks", zap.String("host", host))
	script := base64.StdEncoding.EncodeToString([]byte(blockDeviceScript))
	return c.Exec(ctx, []string{host}, fmt.Sprintf("echo %s | base64 -d | sudo /bin/bash", script))
}

// WipeDevice erases every signature on device and on its partitions, then
// makes the kernel forget the partitions.
//
// wipefs opens each device exclusively, so it refuses a device that anything
// still holds open — a mounted filesystem, an active array, a DRBD backing
// disk — whatever the caller believed about it. A ZFS label is cleared with
// zpool labelclear as well, because a pool keeps two of its four labels at the
// end of the device.
func (c *Client) WipeDevice(ctx context.Context, host, device string, partitions []string) (*ExecResult, error) {
	c.log().Debug("WipeDevice: wiping device",
		zap.String("host", host), zap.String("device", device), zap.Strings("partitions", partitions))
	d := shellQuote(device)
	var b strings.Builder
	b.WriteString("set -e\n")
	for _, p := range partitions {
		fmt.Fprintf(&b, "wipefs -a %s\n", shellQuote(p))
	}
	fmt.Fprintf(&b, `if [ "$(blkid -p -o value -s TYPE %[1]s 2>/dev/null)" = zfs_member ] && command -v zpool >/dev/null 2>&1; then
  zpool labelclear -f %[1]s || true
fi
wipefs -a %[1]s
blockdev --rereadpt %[1]s 2>/dev/null || partx -d %[1]s 2>/dev/null || true
udevadm settle 2>/dev/null || true
`, d)
	b64 := base64.StdEncoding.EncodeToString([]byte(b.String()))
	return c.Exec(ctx, []string{host}, fmt.Sprintf("echo %s | base64 -d | sudo /bin/bash", b64))
}

// ZFSAddVdev adds a disk to an existing pool as a new top-level vdev.
//
// Deliberately without -f: zpool refuses a disk whose redundancy does not
// match the pool's existing vdevs, and that refusal is the operator's to
// overrule, not this function's.
func (c *Client) ZFSAddVdev(ctx context.Context, host, pool, device string) (*ExecResult, error) {
	c.log().Debug("ZFSAddVdev: adding vdev", zap.String("host", host), zap.String("pool", pool), zap.String("device", device))
	return c.Exec(ctx, []string{host}, fmt.Sprintf("sudo zpool add %s %s", shellQuote(pool), shellQuote(device)))
}
