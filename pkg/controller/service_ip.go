package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/deployment"
	"go.uber.org/zap"
)

// The floating IP of an HA resource and of the controller itself is brought up
// by the service-ip@ systemd template, which runs the service-ip helper. Both
// ship with Haify (cmd/service-ip, configs/service-ip@.service). A node without
// them used to be refused with "service-ip is not installed" and left to the
// operator; the controller now installs them from its own copy.
const (
	serviceIPBinaryPath = "/usr/local/bin/service-ip"
	serviceIPUnitPath   = "/etc/systemd/system/service-ip@.service"
)

// serviceIPSourceOverride, when set, is the only place serviceIPSource looks.
var serviceIPSourceOverride string

// serviceIPUnit is configs/service-ip@.service; a test keeps the two equal.
const serviceIPUnit = `[Unit]
Description=High Availability Service IP %i
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
# Go handles parsing the instance ID (e.g., 192.168.1.1-24_eth0)
ExecStart=/usr/local/bin/service-ip up --instance %i --arp-count 5
ExecStop=/usr/local/bin/service-ip down --instance %i

[Install]
WantedBy=multi-user.target
`

// unameMachine maps a Go architecture to what `uname -m` prints for it.
func unameMachine(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "loong64":
		return "loongarch64"
	case "386":
		return "i686"
	}
	return goarch
}

// serviceIPSource finds the service-ip binary this controller installs from:
// the one installed next to the controller (make install puts both in the same
// directory), else the one this node runs itself.
func serviceIPSource() (string, error) {
	log := zap.L()
	var candidates []string
	if serviceIPSourceOverride != "" {
		candidates = append(candidates, serviceIPSourceOverride)
	} else if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "service-ip"))
	}
	if serviceIPSourceOverride == "" {
		candidates = append(candidates, serviceIPBinaryPath)
	}
	log.Debug("serviceIPSource: looking for service-ip", zap.Strings("candidates", candidates),
		zap.Bool("override", serviceIPSourceOverride != ""))
	if c := firstExecutable(candidates); c != "" {
		log.Debug("serviceIPSource: found", zap.String("path", c))
		return c, nil
	}
	log.Debug("serviceIPSource: none executable")
	return "", fmt.Errorf("looked in %s", strings.Join(candidates, " and "))
}

// ensureServiceIP installs the service-ip helper and its unit template on
// every host that lacks either. The binary is this controller's own build, so
// a host of a different architecture is refused rather than handed a program
// it cannot run — the VIP would fail only at the first failover.
func (rm *ResourceManager) ensureServiceIP(ctx context.Context, hosts []string) error {
	log := rm.controller.traceLogger()
	log.Debug("ensureServiceIP: probing", zap.Strings("hosts", hosts))
	// Silent on a node that has both; otherwise it names the node's machine.
	probe := fmt.Sprintf("[ -x %s ] && [ -f %s ] || uname -m", serviceIPBinaryPath, serviceIPUnitPath)
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, hosts, probe)
	if err != nil {
		log.Debug("ensureServiceIP: probe errored", zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("check for service-ip: %w", err)
	}
	log.Debug("ensureServiceIP: probe answered", zap.Duration("took", time.Since(start)))

	want := unameMachine(runtime.GOARCH)
	var missing, foreign []string
	for _, h := range hosts {
		hr := res.Hosts[h]
		if hr == nil || !hr.Success {
			log.Debug("ensureServiceIP: probe failed on host", zap.String("host", h))
			return fmt.Errorf("check for service-ip on %s: %s", rm.nodeLabel(h), hostFailure(hr))
		}
		switch out := strings.TrimSpace(hr.Output); out {
		case "":
			log.Debug("ensureServiceIP: already installed", zap.String("host", h))
		case want:
			log.Debug("ensureServiceIP: missing, same architecture", zap.String("host", h), zap.String("machine", out))
			missing = append(missing, h)
		default:
			log.Debug("ensureServiceIP: missing, other architecture", zap.String("host", h), zap.String("machine", out),
				zap.String("want", want))
			foreign = append(foreign, fmt.Sprintf("%s (%s)", rm.nodeLabel(h), out))
		}
	}
	if len(foreign) > 0 {
		sort.Strings(foreign)
		log.Debug("ensureServiceIP: cannot install on foreign architectures", zap.Strings("hosts", foreign))
		return fmt.Errorf("service-ip is not installed on %s, and this controller only carries it built for %s; install it at %s there",
			strings.Join(foreign, ", "), want, serviceIPBinaryPath)
	}
	if len(missing) == 0 {
		log.Debug("ensureServiceIP: installed everywhere")
		return nil
	}

	src, err := serviceIPSource()
	if err != nil {
		log.Debug("ensureServiceIP: no local copy to install", zap.Strings("hosts", missing), zap.Error(err))
		return fmt.Errorf("service-ip is not installed on %s and this controller has no copy to install (%v); install it at %s",
			rm.nodeLabels(missing), err, serviceIPBinaryPath)
	}
	rm.controller.logger.Info("Installing service-ip",
		zap.Strings("hosts", missing), zap.String("source", src))
	start = time.Now()
	inst, err := rm.deployment.InstallFile(ctx, missing, src, serviceIPBinaryPath, 0755)
	if err != nil {
		log.Debug("ensureServiceIP: install errored", zap.Strings("hosts", missing), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("install service-ip: %w", err)
	}
	if !inst.Success {
		var failed []string
		for h, hr := range inst.Hosts {
			if !hr.Success {
				failed = append(failed, fmt.Sprintf("%s: %s", rm.nodeLabel(h), hostFailure(hr)))
			}
		}
		sort.Strings(failed)
		log.Debug("ensureServiceIP: install failed", zap.Strings("failed", failed), zap.Duration("took", time.Since(start)))
		return fmt.Errorf("install service-ip: %s", strings.Join(failed, "; "))
	}
	log.Debug("ensureServiceIP: binary installed", zap.Strings("hosts", missing), zap.Duration("took", time.Since(start)))
	if err := rm.distributeToAll(ctx, missing, serviceIPUnit, serviceIPUnitPath,
		"sudo systemctl daemon-reload"); err != nil {
		log.Debug("ensureServiceIP: unit install failed", zap.Error(err))
		return fmt.Errorf("install the service-ip@ unit: %w", err)
	}
	log.Debug("ensureServiceIP: installed", zap.Strings("hosts", missing))
	return nil
}

// nodeLabel names a node by its registered name when it has one.
func (rm *ResourceManager) nodeLabel(addr string) string {
	if rm.controller == nil {
		return addr
	}
	return rm.controller.NodeName(addr)
}

func (rm *ResourceManager) nodeLabels(addrs []string) string {
	names := make([]string, len(addrs))
	for i, a := range addrs {
		names[i] = rm.nodeLabel(a)
	}
	return strings.Join(names, ", ")
}

// hostFailure says why a host's command failed, as far as the result tells.
func hostFailure(hr *deployment.HostResult) string {
	switch {
	case hr == nil:
		return "no result"
	case strings.TrimSpace(hr.Output) != "":
		return strings.TrimSpace(hr.Output)
	case hr.Error != nil:
		return hr.Error.Error()
	}
	return "failed"
}

// ensureNonlocalBind lets a service bind to an HA config's VIP before the VIP
// is up on the node, which is what the start order requires: the VIP is
// started last (see generatePromoterConfig). Persisted in sysctl.d so a reboot
// keeps it; the IPv6 knob is set where the kernel has it.
func (rm *ResourceManager) ensureNonlocalBind(ctx context.Context, hosts []string) error {
	rm.controller.traceLogger().Debug("ensureNonlocalBind: setting ip_nonlocal_bind", zap.Strings("hosts", hosts))
	script := `f=/etc/sysctl.d/90-haify-ha-vip.conf
want='# Haify HA: services may bind to the VIP, which starts after them.
net.ipv4.ip_nonlocal_bind = 1
net.ipv6.ip_nonlocal_bind = 1'
[ "$(cat "$f" 2>/dev/null)" = "$want" ] || printf '%s\n' "$want" > "$f"
sysctl -q -w net.ipv4.ip_nonlocal_bind=1
sysctl -q -w net.ipv6.ip_nonlocal_bind=1 2>/dev/null
true`
	return rm.execAllSuccess(ctx, hosts, "echo "+base64Std(script)+" | base64 -d | sudo /bin/sh",
		"allow services to bind to the VIP before it is up (ip_nonlocal_bind)")
}
