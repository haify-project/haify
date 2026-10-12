package controller

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// SetTiebreaker moves a resource's diskless quorum tiebreaker to a different
// node, live.
//
// The tiebreaker is chosen once, when the resource is created, and until now
// there was no way to change it afterwards. That is a real gap for anything
// long-lived: the tiebreaker decides whether the survivors of a failure still
// have a quorum majority, so it belongs in a *separate failure domain* from the
// diskful replicas. Clusters do not stay put — a node is retired, or the
// operator notices (as one did here) that the tiebreaker VM lives on the same
// physical host as one of the replicas, so losing that host takes out two of
// three votes and the survivor suspends I/O. Rebuilding the resource to move
// one diskless vote is not an acceptable answer when the resource is serving
// traffic.
//
// The operation is config-only: a tiebreaker stores no data, so there is
// nothing to resync. The live .res is rewritten (old `on` stanza out, new one
// in), redistributed to every participant plus the new node, and adjusted. The
// old tiebreaker is then torn down. Nothing about the diskful replicas or the
// mounted filesystem is touched, so a promoted resource keeps serving through
// the change.
//
// Passing an empty newNode removes the tiebreaker without adding one, leaving a
// bare 2-node resource (and its quorum risk — the caller is warned).
func (rm *ResourceManager) SetTiebreaker(ctx context.Context, resource, newNode string) error {
	if rm.deployment == nil {
		return fmt.Errorf("deployment client not set")
	}
	if rm.controller.db == nil {
		return fmt.Errorf("database not available")
	}

	dbRes, err := rm.controller.db.GetResource(ctx, resource)
	if err != nil || dbRes == nil {
		return fmt.Errorf("resource %q not found", resource)
	}

	newNode = strings.TrimSpace(newNode)
	if newNode != "" {
		if err := rm.assertNewMemberTLS(ctx, resource, newNode); err != nil {
			return err
		}
	}
	current := splitCSV(dbRes.DisklessNodes)
	diskful := splitCSV(dbRes.Nodes)

	if newNode != "" {
		for _, n := range diskful {
			if n == newNode {
				return fmt.Errorf("node %q already holds a diskful replica of %q; a tiebreaker must be a separate node",
					newNode, resource)
			}
		}
		for _, n := range splitCSV(dbRes.DisklessClients) {
			if n == newNode {
				return fmt.Errorf("node %q is a diskless client of %q; it cannot also be the quorum tiebreaker",
					newNode, resource)
			}
		}
		if len(current) == 1 && current[0] == newNode {
			rm.controller.logger.Info("Tiebreaker already on requested node",
				zap.String("resource", resource), zap.String("node", newNode))
			return nil
		}
	}

	rm.controller.logger.Info("Changing quorum tiebreaker",
		zap.String("resource", resource),
		zap.Strings("from", current),
		zap.String("to", newNode))

	// Read the live config from a diskful replica — it is the authority on the
	// current participant set, which may include clients the DB does not track.
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return fmt.Errorf("resource %q has no reachable hosts", resource)
	}
	resPath := fmt.Sprintf("/etc/drbd.d/%s.res", resource)
	catResult, err := rm.deployment.Exec(ctx, []string{hosts[0]}, "cat "+resPath)
	if err != nil {
		return fmt.Errorf("read resource config: %w", err)
	}
	originalConfig := ""
	readOK := false
	for _, r := range catResult.Hosts {
		originalConfig, readOK = r.Output, r.Success
		break
	}
	if !readOK || strings.TrimSpace(originalConfig) == "" {
		return fmt.Errorf("read resource config for %q: %s", resource, catResult.FailureDetails())
	}

	// Verify the resource's minors are free on the incoming node before touching
	// anything. Minors were allocated when the resource was created, over the
	// nodes it had then; a node joining later can already be using them.
	if newNode != "" {
		if err := rm.assertMinorsFreeOn(ctx, rm.controller.ResolveHost(newNode), resource,
			resourceMinors(originalConfig)); err != nil {
			return err
		}
	}

	newConfig := originalConfig

	// Drop the outgoing tiebreaker's stanza. Match on the DRBD host name, which
	// is the node's real hostname, not the Haify node name.
	oldHosts := make([]string, 0, len(current))
	for _, old := range current {
		drbdName := rm.controller.nodes.GetDRBDNameByRef(old)
		stripped, rerr := removeDisklessClientBlock(newConfig, drbdName)
		if rerr != nil {
			// Already absent from the config (partial earlier change) — the DB
			// row is what is stale, so carry on and let it be corrected below.
			rm.controller.logger.Warn("Outgoing tiebreaker not present in live config",
				zap.String("resource", resource), zap.String("node", old), zap.Error(rerr))
		} else {
			newConfig = stripped
		}
		oldHosts = append(oldHosts, rm.controller.ResolveHost(old))
	}

	if newNode != "" {
		ip := rm.controller.nodes.GetReplicationAddressByName(newNode)
		if ip == "" {
			rm.mu.RLock()
			ip = rm.hostMap[newNode]
			rm.mu.RUnlock()
		}
		if ip == "" {
			ip = newNode
		}
		ip = resolveToIP(ip)

		added, aerr := addDisklessClientBlock(newConfig, rm.controller.nodes.GetDRBDNameByRef(newNode), ip, dbRes.Port)
		if aerr != nil && aerr != errDisklessAlreadyPresent {
			return fmt.Errorf("add tiebreaker %q to config: %w", newNode, aerr)
		}
		if aerr == nil {
			newConfig = added
		}
	}

	// Everyone that must see the new config: the surviving participants in the
	// rewritten config, plus the incoming node.
	participants := make([]string, 0, 8)
	for _, b := range parseOnBlocks(newConfig) {
		participants = append(participants, rm.controller.ResolveHost(b.name))
	}
	if len(participants) == 0 {
		return fmt.Errorf("rewritten config for %q has no hosts; refusing to distribute", resource)
	}
	participants, err = rm.withoutGoneMembers(ctx, resource, newConfig, participants, newNode)
	if err != nil {
		return err
	}

	if _, err := rm.deployment.DistributeConfig(ctx, participants, newConfig, resPath); err != nil {
		return fmt.Errorf("distribute tiebreaker config: %w", err)
	}
	if err := rm.execAllSuccess(ctx, participants,
		fmt.Sprintf("sudo drbdadm adjust %s", resource),
		"adjust peers for tiebreaker change"); err != nil {
		return err
	}

	// Bring the new tiebreaker up. It has no backing disk, so it attaches
	// diskless and only ever votes.
	//
	// `adjust` above already brings the resource up on a node that was handed
	// the config for the first time, so this is usually a no-op — and an
	// unconditional `up` then fails with "Minor or volume exists already". Treat
	// an already-configured minor as success; anything else is a real failure.
	if newNode != "" {
		newHost := rm.controller.ResolveHost(newNode)
		upRes, err := rm.deployment.Exec(ctx, []string{newHost}, fmt.Sprintf("sudo drbdadm up %s", resource))
		if err != nil {
			return fmt.Errorf("bring up tiebreaker %q: %w", newNode, err)
		}
		if !upRes.AllSuccess() {
			detail := upRes.FailureDetails()
			if !strings.Contains(detail, "exists already") && !strings.Contains(detail, "already? still?") {
				return fmt.Errorf("bring up tiebreaker %q: %s", newNode, detail)
			}
			rm.controller.logger.Debug("Tiebreaker already up after adjust",
				zap.String("resource", resource), zap.String("node", newNode))
		}
		// A tiebreaker is often a small node that holds no replica of
		// anything, so the boot unit is not there yet. It has no disk, so a
		// failure is logged and the move goes on.
		_ = rm.ensureDRBDBootUnitEnabled(ctx, []string{newHost})
	}

	// Tear the old one down last: until the new vote is in place, removing it
	// early would leave the resource at 2 votes with no majority on failure.
	for i, old := range current {
		if newNode != "" && old == newNode {
			continue
		}
		host := oldHosts[i]
		if _, err := rm.deployment.Exec(ctx, []string{host}, fmt.Sprintf("sudo drbdadm down %s", resource)); err != nil {
			rm.controller.logger.Warn("Failed to stop resource on old tiebreaker",
				zap.String("resource", resource), zap.String("node", old), zap.Error(err))
		}
		if _, err := rm.deployment.Exec(ctx, []string{host}, "sudo rm -f "+resPath); err != nil {
			rm.controller.logger.Warn("Failed to remove config from old tiebreaker",
				zap.String("resource", resource), zap.String("node", old), zap.Error(err))
		}
	}

	// Record it. Do this last so a failure above leaves the DB describing the
	// state the cluster is actually in.
	if newNode == "" {
		dbRes.DisklessNodes = ""
	} else {
		dbRes.DisklessNodes = newNode
	}
	if err := rm.controller.db.SaveResource(ctx, dbRes); err != nil {
		return fmt.Errorf("record tiebreaker change: %w", err)
	}

	if newNode == "" && len(diskful) < 3 {
		rm.controller.logger.Warn("Resource left without a quorum tiebreaker: a single node failure will suspend I/O",
			zap.String("resource", resource), zap.Int("replicas", len(diskful)))
	}
	return nil
}

// withoutGoneMembers drops from participants the members of config that are
// gone for good, so a tiebreaker can still be set while one is dead.
//
// That is the way out for a resource that lost a member and with it its
// quorum: a new tiebreaker restores the survivors' majority, after which the
// dead member can be removed with remove-replica --lost. Requiring every member
// to adjust made that impossible. A member is only skipped when confirmGone
// agrees it is gone — no SSH, and no answering member still connected to it.
// The incoming node is never skipped: it is the point of the change.
func (rm *ResourceManager) withoutGoneMembers(ctx context.Context, resource, config string, participants []string, newNode string) ([]string, error) {
	newHost := ""
	if newNode != "" {
		newHost = rm.controller.ResolveHost(newNode)
	}
	var live, gone []memberRef
	for i, b := range parseOnBlocks(config) {
		m := memberRef{drbdName: b.name, addr: participants[i]}
		switch {
		case m.addr == newHost:
		case rm.answers(ctx, m.addr):
			live = append(live, m)
		default:
			gone = append(gone, m)
		}
	}
	if len(gone) == 0 {
		return participants, nil
	}
	if _, err := rm.confirmGone(ctx, resource, gone, live); err != nil {
		return nil, fmt.Errorf("a member of %s does not answer: %w", resource, err)
	}
	out := participants
	for _, g := range gone {
		out = without(out, g.addr)
		rm.controller.logger.Warn("Member is gone; it keeps its old config until removed",
			zap.String("resource", resource), zap.String("member", g.drbdName),
			zap.String("then", "haify resource remove-replica "+resource+" --node <node> --lost --yes"))
	}
	return out, nil
}
