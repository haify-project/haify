package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// SetOptions updates DRBD options on an existing resource. It rewrites the
// shared .res config in place — preserving volumes and node sections — and runs
// `drbdadm adjust` so the changes take effect without recreating the resource.
// Keys use the "section/key" form (e.g. "net/max-buffers", "disk/on-io-error");
// a bare key defaults to the resource-level "options" section.
func (rm *ResourceManager) SetOptions(ctx context.Context, resource string, options map[string]string) error {
	log := rm.resourcesLog()
	log.Debug("SetOptions: start", zap.String("resource", resource), zap.Int("count", len(options)))
	if len(options) == 0 {
		log.Debug("SetOptions: no options", zap.String("resource", resource))
		return fmt.Errorf("no options provided")
	}
	if err := rm.rewriteResourceConfig(ctx, resource, func(current string) (string, error) {
		updated, err := applyDrbdOptions(current, options)
		if err != nil {
			log.Debug("SetOptions: options do not apply", zap.String("resource", resource), zap.Error(err))
			return "", fmt.Errorf("failed to apply options: %w", err)
		}
		return updated, nil
	}); err != nil {
		log.Debug("SetOptions: rewrite failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	rm.controller.logger.Info("Updated DRBD options",
		zap.String("resource", resource),
		zap.Strings("options", optionKeys(options)))
	return nil
}

// RepairResourceConfig brings every participant's copy of a resource's config
// back into agreement and applies it.
//
// What it fixes is a config that disagrees with itself about the diskless
// nodes: a volume the tiebreaker's stanza has no `disk none` override for, and
// a tiebreaker whose copy of the file never heard of that volume. Resources
// that gained a volume before AddVolume learned to update diskless nodes — every
// gateway on a cluster with a tiebreaker — are in exactly that state, their
// tiebreaker retrying a connection DRBD keeps refusing.
//
// It also puts every registered node's address from the registry into its
// `on` stanza, so a config left behind by a renumbering that did not finish —
// a node's old address, or two peers on one — comes back right.
//
// Last, it installs the boot unit on every participant, so a member that
// joined through add-replica, add-dr, attach-diskless or set-tiebreaker before
// those paths installed it comes back up after its next reboot.
func (rm *ResourceManager) RepairResourceConfig(ctx context.Context, resource string) error {
	log := rm.resourcesLog()
	byHost := rm.controller.nodes.drbdAddressesByHost()
	log.Debug("RepairResourceConfig: start", zap.String("resource", resource), zap.Int("registered_addresses", len(byHost)))
	if err := rm.rewriteResourceConfig(ctx, resource, func(current string) (string, error) {
		return reconcileDrbdAddresses(current, byHost), nil
	}); err != nil {
		log.Debug("RepairResourceConfig: rewrite failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		log.Debug("RepairResourceConfig: no hosts", zap.String("resource", resource), zap.Error(err))
		return err
	}
	encrypted := false
	if rm.controller.db != nil {
		if dbRes, gerr := rm.controller.db.GetResource(ctx, resource); gerr == nil && dbRes != nil {
			encrypted = dbRes.Encrypted
		}
	}
	log.Debug("RepairResourceConfig: installing boot unit on members", zap.String("resource", resource),
		zap.Strings("hosts", hosts), zap.Bool("encrypted", encrypted))
	return rm.ensureBootUnitOnMembers(ctx, resource,
		dedupe(append(hosts, rm.disklessParticipantHosts(ctx, resource)...)), encrypted)
}

// rewriteResourceConfig reads a resource's config from a diskful node, applies
// change, reconciles the diskless nodes' volume overrides, and — when anything
// differs from what the diskless nodes hold — installs the result on every
// participant and adjusts it.
//
// Every participant, not every diskful node: tiebreakers and diskless clients
// hold the same file. Rewriting it on the diskful nodes alone is how their
// copies drifted in the first place.
func (rm *ResourceManager) rewriteResourceConfig(ctx context.Context, resource string, change func(string) (string, error)) error {
	log := rm.resourcesLog()
	log.Debug("rewriteResourceConfig: start", zap.String("resource", resource))
	hosts, diskless, err := rm.stageResourceConfig(ctx, resource, change)
	if err != nil {
		log.Debug("rewriteResourceConfig: staging failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	// Diskless nodes first. When a diskful node learns that a peer's volume
	// is diskless it drops the bitmap it keeps for that peer, and the kernel
	// refuses that ("Can not drop the bitmap when both sides have a disk")
	// until the peer has actually connected as diskless. Adjusting everyone at
	// once lost that race on the first repair of a live gateway.
	adjust := fmt.Sprintf("sudo drbdadm adjust %s", resource)
	if len(diskless) > 0 {
		start := time.Now()
		err := rm.execAllSuccess(ctx, diskless, adjust, "failed to apply the resource config on the diskless nodes")
		log.Debug("rewriteResourceConfig: adjusted diskless nodes", zap.String("resource", resource), zap.Strings("hosts", diskless),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return err
		}
	}
	// The diskless nodes connect asynchronously, so the diskful adjust can
	// still arrive first. Give it a few seconds' grace; with no diskless node
	// there is nothing to wait for and the first answer stands.
	var err2 error
	for attempt := 0; attempt < 5; attempt++ {
		start := time.Now()
		err2 = rm.execAllSuccess(ctx, hosts, adjust, "failed to apply the resource config")
		log.Debug("rewriteResourceConfig: adjusted diskful nodes", zap.String("resource", resource), zap.Strings("hosts", hosts),
			zap.Int("attempt", attempt+1), zap.Duration("took", time.Since(start)), zap.Error(err2))
		if err2 == nil || len(diskless) == 0 {
			return err2
		}
		select {
		case <-ctx.Done():
			log.Debug("rewriteResourceConfig: context done while retrying", zap.String("resource", resource), zap.Error(ctx.Err()))
			return err2
		case <-time.After(3 * time.Second):
		}
	}
	log.Debug("rewriteResourceConfig: diskful adjust kept failing", zap.String("resource", resource), zap.Error(err2))
	return err2
}

// stageResourceConfig reads a resource's config from a diskful node, applies
// change, reconciles the diskless nodes' volume overrides and installs the
// result on every participant, without applying it. It returns the diskful and
// diskless hosts.
func (rm *ResourceManager) stageResourceConfig(ctx context.Context, resource string, change func(string) (string, error)) ([]string, []string, error) {
	log := rm.resourcesLog()
	if rm.deployment == nil {
		log.Debug("stageResourceConfig: no deployment client", zap.String("resource", resource))
		return nil, nil, fmt.Errorf("deployment client not set")
	}
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		log.Debug("stageResourceConfig: no hosts", zap.String("resource", resource), zap.Error(err))
		return nil, nil, err
	}
	if len(hosts) == 0 {
		log.Debug("stageResourceConfig: resource has no nodes", zap.String("resource", resource))
		return nil, nil, fmt.Errorf("resource %q has no nodes", resource)
	}
	diskless := rm.disklessParticipantHosts(ctx, resource)
	allHosts := append(append([]string(nil), hosts...), diskless...)
	resPath := fmt.Sprintf("/etc/drbd.d/%s.res", resource)

	// Read the current config from a diskful node: it is the one that
	// describes the resource's volumes.
	start := time.Now()
	result, err := rm.deployment.Exec(ctx, []string{hosts[0]}, "cat "+resPath)
	log.Debug("stageResourceConfig: read config", zap.String("resource", resource), zap.String("host", hosts[0]),
		zap.Array("failed_hosts", resourcesFailedHosts{result}), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read config: %w", err)
	}
	var current string
	var found bool
	for _, hr := range result.Hosts {
		current, found = hr.Output, hr.Success
		break
	}
	if !found || strings.TrimSpace(current) == "" {
		log.Debug("stageResourceConfig: config not found", zap.String("resource", resource), zap.String("host", hosts[0]))
		return nil, nil, fmt.Errorf("resource %q config not found on %s", resource, hosts[0])
	}

	updated, err := change(current)
	if err != nil {
		log.Debug("stageResourceConfig: change refused", zap.String("resource", resource), zap.Error(err))
		return nil, nil, err
	}
	updated = reconcileDisklessVolumeOverrides(updated)
	log.Debug("stageResourceConfig: config changed", zap.String("resource", resource),
		zap.Bool("differs", updated != current), zap.Int("bytes", len(updated)))

	start = time.Now()
	_, err = rm.deployment.DistributeConfig(ctx, allHosts, updated, resPath)
	log.Debug("stageResourceConfig: distributed config", zap.String("resource", resource), zap.Strings("hosts", allHosts),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to distribute updated config: %w", err)
	}

	return hosts, diskless, nil
}
