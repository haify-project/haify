package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// controllerExecutable locates the running controller binary; tests replace it.
var controllerExecutable = os.Executable

// controllerBinaryPlan says what Self-HA installs where on the standbys.
//
// The rule: the binary is installed at the path the copied unit's ExecStart
// names, on every standby. The unit is replicated verbatim, so this is the one
// placement under which a standby that takes over runs exactly the build this
// controller runs. A fixed install path is wrong whenever the unit names
// another one — a controller started from /usr/local/bin used to leave standbys
// whose unit pointed at a stale or missing binary, found only at failover.
type controllerBinaryPlan struct {
	// target is the absolute ExecStart path, the same on every node.
	target string
	// unit is the systemd unit content the target was read from.
	unit string
	// sources maps each standby address to the local file it receives.
	sources map[string]string
}

// planControllerBinary works out the binary placement for the standbys and
// checks each one can execute what it will get. A standby whose architecture
// differs from this controller's receives "haify-controller-<goarch>" from the
// running binary's directory when that file exists, and is refused otherwise.
// It changes nothing, so enable calls it before any side effect.
func (rm *ResourceManager) planControllerBinary(ctx context.Context, standbyAddrs []string) (*controllerBinaryPlan, error) {
	log := rm.resourcesLog()
	log.Debug("planControllerBinary: start", zap.Strings("hosts", standbyAddrs))
	unit, err := os.ReadFile(controllerUnitPath)
	usedPackaged := false
	if errors.Is(err, os.ErrNotExist) {
		unit, err = os.ReadFile(packagedControllerUnitPath)
		usedPackaged = true
	}
	log.Debug("planControllerBinary: read controller unit", zap.Bool("packaged", usedPackaged), zap.Int("bytes", len(unit)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("failed to read %s (or the packaged %s): %w", controllerUnitPath, packagedControllerUnitPath, err)
	}
	target, err := unitExecStartPath(string(unit))
	if err != nil {
		log.Debug("planControllerBinary: no usable ExecStart", zap.Error(err))
		return nil, fmt.Errorf("%s: %w", controllerUnitPath, err)
	}
	exe, err := controllerExecutable()
	log.Debug("planControllerBinary: binary paths", zap.String("target", target), zap.String("executable", exe), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("failed to locate controller binary: %w", err)
	}

	plan := &controllerBinaryPlan{target: target, unit: string(unit), sources: make(map[string]string, len(standbyAddrs))}
	if len(standbyAddrs) == 0 {
		log.Debug("planControllerBinary: no standbys")
		return plan, nil
	}
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, standbyAddrs, "uname -m")
	log.Debug("planControllerBinary: probed architectures", zap.Strings("hosts", standbyAddrs),
		zap.Array("failed_hosts", resourcesFailedHosts{res}), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("check node architecture: %w", err)
	}
	var refused []string
	for _, addr := range standbyAddrs {
		hr := res.Hosts[addr]
		machine := ""
		if hr != nil && hr.Success {
			machine = strings.TrimSpace(hr.Output)
		}
		if machine == "" {
			log.Debug("planControllerBinary: architecture unknown", zap.String("host", addr))
			return nil, fmt.Errorf("check node architecture on %s: %s", rm.nodeLabel(addr), hostFailure(hr))
		}
		arch := goarchFromMachine(machine)
		if arch == runtime.GOARCH {
			log.Debug("planControllerBinary: same architecture; running binary", zap.String("host", addr), zap.String("arch", arch))
			plan.sources[addr] = exe
			continue
		}
		perArch := filepath.Join(filepath.Dir(exe), "haify-controller-"+arch)
		if fileExists(perArch) {
			log.Debug("planControllerBinary: per-architecture binary", zap.String("host", addr), zap.String("arch", arch), zap.String("path", perArch))
			plan.sources[addr] = perArch
			continue
		}
		log.Debug("planControllerBinary: no binary for architecture", zap.String("host", addr), zap.String("arch", arch))
		refused = append(refused, fmt.Sprintf("%s (%s)", rm.nodeLabel(addr), machine))
	}
	if len(refused) > 0 {
		sort.Strings(refused)
		log.Debug("planControllerBinary: refusing", zap.Strings("refused", refused))
		return nil, fmt.Errorf("this controller is built for %s and %s cannot run it; place a build for that "+
			"architecture at %s-<goarch> (e.g. haify-controller-arm64), or leave those nodes out with --nodes",
			unameMachine(runtime.GOARCH), strings.Join(refused, ", "),
			filepath.Join(filepath.Dir(exe), "haify-controller"))
	}
	log.Debug("planControllerBinary: planned", zap.String("target", target), zap.Int("standbys", len(plan.sources)))
	return plan, nil
}

// installControllerBinary installs each standby's planned binary at the
// unit's ExecStart path.
func (rm *ResourceManager) installControllerBinary(ctx context.Context, plan *controllerBinaryPlan) error {
	bySource := make(map[string][]string)
	for addr, src := range plan.sources {
		bySource[src] = append(bySource[src], addr)
	}
	srcs := make([]string, 0, len(bySource))
	for src := range bySource {
		srcs = append(srcs, src)
	}
	sort.Strings(srcs)
	for _, src := range srcs {
		hosts := bySource[src]
		sort.Strings(hosts)
		rm.controller.logger.Info("Installing controller binary",
			zap.Strings("hosts", hosts), zap.String("source", src), zap.String("target", plan.target))
		start := time.Now()
		res, err := rm.deployment.InstallFile(ctx, hosts, src, plan.target, 0o755)
		rm.resourcesLog().Debug("installControllerBinary: installed", zap.Strings("hosts", hosts), zap.String("source", src),
			zap.Bool("success", err == nil && res.Success), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return fmt.Errorf("failed to distribute controller binary: %w", err)
		}
		if !res.Success {
			var failed []string
			for h, hr := range res.Hosts {
				if !hr.Success {
					failed = append(failed, fmt.Sprintf("%s: %s", rm.nodeLabel(h), hostFailure(hr)))
				}
			}
			sort.Strings(failed)
			return fmt.Errorf("failed to distribute controller binary to %s", strings.Join(failed, "; "))
		}
	}
	return nil
}

// unitExecStartPath returns the absolute program path of a systemd unit's
// ExecStart in its [Service] section. An empty "ExecStart=" resets the list,
// as systemd treats it; the first command after the last reset is the one run.
func unitExecStartPath(unit string) (string, error) {
	section, cmd := "", ""
	reset := false
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		if section != "[Service]" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "ExecStart" {
			continue
		}
		val = strings.TrimSpace(val)
		if val == "" {
			cmd, reset = "", true
			continue
		}
		if cmd == "" || reset {
			cmd, reset = val, false
		}
	}
	if cmd == "" {
		zap.L().Debug("unitExecStartPath: no ExecStart in [Service]")
		return "", fmt.Errorf("no ExecStart in [Service]")
	}
	// Special executable prefixes: "-", "@", ":", "+", "!", "!!".
	cmd = strings.TrimSpace(strings.TrimLeft(cmd, "-@:+!"))
	if cmd == "" {
		return "", fmt.Errorf("empty ExecStart")
	}
	var path string
	if q := cmd[:1]; q == `"` || q == "'" {
		end := strings.Index(cmd[1:], q)
		if end < 0 {
			return "", fmt.Errorf("unterminated quote in ExecStart")
		}
		path = cmd[1 : end+1]
	} else {
		path = strings.Fields(cmd)[0]
	}
	if !filepath.IsAbs(path) {
		zap.L().Debug("unitExecStartPath: program not absolute", zap.String("path", path))
		return "", fmt.Errorf("ExecStart program %q is not an absolute path", path)
	}
	zap.L().Debug("unitExecStartPath: program", zap.String("path", path))
	return filepath.Clean(path), nil
}

// goarchFromMachine maps what `uname -m` prints to Go's architecture name.
func goarchFromMachine(machine string) string {
	switch machine {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "loongarch64":
		return "loong64"
	case "i386", "i486", "i586", "i686":
		return "386"
	}
	return machine
}
