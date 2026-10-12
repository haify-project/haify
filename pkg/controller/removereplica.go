package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// RemoveReplica takes a diskful replica out of a running resource.
//
// The inverse of AddReplica, and missing until now: Haify could grow a resource
// and never shrink it, so every removal meant hand-editing DRBD config on every
// node. On the volume that carries the controller's own database, with the
// controller running on it, that is not a comfortable place to be careful.
//
// Removal is permanent, unlike a conversion whose single-copy window closes
// when the resync finishes, so the guards are stricter rather than looser: the
// node may not be Primary, must actually hold a diskful replica, and at least
// two diskful copies have to remain.
func (rm *ResourceManager) RemoveReplica(ctx context.Context, resource, node string) error {
	rm.resourcesLog().Debug("RemoveReplica: removing replica", zap.String("resource", resource), zap.String("node", node))
	return rm.RemoveReplicaOptions(ctx, resource, node, false)
}

// RemoveReplicaOptions is RemoveReplica; lost removes the replica of a node
// that is gone for good without running anything on it (removereplica_lost.go).
// Its copy is already gone, so one diskful copy remaining is enough.
func (rm *ResourceManager) RemoveReplicaOptions(ctx context.Context, resource, node string, lost bool) error {
	log := rm.resourcesLog()
	log.Debug("RemoveReplicaOptions: start", zap.String("resource", resource), zap.String("node", node), zap.Bool("lost", lost))
	if rm.deployment == nil {
		log.Debug("RemoveReplicaOptions: no deployment client", zap.String("resource", resource))
		return fmt.Errorf("deployment client not set")
	}
	if rm.controller.db == nil {
		log.Debug("RemoveReplicaOptions: no database", zap.String("resource", resource))
		return fmt.Errorf("database not available")
	}

	dbRes, err := rm.controller.db.GetResource(ctx, resource)
	log.Debug("RemoveReplicaOptions: database lookup", zap.String("resource", resource),
		zap.Bool("found", dbRes != nil), zap.Error(err))
	if err != nil || dbRes == nil {
		return fmt.Errorf("resource %q not found", resource)
	}

	node = strings.TrimSpace(node)
	if node == "" {
		log.Debug("RemoveReplicaOptions: no node given", zap.String("resource", resource))
		return fmt.Errorf("a node is required")
	}

	for _, n := range splitCSV(dbRes.DisklessNodes) {
		if n == node {
			log.Debug("RemoveReplicaOptions: node is the tiebreaker; refusing", zap.String("resource", resource), zap.String("node", node))
			return fmt.Errorf("node %q is the quorum tiebreaker of %q, not a replica; move it with `ha set-tiebreaker`",
				node, resource)
		}
	}
	if dbRes.WANMode && dbRes.DRNode == node {
		log.Debug("RemoveReplicaOptions: node is the DR; refusing", zap.String("resource", resource), zap.String("node", node))
		return fmt.Errorf("node %q is the off-site DR of %q; detaching a DR is a different operation",
			node, resource)
	}

	members := splitCSV(dbRes.Nodes)
	remaining := make([]string, 0, len(members))
	held := false
	for _, n := range members {
		if n == node {
			held = true
			continue
		}
		remaining = append(remaining, n)
	}
	if !held {
		log.Debug("RemoveReplicaOptions: node holds no replica", zap.String("resource", resource), zap.String("node", node),
			zap.Strings("members", members))
		return fmt.Errorf("node %q holds no replica of %q", node, resource)
	}

	// Count what would still hold data. The DR is a member but not a local
	// copy, so it does not make the primary site redundant.
	diskful := 0
	for _, n := range remaining {
		if dbRes.WANMode && n == dbRes.DRNode {
			log.Debug("RemoveReplicaOptions: remaining member is the DR; not counted", zap.String("resource", resource), zap.String("node", n))
			continue
		}
		isTiebreaker := false
		for _, d := range splitCSV(dbRes.DisklessNodes) {
			if d == n {
				isTiebreaker = true
			}
		}
		log.Debug("RemoveReplicaOptions: remaining member", zap.String("resource", resource), zap.String("node", n),
			zap.Bool("tiebreaker", isTiebreaker))
		if !isTiebreaker {
			diskful++
		}
	}
	log.Debug("RemoveReplicaOptions: diskful copies after removal", zap.String("resource", resource),
		zap.Int("count", diskful), zap.Strings("remaining", remaining))
	switch {
	case lost && diskful < 1:
		log.Debug("RemoveReplicaOptions: last diskful replica; refusing", zap.String("resource", resource), zap.String("node", node))
		return fmt.Errorf("%s is the last diskful replica of %s; there is nothing left to remove it from", node, resource)
	case !lost && diskful < 2:
		log.Debug("RemoveReplicaOptions: would leave one copy; refusing", zap.String("resource", resource), zap.String("node", node))
		return fmt.Errorf(
			"removing %s from %s would leave only one diskful copy, permanently; add a replica first",
			node, resource)
	}

	// The leaver's storage goes with it, its snapshots included: while the
	// schedule locks them that is the same deletion resource delete refuses.
	// A lost node's storage is left where it is, so nothing is deleted then.
	if !lost {
		if err := rm.controller.assertResourceUnlocked(ctx, resource, "removing the replica on "+node); err != nil {
			log.Debug("RemoveReplicaOptions: resource is locked", zap.String("resource", resource), zap.Error(err))
			return err
		}
	}

	leaving := rm.controller.ResolveHost(node)
	survivors := make([]string, 0, len(remaining))
	for _, n := range remaining {
		survivors = append(survivors, rm.controller.ResolveHost(n))
	}
	log.Debug("RemoveReplicaOptions: resolved hosts", zap.String("resource", resource), zap.String("host", leaving),
		zap.Strings("survivors", survivors))

	if !lost {
		if err := rm.assertNotPrimary(ctx, resource, node); err != nil {
			log.Debug("RemoveReplicaOptions: node may be Primary", zap.String("resource", resource), zap.String("node", node), zap.Error(err))
			return err
		}
	}

	// Read from the survivors: on the lost path the leaver cannot answer, and
	// asking it first would only cost an SSH timeout.
	resPath := fmt.Sprintf("/etc/drbd.d/%s.res", resource)
	liveConfig, err := rm.readResourceConfig(ctx, survivors, resPath, resource)
	if err != nil {
		log.Debug("RemoveReplicaOptions: could not read live config", zap.String("resource", resource), zap.Error(err))
		return err
	}
	leaverName, err := rm.onBlockNameFor(liveConfig, node)
	if err != nil {
		log.Debug("RemoveReplicaOptions: node not in live config", zap.String("resource", resource), zap.String("node", node), zap.Error(err))
		return fmt.Errorf("%w in the config of %q", err, resource)
	}
	leaverID, _ := nodeIDOf(liveConfig, leaverName)
	log.Debug("RemoveReplicaOptions: leaver in config", zap.String("resource", resource), zap.String("drbd_name", leaverName),
		zap.String("node_id", leaverID))

	if lost {
		witnesses := make([]memberRef, 0, len(remaining))
		for _, n := range remaining {
			witnesses = append(witnesses, memberRef{drbdName: rm.controller.nodes.GetDRBDNameByRef(n), addr: rm.controller.ResolveHost(n)})
		}
		if err := rm.assertLostRemovable(ctx, resource, memberRef{drbdName: leaverName, addr: leaving}, witnesses); err != nil {
			log.Debug("RemoveReplicaOptions: lost replica not removable", zap.String("resource", resource), zap.Error(err))
			return err
		}
	}

	newConfig, err := rm.removeReplicaFromConfig(liveConfig, resource, node)
	if err != nil {
		log.Debug("RemoveReplicaOptions: could not rewrite config", zap.String("resource", resource), zap.Error(err))
		return err
	}

	rm.controller.logger.Info("Removing replica from running resource",
		zap.String("resource", resource), zap.String("node", node), zap.Bool("lost", lost),
		zap.Strings("remaining", remaining))

	// Tiebreakers and diskless clients hold the same file. Left out, they kept
	// the leaver as a member: a vote that never arrives, and a connection
	// retried forever.
	diskless := rm.disklessParticipantHosts(ctx, resource)
	log.Debug("RemoveReplicaOptions: diskless participants", zap.String("resource", resource), zap.Strings("hosts", diskless))

	if !lost {
		// Stop the leaver first. Telling the survivors about a peer that is
		// still connected leaves it trying to reconnect to a mesh it is no
		// longer in.
		log.Debug("RemoveReplicaOptions: stopping resource on leaver", zap.String("resource", resource), zap.String("host", leaving))
		if err := rm.execAllSuccess(ctx, []string{leaving},
			fmt.Sprintf("sudo drbdadm down %s", resource),
			"stop the resource on the leaving node"); err != nil {
			log.Debug("RemoveReplicaOptions: down on leaver failed", zap.String("resource", resource), zap.Error(err))
			return err
		}
	}

	targets := append(append([]string{}, survivors...), diskless...)
	start := time.Now()
	_, err = rm.deployment.DistributeConfig(ctx, targets, newConfig, resPath)
	log.Debug("RemoveReplicaOptions: distributed config", zap.String("resource", resource), zap.String("path", resPath),
		zap.Strings("hosts", targets), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("distribute config without %q: %w", node, err)
	}
	if err := rm.execAllSuccess(ctx, survivors,
		fmt.Sprintf("sudo drbdadm adjust %s", resource),
		"adjust the surviving peers"); err != nil {
		log.Debug("RemoveReplicaOptions: adjust on survivors failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	log.Debug("RemoveReplicaOptions: survivors adjusted", zap.String("resource", resource), zap.Strings("hosts", survivors))
	if len(diskless) > 0 {
		if err := rm.execAllSuccess(ctx, diskless, fmt.Sprintf("sudo drbdadm adjust %s", resource),
			"adjust the diskless members"); err != nil {
			rm.controller.logger.Warn("A diskless member still lists the removed replica; run `haify resource repair` once it answers",
				zap.String("resource", resource), zap.Error(err))
		} else {
			log.Debug("RemoveReplicaOptions: diskless members adjusted", zap.String("resource", resource), zap.Strings("hosts", diskless))
		}
	}
	rm.forgetPeer(ctx, resource, leaverID, leaverName, survivors)

	// Recorded last, so a failure above leaves the row describing reality.
	dbRes.Nodes = strings.Join(remaining, ",")
	err = rm.controller.db.SaveResource(ctx, dbRes)
	log.Debug("RemoveReplicaOptions: saved resource record", zap.String("resource", resource), zap.String("nodes", dbRes.Nodes), zap.Error(err))
	if err != nil {
		return fmt.Errorf("forget the removed replica: %w", err)
	}

	// The command says it destroys the copy, so the storage goes with it. Left
	// behind, the volume held pool space nothing accounted for — on a pool that
	// was already tight, that is how the next add-replica ran it out — and its
	// name blocked adding a replica back onto the same node. A lost node's
	// storage cannot be reached; it is left, and the caller says so.
	if lost {
		rm.controller.logger.Warn("Lost replica removed; its storage and config stay on the node",
			zap.String("resource", resource), zap.String("node", node),
			zap.String("cleanup", LostReplicaCleanup(resource, node)))
	} else if err := rm.deleteReplicaStorage(ctx, resource, leaving); err != nil {
		log.Debug("RemoveReplicaOptions: storage deletion failed", zap.String("resource", resource), zap.String("host", leaving), zap.Error(err))
		return fmt.Errorf("replica removed from %s, but its storage there could not be deleted: %w; remove the volume by hand", node, err)
	} else if err := rm.execAllSuccess(ctx, []string{leaving}, "sudo rm -f "+resPath,
		"delete the resource config on the leaving node"); err != nil {
		// A config there that no longer names the node only makes drbdadm
		// answer "not defined for this host"; node restore cleans it later.
		rm.controller.logger.Warn("Replica removed, but its config stays on the node",
			zap.String("resource", resource), zap.String("node", node), zap.Error(err))
	} else {
		log.Debug("RemoveReplicaOptions: leaver's storage and config deleted", zap.String("resource", resource), zap.String("host", leaving))
	}

	// Its promoter would otherwise outlive its copy of the data.
	if err := rm.SyncPromoters(ctx, resource, node); err != nil {
		log.Debug("RemoveReplicaOptions: promoter sync failed", zap.String("resource", resource), zap.Error(err))
		return fmt.Errorf("replica removed from %s, but the resource's promoters could not be updated: %w; "+
			"run `haify resource repair %s`", node, err, resource)
	}

	rm.controller.logger.Info("Replica removed",
		zap.String("resource", resource), zap.String("node", node))
	return nil
}

// deleteReplicaStorage removes the resource's backing volumes on one node.
func (rm *ResourceManager) deleteReplicaStorage(ctx context.Context, resource, host string) error {
	log := rm.resourcesLog()
	vols, err := rm.controller.db.ListVolumes(ctx, resource)
	log.Debug("deleteReplicaStorage: listed volumes", zap.String("resource", resource), zap.String("host", host),
		zap.Int("count", len(vols)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("list volumes of %s: %w", resource, err)
	}
	for _, v := range vols {
		log.Debug("deleteReplicaStorage: deleting backing volume", zap.String("resource", resource), zap.String("host", host),
			zap.String("volume", v.VolumeName), zap.String("pool", v.Pool))
		if err := rm.deleteBackingVolume(ctx, []string{host}, v); err != nil {
			log.Debug("deleteReplicaStorage: delete failed", zap.String("resource", resource), zap.String("volume", v.VolumeName), zap.Error(err))
			return fmt.Errorf("volume %s: %w", v.VolumeName, err)
		}
	}
	return nil
}

// assertNotPrimary refuses when the node still serves the resource.
func (rm *ResourceManager) assertNotPrimary(ctx context.Context, resource, node string) error {
	log := rm.resourcesLog()
	addr := rm.controller.ResolveHost(node)
	start := time.Now()
	res, err := rm.deployment.DRBDStatus(ctx, []string{addr}, resource)
	log.Debug("assertNotPrimary: read DRBD status", zap.String("resource", resource), zap.String("node", node),
		zap.String("host", addr), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || res == nil {
		// Unreachable is not proof of anything. Refuse rather than tear down a
		// replica whose role is unknown.
		log.Debug("assertNotPrimary: status unreadable; refusing", zap.String("resource", resource), zap.String("node", node))
		return fmt.Errorf("cannot read the role of %s on %s; refusing to remove a replica blind", resource, node)
	}
	for h, r := range res.Hosts {
		if !r.Success {
			log.Debug("assertNotPrimary: status failed; refusing", zap.String("resource", resource), zap.String("host", h))
			return fmt.Errorf("cannot read the role of %s on %s: %s", resource, node, strings.TrimSpace(r.Output))
		}
		role := parseRoleFromStatus(r.Output)
		log.Debug("assertNotPrimary: role", zap.String("resource", resource), zap.String("host", h), zap.String("role", role))
		// An empty or unparseable status is not evidence of Secondary. Treating
		// it as one would tear down a replica whose role was never established,
		// which is the exact case this guard exists for.
		if role == "" || strings.EqualFold(role, "Unknown") {
			return fmt.Errorf("could not read the role of %s on %s; refusing to remove a replica blind", resource, node)
		}
		if strings.EqualFold(role, "Primary") {
			return fmt.Errorf("%s is Primary on %s; move it first (haify ha evict, or resource secondary)", resource, node)
		}
	}
	return nil
}

// readResourceConfig returns the live .res from the first host that answers.
func (rm *ResourceManager) readResourceConfig(ctx context.Context, hosts []string, resPath, resource string) (string, error) {
	log := rm.resourcesLog()
	log.Debug("readResourceConfig: reading live config", zap.String("resource", resource), zap.String("path", resPath),
		zap.Strings("hosts", hosts))
	var lastErr error
	for _, h := range hosts {
		start := time.Now()
		res, err := rm.deployment.Exec(ctx, []string{h}, "cat "+resPath)
		log.Debug("readResourceConfig: read from host", zap.String("resource", resource), zap.String("host", h),
			zap.Array("failed_hosts", resourcesFailedHosts{res}), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			lastErr = err
			continue
		}
		for _, r := range res.Hosts {
			if r.Success && strings.TrimSpace(r.Output) != "" {
				log.Debug("readResourceConfig: got config", zap.String("resource", resource), zap.String("host", h),
					zap.Int("bytes", len(r.Output)))
				return r.Output, nil
			}
		}
		lastErr = fmt.Errorf("no usable config from %s", h)
	}
	log.Debug("readResourceConfig: no host gave a config", zap.String("resource", resource), zap.Error(lastErr))
	return "", fmt.Errorf("read resource config for %q: %v", resource, lastErr)
}

// removeReplicaFromConfig drops one node's `on` stanza from a live resource
// config and rebuilds the connection mesh without it.
//
// The inverse of addReplicaToConfig, and the part of removal that has to be
// exact. A leftover `on` block leaves a member that never connects and still
// counts toward quorum — the resource looks degraded forever. A leftover mesh
// entry names a host with no stanza, and `drbdadm adjust` rejects the whole
// file rather than just that line, which takes the resource down on every node
// the config reaches.
//
// Surviving node-ids are deliberately left alone. DRBD records a peer's node-id
// in metadata, so renumbering a replica that is staying invalidates its bitmap
// slot and forces a full resync of data that never changed.
func (rm *ResourceManager) removeReplicaFromConfig(content, resource, node string) (string, error) {
	log := rm.resourcesLog()
	log.Debug("removeReplicaFromConfig: start", zap.String("resource", resource), zap.String("node", node), zap.Int("bytes", len(content)))
	name, err := rm.onBlockNameFor(content, node)
	if err != nil {
		log.Debug("removeReplicaFromConfig: node not in config", zap.String("resource", resource), zap.String("node", node), zap.Error(err))
		return "", fmt.Errorf("%w in the config of %q", err, resource)
	}

	out, err := removeOnBlock(content, name)
	if err != nil {
		log.Debug("removeReplicaFromConfig: could not remove stanza", zap.String("resource", resource), zap.String("drbd_name", name), zap.Error(err))
		return "", err
	}

	// Rebuild rather than edit: the mesh is derived from whichever LAN stanzas
	// remain, so it cannot drift out of step with them.
	out = stripConnectionMesh(out)
	mesh := lanHostNames(parseOnBlocks(out))
	log.Debug("removeReplicaFromConfig: rebuilding mesh", zap.String("resource", resource), zap.Strings("mesh", mesh))
	if len(mesh) > 1 {
		out, err = insertBeforeResourceClose(out, buildConnectionMesh(mesh))
		if err != nil {
			log.Debug("removeReplicaFromConfig: could not insert mesh", zap.String("resource", resource), zap.Error(err))
			return "", err
		}
	}
	return out, nil
}

// removeOnBlock deletes the `on <name> { ... }` stanza, matching braces so a
// stanza carrying per-volume overrides is removed whole.
func removeOnBlock(content, name string) (string, error) {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))

	depth, inBlock := 0, false
	for _, line := range lines {
		if !inBlock {
			f := strings.Fields(line)
			if len(f) >= 2 && f[0] == "on" && f[1] == name {
				inBlock = true
				depth = strings.Count(line, "{") - strings.Count(line, "}")
				// A one-line stanza opens and closes on the same line.
				if depth <= 0 {
					inBlock = false
				}
				continue
			}
			out = append(out, line)
			continue
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if depth <= 0 {
			inBlock = false
		}
	}
	if inBlock {
		zap.L().Debug("removeOnBlock: unbalanced braces", zap.String("drbd_name", name))
		return "", fmt.Errorf("unbalanced braces while removing %q", name)
	}
	zap.L().Debug("removeOnBlock: removed stanza", zap.String("drbd_name", name),
		zap.Int("lines_before", len(lines)), zap.Int("lines_after", len(out)))
	return strings.Join(out, "\n"), nil
}

// onBlockNameFor finds which `on` stanza belongs to a node.
//
// The DRBD name is tried first, then the node's address. The fallback is not
// only for tests: the name mapping comes from a hostname recorded at
// registration, and a node that was re-imaged, renamed, or registered before
// its hostname was known has a stale or absent one. Removing the wrong stanza —
// or refusing to find any — is a worse outcome than matching on the address,
// which is written into the stanza itself and cannot drift.
func (rm *ResourceManager) onBlockNameFor(content, node string) (string, error) {
	log := rm.resourcesLog()
	blocks := parseOnBlocks(content)

	if name := rm.controller.nodes.GetDRBDNameByRef(node); name != "" {
		for _, b := range blocks {
			if b.name == name {
				log.Debug("onBlockNameFor: matched by DRBD name", zap.String("node", node), zap.String("drbd_name", name))
				return name, nil
			}
		}
		log.Debug("onBlockNameFor: DRBD name has no stanza; trying address", zap.String("node", node), zap.String("drbd_name", name))
	}

	if addr := rm.controller.ResolveHost(node); addr != "" {
		for _, b := range blocks {
			if addressOf(content, b.name) == addr {
				log.Debug("onBlockNameFor: matched by address", zap.String("node", node), zap.String("host", addr), zap.String("drbd_name", b.name))
				return b.name, nil
			}
		}
	}
	log.Debug("onBlockNameFor: no stanza for node", zap.String("node", node), zap.Int("stanzas", len(blocks)))
	return "", fmt.Errorf("node %q is not", node)
}

// addressOf returns the address recorded in a node's `on` stanza.
func addressOf(content, name string) string {
	lines := strings.Split(content, "\n")
	inBlock, depth := false, 0
	for _, line := range lines {
		f := strings.Fields(line)
		if !inBlock {
			if len(f) >= 2 && f[0] == "on" && f[1] == name {
				inBlock = true
				depth = strings.Count(line, "{") - strings.Count(line, "}")
			}
			continue
		}
		if len(f) >= 2 && f[0] == "address" {
			// "address   1.2.3.4:7999;" and "address ipv4 1.2.3.4:7999;"
			hostPort := strings.TrimSuffix(f[len(f)-1], ";")
			if i := strings.LastIndex(hostPort, ":"); i > 0 {
				zap.L().Debug("addressOf: found address", zap.String("drbd_name", name), zap.String("host", hostPort[:i]))
				return hostPort[:i]
			}
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if depth <= 0 {
			zap.L().Debug("addressOf: stanza has no address", zap.String("drbd_name", name))
			return ""
		}
	}
	zap.L().Debug("addressOf: no stanza", zap.String("drbd_name", name))
	return ""
}
