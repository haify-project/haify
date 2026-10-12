package controller

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
)

type resourceConfigVolume struct {
	VolumeID  int
	Minor     int
	DiskPath  string
	StartLine int
	EndLine   int
}

func parseResourceConfigVolumes(content string) []resourceConfigVolume {
	lines := strings.Split(content, "\n")
	// A resource's volumes are the blocks directly inside `resource { }`. The
	// same "volume N {" also opens a per-host override inside an `on` section —
	// the `disk none` a tiebreaker or diskless client carries for each volume.
	// Taking the first "volume N" in the file used to find the tiebreaker's
	// override whenever it came first, and resize then ran `lvresize ... none`.
	// Only when a file has no top-level volumes at all (an adopted resource
	// written per host) are the nested ones what describes the volumes.
	var top, nested []resourceConfigVolume
	var current *resourceConfigVolume
	currentNested := false
	fileDepth, depth := 0, 0

	for idx, line := range lines {
		trimmed := strings.TrimSpace(line)
		opens, closes := strings.Count(line, "{"), strings.Count(line, "}")

		if current == nil && strings.HasPrefix(trimmed, "volume ") && strings.Contains(trimmed, "{") {
			parts := strings.Fields(trimmed)
			if len(parts) >= 2 {
				volID, err := strconv.Atoi(strings.TrimSuffix(parts[1], "{"))
				if err == nil {
					current = &resourceConfigVolume{VolumeID: volID, Minor: -1, StartLine: idx}
					currentNested = fileDepth > 1
					fileDepth += opens - closes
					depth = opens - closes
					if depth <= 0 {
						current.EndLine = idx
						if currentNested {
							nested = append(nested, *current)
						} else {
							top = append(top, *current)
						}
						current = nil
						depth = 0
					}
					continue
				}
			}
		}
		fileDepth += opens - closes

		if current == nil {
			continue
		}

		if strings.Contains(trimmed, "device") && strings.Contains(trimmed, "minor") {
			parts := strings.Fields(trimmed)
			for i, part := range parts {
				if part == "minor" && i+1 < len(parts) {
					if minor, err := strconv.Atoi(strings.TrimSuffix(parts[i+1], ";")); err == nil {
						current.Minor = minor
					}
					break
				}
			}
		}

		// `disk /dev/vg/lv;` names the backing device. `disk { ... }` opens the
		// volume's disk options — written whenever a disk option is set, and by
		// default for a resource on thin storage — and is not a path: taking its
		// "{" for one made a resize run `lvresize ... {`.
		if parts := strings.Fields(trimmed); len(parts) >= 2 && parts[0] == "disk" && !strings.HasPrefix(parts[1], "{") {
			current.DiskPath = strings.TrimSuffix(parts[1], ";")
		}

		depth += opens - closes
		if depth <= 0 {
			current.EndLine = idx
			if currentNested {
				nested = append(nested, *current)
			} else {
				top = append(top, *current)
			}
			current = nil
			depth = 0
		}
	}

	volumes := top
	if len(volumes) == 0 {
		volumes = nested
	}

	// Fall back to the older single-volume syntax (device/disk declared at the
	// resource level, no `volume {}` block) when no volume blocks were found,
	// so adopting a pre-9 / hand-written resource still discovers its volume.
	implicit := false
	if len(volumes) == 0 {
		if v := parseImplicitVolume0(content); v != nil {
			volumes = append(volumes, *v)
			implicit = true
		}
	}

	zap.L().Debug("parseResourceConfigVolumes: parsed volumes", zap.Int("top_level", len(top)),
		zap.Int("nested", len(nested)), zap.Bool("implicit", implicit), zap.Int("count", len(volumes)))
	return volumes
}

// parseImplicitVolume0 handles the older single-volume DRBD syntax where the
// device/disk are declared directly at the resource level instead of inside a
// `volume {}` block, e.g.:
//
//	resource r {
//	  device    /dev/drbd0;
//	  disk      /dev/sdb;
//	  meta-disk internal;
//	  on node { ... }
//	}
//
// It returns a synthesized volume 0, or nil if no resource-level disk is found.
func parseImplicitVolume0(content string) *resourceConfigVolume {
	depth := 0
	vol := resourceConfigVolume{VolumeID: 0, Minor: -1}
	haveDisk := false
	for idx, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		// Only resource-level declarations (depth 1) count — not lines inside
		// on{}, net{}, options{} or a disk{} options block.
		if depth == 1 {
			switch {
			case strings.HasPrefix(trimmed, "device") && !strings.Contains(trimmed, "minor"):
				// old form: `device /dev/drbdN;`
				if fields := strings.Fields(trimmed); len(fields) >= 2 {
					if m, ok := parseDevNodeMinor(strings.TrimSuffix(fields[1], ";")); ok {
						vol.Minor = m
						vol.EndLine = idx
					}
				}
			case strings.HasPrefix(trimmed, "disk") && !strings.Contains(trimmed, "{"):
				// `disk /dev/sdb;` — not a `disk {` options block; "meta-disk"
				// does not match the "disk" prefix.
				if fields := strings.Fields(trimmed); len(fields) >= 2 {
					vol.DiskPath = strings.TrimSuffix(fields[1], ";")
					vol.StartLine = idx
					vol.EndLine = idx
					haveDisk = true
				}
			}
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
	}
	zap.L().Debug("parseImplicitVolume0: resource-level volume", zap.Bool("found", haveDisk),
		zap.String("disk", vol.DiskPath), zap.Int("minor", vol.Minor))
	if !haveDisk {
		return nil
	}
	return &vol
}

func backingPathForVolume(pool, volumeName, storageType string) string {
	if storageType == "zfs" || storageType == "zfs-thin" {
		return fmt.Sprintf("/dev/zvol/%s/%s", pool, volumeName)
	}
	return fmt.Sprintf("/dev/%s/%s", pool, volumeName)
}

func findVolumeRecord(volumes []*database.Volume, volumeID uint32) *database.Volume {
	for _, volume := range volumes {
		if volume.VolumeID == int(volumeID) {
			return volume
		}
	}
	return nil
}

// onNodeRe matches a DRBD `on <node> {` section header.
var onNodeRe = regexp.MustCompile(`(?m)^\s*on\s+(\S+)\s*\{`)

// parseResourceConfigNodes extracts the participating node names (the `on
// <node> {` sections) from a DRBD .res file, in file order and de-duplicated.
func parseResourceConfigNodes(content string) []string {
	var nodes []string
	seen := make(map[string]bool)
	for _, m := range onNodeRe.FindAllStringSubmatch(content, -1) {
		n := m[1]
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		nodes = append(nodes, n)
	}
	zap.L().Debug("parseResourceConfigNodes: parsed nodes", zap.Strings("nodes", nodes))
	return nodes
}

// parsePortFromConfig extracts the DRBD port from the first `address ...:<port>;`
// line in a .res file, reusing portLineRe (`:(\d+);`). Returns 0 when absent.
func parsePortFromConfig(content string) uint32 {
	for _, line := range strings.Split(content, "\n") {
		if !strings.Contains(line, "address") {
			continue
		}
		if m := portLineRe.FindStringSubmatch(line); m != nil {
			if p, err := strconv.Atoi(m[1]); err == nil {
				zap.L().Debug("parsePortFromConfig: found port", zap.Int("port", p))
				return uint32(p)
			}
		}
	}
	zap.L().Debug("parsePortFromConfig: no port in config")
	return 0
}

// volumeNameAndPoolFromDiskPath best-effort derives (volumeName, pool) from a
// DRBD backing-disk path: "/dev/<pool>/<lv>" (LVM) or
// "/dev/zvol/<pool>/<dataset>" (ZFS). Returns empty strings when the path does
// not carry that information.
func volumeNameAndPoolFromDiskPath(diskPath string) (volumeName, pool string) {
	diskPath = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(diskPath), ";"))
	defer func() {
		zap.L().Debug("volumeNameAndPoolFromDiskPath: derived", zap.String("device", diskPath),
			zap.String("volume", volumeName), zap.String("pool", pool))
	}()
	if diskPath == "" {
		return "", ""
	}
	if strings.HasPrefix(diskPath, "/dev/zvol/") {
		rest := strings.TrimPrefix(diskPath, "/dev/zvol/")
		if parts := strings.SplitN(rest, "/", 2); len(parts) == 2 {
			return parts[1], parts[0]
		}
		return rest, ""
	}
	if strings.HasPrefix(diskPath, "/dev/") {
		parts := strings.Split(strings.TrimPrefix(diskPath, "/dev/"), "/")
		if len(parts) >= 2 {
			return parts[len(parts)-1], parts[len(parts)-2]
		}
		return parts[len(parts)-1], ""
	}
	return filepath.Base(diskPath), ""
}

// drbdConfigReferencesDisk reports whether a DRBD .res config already contains a
// volume whose backing disk is diskRef (e.g. "/dev/vg0/res_state1;"). Used to
// keep volume adds idempotent: appending a second volume block for a disk that
// is already referenced makes drbdadm reject the config with "conflicting use
// of disk". The "meta-disk" line is skipped so only real backing-disk lines
// match.
func drbdConfigReferencesDisk(config, diskRef string) bool {
	for _, line := range strings.Split(config, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "disk") && strings.Contains(trimmed, diskRef) {
			zap.L().Debug("drbdConfigReferencesDisk: disk already referenced", zap.String("disk", diskRef))
			return true
		}
	}
	zap.L().Debug("drbdConfigReferencesDisk: disk not referenced", zap.String("disk", diskRef))
	return false
}
