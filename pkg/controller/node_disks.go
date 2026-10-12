package controller

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// The disks on a node and whether each can be given to a pool. Parsing and
// classification are pure so they can be tested on real lsblk output; the
// probe itself is deployment.ListBlockDevices.

// Disk states.
const (
	// DiskFree is empty and can join a pool as it is.
	DiskFree = "free"
	// DiskWipe holds old signatures (a filesystem, a partition table, a label
	// of an array or pool that is not running) but nothing on the node uses
	// it. It can join a pool only when the caller asks for it to be erased.
	DiskWipe = "wipe"
	// DiskPool already belongs to a Haify pool.
	DiskPool = "pool"
	// DiskBusy is in use by something else and is never touched.
	DiskBusy = "busy"
)

// NodeDisk is one disk (or a free partition) on a node.
type NodeDisk struct {
	Node       string
	Device     string
	ByID       string
	Type       string
	SizeBytes  uint64
	Model      string
	Serial     string
	Transport  string
	Rotational bool
	State      string
	Pool       string
	Detail     string
	// partitions are erased before the disk itself when it is wiped.
	partitions []string
}

type blockRow struct {
	name, typ, tran, mount, model, serial string
	parttype                              string
	size                                  uint64
	ro, rota                              bool
	fstype, pttype                        string
	children                              []string
	hasParent                             bool
}

type blockInventory struct {
	rows  map[string]*blockRow
	order []string
	pvs   map[string]string // device -> volume group ("" for a PV in no group)
	zfs   map[string]string // device -> imported zpool
	drbd  map[string]bool
	ids   map[string][]string // device -> /dev/disk/by-id links
	links map[string]string   // by-id link -> device
}

// unescapeLsblk decodes lsblk's raw-mode \xHH escapes.
func unescapeLsblk(s string) string {
	if !strings.Contains(s, `\x`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' {
			if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return strings.TrimSpace(b.String())
}

// parseBlockInventory reads deployment.ListBlockDevices output.
func parseBlockInventory(out string) *blockInventory {
	inv := &blockInventory{rows: map[string]*blockRow{}, pvs: map[string]string{}, zfs: map[string]string{},
		drbd: map[string]bool{}, ids: map[string][]string{}, links: map[string]string{}}
	row := func(name string) *blockRow {
		r := inv.rows[name]
		if r == nil {
			r = &blockRow{name: name}
			inv.rows[name] = r
			inv.order = append(inv.order, name)
		}
		return r
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "|")
		switch {
		case f[0] == "D" && len(f) >= 11:
			r := row(f[1])
			// A multipath device is listed once under each of its paths.
			if parent := unescapeLsblk(f[2]); parent != "" {
				r.hasParent = true
				p := row(parent)
				if !slices.Contains(p.children, r.name) {
					p.children = append(p.children, r.name)
				}
			}
			r.size, _ = strconv.ParseUint(f[3], 10, 64)
			r.typ, r.ro, r.rota, r.tran = f[4], f[5] == "1", f[6] == "1", f[7]
			r.mount, r.model, r.serial = unescapeLsblk(f[8]), unescapeLsblk(f[9]), unescapeLsblk(f[10])
			if len(f) >= 12 {
				r.parttype = strings.ToLower(strings.TrimSpace(f[11]))
			}
		case f[0] == "B" && len(f) >= 4:
			r := row(f[1])
			r.fstype, r.pttype = strings.TrimSpace(f[2]), strings.TrimSpace(f[3])
		case f[0] == "P" && len(f) >= 3:
			inv.pvs[f[1]] = f[2]
		case f[0] == "Z" && len(f) >= 3:
			inv.zfs[f[1]] = f[2]
		case f[0] == "K" && len(f) >= 2 && f[1] != "":
			inv.drbd[f[1]] = true
		case f[0] == "I" && len(f) >= 3:
			inv.ids[f[2]] = append(inv.ids[f[2]], f[1])
			inv.links[f[1]] = f[2]
		}
	}
	zap.L().Debug("parseBlockInventory: parsed", zap.Int("output_bytes", len(out)), zap.Int("devices", len(inv.order)),
		zap.Int("pvs", len(inv.pvs)), zap.Int("zfs", len(inv.zfs)), zap.Int("drbd", len(inv.drbd)), zap.Int("links", len(inv.links)))
	return inv
}

// minPoolDiskBytes is the smallest device offered to a pool. Anything smaller
// is a BIOS boot or firmware partition, a card reader or a cloud-init seed —
// never a disk anyone means to store data on.
const minPoolDiskBytes = 1 << 30

// diskTypes are the lsblk types that can be pool disks.
var diskTypes = map[string]bool{"disk": true, "part": true, "mpath": true}

func isPseudoDisk(name string) bool {
	for _, p := range []string{"/dev/drbd", "/dev/loop", "/dev/zram", "/dev/ram", "/dev/sr", "/dev/nbd"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// stableID picks the /dev/disk/by-id link that names the physical disk:
// the WWN when there is one, then the NVMe EUI, then the first other link.
func stableID(links []string) string {
	var best string
	rank := func(l string) int {
		base := l[strings.LastIndex(l, "/")+1:]
		switch {
		case strings.HasPrefix(base, "wwn-"):
			return 0
		case strings.HasPrefix(base, "nvme-eui."):
			return 1
		case strings.HasPrefix(base, "nvme-nvme."), strings.HasPrefix(base, "lvm-pv-uuid-"),
			strings.HasPrefix(base, "dm-"), strings.HasPrefix(base, "md-"):
			return 8
		}
		return 2
	}
	for _, l := range links {
		if best == "" || rank(l) < rank(best) || (rank(l) == rank(best) && l < best) {
			best = l
		}
	}
	return best
}

// subtree is name and everything layered on it.
func (inv *blockInventory) subtree(name string) []*blockRow {
	var out []*blockRow
	seen := map[string]bool{}
	var walk func(string)
	walk = func(n string) {
		r := inv.rows[n]
		if r == nil || seen[n] {
			return
		}
		seen[n] = true
		out = append(out, r)
		for _, c := range r.children {
			walk(c)
		}
	}
	walk(name)
	return out
}

func short(dev string) string { return strings.TrimPrefix(dev, "/dev/") }

// firmwarePartitions are partition types a machine needs to boot (or that
// another operating system relies on). They are never mounted on a running
// Linux system as a rule, so nothing else marks them as in use. GPT type
// GUIDs, and MBR type codes as lsblk prints them.
var firmwarePartitions = map[string]string{
	"c12a7328-f81f-11d2-ba4b-00a0c93ec93b": "the EFI system partition",
	"0xef":                                 "the EFI system partition",
	"21686148-6449-6e6f-744e-656564454649": "a BIOS boot partition",
	"e3c9e316-0b5c-4db8-817d-f92df00215ae": "a Microsoft reserved partition",
	"de94bba4-06d1-4d40-a16a-bfd50179d6ac": "a Windows recovery partition",
	"9e1a2d38-c612-4316-aa26-8b49521e5a8b": "a PReP boot partition",
	"0x41":                                 "a PReP boot partition",
}

// signatureName says what a blkid TYPE is in words.
func signatureName(fstype string) string {
	switch fstype {
	case "LVM2_member":
		return "an LVM label of a volume group this node does not have"
	case "zfs_member":
		return "a ZFS label of a pool that is not imported (erasing it destroys that pool)"
	case "linux_raid_member":
		return "a member label of a RAID array that is not running"
	case "crypto_LUKS":
		return "an encrypted (LUKS) volume"
	case "swap":
		return "a swap signature"
	case "drbd":
		return "DRBD metadata"
	}
	return "a filesystem (" + fstype + ")"
}

// classify says whether the row can join a pool.
func (inv *blockInventory) classify(node string, r *blockRow) NodeDisk {
	d := NodeDisk{Node: node, Device: r.name, ByID: stableID(inv.ids[r.name]), Type: r.typ, SizeBytes: r.size,
		Model: r.model, Serial: r.serial, Transport: r.tran, Rotational: r.rota}
	log := zap.L()
	busy := func(format string, a ...any) NodeDisk {
		d.State, d.Detail = DiskBusy, fmt.Sprintf(format, a...)
		log.Debug("blockInventory.classify: disk busy", zap.String("node", node), zap.String("device", r.name),
			zap.String("detail", d.Detail))
		return d
	}
	tree := inv.subtree(r.name)
	log.Debug("blockInventory.classify: start", zap.String("node", node), zap.String("device", r.name),
		zap.String("type", r.typ), zap.Uint64("size_bytes", r.size), zap.Int("subtree", len(tree)))
	for _, t := range tree {
		if inv.drbd[t.name] {
			return busy("backing disk of a DRBD resource")
		}
		if what, ok := firmwarePartitions[t.parttype]; ok {
			if t.name == r.name {
				return busy("%s", what)
			}
			return busy("%s holds %s", short(t.name), what)
		}
	}
	for _, t := range tree {
		where := ""
		if t.name != r.name {
			where = short(t.name) + " "
		}
		switch {
		case t.mount == "[SWAP]":
			return busy("%sused as swap", where)
		case t.mount != "":
			return busy("%smounted at %s", where, t.mount)
		}
	}
	for _, t := range tree {
		if vg, ok := inv.pvs[t.name]; ok && vg != "" {
			if strings.HasPrefix(vg, managedNamePrefix) {
				d.State, d.Pool = DiskPool, vg
				log.Debug("blockInventory.classify: disk in a Haify volume group", zap.String("node", node),
					zap.String("device", r.name), zap.String("pool", vg))
				return d
			}
			if t.name == r.name {
				return busy("in LVM volume group %s", vg)
			}
			return busy("%s is in LVM volume group %s", short(t.name), vg)
		}
		if pool, ok := inv.zfs[t.name]; ok {
			if strings.HasPrefix(pool, managedNamePrefix) {
				d.State, d.Pool = DiskPool, pool
				log.Debug("blockInventory.classify: disk in a Haify ZFS pool", zap.String("node", node),
					zap.String("device", r.name), zap.String("pool", pool))
				return d
			}
			return busy("in ZFS pool %s", pool)
		}
	}
	var parts, holders []string
	for _, t := range tree[1:] {
		if t.typ == "part" {
			parts = append(parts, t.name)
		} else {
			holders = append(holders, short(t.name))
		}
	}
	if len(holders) > 0 {
		return busy("used by %s", strings.Join(holders, ", "))
	}
	if r.ro {
		return busy("read-only")
	}
	var found []string
	if _, orphanPV := inv.pvs[r.name]; r.fstype != "" && !orphanPV {
		found = append(found, signatureName(r.fstype))
	}
	if r.pttype != "" || len(parts) > 0 {
		desc := "a partition table"
		if r.pttype != "" {
			desc = "a " + r.pttype + " partition table"
		}
		var names []string
		for _, p := range parts {
			n := short(p)
			if pr := inv.rows[p]; pr.fstype != "" {
				n += " (" + pr.fstype + ")"
			}
			names = append(names, n)
		}
		if len(names) > 0 {
			desc += " with " + strings.Join(names, ", ")
		}
		found = append(found, desc)
	}
	if len(found) > 0 {
		d.State, d.Detail, d.partitions = DiskWipe, strings.Join(found, "; "), parts
		log.Debug("blockInventory.classify: disk holds old signatures", zap.String("node", node),
			zap.String("device", r.name), zap.String("detail", d.Detail), zap.Strings("partitions", parts))
		return d
	}
	d.State = DiskFree
	log.Debug("blockInventory.classify: disk free", zap.String("node", node), zap.String("device", r.name))
	return d
}

// disks lists the node's disks, and the partitions that are themselves free
// or only hold old signatures — a disk whose other partitions are in use
// (the system disk) can still have a spare one.
func (inv *blockInventory) disks(node string) []NodeDisk {
	log := zap.L()
	log.Debug("blockInventory.disks: start", zap.String("node", node), zap.Int("devices", len(inv.order)))
	var out []NodeDisk
	for _, name := range inv.order {
		r := inv.rows[name]
		if !diskTypes[r.typ] || isPseudoDisk(name) || r.size < minPoolDiskBytes {
			log.Debug("blockInventory.disks: not a candidate disk", zap.String("node", node), zap.String("device", name),
				zap.String("type", r.typ), zap.Uint64("size_bytes", r.size))
			continue
		}
		d := inv.classify(node, r)
		if r.typ == "part" && d.State != DiskFree && d.State != DiskWipe {
			log.Debug("blockInventory.disks: partition in use, not listed", zap.String("node", node),
				zap.String("device", name), zap.String("state", d.State))
			continue
		}
		out = append(out, d)
	}
	log.Debug("blockInventory.disks: done", zap.String("node", node), zap.Int("count", len(out)))
	return out
}

// find resolves a device as the caller named it: a /dev path or a
// /dev/disk/by-id (or by-path) link.
func (inv *blockInventory) find(device string) *blockRow {
	if r := inv.rows[device]; r != nil {
		zap.L().Debug("blockInventory.find: found by device path", zap.String("device", device))
		return r
	}
	if target, ok := inv.links[device]; ok {
		zap.L().Debug("blockInventory.find: found by link", zap.String("device", device), zap.String("target", target),
			zap.Bool("listed", inv.rows[target] != nil))
		return inv.rows[target]
	}
	zap.L().Debug("blockInventory.find: no such device", zap.String("device", device))
	return nil
}

// checkPoolDisk says whether device on node may join a pool, and returns it.
func (inv *blockInventory) checkPoolDisk(node, device string, wipe bool) (NodeDisk, error) {
	log := zap.L()
	log.Debug("blockInventory.checkPoolDisk: start", zap.String("node", node), zap.String("device", device), zap.Bool("wipe", wipe))
	r := inv.find(device)
	if r == nil || isPseudoDisk(r.name) || r.size == 0 {
		var free []string
		for _, d := range inv.disks(node) {
			if d.State == DiskFree {
				free = append(free, fmt.Sprintf("%s (%s)", d.Device, formatBytes(d.SizeBytes)))
			}
		}
		hint := "it has no empty disk"
		if len(free) > 0 {
			hint = "empty disks there: " + strings.Join(free, ", ")
		}
		log.Debug("blockInventory.checkPoolDisk: refusing, not a disk", zap.String("node", node),
			zap.String("device", device), zap.Strings("free", free))
		return NodeDisk{}, fmt.Errorf("%s is not a disk on %s; %s", device, node, hint)
	}
	if !diskTypes[r.typ] {
		log.Debug("blockInventory.checkPoolDisk: refusing, wrong device type", zap.String("node", node),
			zap.String("device", device), zap.String("type", r.typ))
		return NodeDisk{}, fmt.Errorf("%s on %s is a %s, not a disk or partition", device, node, r.typ)
	}
	if r.size < minPoolDiskBytes {
		log.Debug("blockInventory.checkPoolDisk: refusing, too small", zap.String("node", node),
			zap.String("device", device), zap.Uint64("size_bytes", r.size))
		return NodeDisk{}, fmt.Errorf("%s on %s is only %s; a pool disk must be at least 1 GiB",
			device, node, formatBytes(r.size))
	}
	d := inv.classify(node, r)
	switch d.State {
	case DiskPool:
		log.Debug("blockInventory.checkPoolDisk: refusing, already in a pool", zap.String("node", node),
			zap.String("device", device), zap.String("pool", d.Pool))
		return d, fmt.Errorf("%s on %s is already in pool %s", device, node, d.Pool)
	case DiskBusy:
		log.Debug("blockInventory.checkPoolDisk: refusing, in use", zap.String("node", node),
			zap.String("device", device), zap.String("detail", d.Detail))
		return d, fmt.Errorf("%s on %s is in use: %s", device, node, d.Detail)
	case DiskWipe:
		if !wipe {
			log.Debug("blockInventory.checkPoolDisk: refusing, holds signatures and wipe not asked", zap.String("node", node),
				zap.String("device", device), zap.String("detail", d.Detail))
			return d, fmt.Errorf("%s on %s holds %s. If nothing on it is needed, retry with --wipe, which erases it",
				device, node, d.Detail)
		}
		log.Debug("blockInventory.checkPoolDisk: holds signatures, will be wiped", zap.String("node", node),
			zap.String("device", device), zap.String("detail", d.Detail))
	}
	log.Debug("blockInventory.checkPoolDisk: accepted", zap.String("node", node), zap.String("device", d.Device),
		zap.String("state", d.State), zap.String("by_id", d.ByID))
	return d, nil
}
