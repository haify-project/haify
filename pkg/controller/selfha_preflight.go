package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Self-HA preflight: which nodes take part, which of them this controller
// runs on, and every runtime requirement the handoff script depends on,
// checked while the controller is still alive to report a failure.

// selfHaTargetNodes resolves the requested node list (or all registered
// nodes) to parallel name/address slices.
func (rm *ResourceManager) selfHaTargetNodes(ctx context.Context, requested []string) ([]string, []string, error) {
	log := rm.controller.traceLogger()
	log.Debug("selfHaTargetNodes: resolving", zap.Strings("requested", requested))
	if len(requested) == 0 {
		log.Debug("selfHaTargetNodes: none requested, using every registered node")
		all, err := rm.controller.nodes.ListNodes(ctx)
		if err != nil {
			log.Debug("selfHaTargetNodes: node list failed", zap.Error(err))
			return nil, nil, fmt.Errorf("failed to list nodes: %w", err)
		}
		for _, n := range all {
			requested = append(requested, n.Name)
		}
	}
	names := make([]string, 0, len(requested))
	addrs := make([]string, 0, len(requested))
	for _, name := range requested {
		addr := rm.controller.nodes.GetNodeAddressByName(name)
		if addr == "" {
			log.Debug("selfHaTargetNodes: node not registered", zap.String("node", name))
			return nil, nil, fmt.Errorf("node %s is not registered", name)
		}
		log.Debug("selfHaTargetNodes: node", zap.String("node", name), zap.String("host", addr))
		names = append(names, name)
		addrs = append(addrs, addr)
	}
	return names, addrs, nil
}

// selfNodeAddress identifies which cluster node this controller process is
// running on by matching the local hostname against the registered nodes.
func (rm *ResourceManager) selfNodeAddress(nodeNames, nodeAddrs []string) (string, error) {
	log := rm.controller.traceLogger()
	hostname, err := os.Hostname()
	if err != nil {
		log.Debug("selfNodeAddress: hostname unknown", zap.Error(err))
		return "", fmt.Errorf("failed to determine local hostname: %w", err)
	}
	log.Debug("selfNodeAddress: matching local hostname", zap.String("hostname", hostname), zap.Strings("nodes", nodeNames))
	for i, name := range nodeNames {
		if name == hostname || nodeAddrs[i] == hostname {
			log.Debug("selfNodeAddress: matched by name", zap.String("node", name), zap.String("host", nodeAddrs[i]))
			return nodeAddrs[i], nil
		}
	}
	if addr := rm.controller.nodes.GetNodeAddressByName(hostname); addr != "" && containsString(nodeAddrs, addr) {
		log.Debug("selfNodeAddress: matched by registered address", zap.String("host", addr))
		return addr, nil
	}
	log.Debug("selfNodeAddress: this host is not a target node", zap.String("hostname", hostname))
	return "", fmt.Errorf("controller host %q is not among the target nodes %v; self-HA must be enabled from a cluster node", hostname, nodeNames)
}

// selfHaPreflight validates every runtime requirement that the handoff
// script depends on, so failures happen while the controller is still alive.
func (rm *ResourceManager) selfHaPreflight(ctx context.Context, selfAddr string, standbyAddrs []string) error {
	log := rm.controller.traceLogger()
	log.Debug("selfHaPreflight: checking", zap.String("self", selfAddr), zap.Strings("standbys", standbyAddrs))
	all := append([]string{selfAddr}, standbyAddrs...)

	// A standby that takes over starts the controller with the dispatch
	// config enable copies to it, and reaches the nodes with the key that
	// config names. The key is not copied — it is each node's own — so it has
	// to be there already.
	dispatchContent, err := os.ReadFile(rm.dispatchConfigPath())
	if err != nil {
		log.Debug("selfHaPreflight: dispatch config unreadable", zap.String("path", rm.dispatchConfigPath()), zap.Error(err))
		return fmt.Errorf("the dispatch config %s is needed on every node and cannot be read here: %w",
			rm.dispatchConfigPath(), err)
	}
	if m := dispatchKeyPath.FindSubmatch(dispatchContent); m != nil {
		key := string(m[1])
		log.Debug("selfHaPreflight: checking the SSH key path on standbys", zap.String("key_path", key))
		for _, addr := range standbyAddrs {
			if err := rm.execAllSuccess(ctx, []string{addr}, "sudo test -r "+key,
				fmt.Sprintf("%s has no SSH key at %s, which the dispatch config uses to reach the nodes", addr, key)); err != nil {
				log.Debug("selfHaPreflight: SSH key missing on standby", zap.String("host", addr), zap.Error(err))
				return err
			}
		}
	} else {
		log.Debug("selfHaPreflight: dispatch config names no key_path, key check skipped")
	}

	// drbd-reactor must be running everywhere.
	log.Debug("selfHaPreflight: checking drbd-reactor is active", zap.Strings("hosts", all))
	if err := rm.execAllSuccess(ctx, all, "systemctl is-active --quiet drbd-reactor",
		"drbd-reactor is not active"); err != nil {
		log.Debug("selfHaPreflight: drbd-reactor not active", zap.Error(err))
		return err
	}
	// The VIP unit runs service-ip; install it where it is missing.
	if err := rm.ensureServiceIP(ctx, all); err != nil {
		log.Debug("selfHaPreflight: service-ip not available", zap.Error(err))
		return err
	}
	// The handoff and disable scripts run AS ROOT (via systemd-run) on
	// whichever node is the active controller at that moment, so root's
	// passwordless SSH must work between EVERY pair of nodes — hence the
	// sudo: deployment Exec may land as a non-root user, but it is root's
	// key/known_hosts that the scripts will use. accept-new also
	// pre-populates each node's root known_hosts as a side effect.
	for _, from := range all {
		for _, to := range all {
			if from == to {
				continue
			}
			log.Debug("selfHaPreflight: checking root SSH", zap.String("from", from), zap.String("to", to))
			cmd := fmt.Sprintf("sudo ssh %s %s true", selfHaSSHOpts, to)
			if err := rm.execAllSuccess(ctx, []string{from}, cmd,
				fmt.Sprintf("passwordless root SSH %s -> %s is required (any node may run the controller after failover)", from, to)); err != nil {
				log.Debug("selfHaPreflight: root SSH failed", zap.String("from", from), zap.String("to", to), zap.Error(err))
				return err
			}
		}
	}
	// The standby controllers must not be running (only one controller may
	// own the database).
	for _, addr := range standbyAddrs {
		start := time.Now()
		result, err := rm.deployment.Exec(ctx, []string{addr}, "systemctl is-active --quiet haify-controller && echo RUNNING || echo STOPPED")
		if err != nil {
			log.Debug("selfHaPreflight: controller state check failed", zap.String("host", addr),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			return fmt.Errorf("failed to check haify-controller state on %s: %w", addr, err)
		}
		for _, hr := range result.Hosts {
			if strings.Contains(hr.Output, "RUNNING") {
				log.Debug("selfHaPreflight: controller already running on standby", zap.String("host", addr))
				return fmt.Errorf("haify-controller is already running on %s; stop it before enabling self-HA", addr)
			}
		}
		log.Debug("selfHaPreflight: standby controller stopped", zap.String("host", addr), zap.Duration("took", time.Since(start)))
	}
	log.Debug("selfHaPreflight: passed", zap.Int("nodes", len(all)))
	return nil
}

// dispatchConfigPath is the dispatch config this controller runs with: the
// configured path, or dispatch's default under the controller's home.
func (rm *ResourceManager) dispatchConfigPath() string {
	if dispatchConfigOverride != "" {
		zap.L().Debug("dispatchConfigPath: override set", zap.String("path", dispatchConfigOverride))
		return dispatchConfigOverride
	}
	p := ""
	if rm.controller.config != nil {
		p = strings.TrimSpace(rm.controller.config.Dispatch.ConfigPath)
	}
	if p != "" {
		if strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				zap.L().Debug("dispatchConfigPath: configured path under home", zap.String("path", p), zap.String("home", home))
				return filepath.Join(home, p[2:])
			}
		}
		zap.L().Debug("dispatchConfigPath: configured path", zap.String("path", p))
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		zap.L().Debug("dispatchConfigPath: home unknown, assuming /root", zap.Error(err))
		home = "/root"
	}
	zap.L().Debug("dispatchConfigPath: dispatch default", zap.String("home", home))
	return filepath.Join(home, ".dispatch", "config.toml")
}

// dispatchKeyPath finds the private key a dispatch config authenticates with.
var dispatchKeyPath = regexp.MustCompile(`(?m)^\s*key_path\s*=\s*"([^"]+)"`)
