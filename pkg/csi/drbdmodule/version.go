// Package drbdmodule decides whether a Kubernetes node has the DRBD kernel
// module Haify needs, and prepares the node for the module loader of the
// haify-csi chart (drbdModuleLoader) when it does not.
//
// The haify-csi binary runs it as `haify-csi drbd-module <command>`: the loader
// DaemonSet uses prepare, bring-up, hold and check; the node plugin waits with
// wait before it serves any volume. Everything here reads the kernel's own
// view of the module (/sys/module/drbd, /proc/drbd), which a container sees
// the same as the host does.
package drbdmodule

import (
	"fmt"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// Version is a DRBD kernel module version, major.minor.patch. A missing
// patch level reads as 0, so "9.2" means 9.2.0.
type Version struct {
	Major, Minor, Patch int
	// Raw is the text the version was parsed from, such as "9.3.4" or
	// "9.2.0-rc.7", kept so messages show what the kernel reported.
	Raw string
}

// ParseVersion reads "9.3.4", "9.2", "9.2.0-rc.7" or "v9.3.4". Anything after
// the numeric part (a pre-release or build suffix) is ignored for ordering:
// 9.2.0-rc.7 counts as 9.2.0.
func ParseVersion(s string) (Version, error) {
	raw := strings.TrimSpace(s)
	zap.L().Debug("ParseVersion: parsing", zap.String("raw", raw))
	text := strings.TrimPrefix(raw, "v")
	if i := strings.IndexAny(text, "-+ ("); i >= 0 {
		text = text[:i]
	}
	parts := strings.Split(text, ".")
	if text == "" || len(parts) < 2 || len(parts) > 3 {
		zap.L().Debug("ParseVersion: not major.minor[.patch]", zap.String("raw", raw))
		return Version{}, fmt.Errorf("%q is not a DRBD version (want major.minor[.patch])", raw)
	}
	nums := [3]int{}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			zap.L().Debug("ParseVersion: bad component", zap.String("raw", raw), zap.String("component", p))
			return Version{}, fmt.Errorf("%q is not a DRBD version: component %q is not a number", raw, p)
		}
		nums[i] = n
	}
	return Version{Major: nums[0], Minor: nums[1], Patch: nums[2], Raw: raw}, nil
}

// Less reports whether v is older than o.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// AtLeast reports whether v is want or newer.
func (v Version) AtLeast(want Version) bool { return !v.Less(want) }

// String prints the version as the kernel reported it, or major.minor.patch
// for a version built in code.
func (v Version) String() string {
	if v.Raw != "" {
		return v.Raw
	}
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// ParseProcDRBD reads the version from the content of /proc/drbd, whose first
// line is "version: 9.3.4 (api:2/proto:86-124)" on DRBD 9 and
// "version: 8.4.11 (api:1/proto:86-101)" on the 8.4 module some
// distributions ship in their kernel.
func ParseProcDRBD(content string) (Version, error) {
	zap.L().Debug("ParseProcDRBD: parsing", zap.Int("bytes", len(content)))
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "version:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "version:"))
		if len(fields) == 0 {
			break
		}
		return ParseVersion(fields[0])
	}
	zap.L().Debug("ParseProcDRBD: no version line")
	return Version{}, fmt.Errorf("/proc/drbd has no \"version:\" line")
}

// Decision is what the loader has to do on a node.
type Decision int

const (
	// Ready: a DRBD module new enough is loaded; nothing to do.
	Ready Decision = iota
	// Load: no DRBD module is loaded; the loader loads one.
	Load
	// Replace: an older DRBD module is loaded and nothing uses it; it is
	// unloaded and the loader loads the new one.
	Replace
	// InUse: an older DRBD module is loaded and has devices; it cannot be
	// replaced while the node runs.
	InUse
)

// String names the decision for messages and logs.
func (d Decision) String() string {
	switch d {
	case Ready:
		return "ready"
	case Load:
		return "load"
	case Replace:
		return "replace"
	case InUse:
		return "in-use"
	}
	return fmt.Sprintf("decision(%d)", int(d))
}

// Decide picks what to do from the loaded module's version (nil when DRBD is
// not loaded), the minimum Haify needs, and how many DRBD block devices exist.
//
// An older module is replaced only when it has no device. Unloading the
// distribution's DRBD 8.4 that drbdadm pulled in at boot is safe, because
// Haify's DRBD 9 resource files never bring a resource up on it; a module
// that does carry devices is someone's running storage and is left alone.
func Decide(loaded *Version, want Version, devices int) Decision {
	var d Decision
	switch {
	case loaded == nil:
		d = Load
	case loaded.AtLeast(want):
		d = Ready
	case devices > 0:
		d = InUse
	default:
		d = Replace
	}
	zap.L().Debug("Decide: decided", zap.Bool("loaded", loaded != nil), zap.String("want", want.String()),
		zap.Int("devices", devices), zap.String("decision", d.String()))
	return d
}
