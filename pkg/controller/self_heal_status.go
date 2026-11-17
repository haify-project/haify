package controller

import (
	"context"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
)

// What self-healing is doing right now (`haify self-heal status`).
//
// The loops say what they decide as events, each reason once. That answers
// "what happened" and not "what is it waiting for": how long a client has to
// stay in use before its replica follows it, why one is passed over, how far
// a swap has got. This file reads the loops' own state for that. It changes
// nothing, and what it reports about a client is what the loop decided on its
// last pass, not a second decision made here.

// The states a watched client can be in (AutoDiskfulClient.state).
const (
	followStateCounting = "counting"
	followStateDue      = "due"
	followStateSkipped  = "skipped"
	followStateWaiting  = "waiting"
	followStatePlanned  = "planned"
	followStateFailed   = "failed"
)

// The phases of a swap (AutoDiskfulSwap.phase).
const (
	followPhaseSyncing    = "syncing"
	followPhaseRemoving   = "removing"
	followPhaseTiebreaker = "tiebreaker"
)

// followVerdict is what auto-diskful last decided about a client that has
// been in use long enough.
type followVerdict struct {
	State string
	// Skip is followSkip's kind, for a skipped client.
	Skip    string
	Reason  string
	RetryAt time.Time
}

// decide records the verdict on a client; the caller must not hold mu.
func (f *dataFollower) decide(key string, v followVerdict) {
	f.mu.Lock()
	f.verdicts[key] = v
	f.mu.Unlock()
}

// noteStep records why a swap's current step waits; the caller must not hold
// mu.
func (f *dataFollower) noteStep(resource, why string) {
	f.mu.Lock()
	f.stepNotes[resource] = why
	f.mu.Unlock()
}

// SelfHealStatus reports what the self-healing loops are doing and waiting
// for.
func (c *Controller) SelfHealStatus(ctx context.Context) *haifypb.GetSelfHealStatusResponse {
	log := c.traceLogger()
	start := time.Now()
	resp := &haifypb.GetSelfHealStatusResponse{Success: true, ObservedUnix: start.Unix()}
	var resources []*database.Resource
	if c.db != nil {
		var err error
		resources, err = c.db.ListResources(ctx)
		log.Debug("Controller.SelfHealStatus: listed resources", zap.Int("count", len(resources)), zap.Error(err))
		if err != nil {
			// The loops' own state is still worth showing; the parts that
			// need the resource list are left out.
			resp.Message = "the resource list could not be read, so replica counts and moves are missing: " + err.Error()
		}
	}
	resp.AutoEvict = c.autoEvictStatus(ctx, resources)
	resp.AutoDiskful = c.autoDiskfulStatus(ctx, resources)
	resp.Moves = c.replicaMoveStatus(ctx, resources)
	log.Debug("Controller.SelfHealStatus: done", zap.String("auto_evict", resp.AutoEvict.GetMode()),
		zap.String("auto_diskful", resp.AutoDiskful.GetMode()), zap.Int("offline_nodes", len(resp.AutoEvict.GetNodes())),
		zap.Int("clients", len(resp.AutoDiskful.GetClients())), zap.Int("swaps", len(resp.AutoDiskful.GetSwaps())),
		zap.Int("moves", len(resp.Moves)), zap.Duration("took", time.Since(start)))
	return resp
}

// selfHealMode is a mode as the config file spells it, "off" for unset.
func selfHealMode(mode string) string {
	if mode == "" {
		return "off"
	}
	return mode
}

// autoEvictStatus lists the nodes that do not answer and what auto-evict
// makes of each. With auto-evict off no node is listed: nothing is counting
// towards anything.
func (c *Controller) autoEvictStatus(ctx context.Context, resources []*database.Resource) *haifypb.AutoEvictStatus {
	log := c.traceLogger()
	out := &haifypb.AutoEvictStatus{Mode: "off"}
	if c.config == nil {
		return out
	}
	cfg := c.config.SelfHeal
	out.Mode = selfHealMode(cfg.AutoEvict)
	out.AfterMinutes = uint32(max(cfg.AfterMinutes, 0))
	out.MaxOfflinePercent = uint32(max(cfg.MaxOfflinePercent, 0))
	e := c.evictor
	if e == nil || c.nodes == nil {
		log.Debug("Controller.autoEvictStatus: auto-evict is not running", zap.String("mode", out.Mode))
		return out
	}
	e.mu.Lock()
	out.Syncing = e.inflight
	e.mu.Unlock()
	nodes, err := c.nodes.ListNodes(ctx)
	if err != nil {
		log.Debug("Controller.autoEvictStatus: cannot list nodes", zap.Error(err))
		return out
	}
	now := time.Now()
	_, out.Gate = e.evictCandidates(nodes, now)
	after := time.Duration(cfg.AfterMinutes) * time.Minute
	for _, n := range nodes {
		if n == nil || (n.OfflineSince.IsZero() && n.State != NodeStateEvicted) {
			continue
		}
		st := &haifypb.AutoEvictNode{Node: n.Name, OfflineSince: unixOrZero(n.OfflineSince)}
		for _, r := range resources {
			if contains(splitCSV(r.Nodes), n.Name) {
				st.Replicas++
			}
		}
		left := after - now.Sub(n.OfflineSince)
		switch {
		case n.State == NodeStateEvicted:
			st.State = "evicted"
			st.Reason = "it holds no replica any more; `haify node restore` returns it to service, `haify node lost` gives it up"
		case n.State == NodeStateMaintenance:
			st.State, st.Reason = "exempt", "it is in maintenance"
		case n.Labels[autoEvictOptOut] == "false":
			st.State, st.Reason = "exempt", "it is labelled "+autoEvictOptOut+"=false"
		case left > 0:
			st.State, st.RemainingSeconds = "waiting", int64(left.Seconds())
		default:
			st.State = "due"
			st.Reason = out.Gate
		}
		log.Debug("Controller.autoEvictStatus: node", zap.String("node", n.Name), zap.String("state", st.State),
			zap.Int64("remaining_seconds", st.RemainingSeconds), zap.Uint32("replicas", st.Replicas))
		out.Nodes = append(out.Nodes, st)
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].Node < out.Nodes[j].Node })
	return out
}

// replicaMoveStatus lists the replica moves on record and how far the new
// replica of each has synced.
func (c *Controller) replicaMoveStatus(ctx context.Context, resources []*database.Resource) []*haifypb.ReplicaMoveStatus {
	var out []*haifypb.ReplicaMoveStatus
	for _, r := range resources {
		if r.MoveFrom == "" {
			continue
		}
		m := &haifypb.ReplicaMoveStatus{Resource: r.Name, From: r.MoveFrom, To: r.MoveTo}
		m.State, m.SyncPercent = c.replicaSyncState(ctx, r.Name, r.MoveTo)
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Resource < out[j].Resource })
	return out
}

// replicaSyncState reads the disk state of resource's replica on node and,
// when DRBD reports one, how far its sync has got. Both are empty when the
// status cannot be read: the caller shows what it knows without them.
func (c *Controller) replicaSyncState(ctx context.Context, resource, node string) (string, *float64) {
	log := c.traceLogger()
	if c.resources == nil {
		return "", nil
	}
	start := time.Now()
	info, err := c.resources.GetResource(ctx, resource)
	if err != nil || info == nil {
		log.Debug("Controller.replicaSyncState: status unreadable", zap.String("resource", resource),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return "", nil
	}
	st := info.NodeStates[node]
	if st == nil {
		addr := c.ResolveHost(node)
		for name, s := range info.NodeStates {
			if s != nil && addr != "" && c.ResolveHost(name) == addr {
				st = s
				break
			}
		}
	}
	if st == nil {
		log.Debug("Controller.replicaSyncState: no state for the node", zap.String("resource", resource), zap.String("node", node))
		return "", nil
	}
	state := st.DiskState
	if st.Connection != "" && !strings.EqualFold(st.Connection, "Connected") {
		state = strings.TrimSpace(state + " " + st.Connection)
	}
	log.Debug("Controller.replicaSyncState: read", zap.String("resource", resource), zap.String("node", node),
		zap.String("state", state), zap.Bool("percent_known", st.SyncPercentKnown), zap.Duration("took", time.Since(start)))
	if st.SyncPercentKnown && st.DiskState != "UpToDate" {
		p := st.SyncPercent
		return state, &p
	}
	return state, nil
}

// autoDiskfulStatus reports the clients auto-diskful watches, its swaps and
// its cooldowns.
func (c *Controller) autoDiskfulStatus(ctx context.Context, resources []*database.Resource) *haifypb.AutoDiskfulStatus {
	out := &haifypb.AutoDiskfulStatus{Mode: "off", CooldownHours: uint32(followCooldown / time.Hour)}
	if c.config != nil {
		out.Mode = selfHealMode(c.config.SelfHeal.AutoDiskful)
		out.AfterMinutes = uint32(max(c.config.SelfHeal.AutoDiskfulAfterMinutes, 0))
	}
	f := c.follower
	if f == nil {
		c.traceLogger().Debug("Controller.autoDiskfulStatus: auto-diskful is not running", zap.String("mode", out.Mode))
		return out
	}
	replicas := map[string]int{}
	for _, r := range resources {
		replicas[r.Name] = len(splitCSV(r.Nodes))
	}
	now := f.now()
	after := time.Duration(out.AfterMinutes) * time.Minute

	// Everything is copied out under the lock; the nodes are asked for the
	// sync progress of the swaps only after it is released.
	f.mu.Lock()
	for key, since := range f.state.InUse {
		resource, node, _ := strings.Cut(key, "/")
		cl := &haifypb.AutoDiskfulClient{Resource: resource, Node: node, InUseSince: since.Unix(),
			InUseSeconds: int64(now.Sub(since).Seconds()), State: followStateCounting}
		if left := after - now.Sub(since); left > 0 {
			cl.RemainingSeconds = int64(left.Seconds())
		} else {
			cl.State = followStateDue
			if v, ok := f.verdicts[key]; ok {
				cl.State, cl.Skip, cl.Reason, cl.RetryAt = v.State, v.Skip, v.Reason, unixOrZero(v.RetryAt)
			}
		}
		out.Clients = append(out.Clients, cl)
	}
	for name, p := range f.state.Pending {
		sw := &haifypb.AutoDiskfulSwap{Resource: name, Node: p.Node, StartedAt: unixOrZero(p.StartedAt),
			ReplicasNow: uint32(replicas[name]), ReplicasTarget: uint32(max(p.Replicas, 0)), Removed: p.Removed,
			Attempts: uint32(max(p.Attempts, 0)), NextTry: unixOrZero(p.NextTry), Detail: f.stepNotes[name]}
		if p.Removed != "" {
			sw.Phase = followPhaseTiebreaker
		}
		out.Swaps = append(out.Swaps, sw)
	}
	for name, at := range f.state.Followed {
		if until := at.Add(followCooldown); until.After(now) {
			out.Cooldowns = append(out.Cooldowns, &haifypb.AutoDiskfulCooldown{Resource: name, MovedAt: at.Unix(), Until: until.Unix()})
		}
	}
	f.mu.Unlock()

	for _, sw := range out.Swaps {
		if sw.Phase != "" {
			continue
		}
		// Which step is due is what converge reads too: the new replica is
		// either still syncing or waits for an old one to go.
		state, percent := c.replicaSyncState(ctx, sw.Resource, sw.Node)
		sw.Phase, sw.SyncPercent = followPhaseSyncing, percent
		if state == "UpToDate" {
			sw.Phase, sw.SyncPercent = followPhaseRemoving, nil
		}
		f.log.Debug("Controller.autoDiskfulStatus: swap", zap.String("resource", sw.Resource), zap.String("node", sw.Node),
			zap.String("phase", sw.Phase), zap.String("state", state))
	}
	sort.Slice(out.Clients, func(i, j int) bool {
		a, b := out.Clients[i], out.Clients[j]
		if a.InUseSince != b.InUseSince {
			return a.InUseSince < b.InUseSince
		}
		return a.Resource+"/"+a.Node < b.Resource+"/"+b.Node
	})
	sort.Slice(out.Swaps, func(i, j int) bool { return out.Swaps[i].Resource < out.Swaps[j].Resource })
	sort.Slice(out.Cooldowns, func(i, j int) bool { return out.Cooldowns[i].Resource < out.Cooldowns[j].Resource })
	f.log.Debug("Controller.autoDiskfulStatus: done", zap.Int("clients", len(out.Clients)), zap.Int("swaps", len(out.Swaps)),
		zap.Int("cooldowns", len(out.Cooldowns)))
	return out
}
