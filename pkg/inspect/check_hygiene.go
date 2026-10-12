package inspect

import (
	"fmt"
	"regexp"
	"strings"

	"go.uber.org/zap"
)

// haifyLVName matches the backing volumes Haify creates — <res>_data, <res>_volN,
// <res>_state[N] — and scheduled snapshots of them. Backup snapshots (_bk_)
// are judged by the backups area, against the backup records.
var haifyLVName = regexp.MustCompile(`^(.+)_(data|vol[0-9]+|state[0-9]*)(_sched_.+)?$`)

// checkHygiene lists what deleted resources left behind: DRBD configs and
// logical volumes for resources the database no longer has. It only lists;
// nothing here is deleted, because a name match is not proof.
func checkHygiene(in *Input) []Check {
	log := zap.L()
	log.Debug("checkHygiene: start", zap.Int("resources", len(in.Resources)), zap.Int("probes", len(in.Probes)))
	known := map[string]bool{}
	for _, r := range in.Resources {
		known[r.Name] = true
	}
	var out []Check
	for _, node := range sortedKeys(in.Probes) {
		p := in.Probes[node]
		addr := sshTarget(in, node)
		var stale []string
		for _, f := range p.ResFiles {
			if !known[f] {
				stale = append(stale, f)
			}
		}
		log.Debug("checkHygiene: node res files", zap.String("node", node), zap.Int("count", len(p.ResFiles)), zap.Strings("stale", stale))
		if len(stale) > 0 {
			var cmds []string
			for _, f := range stale {
				cmds = append(cmds, fmt.Sprintf("sudo drbdadm down %s; sudo rm /etc/drbd.d/%s.res", f, f))
			}
			out = append(out, Check{ID: "hygiene.stale_res_file", Area: AreaHygiene, Subject: node, Status: StatusWarn,
				Message:  fmt.Sprintf("%s in /etc/drbd.d for resources the controller does not know", plural(len(stale), "config", "configs")),
				Evidence: stale,
				Fix:      fmt.Sprintf("ssh %s '%s'", addr, strings.Join(cmds, "; "))})
		}
		var orphans []string
		for _, lv := range p.LVs {
			if !managedVG(lv.VG) || strings.Contains(lv.Name, "_bk_") {
				continue
			}
			m := haifyLVName.FindStringSubmatch(lv.Name)
			if m == nil || known[m[1]] {
				continue
			}
			log.Debug("checkHygiene: orphan volume", zap.String("node", node), zap.String("lv", lv.VG+"/"+lv.Name), zap.String("resource", m[1]))
			orphans = append(orphans, lv.VG+"/"+lv.Name)
		}
		if len(orphans) > 0 {
			out = append(out, Check{ID: "hygiene.orphan_lv", Area: AreaHygiene, Subject: node, Status: StatusWarn,
				Message:  fmt.Sprintf("%s named like a Haify volume of a resource that no longer exists", plural(len(orphans), "logical volume", "logical volumes")),
				Evidence: orphans,
				Fix:      fmt.Sprintf("ssh %s sudo lvremove %s", addr, strings.Join(orphans, " "))})
		}
	}
	log.Debug("checkHygiene: done", zap.Int("findings", len(out)))
	if len(out) == 0 {
		out = append(out, pass("hygiene.leftovers", AreaHygiene, "no DRBD configs or Haify-named volumes left by deleted resources"))
	}
	return out
}
