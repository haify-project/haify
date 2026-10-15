package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// SetPrimary sets a resource to Primary on the specified node
func (rm *ResourceManager) SetPrimary(ctx context.Context, resource, node string, force bool) error {
	log := rm.resourcesLog()
	// Resolve node name to address
	address := rm.controller.ResolveHost(node)

	rm.controller.logger.Info("Setting resource primary",
		zap.String("resource", resource),
		zap.String("node", node),
		zap.String("address", address),
		zap.Bool("force", force))

	if rm.deployment == nil {
		log.Debug("SetPrimary: no deployment client", zap.String("resource", resource))
		return fmt.Errorf("deployment client not set")
	}

	start := time.Now()
	result, err := rm.deployment.DRBDPrimary(ctx, address, resource, force)
	log.Debug("SetPrimary: promote", zap.String("resource", resource), zap.String("host", address), zap.Bool("force", force),
		zap.Bool("success", err == nil && result.Success), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err == nil && result.Success {
		return nil
	}
	if force && err == nil && rm.isDRNodeOf(ctx, resource, node) && isDRPromoteRefusal(result.Output) {
		// A forced promote of a WAN resource's DR node is a DR failover
		// (dr_failover.go).
		return rm.drFailoverPromote(ctx, resource, node, address)
	}

	// A brand-new resource comes up Inconsistent on every node with NO UpToDate
	// replica anywhere, so a normal `drbdsetup primary` fails with "Need access
	// to UpToDate data" (exit 17). This is the initial-sync case: there is no
	// good data to lose, so force-promote ONCE to establish the first UpToDate
	// copy and kick off the initial sync — after which the resource can be
	// demoted/managed normally. A resource created for a gateway never gets a
	// filesystem step (which is where the CSI path already force-primaries), so
	// without this its promote would always fail. The force is strictly scoped
	// to the no-UpToDate-anywhere case: a normal failover (some replica still
	// UpToDate) is never force-promoted here, preserving the split-brain guards
	// in PromoteForNode.
	if !force {
		needsForce, ferr := rm.resourceNeedsInitialForce(ctx, resource, address)
		log.Debug("SetPrimary: initial-sync check", zap.String("resource", resource), zap.Bool("needs_force", needsForce), zap.Error(ferr))
		if ferr != nil {
			rm.controller.logger.Warn("Could not determine initial-sync state after a failed promote; not forcing",
				zap.String("resource", resource), zap.String("node", node), zap.Error(ferr))
		} else if needsForce {
			rm.controller.logger.Warn("Resource has a volume with no UpToDate copy anywhere (fresh initial sync); force-promoting to establish UpToDate",
				zap.String("resource", resource), zap.String("node", node))
			start := time.Now()
			forced, fErr := rm.deployment.DRBDPrimary(ctx, address, resource, true)
			log.Debug("SetPrimary: initial-sync force promote", zap.String("resource", resource), zap.String("host", address),
				zap.Bool("success", fErr == nil && forced.Success), zap.Duration("took", time.Since(start)), zap.Error(fErr))
			if fErr != nil {
				return fmt.Errorf("failed to force-promote initial-sync resource on %s: %w", node, fErr)
			}
			if !forced.Success {
				return fmt.Errorf("failed to force-promote initial-sync resource on %s: %s", node, forced.Output)
			}
			return nil
		}
	}

	if err != nil {
		return fmt.Errorf("failed to set primary: %w", err)
	}
	log.Debug("SetPrimary: promote refused", zap.String("resource", resource), zap.String("node", node))
	return fmt.Errorf("failed to set primary on %s: %s", node, result.Output)
}

// isDRNodeOf reports whether node is the DR node of a WAN resource.
func (rm *ResourceManager) isDRNodeOf(ctx context.Context, resource, node string) bool {
	if rm.controller.db == nil {
		return false
	}
	r, err := rm.controller.db.GetResource(ctx, resource)
	if err != nil || r == nil {
		rm.resourcesLog().Debug("isDRNodeOf: no resource record", zap.String("resource", resource), zap.Error(err))
		return false
	}
	dr := r.WANMode && r.DRNode != "" &&
		(r.DRNode == node || rm.controller.ResolveHost(r.DRNode) == rm.controller.ResolveHost(node))
	rm.resourcesLog().Debug("isDRNodeOf: checked", zap.String("resource", resource), zap.String("node", node), zap.Bool("dr", dr))
	return dr
}

// resourceNeedsInitialForce reports whether a failed non-forced promote should
// be escalated to `drbdadm primary --force`, decided PER VOLUME from
// `drbdsetup status <res> --json` on the given node address. It returns true
// only when the resource is in the fresh initial-sync state — at least one local
// volume is not UpToDate AND has no UpToDate copy on any peer — AND forcing is
// safe for every volume (no volume is locally non-UpToDate while a peer holds a
// real UpToDate copy, which a blanket force would overwrite). Any failure to
// read or parse returns an error so the caller fails closed (never forces on
// uncertainty).
func (rm *ResourceManager) resourceNeedsInitialForce(ctx context.Context, resource, address string) (bool, error) {
	if rm.deployment == nil {
		return false, fmt.Errorf("deployment client not set")
	}
	start := time.Now()
	res, err := rm.deployment.DRBDStatusJSON(ctx, []string{address}, resource)
	rm.resourcesLog().Debug("resourceNeedsInitialForce: read JSON status", zap.String("resource", resource), zap.String("host", address),
		zap.Array("failed_hosts", resourcesFailedHosts{res}), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return false, fmt.Errorf("read drbd status on %s: %w", address, err)
	}
	for _, r := range res.Hosts {
		if !r.Success {
			return false, fmt.Errorf("drbd status on %s failed: %s", address, r.Output)
		}
		return safeToForceInitialSync(r.Output)
	}
	return false, fmt.Errorf("no drbd status result returned for %s", address)
}

// safeToForceInitialSync parses `drbdsetup status <res> --json` (an array of
// resources, each with per-volume devices[] and per-connection peer_devices[])
// and decides, per volume, whether a resource-level force-promote is BOTH
// needed and safe.
//
// The check is per volume — crucially, a single UpToDate volume must NOT mask a
// sibling volume that has no UpToDate data. A gateway auto-adds a state volume
// (volume 1) that becomes UpToDate during its add, while the data volume
// (volume 0) is still Inconsistent everywhere; a resource-level "is any replica
// UpToDate" test is fooled by volume 1 and wrongly refuses the force that
// volume 0 needs. For each LOCAL device:
//   - already UpToDate    -> a force cannot harm it; ignore.
//   - Outdated or Consistent, or a peer holds a copy with data (UpToDate,
//     Outdated, Consistent) of THIS volume -> forcing would rewrite real
//     data: UNSAFE, so refuse the force entirely.
//   - Inconsistent, every peer connected and none holding data -> a fresh
//     unsynced volume with no data to lose: force is needed and safe for it.
//
// Any peer that is not connected refuses the force: it may hold the data.
//
// Returns true only if at least one volume needs the force and NO volume made it
// unsafe. Empty/unparseable input returns an error so callers fail closed.
func safeToForceInitialSync(output string) (bool, error) {
	log := zap.L()
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return false, fmt.Errorf("empty drbdsetup status json")
	}
	var resources []drbdsetupStatus
	if err := json.Unmarshal([]byte(trimmed), &resources); err != nil {
		log.Debug("safeToForceInitialSync: unparseable status", zap.Error(err))
		return false, fmt.Errorf("decode drbdsetup status json: %w", err)
	}
	if len(resources) == 0 {
		return false, fmt.Errorf("drbdsetup status json contained no resources")
	}
	res := resources[0]
	if len(res.Devices) == 0 {
		return false, fmt.Errorf("drbdsetup status json reported no local devices")
	}

	// A peer that is not connected may hold the only good copy: its disk
	// state is unknown, not absent. Forcing past it is how an Outdated
	// replica became the data while the node with the newer writes was still
	// booting (a PVE host rebooted with its VM running).
	for _, conn := range res.Connections {
		if conn.ConnectionState != "Connected" {
			log.Debug("safeToForceInitialSync: a peer is not connected; no force", zap.String("peer", conn.Name),
				zap.String("connection", conn.ConnectionState))
			return false, nil
		}
	}
	// Per volume: does any peer hold a copy that has data?
	peerHasData := make(map[int]bool)
	for _, conn := range res.Connections {
		for _, pd := range conn.PeerDevices {
			switch pd.PeerDiskState {
			case "UpToDate", "Outdated", "Consistent", "DUnknown":
				peerHasData[pd.Volume] = true
			}
		}
	}

	needForce := false
	for _, dev := range res.Devices {
		log.Debug("safeToForceInitialSync: local volume", zap.Int("volume_id", dev.Volume), zap.String("disk", dev.DiskState),
			zap.Bool("peer_has_data", peerHasData[dev.Volume]))
		if dev.DiskState == "UpToDate" {
			continue
		}
		// Only a volume that has never been synced is a fresh one. Outdated
		// and Consistent copies have history a force would rewrite.
		if dev.DiskState != "Inconsistent" || peerHasData[dev.Volume] {
			return false, nil
		}
		// This volume has no copy with data anywhere: fresh, nothing to lose.
		needForce = true
	}
	log.Debug("safeToForceInitialSync: verdict", zap.Bool("needs_force", needForce))
	return needForce, nil
}

// establishInitialSync force-promotes a brand-new resource on the given diskful
// node address and immediately demotes it, so all of its volumes reach UpToDate
// and its peers get a sync source. It is only ever called right after a fresh
// create-md + up, where every replica is Inconsistent and forcing loses no data.
// After it returns, the resource is Secondary+UpToDate and can be promoted with
// a plain, non-forced promote.
func (rm *ResourceManager) establishInitialSync(ctx context.Context, resource, address string) error {
	log := rm.resourcesLog()
	if rm.deployment == nil {
		return fmt.Errorf("deployment client not set")
	}
	rm.controller.logger.Info("Establishing initial UpToDate generation (force-primary then demote)",
		zap.String("resource", resource), zap.String("address", address))

	start := time.Now()
	forced, err := rm.deployment.DRBDPrimary(ctx, address, resource, true)
	log.Debug("establishInitialSync: force promote", zap.String("resource", resource), zap.String("host", address),
		zap.Bool("success", err == nil && forced.Success), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("force-promote for initial sync: %w", err)
	}
	if !forced.Success {
		return fmt.Errorf("force-promote for initial sync on %s: %s", address, forced.Output)
	}

	start = time.Now()
	demoted, err := rm.deployment.DRBDSecondary(ctx, address, resource)
	log.Debug("establishInitialSync: demote", zap.String("resource", resource), zap.String("host", address),
		zap.Bool("success", err == nil && demoted.Success), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("demote after initial sync: %w", err)
	}
	if !demoted.Success {
		return fmt.Errorf("demote after initial sync on %s: %s", address, demoted.Output)
	}
	return nil
}

// SetSecondary sets a resource to Secondary on the specified node
func (rm *ResourceManager) SetSecondary(ctx context.Context, resource, node string) error {
	log := rm.resourcesLog()
	address := rm.controller.ResolveHost(node)

	rm.controller.logger.Info("Setting resource secondary",
		zap.String("resource", resource),
		zap.String("node", node),
		zap.String("address", address))

	if rm.deployment == nil {
		log.Debug("SetSecondary: no deployment client", zap.String("resource", resource))
		return fmt.Errorf("deployment client not set")
	}

	start := time.Now()
	result, err := rm.deployment.DRBDSecondary(ctx, address, resource)
	log.Debug("SetSecondary: demote", zap.String("resource", resource), zap.String("host", address),
		zap.Bool("success", err == nil && result.Success), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("failed to set secondary: %w", err)
	}

	if !result.Success {
		// DRBD says why — "Device is held open by someone" when the volume is
		// mounted or a service has it — and a caller cannot act on a bare
		// "failed"; a drain that stops here is asking for exactly that.
		return fmt.Errorf("failed to set secondary on %s: %s", node, hostFailure(result))
	}

	return nil
}

// PromoteForNode performs a SAFE hard-failover promote of a resource on a node.
//
// Why this exists: on a GRACEFUL move the old Primary demotes first, so a plain
// (non-forced) promote on the new node succeeds. On a HARD failure the old
// Primary is gone/uncontactable and was never demoted, so a non-forced promote
// FAILS and the volume never comes up — no automatic failover. Blindly forcing
// would risk a dual-Primary split-brain if the "failed" node is actually alive
// behind a network partition.
//
// The safe rule is DRBD-native: rely on quorum. haify configures resources with
// `quorum majority` + `on-no-quorum io-error` and auto-adds a diskless
// tiebreaker to 2-node resources, giving a 3-way majority. A hard-failed or
// partitioned old Primary that cannot reach the majority LOSES quorum and its
// DRBD blocks all I/O, so it cannot serve stale writes. Therefore it is safe to
// force-promote a surviving Secondary IFF that survivor currently holds quorum.
//
// Algorithm:
//
//	(a) try a normal, non-forced promote — the safe graceful path;
//	(b) if it fails (a peer still holds Primary / is unreachable), read this
//	    node's DRBD quorum flag via `drbdsetup status --json`;
//	(c) only if quorum == true, retry with `drbdadm primary --force`;
//	(d) if quorum == false (or unknown), REFUSE with an error — promoting
//	    without quorum could split-brain.
func (rm *ResourceManager) PromoteForNode(ctx context.Context, resource, node string) error {
	log := rm.resourcesLog()
	log.Debug("PromoteForNode: start", zap.String("resource", resource), zap.String("node", node))
	// (a) Safe path: a plain promote succeeds on a graceful move (old Primary
	// already Secondary) and on any node that can already take Primary. This
	// path is unchanged from the previous non-forced behavior.
	if err := rm.SetPrimary(ctx, resource, node, false); err == nil {
		log.Debug("PromoteForNode: plain promote succeeded", zap.String("resource", resource), zap.String("node", node))
		return nil
	} else {
		rm.controller.logger.Warn("Normal promote failed; evaluating quorum before any force-promote",
			zap.String("resource", resource), zap.String("node", node), zap.Error(err))

		// A connected peer that is Primary is not a failed one: the volume is
		// simply in use there. Saying so beats the failover verdicts below,
		// which read a diskless client's copy as one missing writes.
		if peer := rm.connectedPrimaryPeer(ctx, resource, node); peer != "" {
			log.Debug("PromoteForNode: a connected peer is Primary; refusing", zap.String("resource", resource), zap.String("peer", peer))
			return fmt.Errorf("%s is in use on %s, which has it Primary; release it there first (promote error: %v)",
				resource, peer, err)
		}

		// (b) The promote failed — most likely a hard failover where the old
		// Primary was never demoted. Decide whether forcing is safe by checking
		// whether THIS node currently holds DRBD quorum.
		hasQuorum, qErr := rm.nodeHasQuorum(ctx, resource, node)
		log.Debug("PromoteForNode: quorum", zap.String("resource", resource), zap.String("node", node),
			zap.Bool("quorum", hasQuorum), zap.Error(qErr))
		if qErr != nil {
			// We cannot prove quorum, so we must not force.
			return fmt.Errorf("refusing to force-promote %s on %s: normal promote failed (%v) and DRBD quorum could not be determined: %w",
				resource, node, err, qErr)
		}
		if !hasQuorum {
			// (d) No quorum -> refuse. Forcing here could create a dual-Primary
			// split-brain if the peer that holds Primary is alive behind a
			// partition. A quorate peer, if any, is the one that should promote.
			return fmt.Errorf("refusing to force-promote %s on %s: node does NOT hold DRBD quorum (majority); forcing could cause split-brain / dual-Primary data corruption (original promote error: %v)",
				resource, node, err)
		}

		// Quorum is not enough on its own. An Outdated disk is one a peer
		// marked as missing writes when it disconnected on purpose — the old
		// Primary going down in an orderly way while still writing — and a
		// forced promote makes that stale copy the data, discarding the newer
		// writes on the node that left. Seen on a PVE host rebooted with its
		// VM running: the guest came up on the outdated replica and the two
		// copies split. Such a copy has to wait for the node that has the
		// writes.
		stale, sErr := rm.nodeStaleDisk(ctx, resource, node)
		log.Debug("PromoteForNode: disk state", zap.String("resource", resource), zap.String("node", node),
			zap.String("stale_disk", stale), zap.Error(sErr))
		if sErr != nil {
			return fmt.Errorf("refusing to force-promote %s on %s: normal promote failed (%v) and its disk state could not be read: %w",
				resource, node, err, sErr)
		} else if stale != "" {
			return fmt.Errorf("refusing to force-promote %s on %s: its copy is %s, so another node has writes it lacks; "+
				"bring that node back, or if it is gone for good, promote by hand knowing those writes are lost (original promote error: %v)",
				resource, node, stale, err)
		}

		// (c) Quorum held -> safe to force. Any old/partitioned Primary that lost
		// quorum is blocked from I/O by on-no-quorum=io-error and cannot serve
		// stale writes, so this node can safely become the sole Primary.
		rm.controller.logger.Warn("Node holds DRBD quorum; force-promoting for hard failover",
			zap.String("resource", resource), zap.String("node", node))
		if fErr := rm.SetPrimary(ctx, resource, node, true); fErr != nil {
			log.Debug("PromoteForNode: forced promote failed", zap.String("resource", resource), zap.Error(fErr))
			// The first failure is usually the one that explains both: a peer
			// that has the device open refuses the forced promote as well.
			return fmt.Errorf("promote %s on %s failed: %v; forcing it (the node holds quorum) failed too: %w",
				resource, node, err, fErr)
		}
		return nil
	}
}

// nodeHasQuorum reports whether the given node currently holds DRBD quorum for
// the resource, read live from `drbdsetup status <res> --json` on that node.
// Any failure to read or parse the status returns an error (never a false
// "has quorum"), so callers guarding a force-promote fail closed.
func (rm *ResourceManager) nodeHasQuorum(ctx context.Context, resource, node string) (bool, error) {
	if rm.deployment == nil {
		return false, fmt.Errorf("deployment client not set")
	}
	address := rm.controller.ResolveHost(node)
	start := time.Now()
	res, err := rm.deployment.DRBDStatusJSON(ctx, []string{address}, resource)
	rm.resourcesLog().Debug("nodeHasQuorum: read JSON status", zap.String("resource", resource), zap.String("host", address),
		zap.Array("failed_hosts", resourcesFailedHosts{res}), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return false, fmt.Errorf("read drbd status on %s: %w", node, err)
	}
	for _, r := range res.Hosts {
		if !r.Success {
			return false, fmt.Errorf("drbd status on %s failed: %s", node, r.Output)
		}
		return localNodeHasQuorum(r.Output)
	}
	return false, fmt.Errorf("no drbd status result returned for %s", node)
}

// connectedPrimaryPeer names a peer that node is connected to and sees as
// Primary for resource, or returns "" (also when the status cannot be read,
// leaving the verdict to the checks that follow).
func (rm *ResourceManager) connectedPrimaryPeer(ctx context.Context, resource, node string) string {
	log := rm.resourcesLog()
	if rm.deployment == nil {
		return ""
	}
	start := time.Now()
	res, err := rm.deployment.DRBDStatusJSON(ctx, []string{rm.controller.ResolveHost(node)}, resource)
	log.Debug("connectedPrimaryPeer: read JSON status", zap.String("resource", resource), zap.String("node", node),
		zap.Array("failed_hosts", resourcesFailedHosts{res}), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return ""
	}
	for _, r := range res.Hosts {
		if !r.Success {
			return ""
		}
		var resources []drbdsetupStatus
		if json.Unmarshal([]byte(strings.TrimSpace(r.Output)), &resources) != nil || len(resources) == 0 {
			log.Debug("connectedPrimaryPeer: status unreadable", zap.String("resource", resource))
			return ""
		}
		for _, c := range resources[0].Connections {
			if c.ConnectionState == "Connected" && c.PeerRole == "Primary" {
				log.Debug("connectedPrimaryPeer: found", zap.String("resource", resource), zap.String("peer", c.Name))
				return c.Name
			}
		}
	}
	log.Debug("connectedPrimaryPeer: none", zap.String("resource", resource), zap.String("node", node))
	return ""
}

// nodeStaleDisk returns the disk state of node's copy of resource when it is
// not one a forced promote may build on (UpToDate or Consistent), or "".
func (rm *ResourceManager) nodeStaleDisk(ctx context.Context, resource, node string) (string, error) {
	start := time.Now()
	res, err := rm.deployment.DRBDStatusJSON(ctx, []string{rm.controller.ResolveHost(node)}, resource)
	rm.resourcesLog().Debug("nodeStaleDisk: read JSON status", zap.String("resource", resource), zap.String("node", node),
		zap.Array("failed_hosts", resourcesFailedHosts{res}), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return "", err
	}
	for _, r := range res.Hosts {
		if !r.Success {
			return "", fmt.Errorf("drbd status on %s failed: %s", node, r.Output)
		}
		return localStaleDisk(r.Output)
	}
	return "", fmt.Errorf("no drbd status result returned for %s", node)
}

func localStaleDisk(output string) (string, error) {
	var resources []drbdsetupStatus
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &resources); err != nil {
		zap.L().Debug("localStaleDisk: unparseable status", zap.Error(err))
		return "", fmt.Errorf("decode drbdsetup status json: %w", err)
	}
	if len(resources) == 0 || len(resources[0].Devices) == 0 {
		return "", fmt.Errorf("drbdsetup status json reported no local devices")
	}
	for _, d := range resources[0].Devices {
		switch d.DiskState {
		case "UpToDate", "Consistent":
		default:
			zap.L().Debug("localStaleDisk: stale volume", zap.Int("volume_id", d.Volume), zap.String("disk", d.DiskState))
			return d.DiskState, nil
		}
	}
	zap.L().Debug("localStaleDisk: every volume usable", zap.Int("volumes", len(resources[0].Devices)))
	return "", nil
}

// localNodeHasQuorum parses `drbdsetup status <res> --json` and reports whether
// the local (queried) node holds quorum. It requires every local device to
// explicitly report quorum:true; if any device is missing the field or reports
// false, it returns false so a guarded force-promote fails closed.
func localNodeHasQuorum(output string) (bool, error) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return false, fmt.Errorf("empty drbdsetup status json")
	}
	var resources []drbdsetupStatus
	if err := json.Unmarshal([]byte(trimmed), &resources); err != nil {
		zap.L().Debug("localNodeHasQuorum: unparseable status", zap.Error(err))
		return false, fmt.Errorf("decode drbdsetup status json: %w", err)
	}
	if len(resources) == 0 {
		return false, fmt.Errorf("drbdsetup status json contained no resources")
	}
	res := resources[0]
	if len(res.Devices) == 0 {
		return false, fmt.Errorf("drbdsetup status json reported no local devices")
	}
	for _, dev := range res.Devices {
		if dev.Quorum == nil || !*dev.Quorum {
			zap.L().Debug("localNodeHasQuorum: volume without quorum", zap.Int("volume_id", dev.Volume), zap.Bool("reported", dev.Quorum != nil))
			return false, nil
		}
	}
	zap.L().Debug("localNodeHasQuorum: every volume has quorum", zap.Int("volumes", len(res.Devices)))
	return true, nil
}
