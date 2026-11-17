package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/event"
)

// Auto-diskful, second half: taking the resource back to its replica count.
//
// Once a client has its disk the resource holds one replica more than it
// should. That is safe, and it is where a swap rests whenever something is in
// the way: the new disk is still syncing, no old replica can be removed right
// now, or the removal failed. converge runs every tick for every swap on
// record and takes whichever step is due, so a swap interrupted anywhere — by
// an error, a controller restart, a failover — goes on from where it is. It
// never adds a second replica: a resource with a swap on record is not looked
// at for a new one.
//
// The replica to remove is chosen when the new one is in sync, not when the
// swap starts: a sync takes hours, and which replica is Primary, healthy and
// reachable is only worth knowing at the moment one is removed.

// converge takes resource's swap one step further.
func (f *dataFollower) converge(ctx context.Context, resource string, nodeByName map[string]*NodeInfo) {
	f.mu.Lock()
	p := f.state.Pending[resource]
	f.mu.Unlock()
	if p == nil {
		return
	}
	log := f.log.With(zap.String("resource", resource), zap.String("node", p.Node))
	now := f.now()
	start := time.Now()
	r, err := f.c.db.GetResource(ctx, resource)
	log.Debug("dataFollower.converge: read record", zap.Bool("found", r != nil), zap.String("removed", p.Removed),
		zap.Int("attempts", p.Attempts), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || r == nil {
		// A resource that cannot be read is assumed deleted only when the
		// list says so too; GetResource fails the same way for both.
		if all, lerr := f.c.db.ListResources(ctx); lerr == nil && !recorded(all, resource) {
			log.Debug("dataFollower.converge: resource is gone; swap forgotten")
			f.dropPending(resource)
		}
		return
	}
	if r.MoveFrom != "" {
		log.Debug("dataFollower.converge: an operator's move is running; waiting", zap.String("from", r.MoveFrom), zap.String("to", r.MoveTo))
		return
	}
	replicas := splitCSV(r.Nodes)
	if p.Removed != "" {
		f.keepVote(ctx, r, p)
		return
	}
	switch {
	case !contains(replicas, p.Node):
		// The disk was never added (the controller stopped between recording
		// the swap and making it), or an operator took it away again.
		log.Debug("dataFollower.converge: the node holds no replica; swap forgotten", zap.Strings("replicas", replicas))
		f.dropPending(resource)
		return
	case len(replicas) <= p.Replicas:
		log.Debug("dataFollower.converge: replica count is back; swap done", zap.Int("replicas", len(replicas)))
		f.publish(resource, event.SeverityInfo, fmt.Sprintf("%s has %d replicas again, one of them on %s where it is used",
			resource, len(replicas), p.Node))
		f.finish(resource)
		return
	case now.Before(p.NextTry):
		log.Debug("dataFollower.converge: waiting before the next try", zap.Time("next_try", p.NextTry))
		return
	}

	info, err := f.c.resources.GetResource(ctx, resource)
	if err != nil || info == nil {
		log.Debug("dataFollower.converge: status unreadable; waiting", zap.Error(err))
		return
	}
	if st := f.stateOf(info, p.Node); st == nil || st.DiskState != "UpToDate" || !linkUp(st) {
		log.Debug("dataFollower.converge: new replica not in sync yet", zap.Duration("since", now.Sub(p.StartedAt)))
		if now.Sub(p.StartedAt) > moveTimeout {
			msg := fmt.Sprintf("the new replica of %s on %s has not reached UpToDate in %s; every replica is kept, and the swap "+
				"goes on when it does", resource, p.Node, moveTimeout)
			f.noteStep(resource, msg)
			f.announceOnce(resource+"|slow", resource, event.SeverityWarning, msg)
		}
		return
	}

	victim, why := pickFollowVictim(p.Node, f.followReplicas(ctx, r, info, nodeByName), splitCSV(r.DisklessNodes),
		p.KeepVote, f.c.resources.labelsByNode(ctx), f.c.resources.faultDomainKey())
	if victim == "" {
		log.Debug("dataFollower.converge: no replica can be removed now", zap.String("reason", why))
		f.retry(ctx, resource, p, "novictim", fmt.Sprintf("the new replica of %s on %s is in sync, but none of the others can be "+
			"removed now (%s); %s has %d replicas until one can", resource, p.Node, why, resource, len(replicas)))
		return
	}
	start = time.Now()
	err = f.c.resources.RemoveReplica(ctx, resource, victim)
	log.Debug("dataFollower.converge: removal returned", zap.String("victim", victim), zap.Duration("took", time.Since(start)), zap.Error(err))
	f.audit("RemoveReplica", resource, victim, start, err)
	if err != nil {
		// The removal can fail after the replica left the resource (deleting
		// its storage, updating the promoters); what the record says decides.
		fresh, gerr := f.c.db.GetResource(ctx, resource)
		if gerr != nil || fresh == nil || contains(splitCSV(fresh.Nodes), victim) {
			f.retry(ctx, resource, p, "remove", fmt.Sprintf("the new replica of %s on %s is in sync, but removing the one on %s "+
				"failed: %v; %s has %d replicas until that works, here or with `haify resource remove-replica %s --node %s`",
				resource, p.Node, victim, err, resource, len(replicas), resource, victim))
			return
		}
		f.publish(resource, event.SeverityWarning, fmt.Sprintf("the replica of %s on %s is removed, with a problem left to fix: %v",
			resource, victim, err))
	}
	f.publish(resource, event.SeverityInfo, fmt.Sprintf("the replica of %s moved from %s to %s, where it is used", resource, victim, p.Node))
	f.mu.Lock()
	p.Removed, p.Attempts, p.NextTry = victim, 0, time.Time{}
	delete(f.stepNotes, resource)
	f.unannounce(resource)
	f.mu.Unlock()
	_ = f.save(ctx)
	if fresh, gerr := f.c.db.GetResource(ctx, resource); gerr == nil && fresh != nil {
		f.keepVote(ctx, fresh, p)
	}
}

// keepVote is the last step of a swap on a resource whose client was the
// third vote beside two replicas. As a replica the node still votes, but the
// replica that left took a vote with it: two replicas and nothing else lose
// quorum when either node does. So the node that gave up its replica stays in
// the resource as the tiebreaker — what detaching a client does for the same
// reason. Without KeepVote, or when the resource has a third vote by now, the
// swap is simply done.
func (f *dataFollower) keepVote(ctx context.Context, r *database.Resource, p *followPending) {
	log := f.log.With(zap.String("resource", r.Name), zap.String("node", p.Removed))
	needed := p.KeepVote && len(splitCSV(r.Nodes)) == 2 && r.DisklessNodes == "" && r.DisklessClients == ""
	log.Debug("dataFollower.keepVote: start", zap.Bool("keep_vote", p.KeepVote), zap.Bool("needed", needed),
		zap.String("replicas", r.Nodes), zap.String("tiebreakers", r.DisklessNodes), zap.String("clients", r.DisklessClients))
	if !needed {
		f.finish(r.Name)
		return
	}
	if f.now().Before(p.NextTry) {
		log.Debug("dataFollower.keepVote: waiting before the next try", zap.Time("next_try", p.NextTry))
		return
	}
	start := time.Now()
	err := f.c.resources.SetTiebreaker(ctx, r.Name, p.Removed)
	log.Debug("dataFollower.keepVote: set-tiebreaker returned", zap.Duration("took", time.Since(start)), zap.Error(err))
	f.audit("SetTiebreaker", r.Name, p.Removed, start, err)
	if err != nil {
		f.retry(ctx, r.Name, p, "tiebreaker", fmt.Sprintf("%s is left with two replicas and no third vote, so losing either node "+
			"suspends its I/O: making %s its tiebreaker failed: %v; this is tried again, or run `haify ha set-tiebreaker %s --node %s`",
			r.Name, p.Removed, err, r.Name, p.Removed))
		return
	}
	f.publish(r.Name, event.SeverityInfo, fmt.Sprintf("%s stays in %s as its tiebreaker, so it keeps three votes", p.Removed, r.Name))
	f.finish(r.Name)
}

// retry puts off the next attempt at a failed step and says what failed, once
// per kind of failure.
func (f *dataFollower) retry(ctx context.Context, resource string, p *followPending, kind, msg string) {
	f.mu.Lock()
	p.Attempts++
	p.NextTry = f.now().Add(followBackoff(p.Attempts))
	attempts, next := p.Attempts, p.NextTry
	f.mu.Unlock()
	f.log.Debug("dataFollower.retry: step failed", zap.String("resource", resource), zap.String("kind", kind),
		zap.Int("attempts", attempts), zap.Time("next_try", next))
	f.noteStep(resource, msg)
	f.announceOnce(resource+"|"+kind, resource, event.SeverityWarning, msg)
	_ = f.save(ctx)
}

// finish closes resource's swap and starts its cooldown.
func (f *dataFollower) finish(resource string) {
	f.log.Debug("dataFollower.finish: swap done", zap.String("resource", resource))
	f.mu.Lock()
	delete(f.state.Pending, resource)
	delete(f.stepNotes, resource)
	f.state.Followed[resource] = f.now()
	f.unannounce(resource)
	f.mu.Unlock()
}

// followReplicas describes r's replicas for pickFollowVictim.
func (f *dataFollower) followReplicas(ctx context.Context, r *database.Resource, info *ResourceInfo, nodeByName map[string]*NodeInfo) []followReplica {
	rm := f.c.resources
	pool, _ := rm.memberPoolAndSize(ctx, r.Name, "")
	fill := rm.poolFillByNode(ctx)[normalizeManagedName(pool)]
	var out []followReplica
	for _, node := range splitCSV(r.Nodes) {
		fr := followReplica{Node: node, Fill: -1}
		if st := f.stateOf(info, node); st != nil && st.Role != "" && !strings.EqualFold(st.Role, "Unknown") {
			fr.Known = true
			fr.Primary = strings.EqualFold(st.Role, "Primary")
			fr.UpToDate = st.DiskState == "UpToDate" && linkUp(st)
		}
		// A drained node still answers, and its replica is as good a one to
		// give up as any.
		if n := nodeByName[node]; n != nil {
			fr.Reachable = n.State == NodeStateOnline || n.State == NodeStateMaintenance
		}
		if v, ok := fill[node]; ok {
			fr.Fill = v
		}
		f.log.Debug("dataFollower.followReplicas: replica", zap.String("resource", r.Name), zap.String("node", node),
			zap.Bool("known", fr.Known), zap.Bool("primary", fr.Primary), zap.Bool("up_to_date", fr.UpToDate),
			zap.Bool("reachable", fr.Reachable), zap.Float64("pool_fill", fr.Fill))
		out = append(out, fr)
	}
	return out
}

// recorded reports whether a resource called name is in list.
func recorded(list []*database.Resource, name string) bool {
	for _, r := range list {
		if r != nil && r.Name == name {
			return true
		}
	}
	return false
}
