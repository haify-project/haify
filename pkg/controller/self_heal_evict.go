package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/event"
)

// Auto-evict ([self_heal] auto_evict).
//
// A node that dies leaves every resource it held a replica of one copy short
// until someone notices and replaces it. Auto-evict does the replacing, under
// conditions chosen so that it never acts on a partition, a reboot or a
// network blip:
//
//   - the node has been offline for after_minutes (OfflineSince, persisted
//     across controller failovers);
//   - at most max_offline_percent of the nodes are offline at once, and the
//     controller reaches a majority of them — otherwise the controller may be
//     the one cut off;
//   - for each resource, the lost-replica guard holds (removereplica_lost.go):
//     the node does not answer over SSH, no surviving member's DRBD is
//     connected to it, and the survivors have quorum and an UpToDate copy;
//   - the node is not in maintenance, not labelled haify.io/auto-evict=false,
//     and the resource is not the controller's own metadata, not WAN-replicated
//     and not labelled haify.io/auto-evict=false.
//
// It replaces one replica at a time — the new one is a full sync — and waits
// for that sync before the next. It never touches a Primary: the node it
// evicts is the dead one. Once nothing refers to the node it is marked
// evicted and gets no new replicas until `haify node restore` cleans what it
// still holds, or `haify node lost` says it will not come back.
//
// "dry-run" decides exactly the same and announces each step as a
// node.evicted event at info severity, changing nothing.

const (
	autoEvictInterval = time.Minute
	autoEvictOptOut   = "haify.io/auto-evict"
)

type autoEvictor struct {
	c   *Controller
	log *zap.Logger

	mu        sync.Mutex
	announced map[string]bool // dry-run plans and refusals already published
	inflight  string          // resource whose new replica is still syncing
	gated     bool            // the too-many-offline gate is closed
}

// evictLog is the logger the evictor traces through: its own, or the global
// one for an evictor built without one (tests build them bare).
func (e *autoEvictor) evictLog() *zap.Logger {
	if e != nil && e.log != nil {
		return e.log
	}
	return zap.L()
}

// startAutoEvict runs the coordinator on the active controller.
func (c *Controller) startAutoEvict(ctx context.Context) {
	if !c.config.SelfHeal.Enabled() || c.db == nil {
		zap.L().Debug("startAutoEvict: auto-evict off", zap.Bool("enabled", c.config.SelfHeal.Enabled()), zap.Bool("has_db", c.db != nil))
		return
	}
	e := &autoEvictor{c: c, log: c.logger.Named("auto-evict"), announced: map[string]bool{}}
	e.log.Info("Auto-evict enabled", zap.String("mode", c.config.SelfHeal.AutoEvict),
		zap.Int("after_minutes", c.config.SelfHeal.AfterMinutes))
	go func() {
		t := time.NewTicker(autoEvictInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				e.log.Debug("startAutoEvict: stopping", zap.Error(ctx.Err()))
				return
			case <-t.C:
				e.tick(ctx)
			}
		}
	}()
}

// evictCandidates are the nodes offline long enough, or nil when the gate
// says no node may be evicted now.
func (e *autoEvictor) evictCandidates(nodes []*NodeInfo, now time.Time) ([]*NodeInfo, string) {
	log := e.evictLog()
	cfg := e.c.config.SelfHeal
	var offline, due []*NodeInfo
	for _, n := range nodes {
		if n.OfflineSince.IsZero() || n.State == NodeStateEvicted {
			continue
		}
		offline = append(offline, n)
		if n.State == NodeStateMaintenance || n.Labels[autoEvictOptOut] == "false" {
			log.Debug("autoEvictor.evictCandidates: offline but exempt", zap.String("node", n.Name), zap.String("state", string(n.State)),
				zap.Bool("opted_out", n.Labels[autoEvictOptOut] == "false"))
			continue
		}
		offlineFor := now.Sub(n.OfflineSince)
		isDue := offlineFor >= time.Duration(cfg.AfterMinutes)*time.Minute
		log.Debug("autoEvictor.evictCandidates: offline node", zap.String("node", n.Name), zap.Duration("offline_for", offlineFor),
			zap.Bool("due", isDue))
		if isDue {
			due = append(due, n)
		}
	}
	if len(due) == 0 {
		log.Debug("autoEvictor.evictCandidates: none due", zap.Int("offline", len(offline)), zap.Int("nodes", len(nodes)))
		return nil, ""
	}
	if len(offline)*100 > cfg.MaxOfflinePercent*len(nodes) {
		log.Debug("autoEvictor.evictCandidates: too many offline; gate closed", zap.Int("offline", len(offline)),
			zap.Int("nodes", len(nodes)), zap.Int("max_offline_percent", cfg.MaxOfflinePercent))
		return nil, fmt.Sprintf("%d of %d nodes are offline, more than self_heal.max_offline_percent (%d%%): "+
			"that looks like a partition, and nothing is evicted", len(offline), len(nodes), cfg.MaxOfflinePercent)
	}
	if 2*(len(nodes)-len(offline)) <= len(nodes) {
		log.Debug("autoEvictor.evictCandidates: no majority reachable; gate closed", zap.Int("offline", len(offline)), zap.Int("nodes", len(nodes)))
		return nil, "the controller reaches no majority of the nodes, so it may be the one cut off; nothing is evicted"
	}
	log.Debug("autoEvictor.evictCandidates: due nodes", zap.Int("due", len(due)), zap.Int("offline", len(offline)))
	return due, ""
}

func (e *autoEvictor) tick(ctx context.Context) {
	log := e.evictLog()
	start := time.Now()
	log.Debug("autoEvictor.tick: start")
	nodes, err := e.c.nodes.ListNodes(ctx)
	if err != nil {
		log.Debug("autoEvictor.tick: cannot list nodes", zap.Error(err))
		return
	}
	due, gate := e.evictCandidates(nodes, time.Now())
	e.mu.Lock()
	if gate != "" && !e.gated {
		e.publish("", event.SeverityWarning, gate)
	}
	e.gated = gate != ""
	e.mu.Unlock()
	if len(due) == 0 || e.syncInFlight(ctx) {
		log.Debug("autoEvictor.tick: nothing to do now", zap.Int("due", len(due)), zap.Bool("gated", gate != ""),
			zap.Duration("took", time.Since(start)))
		return
	}
	for _, n := range due {
		if e.evictOne(ctx, n) {
			// One replacement per tick: each is a full sync.
			log.Debug("autoEvictor.tick: started a replacement", zap.String("node", n.Name), zap.Duration("took", time.Since(start)))
			return
		}
	}
	log.Debug("autoEvictor.tick: done", zap.Int("due", len(due)), zap.Duration("took", time.Since(start)))
}

// syncInFlight reports whether the last replacement is still syncing.
func (e *autoEvictor) syncInFlight(ctx context.Context) bool {
	log := e.evictLog()
	e.mu.Lock()
	r := e.inflight
	e.mu.Unlock()
	if r == "" {
		return false
	}
	info, err := e.c.resources.GetResource(ctx, r)
	if err != nil {
		log.Debug("autoEvictor.syncInFlight: cannot read resource; not waiting", zap.String("resource", r), zap.Error(err))
		return false
	}
	for node, st := range info.NodeStates {
		if strings.Contains(st.Replication, "Sync") || st.DiskState == "Inconsistent" {
			log.Debug("autoEvictor.syncInFlight: still syncing", zap.String("resource", r), zap.String("node", node),
				zap.String("replication", st.Replication), zap.String("disk", st.DiskState))
			return true
		}
	}
	log.Debug("autoEvictor.syncInFlight: replacement synced", zap.String("resource", r))
	e.mu.Lock()
	e.inflight = ""
	e.mu.Unlock()
	return false
}

// evictOne replaces node's next replica; it reports whether it started one.
func (e *autoEvictor) evictOne(ctx context.Context, n *NodeInfo) bool {
	log := e.evictLog()
	log.Debug("autoEvictor.evictOne: start", zap.String("node", n.Name), zap.Time("offline_since", n.OfflineSince))
	rm := e.c.resources
	resources, err := e.c.db.ListResources(ctx)
	if err != nil {
		log.Debug("autoEvictor.evictOne: cannot list resources", zap.Error(err))
		return false
	}
	remaining := 0
	for _, r := range resources {
		if !contains(splitCSV(r.Nodes), n.Name) {
			continue
		}
		remaining++
		switch {
		case r.Name == SelfHaResource, r.WANMode, r.Labels[autoEvictOptOut] == "false":
			log.Debug("autoEvictor.evictOne: resource left for an operator", zap.String("resource", r.Name), zap.Bool("wan", r.WANMode))
			e.announceOnce(n.Name+"/"+r.Name+"/skip", fmt.Sprintf("%s is offline since %s, but its replica of %s is left "+
				"for an operator (the controller's metadata, WAN-replicated, or opted out)", n.Name,
				n.OfflineSince.UTC().Format(time.RFC3339), r.Name))
			continue
		}
		pool, sizeGB := rm.memberPoolAndSize(ctx, r.Name, "")
		survivors := without(splitCSV(r.Nodes), n.Name)
		picked, err := rm.selectAdditionalReplicas(ctx, pool, sizeGB, 1, survivors, nonReplicaMembers(r), nil, nil)
		log.Debug("autoEvictor.evictOne: picked replacement", zap.String("resource", r.Name), zap.String("pool", pool),
			zap.Uint64("size_gb", uint64(sizeGB)), zap.Strings("picked", picked), zap.Error(err))
		if err != nil || len(picked) == 0 {
			e.announceOnce(n.Name+"/"+r.Name+"/noroom", fmt.Sprintf("no node can take %s's replica of %s: %v", n.Name, r.Name, err))
			continue
		}
		plan := fmt.Sprintf("%s has been offline since %s: its replica of %s moves to %s", n.Name,
			n.OfflineSince.UTC().Format(time.RFC3339), r.Name, picked[0])
		if e.c.config.SelfHeal.DryRun() {
			log.Debug("autoEvictor.evictOne: dry run; announcing only", zap.String("resource", r.Name))
			e.announceOnce(n.Name+"/"+r.Name, "dry run: "+plan)
			continue
		}
		if err := rm.RemoveReplicaOptions(ctx, r.Name, n.Name, true); err != nil {
			log.Debug("autoEvictor.evictOne: lost-replica removal refused", zap.String("resource", r.Name), zap.Error(err))
			e.announceOnce(n.Name+"/"+r.Name+"/refused", fmt.Sprintf("%s's replica of %s is not replaced: %v", n.Name, r.Name, err))
			continue
		}
		if err := rm.AddReplica(ctx, r.Name, picked[0]); err != nil {
			log.Debug("autoEvictor.evictOne: new replica failed", zap.String("resource", r.Name), zap.String("to", picked[0]), zap.Error(err))
			e.publish(r.Name, event.SeverityCritical, fmt.Sprintf("%s's replica of %s was removed, but the new one on %s "+
				"failed: %v; add one with `haify resource add-replica %s --node <node>`", n.Name, r.Name, picked[0], err, r.Name))
			return true
		}
		e.mu.Lock()
		e.inflight = r.Name
		e.mu.Unlock()
		e.publish(r.Name, event.SeverityWarning, plan+"; it syncs now")
		return true
	}
	log.Debug("autoEvictor.evictOne: no replacement started", zap.String("node", n.Name), zap.Int("remaining", remaining))
	if remaining == 0 && !e.c.config.SelfHeal.DryRun() {
		err := e.c.nodes.SetNodeState(ctx, n.Address, NodeStateEvicted)
		log.Debug("autoEvictor.evictOne: node holds nothing; marking evicted", zap.String("node", n.Name), zap.Error(err))
		if err == nil {
			e.publish("", event.SeverityWarning, fmt.Sprintf("%s holds no replica any more and is evicted: run "+
				"`haify node restore %s` when it is back, or `haify node lost %s` if it will not be", n.Name, n.Name, n.Name))
		}
	}
	return false
}

func (e *autoEvictor) announceOnce(key, msg string) {
	e.mu.Lock()
	seen := e.announced[key]
	e.announced[key] = true
	e.mu.Unlock()
	e.evictLog().Debug("autoEvictor.announceOnce: announcement", zap.String("key", key), zap.Bool("already_announced", seen))
	if !seen {
		e.publish("", event.SeverityInfo, msg)
	}
}

func (e *autoEvictor) publish(resource string, sev event.Severity, msg string) {
	e.log.Warn(msg)
	if e.c.events != nil {
		e.c.events.Publish(event.Event{Type: event.TypeNodeEvicted, Severity: sev, Status: event.StatusInfo,
			Resource: resource, Message: msg})
	}
}
