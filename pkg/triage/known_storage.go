package triage

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// ── 3. thin pool ────────────────────────────────────────────────────────

// matchThinPool reads the `lvs` output for a pool that is out of room.
//
// Data and metadata are separate exhaustions with the same consequence and
// different fixes, and the metadata one is the one that surprises people: a
// pool with 40% of its data space free stops accepting writes when its much
// smaller metadata volume fills.
func matchThinPool(in Input) []Finding {
	log := zap.L()
	log.Debug("matchThinPool: looking for thin pools at 90% or more")
	type hit struct {
		lv, vg string
		pct    float64
		nodes  map[string]bool
		ev     []Evidence
	}
	data, meta := map[string]*hit{}, map[string]*hit{}

	record(in, "storage", func(node, line string) {
		lv, vg, dataPct, metaPct, ok := parseLVSLine(line)
		if !ok {
			return
		}
		key := vg + "/" + lv
		if dataPct >= 90 {
			h := data[key]
			if h == nil {
				h = &hit{lv: lv, vg: vg, nodes: map[string]bool{}}
				data[key] = h
			}
			h.pct = maxFloat(h.pct, dataPct)
			h.nodes[node] = true
			h.ev = append(h.ev, Evidence{Source: node + "/storage", Node: node, Line: strings.TrimSpace(line)})
		}
		if metaPct >= 90 {
			h := meta[key]
			if h == nil {
				h = &hit{lv: lv, vg: vg, nodes: map[string]bool{}}
				meta[key] = h
			}
			h.pct = maxFloat(h.pct, metaPct)
			h.nodes[node] = true
			h.ev = append(h.ev, Evidence{Source: node + "/storage", Node: node, Line: strings.TrimSpace(line)})
		}
	})

	var out []Finding
	emit := func(m map[string]*hit, dimension string) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			h := m[k]
			sev := SeverityWarning
			if h.pct >= 99 {
				sev = SeverityCritical
			} else if h.pct >= 95 {
				sev = SeverityError
			}
			log.Debug("matchThinPool: pool over threshold", zap.String("pool", k), zap.String("dimension", dimension),
				zap.Float64("percent", h.pct), zap.String("severity", string(sev)), zap.Strings("nodes", sortedKeys(h.nodes)))
			cause := "A thin pool that runs out of data space stops accepting writes, and every " +
				"volume in it stops at once — including ones that are themselves nearly empty."
			advice := []string{
				fmt.Sprintf("See what is using it: lvs -o lv_name,lv_size,data_percent %s", h.vg),
				fmt.Sprintf("Extend the pool if there is free space in the group: lvextend -L +20G %s/%s", h.vg, h.lv),
				fmt.Sprintf("If the group itself is full, add a disk first: haify pool add-disk --name %s --disks /dev/<device>", h.vg),
				"Delete snapshots that are no longer needed — an old snapshot pins every block it was taken over.",
			}
			if dimension == "metadata" {
				cause = "A thin pool's metadata volume is much smaller than its data space and fills " +
					"independently. When it fills the pool stops accepting writes even though the " +
					"data space still shows room free, which is why this looks like a pool with " +
					"plenty of space refusing to write."
				advice = []string{
					fmt.Sprintf("Confirm which dimension is full: lvs -o lv_name,data_percent,metadata_percent %s/%s", h.vg, h.lv),
					fmt.Sprintf("Extend the metadata volume: lvextend --poolmetadatasize +1G %s/%s", h.vg, h.lv),
					"Delete snapshots that are no longer needed — each one costs metadata whether or not it costs data.",
				}
			}
			out = append(out, Finding{
				ID:       "thin-pool-" + dimension,
				Title:    fmt.Sprintf("thin pool %s/%s is %.1f%% full on %s space", h.vg, h.lv, h.pct, dimension),
				Severity: sev,
				Count:    len(h.ev),
				Nodes:    sortedKeys(h.nodes),
				Known:    true,
				Cause:    cause,
				Advice:   advice,
				Caution: "Do not free space by removing a logical volume that a DRBD resource is " +
					"still backed by; that destroys the replica, and the peers will not put it back.",
				Evidence: h.ev,
			})
		}
	}
	emit(data, "data")
	emit(meta, "metadata")
	log.Debug("matchThinPool: verdict", zap.Int("data_full", len(data)), zap.Int("metadata_full", len(meta)),
		zap.Int("findings", len(out)))
	return out
}

// parseLVSLine reads one row of `lvs -o lv_name,vg_name,lv_size,data_percent,metadata_percent`.
// A row whose percentage columns are blank is a plain volume, not a pool.
func parseLVSLine(line string) (lv, vg string, dataPct, metaPct float64, ok bool) {
	f := strings.Fields(line)
	if len(f) < 5 || f[0] == "LV" {
		return "", "", 0, 0, false
	}
	d, errD := strconv.ParseFloat(strings.TrimSuffix(f[3], "%"), 64)
	m, errM := strconv.ParseFloat(strings.TrimSuffix(f[4], "%"), 64)
	if errD != nil && errM != nil {
		return "", "", 0, 0, false
	}
	if errD != nil {
		d = 0
	}
	if errM != nil {
		m = 0
	}
	return f[0], f[1], d, m, true
}

// ── 4. the filesystem gave up ───────────────────────────────────────────

func matchFilesystemAborted(in Input) []Finding {
	ev, nodes := nodeLinesMatching(in, func(l string) bool {
		ll := strings.ToLower(l)
		return strings.Contains(ll, "remounting filesystem read-only") ||
			strings.Contains(ll, "aborting journal") ||
			strings.Contains(ll, "ext4-fs error") ||
			(strings.Contains(ll, "xfs") && strings.Contains(ll, "corruption"))
	}, "kernel_errors", "drbd_kernel")
	if len(ev) == 0 {
		zap.L().Debug("matchFilesystemAborted: verdict, no filesystem went read-only")
		return nil
	}
	zap.L().Debug("matchFilesystemAborted: verdict, filesystem aborted", zap.Int("count", len(ev)), zap.Strings("nodes", nodes))
	return []Finding{{
		ID:       "filesystem-aborted",
		Title:    "a filesystem hit an error and went read-only",
		Severity: SeverityCritical,
		Count:    len(ev),
		Nodes:    nodes,
		Known:    true,
		Cause: "The kernel found something it could not reconcile and stopped writing rather than " +
			"make it worse. On a DRBD volume this is usually the block layer underneath — a full " +
			"thin pool, a failing disk, or a resource that lost its backing device — not the " +
			"filesystem itself.",
		Advice: []string{
			"Look at what is underneath before touching the filesystem: check this report's thin-pool and DRBD findings first.",
			"Read the surrounding kernel messages for the first error, not the loudest: dmesg -T | grep -iE 'ext4|xfs|drbd|I/O' | head -50",
			"Once the cause underneath is fixed, unmount and check: umount <mountpoint> && fsck -y <device>",
		},
		Caution: "Do not remount read-write to 'try again'. If the block layer is still broken that " +
			"turns a stopped filesystem into a corrupted one.",
		Evidence: ev,
	}}
}

// ── 10. the mount itself is full ────────────────────────────────────────

func matchMountFull(in Input) []Finding {
	log := zap.L()
	log.Debug("matchMountFull: looking for mounts at 90% or more")
	type hit struct {
		mount string
		pct   int
		nodes map[string]bool
		ev    []Evidence
	}
	hits := map[string]*hit{}
	record(in, "mounts", func(node, line string) {
		f := strings.Fields(line)
		if len(f) < 6 || f[0] == "Filesystem" {
			return
		}
		pct, err := strconv.Atoi(strings.TrimSuffix(f[4], "%"))
		if err != nil || pct < 90 {
			return
		}
		mount := f[5]
		h := hits[mount]
		if h == nil {
			h = &hit{mount: mount, nodes: map[string]bool{}}
			hits[mount] = h
		}
		if pct > h.pct {
			h.pct = pct
		}
		h.nodes[node] = true
		h.ev = append(h.ev, Evidence{Source: node + "/mounts", Node: node, Line: strings.TrimSpace(line)})
	})
	keys := make([]string, 0, len(hits))
	for k := range hits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Finding, 0, len(keys))
	for _, k := range keys {
		h := hits[k]
		sev := SeverityWarning
		if h.pct >= 98 {
			sev = SeverityCritical
		} else if h.pct >= 95 {
			sev = SeverityError
		}
		log.Debug("matchMountFull: mount over threshold", zap.String("mount", h.mount), zap.Int("percent", h.pct),
			zap.String("severity", string(sev)), zap.Strings("nodes", sortedKeys(h.nodes)))
		out = append(out, Finding{
			ID:       "mount-full",
			Title:    fmt.Sprintf("%s is %d%% full", h.mount, h.pct),
			Severity: sev,
			Count:    len(h.ev),
			Nodes:    sortedKeys(h.nodes),
			Known:    true,
			Cause: "A DRBD-backed filesystem that fills stops the service using it, and on the " +
				"controller's own volume that means the control plane stops being able to write " +
				"its database.",
			Advice: []string{
				fmt.Sprintf("Find what grew: du -xh %s --max-depth=2 | sort -h | tail -20", h.mount),
				"Grow the volume rather than deleting blindly — DRBD resizes online: haify resource resize-volume --resource <resource> --volume 0 --size <new size>",
			},
			Evidence: h.ev,
		})
	}
	log.Debug("matchMountFull: verdict", zap.Int("findings", len(out)))
	return out
}
