package controller

import (
	"fmt"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// Editing a resource's .res for a volume that moves to another pool one node
// at a time. A volume's disk path is written once, at resource level, and
// every diskful node shares it; while the move is under way the nodes already
// moved carry their own `volume N { disk ...; }` inside their `on` section,
// which overrides it for that node only. Once every node has moved, the
// resource-level path changes and the overrides go.
//
// Traces here never carry the config text: it can hold a shared-secret.

// onSectionSpan finds the lines of `on <name> { ... }` (the opening line and
// the line that closes it), or -1, -1.
func onSectionSpan(lines []string, name string) (int, int) {
	depth, start := 0, -1
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if start < 0 && depth == 1 {
			if f := strings.Fields(t); len(f) >= 2 && f[0] == "on" && strings.TrimSuffix(f[1], "{") == name && strings.Contains(t, "{") {
				start = i
			}
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if start >= 0 && i > start && depth == 1 {
			return start, i
		}
	}
	return -1, -1
}

// volumeBlockSpan finds `volume <n> { ... }` between from and to (exclusive).
func volumeBlockSpan(lines []string, from, to, volNum int) (int, int) {
	for i := from; i < to; i++ {
		f := strings.Fields(strings.TrimSpace(lines[i]))
		if len(f) >= 2 && f[0] == "volume" && strings.TrimSuffix(f[1], "{") == strconv.Itoa(volNum) {
			depth := 0
			for k := i; k < to; k++ {
				depth += strings.Count(lines[k], "{") - strings.Count(lines[k], "}")
				if depth == 0 {
					return i, k
				}
			}
		}
	}
	return -1, -1
}

// setHostVolumeDisk gives node onName its own disk for volume volNum,
// replacing any override it already had.
func setHostVolumeDisk(content, onName string, volNum, minor int, disk string) (string, error) {
	content = removeHostVolumeDisk(content, onName, volNum)
	lines := strings.Split(content, "\n")
	start, end := onSectionSpan(lines, onName)
	if start < 0 {
		zap.L().Debug("setHostVolumeDisk: no on section for the node", zap.String("node", onName))
		return "", fmt.Errorf("no `on %s` section in the resource config", onName)
	}
	zap.L().Debug("setHostVolumeDisk: adding disk override", zap.String("node", onName), zap.Int("volume", volNum),
		zap.Int("minor", minor), zap.String("disk", disk))
	block := []string{
		fmt.Sprintf("        volume %d {", volNum),
		fmt.Sprintf("            device    minor %d;", minor),
		fmt.Sprintf("            disk      %s;", disk),
		"            meta-disk internal;",
		"        }",
	}
	out := append(append(append([]string{}, lines[:end]...), block...), lines[end:]...)
	return strings.Join(out, "\n"), nil
}

// removeHostVolumeDisk drops node onName's disk override for volNum. A
// `disk none` override (a diskless member) is not a moved disk and stays.
func removeHostVolumeDisk(content, onName string, volNum int) string {
	lines := strings.Split(content, "\n")
	start, end := onSectionSpan(lines, onName)
	if start < 0 {
		zap.L().Debug("removeHostVolumeDisk: no on section, nothing to remove", zap.String("node", onName))
		return content
	}
	vs, ve := volumeBlockSpan(lines, start+1, end, volNum)
	if vs < 0 {
		zap.L().Debug("removeHostVolumeDisk: no override, nothing to remove", zap.String("node", onName), zap.Int("volume", volNum))
		return content
	}
	for _, l := range lines[vs : ve+1] {
		if f := strings.Fields(strings.TrimSpace(l)); len(f) >= 2 && f[0] == "disk" && strings.TrimSuffix(f[1], ";") == "none" {
			zap.L().Debug("removeHostVolumeDisk: diskless member, override kept", zap.String("node", onName), zap.Int("volume", volNum))
			return content
		}
	}
	zap.L().Debug("removeHostVolumeDisk: override removed", zap.String("node", onName), zap.Int("volume", volNum))
	return strings.Join(append(append([]string{}, lines[:vs]...), lines[ve+1:]...), "\n")
}

// setResourceVolumeDisk changes the resource-level disk of volume volNum.
func setResourceVolumeDisk(content string, volNum int, disk string) (string, error) {
	lines := strings.Split(content, "\n")
	for _, v := range parseResourceConfigVolumes(content) {
		if v.VolumeID != volNum {
			continue
		}
		for i := v.StartLine; i <= v.EndLine && i < len(lines); i++ {
			f := strings.Fields(strings.TrimSpace(lines[i]))
			if len(f) >= 2 && f[0] == "disk" && !strings.HasSuffix(f[1], "{") {
				indent := lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " \t"))]
				lines[i] = fmt.Sprintf("%sdisk      %s;", indent, disk)
				zap.L().Debug("setResourceVolumeDisk: disk line rewritten", zap.Int("volume", volNum), zap.String("disk", disk))
				return strings.Join(lines, "\n"), nil
			}
		}
	}
	zap.L().Debug("setResourceVolumeDisk: no disk line for the volume", zap.Int("volume", volNum))
	return "", fmt.Errorf("volume %d has no disk line in the resource config", volNum)
}
