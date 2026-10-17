package controller

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// What an option change must pass before it reaches a node.
//
// Locally: the key names a section a .res file has and is a single word, and
// the value is one DRBD token or one quoted string, so no value can close a
// block or start another statement. Then on a node: the whole new config is
// parsed by that node's own drbdadm (`drbdadm -c <file> -d adjust`), which
// knows every option its DRBD version has, with its type, range and allowed
// words. Only a config that parses is installed.

// optionSections are the sections a key may name. disk options go into every
// volume block; DRBD's peer-device options (resync-rate, c-plan-ahead, ...)
// are written there too.
var optionSections = map[string]bool{"options": true, "net": true, "disk": true, "startup": true, "handlers": true}

var (
	optionKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	// A bare DRBD token: a number with an optional unit, a word, a path.
	optionTokenPattern = regexp.MustCompile(`^[A-Za-z0-9._:/@+%-]+$`)
	// A quoted string, for a handler command with arguments.
	optionQuotedPattern = regexp.MustCompile(`^"[^"\\;{}#\r\n]*"$`)
)

// managedOptions are set by their own commands, which do what the option alone
// does not; set here they would be undone or would break the command.
var managedOptions = map[string]string{
	"options/auto-promote":    "Haify promotes resources itself (failover, CSI, gateways), so auto-promote stays off",
	"net/allow-two-primaries": "use haify resource dual-primary, which allows two Primaries only for as long as needed",
	"net/tls":                 "use haify resource tls, which installs the certificates and switches each link in turn",
}

// splitOptionKey returns the section and key of "section/key" or "key".
func splitOptionKey(k string) (section, key string) {
	k = strings.TrimSpace(k)
	if i := strings.Index(k, "/"); i >= 0 {
		return strings.ToLower(strings.TrimSpace(k[:i])), strings.TrimSpace(k[i+1:])
	}
	return "options", k
}

// validateDrbdOptions checks option keys and values before anything is
// written. An empty value means the key is to be removed.
func validateDrbdOptions(options map[string]string) error {
	log := zap.L()
	keys := make([]string, 0, len(options))
	for k := range options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		section, key := splitOptionKey(k)
		if !optionSections[section] {
			log.Debug("validateDrbdOptions: unknown section", zap.String("option", k))
			return fmt.Errorf("option %q: section %q is not one of options, net, disk, startup, handlers", k, section)
		}
		if !optionKeyPattern.MatchString(key) {
			log.Debug("validateDrbdOptions: malformed key", zap.String("option", k))
			return fmt.Errorf("option %q: a key is lowercase letters, digits and dashes", k)
		}
		if why, managed := managedOptions[section+"/"+key]; managed {
			log.Debug("validateDrbdOptions: managed key", zap.String("option", k))
			return fmt.Errorf("option %q cannot be set here: %s", k, why)
		}
		v := strings.TrimSpace(options[k])
		if v == "" {
			continue
		}
		if !optionTokenPattern.MatchString(v) && !optionQuotedPattern.MatchString(v) {
			// The value is not traced: it may be a shared-secret.
			log.Debug("validateDrbdOptions: malformed value", zap.String("option", k))
			return fmt.Errorf("option %q: the value must be one word, number or path, or one double-quoted string without ; { } or #", k)
		}
	}
	return nil
}

// checkDrbdConfig has a node's drbdadm parse config, the resource's whole new
// .res, without applying it. It tries the resource's diskful nodes in turn
// until one can run the check.
func (rm *ResourceManager) checkDrbdConfig(ctx context.Context, resource, config string) error {
	log := rm.resourcesLog()
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		log.Debug("checkDrbdConfig: no hosts", zap.String("resource", resource), zap.Error(err))
		return err
	}
	path := fmt.Sprintf("/var/lib/haify/drbd-check/%s.res", resource)
	cmd := fmt.Sprintf("out=$(sudo drbdadm -c %s -d adjust %s 2>&1); rc=$?; sudo rm -f %s; "+
		"[ $rc -eq 0 ] || { echo \"$out\" | grep -v '^drbdsetup \\|^\\*\\*\\*\\|Aborted'; exit 1; }", path, resource, path)
	var lastErr error
	for _, h := range hosts {
		start := time.Now()
		if _, err := rm.deployment.DistributeConfig(ctx, []string{h}, config, path); err != nil {
			log.Debug("checkDrbdConfig: staging the config failed", zap.String("host", h),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			lastErr = err
			continue
		}
		res, err := rm.deployment.Exec(ctx, []string{h}, cmd)
		out := strings.TrimSpace(hostOutput(res, h))
		log.Debug("checkDrbdConfig: drbdadm parsed the config", zap.String("resource", resource), zap.String("host", h),
			zap.Bool("ok", err == nil && res != nil && res.AllSuccess()), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil || res == nil {
			lastErr = fmt.Errorf("check on %s: %v", h, err)
			continue
		}
		if !res.AllSuccess() {
			if out == "" {
				// Nothing from drbdadm: the command itself did not run.
				lastErr = fmt.Errorf("check on %s: %s", h, res.FailureDetails())
				continue
			}
			return fmt.Errorf("DRBD rejects the options: %s", parseErrorText(out, path, resource))
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("resource %q has no nodes", resource)
	}
	return fmt.Errorf("could not check the options on any node of %s: %w", resource, lastErr)
}

// secretOptions are reported as set, never with their value.
var secretOptions = map[string]bool{"net/shared-secret": true}

// ResourceOption is one DRBD option a resource's config sets.
type ResourceOption struct {
	Key    string
	Value  string
	Secret bool
}

// readDrbdOptions lists the options a .res sets at resource level, as
// "section/key", and the disk options of its first volume. Per-node and
// per-connection settings are not options in this sense and are left out.
func readDrbdOptions(config string) ([]ResourceOption, error) {
	lines := strings.Split(config, "\n")
	open, close, err := resourceBlock(lines)
	if err != nil {
		return nil, err
	}
	var out []ResourceOption
	collect := func(section string, bo, bc int) {
		depth := 0
		for i := bo + 1; i < bc; i++ {
			t := strings.TrimSpace(lines[i])
			if depth == 0 && t != "" && !strings.HasPrefix(t, "#") && !strings.Contains(t, "{") {
				t = strings.TrimSuffix(t, ";")
				f := strings.Fields(t)
				if len(f) >= 1 {
					key := section + "/" + f[0]
					o := ResourceOption{Key: key, Value: strings.TrimSpace(strings.TrimPrefix(t, f[0]))}
					if secretOptions[key] {
						o.Value, o.Secret = "", true
					}
					out = append(out, o)
				}
			}
			depth += braceDelta(lines[i])
			if depth < 0 {
				depth = 0
			}
		}
	}
	for _, s := range []string{"options", "net", "startup", "handlers"} {
		if bo, bc, ok := findChildBlock(lines, open, close, s); ok {
			collect(s, bo, bc)
		}
	}
	if vols, err := volumeHeaders(lines); err == nil && len(vols) > 0 {
		if vo, vc, ok := findChildBlock(lines, open, close, vols[0]); ok {
			if bo, bc, ok := findChildBlock(lines, vo, vc, "disk"); ok {
				collect("disk", bo, bc)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// GetOptions returns the DRBD options resource's config sets, read from one of
// its diskful nodes.
func (rm *ResourceManager) GetOptions(ctx context.Context, resource string) ([]ResourceOption, error) {
	log := rm.resourcesLog()
	log.Debug("GetOptions: start", zap.String("resource", resource))
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		log.Debug("GetOptions: no hosts", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	path := fmt.Sprintf("/etc/drbd.d/%s.res", resource)
	var lastErr error
	for _, h := range hosts {
		start := time.Now()
		res, err := rm.deployment.Exec(ctx, []string{h}, "cat "+path)
		out := hostOutput(res, h)
		log.Debug("GetOptions: read config", zap.String("host", h), zap.Int("bytes", len(out)),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil || res == nil || !res.AllSuccess() || strings.TrimSpace(out) == "" {
			lastErr = fmt.Errorf("could not read %s on %s", path, h)
			continue
		}
		opts, err := readDrbdOptions(out)
		log.Debug("GetOptions: parsed", zap.String("resource", resource), zap.Int("options", len(opts)), zap.Error(err))
		return opts, err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("resource %q has no nodes", resource)
	}
	return nil, lastErr
}

// parseErrorText is what drbdadm said about a config, without the noise of the
// abort that follows some of its messages, and naming the resource's own file
// instead of the copy that was checked.
func parseErrorText(out, path, resource string) string {
	var keep []string
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || t == "Aborted" || strings.HasPrefix(t, "***") || strings.HasPrefix(t, "drbdsetup ") ||
			strings.Contains(t, "Aborted") && strings.Contains(t, "drbdadm") {
			continue
		}
		keep = append(keep, strings.TrimRight(l, " \t"))
	}
	return strings.ReplaceAll(strings.Join(keep, "\n"), path, resource+".res")
}
