package controller

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/event"
)

// Auto-diskful ([self_heal] auto_diskful): the data follows its user.
//
// A node without a replica can use a volume as a diskless client: a pod
// scheduled there with allowRemoteVolumeAccess, a VM started there by a
// hypervisor plugin, `resource attach-diskless`. Every read and write then
// crosses the replication network, for as long as the workload stays. When it
// stays, the replica is in the wrong place.
//
// So a client that has been Primary without a break for
// auto_diskful_after_minutes gets a disk of its own, in place (AddReplica on
// a diskless member: it keeps what it has open and resyncs underneath). When
// that disk is UpToDate, one of the old replicas is removed the ordinary way
// (RemoveReplica, which makes the survivors forget the peer), so the replica
// count is what it was. Between the two steps the resource has one copy more,
// never one fewer.
//
// It is off by default, because every such move writes the whole volume once
// more, and it yields to everything else: one self-healing sync at a time,
// and a long list of resources it leaves alone (followSkip), each with an
// event saying why.
//
// What it must remember is kept in the database (followState), because the
// second step can come hours after the first and on another controller.

const (
	followInterval = time.Minute
	followStateKey = "auto_diskful"
	// followGap is how old the stored readings may be and still be continued.
	// A controller failover is over well inside it; past it nobody watched,
	// and "in use without a break" cannot be claimed.
	followGap = 5 * time.Minute
	// followCooldown keeps a workload that hops between nodes from dragging
	// the volume behind it sync after sync.
	followCooldown = 24 * time.Hour
	// followRetryFirst and followRetryMax bound the pause between attempts at
	// a step that failed.
	followRetryFirst = 5 * time.Minute
	followRetryMax   = 6 * time.Hour
	autoDiskfulLabel = "haify.io/auto-diskful"
	// csiTopologyLabel is what the CSI driver records about where pods of a
	// volume may run: csiTopologyAnywhere for a remote-access volume, else the
	// replica nodes its PersistentVolume is pinned to.
	csiTopologyLabel    = "haify.csi/topology"
	csiTopologyAnywhere = "any"
	// followAuditUser is who the audit trail names for these changes.
	followAuditUser = "self-heal/auto-diskful"
)

// followPending is a client that was given a disk and the removal that has
// to follow it.
type followPending struct {
	// Node is the former client.
	Node string
	// Replicas is the replica count to return to.
	Replicas int
	// KeepVote says the removed replica's node stays as the tiebreaker: the
	// client was the third vote beside two replicas, and after the swap
	// nothing else would be.
	KeepVote bool
	// Removed is the replica that was removed, once it was.
	Removed   string    `json:",omitempty"`
	StartedAt time.Time `json:",omitempty"`
	// Attempts and NextTry space out retries of a step that failed.
	Attempts int       `json:",omitempty"`
	NextTry  time.Time `json:",omitempty"`
}

// followState is what auto-diskful keeps in the database.
type followState struct {
	// SavedAt dates the readings in InUse.
	SavedAt time.Time
	// InUse is since when each client ("resource/node") has been Primary
	// without a break.
	InUse map[string]time.Time `json:",omitempty"`
	// Pending is, per resource, the swap that is under way.
	Pending map[string]*followPending `json:",omitempty"`
	// Followed is when each resource's replica last moved to its user.
	Followed map[string]time.Time `json:",omitempty"`
}

type dataFollower struct {
	c   *Controller
	log *zap.Logger
	now func() time.Time

	mu        sync.Mutex
	state     followState
	announced map[string]bool      // skips, plans and failures already published
	retryAt   map[string]time.Time // clients whose conversion failed: not before
	// verdicts is what was last decided about each client that is due, and
	// stepNotes why each swap's current step waits. Both are what the events
	// said, kept so that `haify self-heal status` can say it again
	// (self_heal_status.go); neither decides anything.
	verdicts  map[string]followVerdict
	stepNotes map[string]string
}

func newDataFollower(c *Controller) *dataFollower {
	return &dataFollower{c: c, log: c.traceLogger().Named("auto-diskful"), now: time.Now,
		announced: map[string]bool{}, retryAt: map[string]time.Time{},
		verdicts: map[string]followVerdict{}, stepNotes: map[string]string{}}
}

// startAutoDiskful runs the follower on the active controller.
func (c *Controller) startAutoDiskful(ctx context.Context) {
	if c.config == nil || !c.config.SelfHeal.DiskfulEnabled() || c.db == nil || c.resources == nil {
		zap.L().Debug("startAutoDiskful: auto-diskful off", zap.Bool("has_db", c.db != nil))
		return
	}
	f := newDataFollower(c)
	f.load(ctx)
	c.follower = f
	f.log.Info("Auto-diskful enabled", zap.String("mode", c.config.SelfHeal.AutoDiskful),
		zap.Int("after_minutes", c.config.SelfHeal.AutoDiskfulAfterMinutes))
	go func() {
		t := time.NewTicker(followInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				f.log.Debug("startAutoDiskful: stopping", zap.Error(ctx.Err()))
				return
			case <-t.C:
				f.tick(ctx)
			}
		}
	}()
}

// following reports whether resource is in the middle of a swap, for the
// planners that must not move its replicas meanwhile.
func (f *dataFollower) following(resource string) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.Pending[resource] != nil
}

// load reads the stored state. The swaps under way and the cooldowns are
// taken as they are. The use timers are continued only when they were written
// a moment ago — a restart or a failover — and dropped otherwise: after a
// longer gap the client may have stopped and started again unseen, and
// starting the count again costs an hour, where trusting a stale one could
// cost a full sync nobody needed.
func (f *dataFollower) load(ctx context.Context) {
	start := time.Now()
	raw, err := f.c.db.LoadSelfHealState(ctx, followStateKey)
	f.log.Debug("dataFollower.load: read state", zap.Int("size_bytes", len(raw)), zap.Duration("took", time.Since(start)), zap.Error(err))
	var st followState
	if err == nil && len(raw) > 0 {
		if uerr := json.Unmarshal(raw, &st); uerr != nil {
			f.log.Warn("Stored auto-diskful state is unreadable; starting empty", zap.Error(uerr))
			st = followState{}
		}
	}
	now := f.now()
	if age := now.Sub(st.SavedAt); age < 0 || age > followGap {
		f.log.Debug("dataFollower.load: readings too old to continue; use timers start again",
			zap.Duration("age", age), zap.Int("timers", len(st.InUse)))
		st.InUse = nil
	}
	for k, since := range st.InUse {
		if since.After(now) {
			f.log.Debug("dataFollower.load: timer from the future dropped", zap.String("client", k))
			delete(st.InUse, k)
		}
	}
	f.mu.Lock()
	f.state = st
	f.ensureMaps()
	f.mu.Unlock()
	f.log.Debug("dataFollower.load: state loaded", zap.Int("timers", len(st.InUse)), zap.Int("pending", len(st.Pending)),
		zap.Int("cooldowns", len(st.Followed)))
}

// ensureMaps makes the state's maps writable; the caller holds mu.
func (f *dataFollower) ensureMaps() {
	if f.state.InUse == nil {
		f.state.InUse = map[string]time.Time{}
	}
	if f.state.Pending == nil {
		f.state.Pending = map[string]*followPending{}
	}
	if f.state.Followed == nil {
		f.state.Followed = map[string]time.Time{}
	}
}

// save writes the state. A failure is logged and the tick goes on: the next
// one writes again, and what was lost is at worst a timer.
func (f *dataFollower) save(ctx context.Context) error {
	f.mu.Lock()
	f.state.SavedAt = f.now()
	raw, err := json.Marshal(f.state)
	f.mu.Unlock()
	if err == nil {
		start := time.Now()
		err = f.c.db.SaveSelfHealState(ctx, followStateKey, raw)
		f.log.Debug("dataFollower.save: wrote state", zap.Int("size_bytes", len(raw)), zap.Duration("took", time.Since(start)), zap.Error(err))
	}
	if err != nil {
		f.c.traceLogger().Warn("Could not store the auto-diskful state", zap.Error(err))
	}
	return err
}

// followCandidate is a client that has been in use long enough.
type followCandidate struct {
	res   *database.Resource
	info  *ResourceInfo
	node  string
	since time.Time
}

func (f *dataFollower) tick(ctx context.Context) {
	start := time.Now()
	f.log.Debug("dataFollower.tick: start")
	resources, err := f.c.db.ListResources(ctx)
	if err != nil {
		f.log.Debug("dataFollower.tick: cannot list resources", zap.Error(err))
		return
	}
	nodes, err := f.c.nodes.ListNodes(ctx)
	if err != nil {
		f.log.Debug("dataFollower.tick: cannot list nodes", zap.Error(err))
		return
	}
	byName := make(map[string]*database.Resource, len(resources))
	for _, r := range resources {
		byName[r.Name] = r
	}
	nodeByName := make(map[string]*NodeInfo, len(nodes))
	for _, n := range nodes {
		if n != nil {
			nodeByName[n.Name] = n
		}
	}

	// The swaps under way first: finishing one is what frees the slot.
	f.mu.Lock()
	pending := make([]string, 0, len(f.state.Pending))
	for name := range f.state.Pending {
		pending = append(pending, name)
	}
	f.mu.Unlock()
	sort.Strings(pending)
	for _, name := range pending {
		f.converge(ctx, name, nodeByName)
	}

	due := f.observe(ctx, resources)
	started := false
	for _, cand := range due {
		if f.consider(ctx, cand, resources, nodeByName) {
			// One conversion per tick: each is a full sync.
			started = true
			break
		}
	}
	f.forget(byName)
	_ = f.save(ctx)
	f.log.Debug("dataFollower.tick: done", zap.Int("resources", len(resources)), zap.Int("pending", len(pending)),
		zap.Int("due", len(due)), zap.Bool("started", started), zap.Duration("took", time.Since(start)))
}

// observe reads the role of every diskless client and keeps the timers: a
// client that is the resource's one Primary keeps or starts its timer, and
// anything else ends it — a client that is Secondary or not connected, a
// resource whose status cannot be read, and two Primaries at once, which is
// a live migration passing through. It returns the clients in use for
// auto_diskful_after_minutes, longest first.
func (f *dataFollower) observe(ctx context.Context, resources []*database.Resource) []followCandidate {
	now := f.now()
	after := time.Duration(f.c.config.SelfHeal.AutoDiskfulAfterMinutes) * time.Minute
	seen := map[string]bool{}
	var due []followCandidate
	for _, r := range resources {
		clients := splitCSV(r.DisklessClients)
		if len(clients) == 0 || f.following(r.Name) {
			continue
		}
		start := time.Now()
		info, err := f.c.resources.GetResource(ctx, r.Name)
		if err != nil || info == nil {
			f.log.Debug("dataFollower.observe: status unreadable; its timers end", zap.String("resource", r.Name),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			continue
		}
		primaries := 0
		for _, st := range info.NodeStates {
			if st != nil && strings.EqualFold(st.Role, "Primary") {
				primaries++
			}
		}
		for _, node := range clients {
			st := f.stateOf(info, node)
			inUse := primaries == 1 && st != nil && strings.EqualFold(st.Role, "Primary") && linkUp(st)
			key := r.Name + "/" + node
			f.mu.Lock()
			since, timed := f.state.InUse[key]
			if inUse && !timed {
				since = now
				f.state.InUse[key] = since
			}
			f.mu.Unlock()
			f.log.Debug("dataFollower.observe: client", zap.String("resource", r.Name), zap.String("node", node),
				zap.Bool("in_use", inUse), zap.Int("primaries", primaries), zap.Bool("timed", timed),
				zap.Duration("took", time.Since(start)))
			if !inUse {
				continue
			}
			seen[key] = true
			if now.Sub(since) >= after {
				due = append(due, followCandidate{res: r, info: info, node: node, since: since})
			}
		}
	}
	f.mu.Lock()
	for key := range f.state.InUse {
		if !seen[key] {
			f.log.Debug("dataFollower.observe: no longer in use; timer ends", zap.String("client", key))
			delete(f.state.InUse, key)
			delete(f.retryAt, key)
			delete(f.verdicts, key)
			f.unannounce(key)
		}
	}
	f.mu.Unlock()
	sort.Slice(due, func(i, j int) bool {
		if !due[i].since.Equal(due[j].since) {
			return due[i].since.Before(due[j].since)
		}
		return due[i].res.Name < due[j].res.Name
	})
	f.log.Debug("dataFollower.observe: done", zap.Int("in_use", len(seen)), zap.Int("due", len(due)))
	return due
}

// forget drops the cooldowns of resources that are gone or past it.
func (f *dataFollower) forget(byName map[string]*database.Resource) {
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, at := range f.state.Followed {
		if byName[name] == nil || now.Sub(at) > followCooldown {
			delete(f.state.Followed, name)
		}
	}
}

// stateOf is node's state in info, under whichever of its names DRBD or the
// registry filed it.
func (f *dataFollower) stateOf(info *ResourceInfo, node string) *ResourceNodeState {
	_, st := f.c.nodeStateOf(info, node)
	return st
}

// linkUp reports whether a member's state was read over a live
// connection; the node that answered has no connection to itself.
func linkUp(st *ResourceNodeState) bool {
	return st.Connection == "" || strings.EqualFold(st.Connection, "Connected")
}

// followBackoff is the pause before attempt number attempts+1.
func followBackoff(attempts int) time.Duration {
	d := followRetryFirst
	for i := 1; i < attempts && d < followRetryMax; i++ {
		d *= 2
	}
	return min(d, followRetryMax)
}

// announceOnce publishes msg unless key was already announced; the caller
// must not hold mu.
func (f *dataFollower) announceOnce(key, resource string, sev event.Severity, msg string) {
	f.mu.Lock()
	seen := f.announced[key]
	f.announced[key] = true
	f.mu.Unlock()
	f.log.Debug("dataFollower.announceOnce: announcement", zap.String("key", key), zap.Bool("already_announced", seen))
	if !seen {
		f.publish(resource, sev, msg)
	}
}

// unannounce forgets what was announced about a client or a resource, so
// that the same reason is said again the next time it applies; the caller
// holds mu.
func (f *dataFollower) unannounce(prefix string) {
	for key := range f.announced {
		if key == prefix || strings.HasPrefix(key, prefix+"|") {
			delete(f.announced, key)
		}
	}
}

func (f *dataFollower) publish(resource string, sev event.Severity, msg string) {
	f.c.traceLogger().Info(msg, zap.String("resource", resource))
	if f.c.events != nil {
		f.c.events.Publish(event.Event{Type: event.TypeReplicaMoved, Severity: sev, Status: event.StatusInfo,
			Resource: resource, Message: msg})
	}
}

// audit records a change auto-diskful made in the audit trail, where the
// same change made through the API would be.
func (f *dataFollower) audit(method, resource, node string, start time.Time, err error) {
	result := "OK"
	msg := ""
	if err != nil {
		result, msg = "FAILED", err.Error()
	}
	f.log.Debug("dataFollower.audit: recording", zap.String("method", method), zap.String("resource", resource),
		zap.String("node", node), zap.String("result", result))
	sink := f.c.auditSink()
	if sink == nil {
		return
	}
	// Target is the bare resource name, as the API's entries have it: the
	// trail is filtered by exact target.
	sink(&database.AuditEvent{Timestamp: start, Method: method, Client: "controller", User: followAuditUser,
		Target: resource, Result: result, Granted: true, Latency: time.Since(start), Error: msg})
}
