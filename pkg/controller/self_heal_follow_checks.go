package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/event"
)

// Auto-diskful: which clients get a disk, and which replica goes.
//
// Everything here errs towards doing nothing. A client that is passed over
// keeps working exactly as before, over the network; a conversion that should
// not have happened is a full sync, and a removal that should not have
// happened is a lost copy.

// followSkip says why cand gets no replica of its own now: a short kind, for
// announcing each reason once, and the reason in words. Both are empty when
// nothing stands in the way. The cheap checks come first; the ones that ask
// the nodes run only for a client that passed the rest.
func (f *dataFollower) followSkip(ctx context.Context, cand followCandidate, nodeByName map[string]*NodeInfo) (string, string) {
	r, node, rm := cand.res, cand.node, f.c.resources
	log := f.log.With(zap.String("resource", r.Name), zap.String("node", node))
	n := nodeByName[node]
	replicas := splitCSV(r.Nodes)
	switch {
	case r.Name == SelfHaResource:
		return "metadata", "it is the controller's own metadata"
	case r.Labels[autoDiskfulLabel] == "false":
		return "optout", "it is labelled " + autoDiskfulLabel + "=false"
	case r.WANMode:
		return "wan", "it is replicated to another site, and the replicas of such a resource are placed by hand"
	case r.MoveFrom != "":
		return "moving", fmt.Sprintf("its replica is already moving from %s to %s", r.MoveFrom, r.MoveTo)
	case n == nil:
		return "node", node + " is not a registered node"
	case n.Labels[autoDiskfulLabel] == "false":
		return "optout", fmt.Sprintf("%s is labelled %s=false", node, autoDiskfulLabel)
	case n.LocalAgent || f.c.nodes.isLocalAgentHost(node):
		return "node", node + " is a local-agent node, on which the controller cannot create storage"
	case n.State != NodeStateOnline:
		return "node", fmt.Sprintf("%s is %s (drained, in maintenance or not answering), and such a node gets no new replica", node, n.State)
	case len(replicas) < 2:
		return "single", "it has one replica, and removing that one after the sync would leave a single copy again, " +
			"which replica removal refuses"
	case externallyPromoted(r.Name, r.Labels) == "the CSI driver" && r.Labels[csiTopologyLabel] == "":
		// Without the label nothing says its pods may run elsewhere, and a
		// pod sent back to a node whose replica was removed could not start.
		return "pinned", "it is a CSI volume that does not record where its Pods may run (the label " + csiTopologyLabel +
			" is missing), so its replicas stay where they are"
	case externallyPromoted(r.Name, r.Labels) == "the CSI driver" && r.Labels[csiTopologyLabel] != csiTopologyAnywhere:
		return "pinned", "its PersistentVolume is pinned to the nodes that hold its replicas " +
			"(the StorageClass does not set allowRemoteVolumeAccess), so they stay where they are"
	}
	f.mu.Lock()
	followed, cooling := f.state.Followed[r.Name]
	f.mu.Unlock()
	if cooling && f.now().Sub(followed) < followCooldown {
		return "cooldown", fmt.Sprintf("its replica already moved to its user at %s, and the next move is not before %s",
			followed.UTC().Format(time.RFC3339), followed.Add(followCooldown).UTC().Format(time.RFC3339))
	}
	if rm.reactorManaged(ctx, r.Name) {
		return "promoter", "a drbd-reactor promoter (an HA service, a gateway or an app) decides where it runs, " +
			"and its replicas are where that promoter may start it"
	}
	// A resource that is already behind is not given more to sync, and its
	// remaining good copies are not the ones to start removing.
	for _, m := range replicas {
		st := f.stateOf(cand.info, m)
		if st == nil || st.DiskState != "UpToDate" || !linkUp(st) {
			state := "unknown"
			if st != nil {
				state = strings.TrimSpace(st.DiskState + " " + st.Connection)
			}
			log.Debug("dataFollower.followSkip: a replica is not in sync", zap.String("replica", m), zap.String("state", state))
			return "degraded", fmt.Sprintf("its replica on %s is not UpToDate and connected (%s); it gets a new one "+
				"only while every replica it has is in sync", m, state)
		}
	}
	if err := f.c.assertResourceUnlocked(ctx, r.Name, "removing a replica after the move"); err != nil {
		return "locked", err.Error()
	}
	// allow-two-primaries is set only around a live migration. The one
	// Primary seen a moment ago may be the source just before the target is
	// promoted; converting it now would hand the volume a sync and a config
	// change in the middle of the hand-off.
	start := time.Now()
	hosts, err := rm.dualPrimaryHosts(ctx, r.Name)
	var open []string
	if err == nil {
		open, err = rm.dualPrimaryStrandedHosts(ctx, hosts, r.Name)
	}
	log.Debug("dataFollower.followSkip: dual-primary window", zap.Strings("hosts", hosts), zap.Strings("open_on", open),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return "migration", fmt.Sprintf("whether a live migration is under way could not be read: %v", err)
	}
	if len(open) > 0 {
		sort.Strings(open)
		return "migration", "a live migration window is open on it (allow-two-primaries is set on " + strings.Join(open, ", ") + ")"
	}
	return f.poolSkip(ctx, r, node)
}

// poolSkip is the part of followSkip that asks whether node's pool can take
// the replica. AddReplica asks too, but lets a pool it cannot read pass; here
// a node must be seen to have the pool and the room.
func (f *dataFollower) poolSkip(ctx context.Context, r *database.Resource, node string) (string, string) {
	rm := f.c.resources
	pool, sizeGB := rm.memberPoolAndSize(ctx, r.Name, "")
	if pool == "" || sizeGB == 0 {
		return "pool", "its pool or its size is not recorded"
	}
	managed := normalizeManagedName(pool)
	shown := strings.TrimPrefix(managed, managedNamePrefix)
	start := time.Now()
	free, err := rm.knownPoolFree(ctx, managed)
	have, known := free[node]
	f.log.Debug("dataFollower.poolSkip: pool room", zap.String("resource", r.Name), zap.String("node", node),
		zap.String("pool", managed), zap.Uint64("size_gb", sizeGB), zap.Bool("known", known), zap.Uint64("free_bytes", have),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	switch {
	case err != nil:
		return "pool", fmt.Sprintf("the free space of the pool %s could not be read: %v", shown, err)
	case !known:
		return "pool", fmt.Sprintf("%s has no pool %s, or the pool reports no free space", node, shown)
	case have < sizeGB<<30:
		return "pool", fmt.Sprintf("the pool %s on %s has %s free and the volume is %d GiB, which the sync writes in full",
			shown, node, freeText(have), sizeGB)
	}
	if err := rm.assertOvercommit(ctx, pool, []string{node}, sizeGB); err != nil {
		return "pool", err.Error()
	}
	return "", ""
}

// busy names the self-healing sync that is running, or "" when a new one may
// start: a swap of this follower whose new replica has not finished, a
// replica that is moving, or a replacement auto-evict is waiting for.
func (f *dataFollower) busy(ctx context.Context, resources []*database.Resource) string {
	f.mu.Lock()
	var syncing []string
	for name, p := range f.state.Pending {
		if p.Removed == "" {
			syncing = append(syncing, name)
		}
	}
	f.mu.Unlock()
	if len(syncing) > 0 {
		sort.Strings(syncing)
		return "the new replica of " + syncing[0] + " is not finished"
	}
	for _, r := range resources {
		if r.MoveFrom != "" {
			return fmt.Sprintf("the replica of %s is moving from %s to %s", r.Name, r.MoveFrom, r.MoveTo)
		}
	}
	if name := f.c.evictor.syncing(ctx); name != "" {
		return "the replacement replica of " + name + " is syncing"
	}
	return ""
}

// consider gives cand's node a replica of its own if nothing stands in the
// way; it reports whether it tried, which ends the tick's search.
func (f *dataFollower) consider(ctx context.Context, cand followCandidate, resources []*database.Resource, nodeByName map[string]*NodeInfo) bool {
	r, node, rm := cand.res, cand.node, f.c.resources
	key := r.Name + "/" + node
	log := f.log.With(zap.String("resource", r.Name), zap.String("node", node))
	now := f.now()
	since := cand.since.UTC().Format(time.RFC3339)
	log.Debug("dataFollower.consider: start", zap.Time("in_use_since", cand.since))
	f.mu.Lock()
	notBefore, failed := f.retryAt[key]
	f.mu.Unlock()
	if failed && now.Before(notBefore) {
		log.Debug("dataFollower.consider: its last conversion failed; not yet", zap.Time("not_before", notBefore))
		return false
	}
	if kind, why := f.followSkip(ctx, cand, nodeByName); kind != "" {
		log.Debug("dataFollower.consider: skipped", zap.String("kind", kind), zap.String("reason", why))
		f.decide(key, followVerdict{State: followStateSkipped, Skip: kind, Reason: why})
		f.announceOnce(key+"|"+kind, r.Name, event.SeverityInfo, fmt.Sprintf(
			"%s has used %s as a diskless client since %s, but gets no replica of its own: %s", node, r.Name, since, why))
		return false
	}
	if what := f.busy(ctx, resources); what != "" {
		log.Debug("dataFollower.consider: another sync is running", zap.String("busy", what))
		f.decide(key, followVerdict{State: followStateWaiting, Reason: "self-healing runs one sync at a time, and " + what})
		f.announceOnce(key+"|busy", r.Name, event.SeverityInfo, fmt.Sprintf(
			"%s has used %s as a diskless client since %s; its own replica waits, because self-healing runs one sync "+
				"at a time and %s", node, r.Name, since, what))
		return false
	}
	replicas := splitCSV(r.Nodes)
	plan := fmt.Sprintf("%s has used %s as a diskless client since %s: it gets a replica of its own, and one of the "+
		"replicas on %s is removed once the new one is in sync", node, r.Name, since, strings.Join(replicas, ", "))
	if f.c.config.SelfHeal.DiskfulDryRun() {
		log.Debug("dataFollower.consider: dry run; announcing only")
		f.decide(key, followVerdict{State: followStatePlanned, Reason: "dry run: it would get a replica of its own, and one of the " +
			"replicas on " + strings.Join(replicas, ", ") + " would be removed once the new one is in sync"})
		f.announceOnce(key+"|plan", r.Name, event.SeverityInfo, "dry run: "+plan)
		return false
	}

	// Recorded before the disk is added, as a move is: a controller that
	// dies in between leaves a record of a swap that did not start, which the
	// next one drops, and never an extra replica nobody remembers.
	p := &followPending{Node: node, Replicas: len(replicas), KeepVote: lastVoteBesidesTwoReplicas(r, node), StartedAt: now}
	f.mu.Lock()
	f.state.Pending[r.Name] = p
	delete(f.verdicts, key)
	f.mu.Unlock()
	if err := f.save(ctx); err != nil {
		log.Debug("dataFollower.consider: the swap could not be recorded; not starting", zap.Error(err))
		f.dropPending(r.Name)
		return false
	}

	// Under the lock a detach takes, so the client cannot be detached (its
	// pod leaving at this moment) half-way through getting its disk.
	unlock := rm.resLocks.lock(r.Name)
	start := time.Now()
	err := rm.AddReplica(ctx, r.Name, node)
	unlock()
	log.Debug("dataFollower.consider: conversion returned", zap.Duration("took", time.Since(start)), zap.Error(err))
	f.audit("AddReplica", r.Name, node, start, err)
	if err != nil {
		// The conversion can fail after the node got its disk (placing the
		// promoters is its last step); what the record says decides.
		fresh, gerr := f.c.db.GetResource(ctx, r.Name)
		if gerr != nil || fresh == nil || !contains(splitCSV(fresh.Nodes), node) {
			log.Debug("dataFollower.consider: conversion failed; the node is still a client", zap.Error(gerr))
			f.dropPending(r.Name)
			f.mu.Lock()
			f.retryAt[key] = now.Add(time.Hour)
			f.verdicts[key] = followVerdict{State: followStateFailed, Reason: fmt.Sprintf("giving it a replica of its own failed: %v", err),
				RetryAt: now.Add(time.Hour)}
			f.mu.Unlock()
			_ = f.save(ctx)
			f.publish(r.Name, event.SeverityWarning, fmt.Sprintf("%s has used %s as a diskless client since %s, but giving "+
				"it a replica of its own failed: %v; it stays a diskless client, and this is tried again in an hour",
				node, r.Name, since, err))
			return true
		}
		f.publish(r.Name, event.SeverityWarning, fmt.Sprintf("%s now has a replica of %s, with a problem left to fix: %v", node, r.Name, err))
	}
	f.publish(r.Name, event.SeverityInfo, plan+"; it syncs now")
	return true
}

// dropPending forgets resource's swap; the caller saves.
func (f *dataFollower) dropPending(resource string) {
	f.mu.Lock()
	delete(f.state.Pending, resource)
	delete(f.stepNotes, resource)
	f.unannounce(resource)
	f.mu.Unlock()
}

// followReplica is a replica as the choice of the one to remove sees it.
type followReplica struct {
	Node string
	// Primary and UpToDate are from DRBD; Known is false when the replica's
	// state could not be read at all, which rules it in for nothing.
	Primary, UpToDate, Known bool
	// Reachable says its node answers: removing a replica runs on that node.
	Reachable bool
	// Fill is how full its pool is, 0..1, or negative when not known.
	Fill float64
}

// pickFollowVictim chooses the replica to remove now that newNode's is in
// sync, or says why none can be.
//
// Never newNode's, never a Primary's, never one whose state or node cannot
// be read, and never one whose removal would leave fewer than two UpToDate
// copies — so the replica the Primary would fall back on is not the one that
// goes. Among the rest, in order: a replica that is itself behind before a
// good one; a choice that leaves no fault domain holding every copy or half
// the votes; the choice that leaves the replicas in the most fault domains;
// the replica on the fullest pool; the name.
//
// tiebreakers are the resource's; with keepVote the removed replica's node
// becomes one, and is judged as one.
func pickFollowVictim(newNode string, replicas []followReplica, tiebreakers []string, keepVote bool,
	labels map[string]map[string]string, key string) (string, string) {
	log := zap.L()
	type choice struct {
		r       followReplica
		risk    bool
		domains int
	}
	var choices []choice
	var refused []string
	for _, v := range replicas {
		var why string
		switch {
		case v.Node == newNode:
			continue
		case !v.Known:
			why = "the state of the one on " + v.Node + " cannot be read"
		case v.Primary:
			why = v.Node + " is Primary"
		case !v.Reachable:
			why = v.Node + " does not answer"
		}
		upToDate := 0
		var left []string
		domains := map[string]bool{}
		for _, o := range replicas {
			if o.Node == v.Node {
				continue
			}
			left = append(left, o.Node)
			domains[faultDomain(o.Node, labels[o.Node], key)] = true
			if o.Known && o.UpToDate {
				upToDate++
			}
		}
		if why == "" && upToDate < 2 {
			why = "without the one on " + v.Node + " fewer than two UpToDate copies would remain"
		}
		if why != "" {
			log.Debug("pickFollowVictim: replica not removable", zap.String("node", v.Node), zap.String("reason", why))
			refused = append(refused, why)
			continue
		}
		votes := tiebreakers
		if keepVote {
			votes = append(append([]string(nil), tiebreakers...), v.Node)
		}
		c := choice{r: v, risk: faultDomainRisk(left, votes, labels, key) != "", domains: len(domains)}
		log.Debug("pickFollowVictim: candidate", zap.String("node", v.Node), zap.Bool("up_to_date", v.UpToDate),
			zap.Bool("fault_domain_risk", c.risk), zap.Int("domains_left", c.domains), zap.Float64("pool_fill", v.Fill))
		choices = append(choices, c)
	}
	if len(choices) == 0 {
		sort.Strings(refused)
		return "", strings.Join(refused, "; ")
	}
	sort.Slice(choices, func(i, j int) bool {
		a, b := choices[i], choices[j]
		switch {
		case a.r.UpToDate != b.r.UpToDate:
			return !a.r.UpToDate
		case a.risk != b.risk:
			return !a.risk
		case a.domains != b.domains:
			return a.domains > b.domains
		case a.r.Fill != b.r.Fill:
			return a.r.Fill > b.r.Fill
		}
		return a.r.Node < b.r.Node
	})
	log.Debug("pickFollowVictim: chosen", zap.String("node", choices[0].r.Node), zap.Int("candidates", len(choices)))
	return choices[0].r.Node, ""
}
