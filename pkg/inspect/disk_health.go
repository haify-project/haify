package inspect

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// Disk health: the disks under every pool, and what SMART (or the NVMe health
// log) says about them. A disk rarely dies without warning — reallocated and
// pending sectors climb, an SSD wears out — and the warning is only useful if
// someone reads it before the pool loses the disk.

// managedPrefix marks the volume groups Haify manages; the node's own (its
// root VG, a hypervisor's) are not pools and are left alone.
const managedPrefix = "haify_"

// DiskProbeScript prints one line per physical volume of a Haify pool:
// pv|vg|size|used|disk|<base64 smartctl json, or NOSMARTCTL>. The disk is the
// PV's parent when the PV is a partition.
const DiskProbeScript = `pvs --noheadings --units b --nosuffix --separator '|' -o pv_name,vg_name,pv_size,pv_used 2>/dev/null |
while IFS='|' read -r pv vg size used; do
  pv=$(echo $pv); vg=$(echo $vg)
  case "$pv" in /dev/drbd*) continue;; esac
  case "$vg" in ` + managedPrefix + `*) ;; *) continue;; esac
  disk=$pv
  # -d: without it lsblk walks the device's children (every LV on it).
  if [ "$(lsblk -ndo type "$pv" 2>/dev/null)" = part ]; then
    parent=$(lsblk -ndo pkname "$pv" 2>/dev/null)
    [ -n "$parent" ] && disk=/dev/$parent
  fi
  if command -v smartctl >/dev/null 2>&1; then
    s=$(smartctl -H -A -i -j "$disk" 2>/dev/null | base64 -w0)
  else
    s=NOSMARTCTL
  fi
  echo "$pv|$vg|$size|$used|$disk|$s"
done`

// DiskStatus values.
const (
	DiskOK      = "ok"
	DiskWarn    = "warn"
	DiskFail    = "fail"
	DiskUnknown = "unknown"
)

// Disk is one physical volume under a pool, and its disk's health.
type Disk struct {
	Node      string
	Pool      string
	PV        string
	Device    string
	SizeBytes uint64
	UsedBytes uint64
	Status    string
	Detail    string
	Model     string
	Serial    string
}

// ParseDiskProbe reads DiskProbeScript's output from node.
func ParseDiskProbe(node, out string) []Disk {
	log := zap.L()
	var disks []Disk
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(strings.TrimSpace(line), "|", 6)
		if len(f) != 6 || f[0] == "" || !strings.HasPrefix(f[1], managedPrefix) {
			log.Debug("ParseDiskProbe: line is not a pool PV, skipping", zap.String("node", node), zap.Int("fields", len(f)))
			continue
		}
		size, _ := strconv.ParseUint(f[2], 10, 64)
		used, _ := strconv.ParseUint(f[3], 10, 64)
		d := Disk{Node: node, Pool: f[1], PV: f[0], Device: f[4], SizeBytes: size, UsedBytes: used}
		switch f[5] {
		case "NOSMARTCTL":
			d.Status, d.Detail = DiskUnknown, "smartctl is not installed on the node (apt install smartmontools)"
		default:
			raw, err := base64.StdEncoding.DecodeString(f[5])
			if err != nil || len(raw) == 0 {
				d.Status, d.Detail = DiskUnknown, "smartctl gave no report"
			} else {
				d.Status, d.Detail, d.Model, d.Serial = ParseSmart(raw)
			}
		}
		log.Debug("ParseDiskProbe: disk", zap.String("node", node), zap.String("pool", d.Pool), zap.String("pv", d.PV),
			zap.String("device", d.Device), zap.Uint64("size_bytes", d.SizeBytes), zap.String("status", d.Status),
			zap.String("detail", d.Detail))
		disks = append(disks, d)
	}
	log.Debug("ParseDiskProbe: done", zap.String("node", node), zap.Int("count", len(disks)))
	return disks
}

type smartReport struct {
	ModelName    string `json:"model_name"`
	SerialNumber string `json:"serial_number"`
	SmartStatus  *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	NVMe *struct {
		CriticalWarning int    `json:"critical_warning"`
		PercentageUsed  int    `json:"percentage_used"`
		MediaErrors     uint64 `json:"media_errors"`
	} `json:"nvme_smart_health_information_log"`
	ATA *struct {
		Table []struct {
			ID  int `json:"id"`
			Raw struct {
				Value uint64 `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	Smartctl struct {
		Messages []struct {
			String string `json:"string"`
		} `json:"messages"`
	} `json:"smartctl"`
}

// ParseSmart turns `smartctl -H -A -i -j` output into a status and the one
// sentence that explains it.
func ParseSmart(raw []byte) (status, detail, model, serial string) {
	var r smartReport
	if err := json.Unmarshal(raw, &r); err != nil {
		zap.L().Debug("ParseSmart: report does not parse", zap.Int("bytes", len(raw)), zap.Error(err))
		return DiskUnknown, "unreadable smartctl report", "", ""
	}
	model, serial = r.ModelName, r.SerialNumber
	zap.L().Debug("ParseSmart: report", zap.String("model", model), zap.String("serial", serial),
		zap.Bool("has_smart_status", r.SmartStatus != nil), zap.Bool("nvme", r.NVMe != nil), zap.Bool("ata", r.ATA != nil))
	if r.SmartStatus == nil {
		why := "the device reports no SMART data (a virtual disk?)"
		if len(r.Smartctl.Messages) > 0 {
			why = r.Smartctl.Messages[0].String
		}
		return DiskUnknown, why, model, serial
	}
	var fails, warns []string
	if !r.SmartStatus.Passed {
		fails = append(fails, "SMART overall health check FAILED")
	}
	if n := r.NVMe; n != nil {
		if n.CriticalWarning != 0 {
			fails = append(fails, fmt.Sprintf("NVMe critical warning 0x%x", n.CriticalWarning))
		}
		switch {
		case n.PercentageUsed >= 100:
			fails = append(fails, fmt.Sprintf("worn out (%d%% of rated endurance used)", n.PercentageUsed))
		case n.PercentageUsed >= 90:
			warns = append(warns, fmt.Sprintf("%d%% of rated endurance used", n.PercentageUsed))
		}
		if n.MediaErrors > 0 {
			warns = append(warns, fmt.Sprintf("%d media errors", n.MediaErrors))
		}
	}
	if a := r.ATA; a != nil {
		for _, attr := range a.Table {
			v := attr.Raw.Value
			switch {
			case attr.ID == 5 && v > 0:
				warns = append(warns, fmt.Sprintf("%d reallocated sectors", v))
			case attr.ID == 197 && v > 0:
				warns = append(warns, fmt.Sprintf("%d sectors pending reallocation", v))
			case attr.ID == 198 && v > 0:
				fails = append(fails, fmt.Sprintf("%d uncorrectable sectors", v))
			}
		}
	}
	zap.L().Debug("ParseSmart: judged", zap.String("model", model), zap.Strings("fails", fails), zap.Strings("warns", warns))
	switch {
	case len(fails) > 0:
		return DiskFail, strings.Join(append(fails, warns...), "; "), model, serial
	case len(warns) > 0:
		return DiskWarn, strings.Join(warns, "; "), model, serial
	}
	return DiskOK, "SMART health passed", model, serial
}

// checkDisks reports every disk under a pool whose health is not ok. A disk
// without SMART (virtual disks) is not a finding: there is nothing to act on.
func checkDisks(in *Input) []Check {
	log := zap.L()
	if !in.DisksProbed {
		log.Debug("checkDisks: disks were not probed")
		return nil
	}
	log.Debug("checkDisks: start", zap.Int("disks", len(in.Disks)))
	var out []Check
	failing := 0
	for _, d := range in.Disks {
		log.Debug("checkDisks: disk", zap.String("node", d.Node), zap.String("device", d.Device), zap.String("status", d.Status))
		var st Status
		switch d.Status {
		case DiskFail:
			st = StatusFail
		case DiskWarn:
			st = StatusWarn
		default:
			continue
		}
		failing++
		out = append(out, Check{ID: "disk.health", Area: AreaPools, Subject: d.Node + ":" + d.Device, Status: st,
			Message:  fmt.Sprintf("%s (pool %s on %s) is failing: %s", d.Device, d.Pool, d.Node, d.Detail),
			Evidence: []string{strings.TrimSpace(d.Model + " " + d.Serial)},
			Fix: fmt.Sprintf("move its data to a healthy disk while it still reads: haify pool replace-disk --pool %s --node %s --disk %s --new-disk <device>",
				d.Pool, d.Node, d.PV)})
	}
	if failing == 0 {
		out = append(out, Check{ID: "disk.health", Area: AreaPools, Subject: "disks", Status: StatusPass,
			Message: fmt.Sprintf("%d disk(s) under pools, none reporting trouble", len(in.Disks))})
	}
	return out
}
