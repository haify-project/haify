package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Dual-primary support exists for ONE reason: the live-migration hand-off in
// the Proxmox VE storage plugin. During a live migration the source and target
// hypervisor both hold the disk open for a moment, which DRBD forbids unless
// allow-two-primaries is set. The plugin brackets the migration with
// SetDualPrimary(on) / SetDualPrimary(off).
//
// This is deliberately a RUNTIME-only toggle (`drbdadm net-options`), never
// written into the resource's .res file. That asymmetry is a safety feature: a
// node reboot or `drbdadm adjust` re-reads the on-disk config, which never
// contains allow-two-primaries, so the cluster self-heals back to single-primary
// even if a controller crash strands the "off" call.
//
// See docs/design/proxmox-storage-plugin.md.
const (
	dualPrimaryEnableCmdFmt  = "sudo drbdadm net-options --allow-two-primaries=yes %s"
	dualPrimaryDisableCmdFmt = "sudo drbdadm net-options --allow-two-primaries=no %s"
	dualPrimaryShowCmdFmt    = "sudo drbdsetup show %s"
)

// SetDualPrimary toggles DRBD's allow-two-primaries for a resource on every
// participating node.
//
// enable=true is strict: it refuses WAN resources outright and fails if any node
// rejects the command, because a partially-applied dual-primary is worse than
// none (DRBD needs the option on both ends of a connection, so a half-applied
// toggle produces a migration that fails midway with a confusing error).
//
// enable=false is idempotent and best-effort by design — it runs in the
// plugin's cleanup path, where the alternative to "try anyway" is leaving a
// resource stranded in dual-primary. It therefore tolerates a missing resource
// and per-node command failures, but then VERIFIES the effective state and
// reports an error if any node still has allow-two-primaries set. Tolerating the
// command while checking the outcome is what keeps "best-effort" from meaning
// "silently gave up".
func (rm *ResourceManager) SetDualPrimary(ctx context.Context, resource string, enable bool) error {
	return rm.SetDualPrimaryOn(ctx, resource, enable, nil)
}

// SetDualPrimaryOn is SetDualPrimary; nodes, when set, limits an enable to
// those participants.
//
// allow-two-primaries only matters on a connection whose two ends are both
// Primary, and a live migration has exactly one: source to target. Requiring
// every participant to accept the option meant one PVE host that was down —
// still attached as a diskless client from a guest it once ran — blocked every
// live migration of that guest. Disabling is unchanged: it always covers every
// participant, because a window left open anywhere is what it exists to close.
func (rm *ResourceManager) SetDualPrimaryOn(ctx context.Context, resource string, enable bool, nodes []string) error {
	log := rm.controller.traceLogger()
	resource = strings.TrimSpace(resource)
	log.Debug("ResourceManager.SetDualPrimaryOn: start", zap.String("resource", resource), zap.Bool("enable", enable),
		zap.Strings("nodes", nodes))
	if resource == "" {
		log.Debug("ResourceManager.SetDualPrimaryOn: resource name missing")
		return fmt.Errorf("resource name is required")
	}
	if rm.deployment == nil {
		log.Debug("ResourceManager.SetDualPrimaryOn: no deployment client")
		return fmt.Errorf("deployment client not set")
	}

	// WAN resources replicate with protocol A (async). Two Primaries over an
	// async link is data corruption, not a performance trade-off, so the guard
	// lives here rather than in any caller.
	if rm.controller.db != nil {
		start := time.Now()
		dbRes, err := rm.controller.db.GetResource(ctx, resource)
		log.Debug("ResourceManager.SetDualPrimaryOn: db get resource", zap.String("resource", resource),
			zap.Bool("found", dbRes != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
		switch {
		case err != nil || dbRes == nil:
			// Unknown resource: for disable there is nothing to restore, so the
			// cleanup path succeeds. For enable it is a genuine error, and the
			// host lookup below produces the descriptive one.
			if !enable {
				log.Debug("ResourceManager.SetDualPrimaryOn: unknown resource, nothing to disable", zap.String("resource", resource))
				return nil
			}
		case dbRes.WANMode && enable:
			log.Debug("ResourceManager.SetDualPrimaryOn: WAN resource, enable refused", zap.String("resource", resource))
			return fmt.Errorf(
				"resource %q replicates over WAN (protocol A, asynchronous); dual-primary is refused because two Primaries over an async link corrupts data",
				resource)
		}
	}

	hosts, err := rm.dualPrimaryHosts(ctx, resource)
	if err != nil {
		log.Debug("ResourceManager.SetDualPrimaryOn: resolving hosts failed", zap.Bool("enable", enable), zap.Error(err))
		if !enable {
			return nil
		}
		return err
	}
	if len(hosts) == 0 {
		log.Debug("ResourceManager.SetDualPrimaryOn: resource has no hosts", zap.Bool("enable", enable))
		if !enable {
			return nil
		}
		return fmt.Errorf("resource %q has no nodes", resource)
	}

	if enable && len(nodes) > 0 {
		hosts, err = pickDualPrimaryHosts(hosts, rm.resolveAll(nodes))
		if err != nil {
			log.Debug("ResourceManager.SetDualPrimaryOn: requested node not a participant", zap.Error(err))
			return fmt.Errorf("resource %q: %w", resource, err)
		}
		log.Debug("ResourceManager.SetDualPrimaryOn: limited to requested nodes", zap.Strings("hosts", hosts))
	}

	if enable {
		log.Debug("ResourceManager.SetDualPrimaryOn: enabling allow-two-primaries", zap.Strings("hosts", hosts))
		if err := rm.execAllSuccess(ctx, hosts,
			fmt.Sprintf(dualPrimaryEnableCmdFmt, resource),
			fmt.Sprintf("failed to enable dual-primary on %s", resource)); err != nil {
			log.Debug("ResourceManager.SetDualPrimaryOn: enable failed", zap.Error(err))
			return err
		}
		rm.controller.logger.Info("Enabled DRBD dual-primary (live-migration window)",
			zap.String("resource", resource), zap.Strings("hosts", hosts))
		return nil
	}

	// Disable: issue the command everywhere, ignoring per-host failures, then
	// confirm no node is left dual-primary.
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, hosts, fmt.Sprintf(dualPrimaryDisableCmdFmt, resource))
	if err != nil {
		rm.controller.logger.Warn("Dual-primary disable command failed; verifying effective state",
			zap.String("resource", resource), zap.Error(err))
	} else if ce := log.Check(zap.DebugLevel, "ResourceManager.SetDualPrimaryOn: disable command sent"); ce != nil {
		ce.Write(zap.Strings("hosts", hosts), zap.Strings("failed_hosts", tracedFailedHosts(res)),
			zap.Duration("took", time.Since(start)))
	}

	stranded, err := rm.dualPrimaryStrandedHosts(ctx, hosts, resource)
	if err != nil {
		log.Debug("ResourceManager.SetDualPrimaryOn: verifying disable failed", zap.Error(err))
		return fmt.Errorf("failed to verify dual-primary was disabled on %s: %w", resource, err)
	}
	if len(stranded) > 0 {
		log.Debug("ResourceManager.SetDualPrimaryOn: hosts still dual-primary", zap.Strings("stranded", stranded))
		return fmt.Errorf(
			"resource %q is still in dual-primary on %v — allow-two-primaries must be cleared before the resource is used again",
			resource, stranded)
	}

	rm.controller.logger.Info("Disabled DRBD dual-primary",
		zap.String("resource", resource), zap.Strings("hosts", hosts))
	return nil
}

// dualPrimaryHosts returns every node that participates in the resource's DRBD
// mesh: replicas, quorum tiebreakers and diskless clients alike.
//
// allow-two-primaries is a per-connection net option, so it has to be set on
// both ends of every connection — including diskless clients, which is where it
// matters most here: a PVE host that contributes no disks attaches diskless and
// is exactly the node that live-migration promotes.
func (rm *ResourceManager) dualPrimaryHosts(ctx context.Context, resource string) ([]string, error) {
	log := rm.controller.traceLogger()
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		log.Debug("ResourceManager.dualPrimaryHosts: resolving replica hosts failed", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}

	seen := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		seen[h] = true
	}

	if rm.controller.db != nil {
		if dbRes, derr := rm.controller.db.GetResource(ctx, resource); derr == nil && dbRes != nil {
			for _, node := range append(splitCSV(dbRes.DisklessNodes), splitCSV(dbRes.DisklessClients)...) {
				addr := rm.controller.ResolveHost(node)
				if addr != "" && !seen[addr] {
					log.Debug("ResourceManager.dualPrimaryHosts: adding diskless participant", zap.String("node", node), zap.String("host", addr))
					seen[addr] = true
					hosts = append(hosts, addr)
				}
			}
		} else {
			log.Debug("ResourceManager.dualPrimaryHosts: resource record unreadable, replicas only",
				zap.String("resource", resource), zap.Error(derr))
		}
	}

	log.Debug("ResourceManager.dualPrimaryHosts: resolved", zap.String("resource", resource), zap.Strings("hosts", hosts))
	return hosts, nil
}

// dualPrimaryStrandedHosts returns the hosts whose live DRBD config still has
// allow-two-primaries enabled. `drbdsetup show` prints only non-default options,
// and an unconfigured resource prints nothing — so both "already single-primary"
// and "resource is down" read as not stranded, which is correct.
func (rm *ResourceManager) dualPrimaryStrandedHosts(ctx context.Context, hosts []string, resource string) ([]string, error) {
	log := rm.controller.traceLogger()
	start := time.Now()
	result, err := rm.deployment.Exec(ctx, hosts, fmt.Sprintf(dualPrimaryShowCmdFmt, resource))
	if err != nil {
		log.Debug("ResourceManager.dualPrimaryStrandedHosts: drbdsetup show failed", zap.Strings("hosts", hosts),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, err
	}
	log.Debug("ResourceManager.dualPrimaryStrandedHosts: drbdsetup show ran", zap.Strings("hosts", hosts),
		zap.Duration("took", time.Since(start)))

	var stranded []string
	for host, hr := range result.Hosts {
		if !hr.Success {
			// The show command itself failed (node unreachable). We cannot prove
			// it is clear, but we also cannot prove it is stranded; a node we
			// cannot reach is not holding the volume open either, and it
			// re-reads the .res file (no dual-primary) when it comes back.
			rm.controller.logger.Warn("Could not verify dual-primary state",
				zap.String("resource", resource), zap.String("host", host),
				zap.String("output", strings.TrimSpace(hr.Output)))
			continue
		}
		two := hasAllowTwoPrimaries(hr.Output)
		log.Debug("ResourceManager.dualPrimaryStrandedHosts: host checked", zap.String("host", host),
			zap.Bool("allow_two_primaries", two))
		if two {
			stranded = append(stranded, host)
		}
	}
	return stranded, nil
}

// hasAllowTwoPrimaries reports whether a `drbdsetup show` dump enables
// allow-two-primaries. drbdsetup renders it as `allow-two-primaries yes;`.
func hasAllowTwoPrimaries(showOutput string) bool {
	for _, line := range strings.Split(showOutput, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "allow-two-primaries") {
			continue
		}
		if strings.Contains(line, "yes") {
			return true
		}
	}
	return false
}

// pickDualPrimaryHosts returns the requested hosts, which must all take part
// in the resource: opening the window on a node outside it would do nothing
// and report success.
func pickDualPrimaryHosts(participants, requested []string) ([]string, error) {
	in := make(map[string]bool, len(participants))
	for _, h := range participants {
		in[h] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, h := range requested {
		if !in[h] {
			zap.L().Debug("pickDualPrimaryHosts: requested host not a participant", zap.String("host", h))
			return nil, fmt.Errorf("%s does not take part in it, so it cannot open a dual-primary window", h)
		}
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out, nil
}
