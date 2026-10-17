package controller

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// applyDrbdOptions edits an existing DRBD .res config in place, applying each
// "section/key" -> value entry. Keys without a section go to the resource-level
// "options" section; "disk/..." keys go to the disk-options sub-block of every
// resource-level volume block, as generateDrbdConfig writes them at create
// time. Existing keys are replaced, missing keys and missing blocks are
// created, and a key given an empty value is removed, so DRBD's default applies
// again. Volumes, on-sections and unrelated settings are preserved, so this is
// safe to run against a live, multi-volume resource.
func applyDrbdOptions(config string, raw map[string]string) (string, error) {
	// Option values are not traced: net/shared-secret is one of them.
	log := zap.L()
	log.Debug("applyDrbdOptions: applying", zap.Int("options", len(raw)), zap.Int("config_bytes", len(config)))
	if strings.TrimSpace(config) == "" {
		log.Debug("applyDrbdOptions: empty config")
		return "", fmt.Errorf("empty resource config")
	}
	lines := strings.Split(config, "\n")
	for k, v := range raw {
		section, key := "options", strings.TrimSpace(k)
		if i := strings.Index(k, "/"); i >= 0 {
			section = strings.ToLower(strings.TrimSpace(k[:i]))
			key = strings.TrimSpace(k[i+1:])
		}
		if key == "" {
			log.Debug("applyDrbdOptions: invalid option key", zap.String("option", k))
			return "", fmt.Errorf("invalid option key %q", k)
		}
		log.Debug("applyDrbdOptions: setting option", zap.String("section", section), zap.String("key", key),
			zap.Bool("has_value", strings.TrimSpace(v) != ""))
		paths := [][]string{{section}}
		if section == "disk" {
			// disk options live inside each per-volume block, not at top level.
			volumes, err := volumeHeaders(lines)
			if err != nil {
				log.Debug("applyDrbdOptions: reading volume blocks failed", zap.Error(err))
				return "", err
			}
			log.Debug("applyDrbdOptions: disk option goes into every volume block", zap.Strings("volumes", volumes))
			paths = paths[:0]
			for _, vol := range volumes {
				paths = append(paths, []string{vol, "disk"})
			}
		}
		value := strings.TrimSpace(v)
		for _, path := range paths {
			var err error
			if value == "" {
				lines, err = removeOptionAtPath(lines, path, key)
			} else {
				lines, err = setOptionAtPath(lines, path, key, value)
			}
			if err != nil {
				log.Debug("applyDrbdOptions: setting option failed", zap.Strings("path", path), zap.String("key", key), zap.Error(err))
				return "", err
			}
		}
	}
	log.Debug("applyDrbdOptions: done", zap.Int("lines", len(lines)))
	return strings.Join(lines, "\n"), nil
}

// volumeHeaders returns the headers ("volume 0", "volume 1", ...) of the
// resource-level volume blocks. Per-node overrides inside `on` stanzas are not
// direct children of the resource and are not returned. A config without any
// volume block yields "volume 0", which setOptionAtPath then creates.
func volumeHeaders(lines []string) ([]string, error) {
	open, close, err := resourceBlock(lines)
	if err != nil {
		zap.L().Debug("volumeHeaders: no resource block", zap.Error(err))
		return nil, err
	}
	var headers []string
	depth := 0
	for i := open + 1; i < close; i++ {
		if depth == 0 {
			if f := strings.Fields(strings.TrimSpace(lines[i])); len(f) >= 3 && f[0] == "volume" && f[2] == "{" {
				headers = append(headers, "volume "+f[1])
			}
		}
		depth += braceDelta(lines[i])
		if depth < 0 {
			depth = 0
		}
	}
	if len(headers) == 0 {
		zap.L().Debug("volumeHeaders: no volume block, using volume 0")
		headers = []string{"volume 0"}
	}
	zap.L().Debug("volumeHeaders: found", zap.Strings("volumes", headers))
	return headers, nil
}

func braceDelta(line string) int {
	return strings.Count(line, "{") - strings.Count(line, "}")
}

func indentOf(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

// isBlockHeader reports whether a line opens a "<header> {" block (e.g.
// "options {", "volume 0 {"). It excludes value lines like "disk /dev/x;".
func isBlockHeader(line, header string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, header) {
		return false
	}
	rest := strings.TrimSpace(t[len(header):])
	return strings.HasPrefix(rest, "{")
}

// resourceBlock returns the open/close line indices of the outer "resource { }"
// block (its braces), spanning the whole config.
func resourceBlock(lines []string) (open, close int, err error) {
	open = -1
	for i, l := range lines {
		if strings.Contains(l, "{") {
			open = i
			break
		}
	}
	if open == -1 {
		zap.L().Debug("resourceBlock: no opening brace", zap.Int("lines", len(lines)))
		return 0, 0, fmt.Errorf("no resource block found")
	}
	depth := 0
	for j := open; j < len(lines); j++ {
		depth += braceDelta(lines[j])
		if depth == 0 {
			return open, j, nil
		}
	}
	zap.L().Debug("resourceBlock: unbalanced braces", zap.Int("open", open), zap.Int("lines", len(lines)))
	return 0, 0, fmt.Errorf("unbalanced braces in config")
}

// findChildBlock finds a direct child block named header inside the parent
// block (parentOpen..parentClose), returning its open/close brace lines.
func findChildBlock(lines []string, parentOpen, parentClose int, header string) (open, close int, found bool) {
	depth := 0 // relative to just inside the parent
	for i := parentOpen + 1; i < parentClose; i++ {
		if depth == 0 && isBlockHeader(lines[i], header) {
			d := 0
			for j := i; j <= parentClose; j++ {
				d += braceDelta(lines[j])
				if d == 0 {
					zap.L().Debug("findChildBlock: found", zap.String("header", header), zap.Int("open", i), zap.Int("close", j))
					return i, j, true
				}
			}
			zap.L().Debug("findChildBlock: block never closes inside parent", zap.String("header", header))
			return 0, 0, false
		}
		depth += braceDelta(lines[i])
		if depth < 0 {
			depth = 0
		}
	}
	zap.L().Debug("findChildBlock: not found", zap.String("header", header))
	return 0, 0, false
}

// createChildBlock inserts an empty "<header> {\n}" block just before the
// parent's closing brace and returns the updated lines plus the new block's
// open/close indices.
func createChildBlock(lines []string, parentOpen, parentClose int, header string) ([]string, int, int) {
	zap.L().Debug("createChildBlock: creating block", zap.String("header", header), zap.Int("at_line", parentClose))
	indent := indentOf(lines[parentOpen]) + "    "
	block := []string{indent + header + " {", indent + "}"}
	out := make([]string, 0, len(lines)+len(block))
	out = append(out, lines[:parentClose]...)
	out = append(out, block...)
	out = append(out, lines[parentClose:]...)
	return out, parentClose, parentClose + 1
}

// setKeyInBlock replaces or inserts a "key value;" line as a direct child of
// the block (open..close).
func setKeyInBlock(lines []string, open, close int, key, value string) []string {
	indent := indentOf(lines[open]) + "    "
	newLine := fmt.Sprintf("%s%s %s;", indent, key, value)
	depth := 0
	for i := open + 1; i < close; i++ {
		t := strings.TrimSpace(lines[i])
		if depth == 0 && !strings.Contains(t, "{") {
			if f := strings.Fields(t); len(f) >= 1 && f[0] == key {
				zap.L().Debug("setKeyInBlock: replacing existing key", zap.String("key", key), zap.Int("line", i))
				lines[i] = newLine
				return lines
			}
		}
		depth += braceDelta(lines[i])
		if depth < 0 {
			depth = 0
		}
	}
	zap.L().Debug("setKeyInBlock: inserting new key", zap.String("key", key), zap.Int("line", close))
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:close]...)
	out = append(out, newLine)
	out = append(out, lines[close:]...)
	return out
}

// setOptionAtPath walks the block path (e.g. ["net"] or ["volume 0","disk"]),
// creating blocks as needed, then sets key=value in the deepest block.
func setOptionAtPath(lines []string, path []string, key, value string) ([]string, error) {
	open, close, err := resourceBlock(lines)
	if err != nil {
		zap.L().Debug("setOptionAtPath: no resource block", zap.Strings("path", path), zap.Error(err))
		return nil, err
	}
	for _, header := range path {
		childOpen, childClose, found := findChildBlock(lines, open, close, header)
		if !found {
			zap.L().Debug("setOptionAtPath: block missing, creating it", zap.String("header", header))
			lines, childOpen, childClose = createChildBlock(lines, open, close, header)
		}
		open, close = childOpen, childClose
	}
	return setKeyInBlock(lines, open, close, key, value), nil
}

// removeOptionAtPath deletes key from the block at path, if the block and the
// key are there; a missing one is already what was asked for.
func removeOptionAtPath(lines []string, path []string, key string) ([]string, error) {
	open, close, err := resourceBlock(lines)
	if err != nil {
		zap.L().Debug("removeOptionAtPath: no resource block", zap.Strings("path", path), zap.Error(err))
		return nil, err
	}
	for _, header := range path {
		childOpen, childClose, found := findChildBlock(lines, open, close, header)
		if !found {
			zap.L().Debug("removeOptionAtPath: block missing, nothing to remove", zap.String("header", header))
			return lines, nil
		}
		open, close = childOpen, childClose
	}
	depth := 0
	for i := open + 1; i < close; i++ {
		t := strings.TrimSpace(lines[i])
		if depth == 0 && !strings.Contains(t, "{") {
			if f := strings.Fields(t); len(f) >= 1 && strings.TrimSuffix(f[0], ";") == key {
				zap.L().Debug("removeOptionAtPath: removing key", zap.String("key", key), zap.Int("line", i))
				return append(lines[:i:i], lines[i+1:]...), nil
			}
		}
		depth += braceDelta(lines[i])
		if depth < 0 {
			depth = 0
		}
	}
	zap.L().Debug("removeOptionAtPath: key not set", zap.String("key", key))
	return lines, nil
}
