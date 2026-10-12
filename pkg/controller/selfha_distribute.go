package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/deployment"
	"go.uber.org/zap"
)

// Self-HA distribution: copying the controller's binary, config, dispatch
// config and units to the standbys, and running the detached handoff and
// disable scripts. Traces log paths and byte counts only: controller.toml and
// the dispatch config can hold tokens and credentials.

// distributeControllerArtifacts ships the running controller binary, its
// config, and the systemd unit to every standby, and disables autostart there
// (drbd-reactor decides who runs the controller from now on). The binary lands
// at the path the unit's ExecStart names; see controllerBinaryPlan.
func (rm *ResourceManager) distributeControllerArtifacts(ctx context.Context, binPlan *controllerBinaryPlan, standbyAddrs, allAddrs []string) error {
	log := ctrlLogOrGlobal(rm.controller)
	log.Debug("distributeControllerArtifacts: distributing", zap.Strings("standbys", standbyAddrs), zap.Strings("hosts", allAddrs))
	if err := rm.installControllerBinary(ctx, binPlan); err != nil {
		log.Debug("distributeControllerArtifacts: binary install failed", zap.Error(err))
		return err
	}

	// Config: replicate this node's controller.toml verbatim.
	cfgContent, err := os.ReadFile(controllerConfigPath)
	if err != nil {
		log.Debug("distributeControllerArtifacts: controller config unreadable", zap.String("path", controllerConfigPath), zap.Error(err))
		return fmt.Errorf("failed to read %s: %w", controllerConfigPath, err)
	}
	log.Debug("distributeControllerArtifacts: copying controller config", zap.String("path", controllerConfigPath),
		zap.Int("bytes", len(cfgContent)))
	if err := rm.distributeToAll(ctx, standbyAddrs, string(cfgContent), controllerConfigPath, ""); err != nil {
		log.Debug("distributeControllerArtifacts: controller config copy failed", zap.Error(err))
		return fmt.Errorf("failed to distribute controller config: %w", err)
	}

	// Dispatch config: the controller cannot start without it — it is how the
	// controller reaches every node — and it lives outside controller.toml.
	// It used to be left behind, so enable reported success, and the first
	// failover brought the controller up on a standby that exited at once
	// ("dispatch config ... no such file"): the VIP moved and nothing answered
	// on it.
	dispatchPath := rm.dispatchConfigPath()
	dispatchContent, err := os.ReadFile(dispatchPath)
	if err != nil {
		log.Debug("distributeControllerArtifacts: dispatch config unreadable", zap.String("path", dispatchPath), zap.Error(err))
		return fmt.Errorf("failed to read the dispatch config %s: %w", dispatchPath, err)
	}
	log.Debug("distributeControllerArtifacts: copying dispatch config", zap.String("path", dispatchPath),
		zap.Int("bytes", len(dispatchContent)))
	if err := rm.distributeToAll(ctx, standbyAddrs, string(dispatchContent), dispatchPath, ""); err != nil {
		log.Debug("distributeControllerArtifacts: dispatch config copy failed", zap.Error(err))
		return fmt.Errorf("failed to distribute the dispatch config: %w", err)
	}

	// Systemd unit: replicate verbatim (the content the binary's target path
	// was read from) and daemon-reload.
	log.Debug("distributeControllerArtifacts: copying controller unit", zap.String("path", controllerUnitPath))
	if err := rm.distributeToAll(ctx, standbyAddrs, binPlan.unit, controllerUnitPath,
		"sudo systemctl daemon-reload"); err != nil {
		log.Debug("distributeControllerArtifacts: unit copy failed", zap.Error(err))
		return fmt.Errorf("failed to distribute controller unit: %w", err)
	}

	// Standbys must not autostart the controller.
	if len(standbyAddrs) > 0 {
		log.Debug("distributeControllerArtifacts: disabling controller autostart on standbys", zap.Strings("hosts", standbyAddrs))
		if err := rm.execAllSuccess(ctx, standbyAddrs, "sudo systemctl disable haify-controller 2>/dev/null; true",
			"failed to disable haify-controller autostart"); err != nil {
			log.Debug("distributeControllerArtifacts: disabling autostart failed", zap.Error(err))
			return err
		}
	}

	// Mount unit for the database volume, on every node.
	mountUnit := rm.generateSystemdMountUnit(SelfHaResource, selfHaMountPoint, selfHaFsType)
	mountUnitPath := fmt.Sprintf("/etc/systemd/system/%s.mount",
		strings.ReplaceAll(strings.TrimPrefix(selfHaMountPoint, "/"), "/", "-"))
	log.Debug("distributeControllerArtifacts: copying mount unit", zap.String("path", mountUnitPath), zap.Strings("hosts", allAddrs))
	if err := rm.distributeToAll(ctx, allAddrs, mountUnit, mountUnitPath,
		"sudo systemctl daemon-reload"); err != nil {
		log.Debug("distributeControllerArtifacts: mount unit copy failed", zap.Error(err))
		return fmt.Errorf("failed to distribute mount unit: %w", err)
	}
	log.Debug("distributeControllerArtifacts: done")
	return nil
}

// distributeToAll writes content to remotePath on all hosts, optionally
// running postCmd afterwards, and fails if any host fails.
func (rm *ResourceManager) distributeToAll(ctx context.Context, hosts []string, content, remotePath, postCmd string) error {
	log := ctrlLogOrGlobal(rm.controller)
	if len(hosts) == 0 {
		log.Debug("distributeToAll: no hosts, nothing to write", zap.String("path", remotePath))
		return nil
	}
	opts := []deployment.ConfigOption{}
	if postCmd != "" {
		opts = append(opts, deployment.WithPostCommand(postCmd))
	}
	start := time.Now()
	result, err := rm.deployment.DistributeConfig(ctx, hosts, content, remotePath, opts...)
	if err != nil {
		log.Debug("distributeToAll: distribution errored", zap.Strings("hosts", hosts), zap.String("path", remotePath),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return err
	}
	if !result.Success {
		var failed []string
		for host, hr := range result.Hosts {
			if !hr.Success {
				failed = append(failed, fmt.Sprintf("%s: %s", host, strings.TrimSpace(hr.Output)))
			}
		}
		log.Debug("distributeToAll: some hosts failed", zap.String("path", remotePath), zap.Strings("failed", failed),
			zap.Duration("took", time.Since(start)))
		return fmt.Errorf("distribution of %s failed: %v", remotePath, failed)
	}
	log.Debug("distributeToAll: written", zap.Strings("hosts", hosts), zap.String("path", remotePath),
		zap.Int("bytes", len(content)), zap.Bool("post_command", postCmd != ""), zap.Duration("took", time.Since(start)))
	return nil
}

// execAllSuccess runs cmd on hosts and converts any failure into a single
// descriptive error.
func (rm *ResourceManager) execAllSuccess(ctx context.Context, hosts []string, cmd, failMsg string) error {
	log := ctrlLogOrGlobal(rm.controller)
	start := time.Now()
	result, err := rm.deployment.Exec(ctx, hosts, cmd)
	if err != nil {
		log.Debug("execAllSuccess: exec errored", zap.Strings("hosts", hosts), zap.String("cmd", traceCmdSummary(cmd)),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("%s: %w", failMsg, err)
	}
	if !result.AllSuccess() {
		log.Debug("execAllSuccess: failed on some hosts", zap.String("cmd", traceCmdSummary(cmd)), zap.Strings("failed", result.FailedHosts()),
			zap.Duration("took", time.Since(start)))
		for host, hr := range result.Hosts {
			if !hr.Success {
				return fmt.Errorf("%s (node %s): %s", failMsg, host, strings.TrimSpace(hr.Output))
			}
		}
		return fmt.Errorf("%s on: %v", failMsg, result.FailedHosts())
	}
	log.Debug("execAllSuccess: succeeded", zap.Strings("hosts", hosts), zap.String("cmd", traceCmdSummary(cmd)), zap.Duration("took", time.Since(start)))
	return nil
}

// runDetachedScript writes a script to this node and launches it through
// systemd-run, so it keeps running after the controller stops itself.
func (rm *ResourceManager) runDetachedScript(ctx context.Context, selfAddr, scriptPath, content, unitName string) error {
	log := ctrlLogOrGlobal(rm.controller)
	log.Debug("runDetachedScript: installing", zap.String("host", selfAddr), zap.String("script", scriptPath),
		zap.String("unit", unitName), zap.Int("bytes", len(content)))
	if err := rm.distributeToAll(ctx, []string{selfAddr}, content, scriptPath,
		fmt.Sprintf("sudo chmod 755 %s", scriptPath)); err != nil {
		log.Debug("runDetachedScript: install failed", zap.String("script", scriptPath), zap.Error(err))
		return fmt.Errorf("failed to install script %s: %w", scriptPath, err)
	}
	launch := fmt.Sprintf("sudo systemd-run --unit=%s "+collectFlag+" /bin/bash %s", unitName, scriptPath)
	if err := rm.execAllSuccess(ctx, []string{selfAddr}, launch,
		fmt.Sprintf("failed to launch %s", unitName)); err != nil {
		log.Debug("runDetachedScript: launch failed", zap.String("unit", unitName), zap.Error(err))
		return err
	}
	log.Debug("runDetachedScript: launched", zap.String("host", selfAddr), zap.String("unit", unitName))
	return nil
}

// traceCmdSummary is what a trace shows of a remote command: its head, short
// enough to read, and never the body of a base64-wrapped script, which can
// carry anything the script was built with. It slices rather than copies, so
// it costs nothing when Debug is off.
func traceCmdSummary(cmd string) string {
	const maxLen = 120
	if strings.Contains(cmd, "base64 -d") {
		return "base64-wrapped script"
	}
	if len(cmd) > maxLen {
		return cmd[:maxLen]
	}
	return cmd
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
