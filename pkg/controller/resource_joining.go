package controller

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
)

// Replicas that are joining a resource.
//
// A replica added to a resource that already holds data starts empty and
// syncs. DRBD shows that the same way it shows a replica that fell behind:
// Inconsistent, SyncTarget. They are not the same thing. A replica that fell
// behind is a copy the resource is missing; a replica that is joining is a
// copy the resource never had, on top of the ones it has. Moving a replica to
// where its volume is used, rebalancing, and raising the replica count all
// produce the second kind, sometimes for hours, and reporting each of them as
// a degraded volume teaches people that the report means nothing.
//
// DRBD cannot tell the two apart, so the controller remembers: AddReplica
// records the new replica as joining, with the number of UpToDate replicas the
// resource is committed to keeping meanwhile. The record is removed the first
// time the replica is seen UpToDate, and for good: from then on a resync of it
// is a replica that fell behind.
//
// The record only says what kind of sync this is. Whether to stay quiet about
// it is decided by whoever reports health (pkg/alert, the CSI driver), and
// only while the committed number of UpToDate, connected replicas is there.

// markJoining records node's replica of resource as joining. committed is how
// many UpToDate replicas the resource has to keep while it syncs; 0 takes the
// number of replicas it had before this one, which is right whenever the
// replica is an addition and wrong when it replaces one that was already
// removed (auto-evict passes the number from before the removal).
func (rm *ResourceManager) markJoining(ctx context.Context, resource, node string, before, committed int) {
	log := rm.resourcesLog()
	if rm.controller == nil || rm.controller.db == nil {
		return
	}
	if committed <= 0 {
		committed = before
	}
	start := time.Now()
	recorded := false
	err := rm.controller.db.UpdateResourceJoining(ctx, resource, func(r *database.Resource) ([]database.JoiningReplica, bool) {
		kept := r.Joining[:0:0]
		for _, j := range r.Joining {
			if j.Node != node {
				kept = append(kept, j)
			}
		}
		if !contains(splitCSV(r.Nodes), node) {
			// The add did not get as far as making the node a replica, so
			// what was announced before it is withdrawn.
			return kept, len(kept) != len(r.Joining)
		}
		recorded = true
		return append(kept, database.JoiningReplica{Node: node, Since: time.Now(), Committed: committed}), true
	})
	log.Debug("markJoining: done", zap.String("resource", resource), zap.String("node", node), zap.Int("committed", committed),
		zap.Bool("recorded", recorded), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		// Without the record its sync is reported as a degraded replica,
		// which is the cautious way to be wrong.
		rm.controller.logger.Warn("Could not record the new replica as joining; its first sync will be reported as a degraded replica",
			zap.String("resource", resource), zap.String("node", node), zap.Error(err))
	}
}

// pendingJoinLimit is how long an announced replica may stay unconfirmed. An
// add that neither confirmed nor withdrew its announcement was cut short (the
// controller stopped in the middle of it); past this the announcement is
// dropped rather than left to excuse a sync nobody is running.
const pendingJoinLimit = 30 * time.Minute

// announceJoining records, before an add begins, that node's replica of
// resource is about to join. markJoining confirms or withdraws it afterwards.
func (rm *ResourceManager) announceJoining(ctx context.Context, resource, node string, before, committed int) {
	log := rm.resourcesLog()
	if rm.controller == nil || rm.controller.db == nil {
		return
	}
	if committed <= 0 {
		committed = before
	}
	start := time.Now()
	err := rm.controller.db.UpdateResourceJoining(ctx, resource, func(r *database.Resource) ([]database.JoiningReplica, bool) {
		kept := r.Joining[:0:0]
		for _, j := range r.Joining {
			if j.Node != node {
				kept = append(kept, j)
			}
		}
		return append(kept, database.JoiningReplica{Node: node, Since: time.Now(), Committed: committed, Pending: true}), true
	})
	log.Debug("announceJoining: done", zap.String("resource", resource), zap.String("node", node), zap.Int("committed", committed),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		rm.controller.logger.Warn("Could not announce the new replica as joining; the start of its first sync may be reported as a degraded replica",
			zap.String("resource", resource), zap.String("node", node), zap.Error(err))
	}
}

// nodeStateOf is node's state in info, under whichever of its names DRBD or
// the registry filed it, and the key it is filed under.
func (c *Controller) nodeStateOf(info *ResourceInfo, node string) (string, *ResourceNodeState) {
	if st := info.NodeStates[node]; st != nil {
		return node, st
	}
	addr := c.ResolveHost(node)
	drbdName := ""
	if c.nodes != nil {
		drbdName = c.nodes.GetDRBDNameByRef(node)
	}
	for name, st := range info.NodeStates {
		if st == nil {
			continue
		}
		if (drbdName != "" && name == drbdName) || (addr != "" && c.ResolveHost(name) == addr) {
			return name, st
		}
	}
	return "", nil
}

// settleJoining brings the record of joining replicas up to what DRBD shows
// and puts what is left into info. A replica leaves the record when it is
// UpToDate over a live connection, or is no longer a replica at all; one whose
// state cannot be read stays, since nothing was learned about it.
//
// It runs on every status read. That is what keeps the record short-lived:
// the health poll reads every resource every few seconds, so a replica is
// taken off the record within one poll of finishing its sync, long before it
// could fall behind and have that mistaken for joining.
func (rm *ResourceManager) settleJoining(ctx context.Context, r *database.Resource, info *ResourceInfo) {
	if len(r.Joining) == 0 {
		return
	}
	log := rm.resourcesLog()
	replicas := splitCSV(r.Nodes)
	var still []database.JoiningReplica
	for _, j := range r.Joining {
		key, st := rm.controller.nodeStateOf(info, j.Node)
		switch {
		case !contains(replicas, j.Node) && (!j.Pending || time.Since(j.Since) > pendingJoinLimit):
			log.Debug("settleJoining: not a replica; dropped", zap.String("resource", r.Name), zap.String("node", j.Node),
				zap.Bool("pending", j.Pending))
		case st != nil && st.DiskState == "UpToDate" && linkUp(st):
			log.Debug("settleJoining: in sync; no longer joining", zap.String("resource", r.Name), zap.String("node", j.Node),
				zap.Duration("joined_for", time.Since(j.Since)))
		default:
			still = append(still, j)
			info.JoiningNodes = append(info.JoiningNodes, j.Node)
			info.CommittedReplicas = max(info.CommittedReplicas, j.Committed)
			if st != nil {
				st.Joining = true
			}
			log.Debug("settleJoining: still joining", zap.String("resource", r.Name), zap.String("node", j.Node),
				zap.String("state_key", key), zap.Bool("state_known", st != nil), zap.Int("committed", j.Committed))
		}
	}
	if len(still) == len(r.Joining) {
		return
	}
	// This runs beside whatever else is changing the resource, and only the
	// joining record is this function's to change: the entries found done
	// are taken out of the record as it is stored now, in one transaction.
	// An entry recorded since this read (the same node added again) is
	// newer than what was seen and stays.
	done := map[string]time.Time{}
	for _, j := range r.Joining {
		done[j.Node] = j.Since
	}
	for _, j := range still {
		delete(done, j.Node)
	}
	start := time.Now()
	err := rm.controller.db.UpdateResourceJoining(ctx, r.Name, func(fresh *database.Resource) ([]database.JoiningReplica, bool) {
		kept := fresh.Joining[:0:0]
		for _, j := range fresh.Joining {
			if since, finished := done[j.Node]; !finished || !j.Since.Equal(since) {
				kept = append(kept, j)
			}
		}
		return kept, len(kept) != len(fresh.Joining)
	})
	log.Debug("settleJoining: record updated", zap.String("resource", r.Name), zap.Int("still_joining", len(still)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		rm.controller.logger.Warn("Could not update the record of joining replicas", zap.String("resource", r.Name), zap.Error(err))
	}
}
