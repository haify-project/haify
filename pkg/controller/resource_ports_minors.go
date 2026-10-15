package controller

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// nextGlobalMinor returns the lowest unused DRBD device minor on host,
// derived from every resource config present: minors are a node-global
// namespace and drbdadm rejects configs that reuse one.
// findPortConflict returns the name of an existing DRBD resource on host that
// already binds the given TCP port, or "" if the port is free. The resource
// being created (selfName) is ignored so re-runs don't flag themselves.
func (rm *ResourceManager) findPortConflict(ctx context.Context, host string, port uint32, selfName string) (string, error) {
	log := rm.resourcesLog()
	cmd := fmt.Sprintf("grep -lE 'address[^;]*:%d;' /etc/drbd.d/*.res 2>/dev/null || true", port)
	start := time.Now()
	result, err := rm.deployment.Exec(ctx, []string{host}, cmd)
	log.Debug("findPortConflict: scanned configs", zap.String("host", host), zap.Uint32("port", port),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return "", err
	}
	for _, hr := range result.Hosts {
		for _, line := range strings.Split(hr.Output, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			base := strings.TrimSuffix(filepath.Base(line), ".res")
			if base != selfName {
				log.Debug("findPortConflict: port taken", zap.Uint32("port", port), zap.String("owner", base))
				return base, nil
			}
		}
	}
	log.Debug("findPortConflict: port free", zap.String("host", host), zap.Uint32("port", port))
	return "", nil
}

// nextGlobalMinor returns a device minor free on EVERY given host.
//
// Minors are a node-global namespace, and a resource's minor has to be free on
// all of its nodes — not just the first one. Probing a single host picks a
// minor that some *other* participating node already uses for an unrelated
// resource, and drbdadm rejects the whole config at create-md time with
// "conflicting use of device-minor". That is easy to miss while every resource
// spans the same node set, and shows up as soon as one node carries a resource
// the others do not (a WAN/DR pair, a node added later).
//
// Scanning only .res files also misses minors still held by the kernel from a
// previously-removed resource: its config is gone but the /dev/drbdN node
// lingers and the minor stays "configured", so reusing it makes create-md fail
// with "Device 'N' is configured". Take the max over both the configs and the
// live /dev/drbd* device nodes, across all hosts.
// assertMinorsFreeOn checks that every device minor `resource` uses is free on
// `host`, and returns a diagnosable error naming the squatter if not.
//
// Minors are allocated once, at create time, over the hosts the resource had
// *then*. Every later operation that pulls an additional node in — a quorum
// tiebreaker being moved, a diskless client attaching — inherits those minors
// without checking whether the incoming node already uses them for something
// else. When it does, DRBD refuses with "Minor or volume exists already
// (delete it first)" from deep inside a drbdsetup invocation, long after the
// config has been distributed. Failing here instead says which resource is in
// the way, before anything is changed.
func (rm *ResourceManager) assertMinorsFreeOn(ctx context.Context, host, resource string, minors []int) error {
	log := rm.resourcesLog()
	if len(minors) == 0 {
		log.Debug("assertMinorsFreeOn: no minors to check", zap.String("resource", resource), zap.String("host", host))
		return nil
	}
	// List every minor the node already has, with the resource that owns it.
	// drbdsetup covers minors held by the kernel even when no .res mentions
	// them (a removed resource whose device node lingers).
	cmd := "grep -H -E '^[[:space:]]*device[[:space:]]+minor' /etc/drbd.d/*.res 2>/dev/null; " +
		"sudo drbdsetup show --show-defaults 2>/dev/null | grep -E '^resource|volume|device' || true"
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, []string{host}, cmd)
	log.Debug("assertMinorsFreeOn: listed minors on node", zap.String("resource", resource), zap.String("host", host),
		zap.Ints("minors", minors), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		// A node we cannot inspect is a node we cannot vouch for, but refusing
		// the whole operation on a transient SSH hiccup is worse than letting
		// DRBD be the backstop.
		rm.controller.logger.Warn("Could not verify device minors on node; proceeding",
			zap.String("host", host), zap.String("resource", resource), zap.Error(err))
		return nil
	}

	want := make(map[int]bool, len(minors))
	for _, m := range minors {
		want[m] = true
	}

	for _, hr := range res.Hosts {
		if minor, owner, ok := minorTaken(hr.Output, resource, want); ok {
			log.Debug("assertMinorsFreeOn: minor taken", zap.String("resource", resource), zap.String("host", host),
				zap.Int("minor", minor), zap.String("owner", owner))
			return fmt.Errorf("device minor %d needed by %q is already used on %s by %s; "+
				"free it there (drbdadm down + remove its .res) or recreate %q on a free minor",
				minor, resource, host, owner, resource)
		}
	}
	log.Debug("assertMinorsFreeOn: minors free", zap.String("resource", resource), zap.String("host", host))
	return nil
}

// minorTaken reads the minors a node reports — `grep -H` lines from its .res
// files, then `drbdsetup show` — and says whether one `resource` wants is held
// by something else, and by what.
//
// Both sources name the owner. A grep line carries the file's path; a
// drbdsetup line does not, but follows the `resource "<name>" {` it belongs
// to. Reading only the first, every drbdsetup line looked ownerless, so the
// resource's own minor — still up from an attach whose teardown did not run —
// read as a squatter, and the re-attach was refused for good: a queen pod
// stayed in ContainerCreating for ten hours on exactly that.
func minorTaken(output, resource string, want map[int]bool) (minor int, owner string, taken bool) {
	current := ""
	for _, line := range strings.Split(output, "\n") {
		if name, ok := drbdsetupResource(line); ok {
			current = name
			continue
		}
		m, ok := parseAnyDeviceMinor(line)
		if !ok || !want[m] {
			continue
		}
		owner := current
		if idx := strings.Index(line, ".res:"); idx > 0 {
			owner = strings.TrimSuffix(filepath.Base(line[:idx+4]), ".res")
		}
		if owner == resource {
			continue // our own, from a re-run of the same operation
		}
		if owner == "" {
			owner = "another resource or a stale device node"
		}
		zap.L().Debug("minorTaken: wanted minor held by another owner", zap.String("resource", resource),
			zap.Int("minor", m), zap.String("owner", owner))
		return m, owner, true
	}
	zap.L().Debug("minorTaken: no wanted minor held by another owner", zap.String("resource", resource), zap.Int("wanted", len(want)))
	return 0, "", false
}

// drbdsetupResourceRe matches the line that opens a resource in `drbdsetup
// show`: `resource "pvc_x" {` (quoted in DRBD 9) or `resource pvc_x {`.
var drbdsetupResourceRe = regexp.MustCompile(`^resource\s+"?([^"\s{]+)"?\s*\{`)

func drbdsetupResource(line string) (string, bool) {
	m := drbdsetupResourceRe.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// deviceMinorRe finds a `device ... minor N` anywhere in a line.
//
// parseDeviceMinor only matches a line that *starts* with `device`, which is
// true of a generated .res but not of the two forms this code has to read:
// `grep -H` output ("<path>:        device minor 2;") and an inline volume
// stanza ("volume 0 { device minor 3; }").
var deviceMinorRe = regexp.MustCompile(`\bdevice\b[^;{}]*\bminor\s+(\d+)`)

// parseAnyDeviceMinor extracts a device minor from anywhere in a line.
func parseAnyDeviceMinor(line string) (int, bool) {
	m := deviceMinorRe.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	minor, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return minor, true
}

// resourceMinors reports the device minors a resource's live config uses, in
// file order, deduplicated (every node's `on` stanza repeats the same minors).
func resourceMinors(config string) []int {
	var minors []int
	seen := make(map[int]bool)
	for _, line := range strings.Split(config, "\n") {
		if m, ok := parseAnyDeviceMinor(line); ok && !seen[m] {
			seen[m] = true
			minors = append(minors, m)
		}
	}
	zap.L().Debug("resourceMinors: parsed minors", zap.Ints("minors", minors))
	return minors
}

func (rm *ResourceManager) nextGlobalMinor(ctx context.Context, hosts []string) (int, error) {
	log := rm.resourcesLog()
	if len(hosts) == 0 {
		log.Debug("nextGlobalMinor: no hosts")
		return 0, fmt.Errorf("no hosts to allocate a device minor on")
	}
	start := time.Now()
	result, err := rm.deployment.Exec(ctx, hosts,
		"cat /etc/drbd.d/*.res 2>/dev/null; ls -1d /dev/drbd[0-9]* 2>/dev/null || true")
	log.Debug("nextGlobalMinor: read configs and device nodes", zap.Strings("hosts", hosts),
		zap.Array("failed_hosts", resourcesFailedHosts{result}), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return 0, err
	}
	maxMinor := -1
	for _, hr := range result.Hosts {
		for _, line := range strings.Split(hr.Output, "\n") {
			if minor, ok := parseDeviceMinor(line); ok && minor > maxMinor {
				maxMinor = minor
			}
			if minor, ok := parseDevNodeMinor(line); ok && minor > maxMinor {
				maxMinor = minor
			}
		}
	}
	log.Debug("nextGlobalMinor: chose minor", zap.Int("max_used", maxMinor), zap.Int("minor", maxMinor+1))
	return maxMinor + 1, nil
}

var portLineRe = regexp.MustCompile(`:(\d+);`)

// parsePortsFromResConfigs extracts DRBD ports from the `address ...:<port>;`
// lines of concatenated .res file contents.
func parsePortsFromResConfigs(text string) []uint32 {
	var ports []uint32
	for _, m := range portLineRe.FindAllStringSubmatch(text, -1) {
		if p, err := strconv.Atoi(m[1]); err == nil {
			ports = append(ports, uint32(p))
		}
	}
	zap.L().Debug("parsePortsFromResConfigs: parsed ports", zap.Int("count", len(ports)))
	return ports
}

// lowestFreePort returns the lowest port >= base not present in used.
func lowestFreePort(used []uint32, base uint32) uint32 {
	set := map[uint32]bool{}
	for _, p := range used {
		set[p] = true
	}
	for p := base; ; p++ {
		if !set[p] {
			zap.L().Debug("lowestFreePort: chose port", zap.Uint32("base", base), zap.Int("used", len(used)), zap.Uint32("port", p))
			return p
		}
	}
}

// usedPorts returns the DRBD ports the .res files on host claim.
func (rm *ResourceManager) usedPorts(ctx context.Context, host string) ([]uint32, error) {
	start := time.Now()
	result, err := rm.deployment.Exec(ctx, []string{host}, "cat /etc/drbd.d/*.res 2>/dev/null || true")
	rm.resourcesLog().Debug("usedPorts: read configs", zap.String("host", host),
		zap.Array("failed_hosts", resourcesFailedHosts{result}), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	text := ""
	for _, hr := range result.Hosts {
		text += hr.Output
	}
	return parsePortsFromResConfigs(text), nil
}

// parseDevNodeMinor extracts N from a DRBD device node path like
// "/dev/drbd1005", ignoring the symlink tree under /dev/drbd/.
func parseDevNodeMinor(line string) (int, bool) {
	trimmed := strings.TrimSpace(line)
	const prefix = "/dev/drbd"
	if !strings.HasPrefix(trimmed, prefix) {
		return 0, false
	}
	rest := strings.TrimPrefix(trimmed, prefix)
	if rest == "" || !isAllDigits(rest) {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseDeviceMinor extracts N from DRBD config lines like
// "device minor 7;" or "device /dev/drbd7 minor 7;" regardless of spacing.
func parseDeviceMinor(line string) (int, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "device") || !strings.Contains(trimmed, "minor") {
		return 0, false
	}
	fields := strings.Fields(trimmed)
	for i, f := range fields {
		if f == "minor" && i+1 < len(fields) {
			if minor, err := strconv.Atoi(strings.TrimSuffix(fields[i+1], ";")); err == nil {
				return minor, true
			}
		}
	}
	return 0, false
}
