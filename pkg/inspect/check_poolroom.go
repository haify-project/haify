package inspect

import (
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// cowBytes is the copy-on-write area Haify reserves for a snapshot of a thick
// volume: 20% of the origin, at least 256 MiB. It matches cowBytes in
// pkg/controller, which sizes it from the volume's whole GiB.
func cowBytes(sizeBytes uint64) uint64 {
	mb := (sizeBytes >> 30) * 1024 / 5
	if mb < 256 {
		mb = 256
	}
	return mb << 20
}

// snapshotRoom checks each thick volume group: a snapshot there reserves its
// copy-on-write area up front from the group's free space, so a group with
// less free space than that fails every scheduled snapshot and backup of the
// volume — with nothing failing until the schedule fires.
//
// It returns the findings and how many thick groups it judged.
func snapshotRoom(in *Input) ([]Check, int) {
	log := zap.L()
	log.Debug("snapshotRoom: start", zap.Int("probes", len(in.Probes)))
	var out []Check
	groups := 0
	for _, node := range sortedKeys(in.Probes) {
		p := in.Probes[node]
		if !p.LVsOK {
			log.Debug("snapshotRoom: node has no lvs, skipping", zap.String("node", node))
			continue
		}
		thin := map[string]bool{}
		vols := map[string][]LV{}
		for _, lv := range p.LVs {
			if !managedVG(lv.VG) {
				continue
			}
			if lv.Segtype == "thin-pool" {
				thin[lv.VG] = true
				continue
			}
			m := haifyLVName.FindStringSubmatch(lv.Name)
			if m == nil || m[3] != "" || strings.Contains(lv.Name, "_bk_") || lv.Segtype == "snapshot" {
				continue
			}
			vols[lv.VG] = append(vols[lv.VG], lv)
		}
		for _, vg := range sortedKeys(vols) {
			free, known := p.VGFree[vg]
			if thin[vg] || !known {
				log.Debug("snapshotRoom: group thin or free space unknown, not judged", zap.String("node", node),
					zap.String("vg", vg), zap.Bool("thin", thin[vg]), zap.Bool("free_known", known))
				continue
			}
			groups++
			var short []string
			for _, lv := range vols[vg] {
				need := cowBytes(lv.SizeBytes)
				log.Debug("snapshotRoom: volume snapshot reserve", zap.String("node", node), zap.String("vg", vg),
					zap.String("lv", lv.Name), zap.Uint64("size_bytes", lv.SizeBytes), zap.Uint64("need_bytes", need),
					zap.Uint64("free_bytes", free), zap.Bool("fits", need <= free))
				if need > free {
					short = append(short, fmt.Sprintf("%s (%s, snapshot needs %s)", lv.Name, gib(lv.SizeBytes), gib(need)))
				}
			}
			if len(short) == 0 {
				continue
			}
			sort.Strings(short)
			out = append(out, Check{ID: "pool.snapshot_room", Area: AreaPools, Subject: node + ":" + vg, Status: StatusWarn,
				Message: fmt.Sprintf("thick pool %s has %s free, less than a snapshot of %s reserves: its scheduled snapshots and backups fail here. "+
					"Add a device, or convert the pool to thin (haify pool convert-thin --node %s --pool %s)",
					vg, gib(free), plural(len(short), "volume", "volumes"), node, strings.TrimPrefix(vg, "haify_")),
				Evidence: append([]string{fmt.Sprintf("vg_free %s", gib(free))}, short...),
				Fix:      fmt.Sprintf("haify pool add --pool %s --nodes %s --devices <new-device>", strings.TrimPrefix(vg, "haify_"), node)})
		}
	}
	log.Debug("snapshotRoom: done", zap.Int("findings", len(out)), zap.Int("groups", groups))
	return out, groups
}

func gib(b uint64) string { return fmt.Sprintf("%.2f GiB", float64(b)/(1<<30)) }
