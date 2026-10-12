package controller

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const managedNamePrefix = "haify_"

func normalizeManagedName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.HasPrefix(name, managedNamePrefix) {
		return name
	}
	return managedNamePrefix + name
}

func normalizeLVMPoolType(poolType string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(poolType)) {
	case "", "vg", "lvm":
		return "vg", nil
	case "lvm-thin", "thin-pool", "thin_pool":
		return "thin_pool", nil
	case "lvm-thin-vdo", "thin-vdo", "thin_vdo":
		return vdoPoolType, nil
	default:
		return "", fmt.Errorf("unsupported LVM pool type: %s", poolType)
	}
}

func normalizeManagedZFSPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return path
	}

	base := path
	suffix := ""
	if at := strings.Index(path, "@"); at >= 0 {
		base = path[:at]
		suffix = path[at:]
	}

	parts := strings.Split(base, "/")
	if len(parts) == 0 {
		return normalizeManagedName(base) + suffix
	}

	parts[0] = normalizeManagedName(parts[0])
	return strings.Join(parts, "/") + suffix
}

func parseByteCount(raw string) (uint64, error) {
	value := strings.TrimSpace(raw)
	value = strings.TrimSuffix(value, "B")
	value = strings.TrimSuffix(value, "b")
	if value == "" {
		return 0, fmt.Errorf("empty byte count")
	}

	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, err
	}
	if parsed < 0 {
		return 0, fmt.Errorf("negative byte count: %s", raw)
	}

	return uint64(parsed), nil
}

// parseLVMPoolLine parses one `vgs -o vg_name,vg_size,vg_free[,pv_name]` line.
// The optional 4th field (pv_name) lets a single vgs call also report the
// physical devices backing the VG (one row per PV); pv is "" when absent.
func parseLVMPoolLine(line string) (name string, total, free uint64, pv string, ok bool) {
	fields := strings.Split(strings.TrimSpace(line), "|")
	if len(fields) < 3 {
		return "", 0, 0, "", false
	}

	name = strings.TrimSpace(fields[0])
	if name == "" {
		return "", 0, 0, "", false
	}

	total, err := parseByteCount(fields[1])
	if err != nil {
		return "", 0, 0, "", false
	}

	free, err = parseByteCount(fields[2])
	if err != nil {
		return "", 0, 0, "", false
	}

	if len(fields) >= 4 {
		pv = strings.TrimSpace(fields[3])
	}

	return name, total, free, pv, true
}

// parseZFSSnapshotLine reads one line of `zfs list -t snapshot -o
// name,used,refer,creation -H -p`: the snapshot, its dataset and its creation
// time, which -p gives as Unix seconds and this returns in the form lvs uses
// for lv_time, so callers parse LVM and ZFS snapshots alike. A creation field
// in any other form is passed through whole.
func parseZFSSnapshotLine(line string) (string, string, string, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 4 {
		return "", "", "", false
	}

	parts := strings.SplitN(fields[0], "@", 2)
	if len(parts) != 2 {
		return "", "", "", false
	}

	created := strings.Join(fields[3:], " ")
	if secs, err := strconv.ParseInt(created, 10, 64); err == nil {
		created = time.Unix(secs, 0).UTC().Format("2006-01-02 15:04:05 -0700")
	}
	return parts[1], parts[0], created, true
}
