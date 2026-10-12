package deployment

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// lvmcache — putting an SSD in front of a slow LVM pool.
//
// These are the raw node interactions only; every decision about whether a
// cache may be attached or detached lives in the controller, where it can be
// tested without a node. The commands are deliberately thin wrappers so that
// what actually runs on a storage node is readable in one place.

// LVMCacheFields is the column list used for every cache query.
//
// The cache does not appear on the row an operator would expect. Caching a
// thin pool attaches the cache to the pool's *data* sub-LV, "[pool_tdata]",
// which lvs hides unless -a is given; the fast device likewise becomes an
// internal LV named "<name>_cvol". So the query asks for every LV in the group,
// including internal ones, and the caller finds the cache by segment type
// rather than by name.
//
// vg_name comes first so a single query can cover every group on a host, which
// is what makes surfacing cache state in `pool list` one extra SSH round for
// the whole cluster rather than one per pool.
const LVMCacheFields = "vg_name,lv_name,lv_attr,segtype,lv_size,cache_mode," +
	"cache_total_blocks,cache_used_blocks,cache_dirty_blocks," +
	"cache_read_hits,cache_read_misses,cache_write_hits,cache_write_misses,devices"

// LVSCacheReport lists every LV on the given hosts with its cache columns.
// vgName may be empty to cover all volume groups.
//
// --units b --nosuffix keeps the sizes machine-readable; the block counters are
// plain integers already and are empty for an LV that is not currently active,
// which the parser treats as zero.
func (c *Client) LVSCacheReport(ctx context.Context, hosts []string, vgName string) (*ExecResult, error) {
	c.log().Debug("LVSCacheReport: reading cache state",
		zap.Strings("hosts", hosts), zap.String("vg", vgName), zap.Bool("all_groups", vgName == ""))
	cmd := fmt.Sprintf("sudo lvs -a --noheadings --nosuffix --units b --separator '|' -o %s", LVMCacheFields)
	if vgName != "" {
		cmd += " " + vgName
	}
	return c.Exec(ctx, hosts, cmd)
}

// ProbeBlockDevice reports what a device currently is, so the caller can refuse
// to spend a disk that already holds something.
//
// One round trip rather than four: a device that is a PV of another volume
// group, a device carrying a filesystem, a device with partitions on it and a
// device that is mounted are four different refusals, and asking about them
// separately is four SSH latencies for a question with one answer. Every
// lookup is guarded with `|| true`-style redirection so that a device which is
// simply free still exits zero — "not a PV" must not read as "the probe
// failed".
func (c *Client) ProbeBlockDevice(ctx context.Context, host, device string) (*ExecResult, error) {
	c.log().Debug("ProbeBlockDevice: probing device", zap.String("host", host), zap.String("device", device))
	d := shellQuote(device)
	cmd := strings.Join([]string{
		fmt.Sprintf("if [ -b %s ]; then echo block=yes; else echo block=no; fi", d),
		fmt.Sprintf("echo size=$(sudo blockdev --getsize64 %s 2>/dev/null)", d),
		fmt.Sprintf("echo fstype=$(sudo blkid -o value -s TYPE %s 2>/dev/null)", d),
		fmt.Sprintf("echo mount=$(lsblk -ndo MOUNTPOINT %s 2>/dev/null)", d),
		// lsblk lists the device itself first, then anything layered on it, so
		// everything past the first line is a partition or mapper holder.
		fmt.Sprintf("echo holders=$(lsblk -nro NAME %s 2>/dev/null | tail -n +2 | tr '\\n' ' ')", d),
		fmt.Sprintf("echo pvvg=$(sudo pvs --noheadings -o vg_name %s 2>/dev/null | tr -d ' ')", d),
	}, "; ")
	return c.Exec(ctx, []string{host}, cmd)
}

// VGExtend adds a physical volume to an existing volume group.
func (c *Client) VGExtend(ctx context.Context, hosts []string, vgName, device string) (*ExecResult, error) {
	c.log().Debug("VGExtend: adding device to volume group",
		zap.Strings("hosts", hosts), zap.String("vg", vgName), zap.String("device", device))
	return c.Exec(ctx, hosts, fmt.Sprintf("sudo vgextend %s %s", vgName, shellQuote(device)))
}

// VGReduceAndRemovePV takes a physical volume back out of a volume group and
// clears its LVM label.
//
// This is the half of cache removal that is easy to skip and expensive to have
// skipped: an SSD left behind as a member PV is free space in the volume group,
// and the next `lvextend -l +100%FREE` — which is exactly what growing a thin
// pool does here — would quietly move pool data onto it. vgreduce refuses while
// anything still allocates from the device, so this cannot strand data.
func (c *Client) VGReduceAndRemovePV(ctx context.Context, hosts []string, vgName, device string) (*ExecResult, error) {
	c.log().Debug("VGReduceAndRemovePV: taking device out of volume group",
		zap.Strings("hosts", hosts), zap.String("vg", vgName), zap.String("device", device))
	d := shellQuote(device)
	return c.Exec(ctx, hosts, fmt.Sprintf("sudo vgreduce %s %s && sudo pvremove -y %s", vgName, d, d))
}

// LVCreateCacheVol carves the whole fast device into one logical volume, which
// lvconvert then turns into a cache.
//
// -l 100%PVS with the device named restricts the allocation to that PV: without
// it LVM is free to satisfy the request from the slow disks, producing a
// "cache" on the same spindles as the origin.
func (c *Client) LVCreateCacheVol(ctx context.Context, hosts []string, vgName, lvName, device string) (*ExecResult, error) {
	c.log().Debug("LVCreateCacheVol: creating cache volume",
		zap.Strings("hosts", hosts), zap.String("vg", vgName), zap.String("lv", lvName), zap.String("device", device))
	return c.Exec(ctx, hosts, fmt.Sprintf("sudo lvcreate -y -n %s -l 100%%PVS %s %s",
		lvName, vgName, shellQuote(device)))
}

// LVConvertToCache attaches a cache volume in front of an existing LV.
//
// The mode is always stated. LVM's own default is writethrough today, but a
// default that is inherited rather than declared is one upgrade away from
// changing under a running cluster, and the difference between the two modes
// here is whether an acknowledged write exists anywhere but one node's SSD.
func (c *Client) LVConvertToCache(ctx context.Context, hosts []string, vgName, lvName, cacheVol, mode string) (*ExecResult, error) {
	c.log().Debug("LVConvertToCache: attaching cache",
		zap.Strings("hosts", hosts), zap.String("vg", vgName), zap.String("lv", lvName),
		zap.String("cache_vol", cacheVol), zap.String("mode", mode))
	return c.Exec(ctx, hosts, fmt.Sprintf("sudo lvconvert -y --type cache --cachevol %s --cachemode %s %s/%s",
		cacheVol, mode, vgName, lvName))
}

// LVUncache flushes a cache and detaches it, deleting the cache volume.
//
// Deliberately without --force. On a writeback cache lvconvert writes the dirty
// blocks down to the slow device first, and --force is what turns "I cannot
// flush this" into "I discarded it" — which is precisely the outcome the caller
// must be told about rather than have chosen for it.
func (c *Client) LVUncache(ctx context.Context, hosts []string, vgName, lvName string) (*ExecResult, error) {
	c.log().Debug("LVUncache: flushing and detaching cache",
		zap.Strings("hosts", hosts), zap.String("vg", vgName), zap.String("lv", lvName))
	return c.Exec(ctx, hosts, fmt.Sprintf("sudo lvconvert -y --uncache %s/%s", vgName, lvName))
}

// shellQuote makes an operator-supplied device path safe to interpolate into a
// command line. Device paths reach this package straight from a gRPC request,
// and Exec hands the string to `sh -c`.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
