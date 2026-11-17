package controller

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sync"
	"time"

	"github.com/haify-project/haify/pkg/alert"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/event"
	"github.com/haify-project/haify/pkg/gateway"
	"go.uber.org/zap"
)

// `haify ha evict <resource> --live`: moving an HA guest to another replica
// without stopping it.
//
// A plain evict has drbd-reactor stop the guest on one node and start it on
// another. A live move cannot go through drbd-reactor at all: its promoter
// knows one Primary and a chain that is either started or stopped, and a
// live migration needs two Primaries and a guest that is in both places for
// a moment. So the promoter is taken off the resource on every replica, the
// guest is migrated as a guest without HA would be, and the promoter is put
// back with the guest already running where it should — which the
// VirtualDomain agent's start accepts as started.
//
// The order, and what each step protects against, is in ha_live_run.go; how
// every ending — moved, failed, interrupted — is brought back to one
// Primary, one running guest and an enabled promoter is in ha_live_settle.go.

// Phases of a live move, as the API shows them.
const (
	livePhaseChecking  = "checking"
	livePhaseReleasing = "releasing"
	livePhaseWindow    = "window"
	livePhaseMigrating = "migrating"
	livePhaseSettling  = "settling"
	livePhaseDone      = "done"
)

// Outcomes of a live move.
const (
	liveOutcomeMoved     = "moved"
	liveOutcomeStayed    = "stayed"
	liveOutcomeRestarted = "restarted"
	liveOutcomeAttention = "attention"
)

const (
	// liveDefaultTimeout bounds the memory copy when the caller names none.
	liveDefaultTimeout = 30 * time.Minute
	// liveMaxTimeout is the longest a caller may ask for.
	liveMaxTimeout = 6 * time.Hour
	// liveMarkerGrace is how much longer than the move the hook's marker
	// stays valid: settling comes after the copy.
	liveMarkerGrace = 15 * time.Minute
)

// liveResourceRe is the resource names a live move builds unit and file
// names from.
var liveResourceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// haLiveJobs is the live moves in progress in this controller.
type haLiveJobs struct {
	mu      sync.Mutex
	running map[string]*haLiveJob
}

// haLiveJob is one live move.
type haLiveJob struct {
	rm        *ResourceManager
	guest     *vmGuest
	agentPath string
	// hosts is every replica that may run the guest, source and target
	// among them.
	hosts      []string
	sourceHost string
	targetHost string

	mu  sync.Mutex
	rec database.HaMigration
}

// reserve registers a job for its resource, or returns false when one runs.
func (j *haLiveJobs) reserve(resource string, job *haLiveJob) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running == nil {
		j.running = map[string]*haLiveJob{}
	}
	if _, busy := j.running[resource]; busy {
		return false
	}
	j.running[resource] = job
	return true
}

func (j *haLiveJobs) release(resource string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.running, resource)
}

func (j *haLiveJobs) get(resource string) *haLiveJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.running[resource]
}

// liveMigrationRunning returns a sentence saying that a live move of
// resource's guest is in progress, or "" when none is. Operations that write
// the promoter config or move the resource check it first: the config is
// disabled on purpose while a move runs.
func (rm *ResourceManager) liveMigrationRunning(resource string) string {
	if j := rm.live.get(resource); j != nil {
		rec := j.snapshot()
		return fmt.Sprintf("a live move of %s from %s to %s is in progress (%s); `haify ha migration %s` follows it",
			rec.Domain, rec.SourceNode, rec.TargetNode, rec.Phase, resource)
	}
	return ""
}

// liveMoveAlertWindow is how long after a live move ended the alert monitor
// still takes a change of Primary for that move. A healthy cluster is polled
// only every alert.DefaultIdleInterval, so the monitor may first see the
// demotion that ended the move that much later.
const liveMoveAlertWindow = alert.DefaultIdleInterval

// liveMoveRecent reports whether a live move of resource's guest is running
// or ended within liveMoveAlertWindow, and for one that ended, the node it
// left the guest on.
func (rm *ResourceManager) liveMoveRecent(ctx context.Context, resource string) (recent bool, endedOn string) {
	log := rm.controller.traceLogger()
	if rm.live.get(resource) != nil {
		log.Debug("ResourceManager.liveMoveRecent: move running", zap.String("resource", resource))
		return true, ""
	}
	if rm.controller.db == nil {
		log.Debug("ResourceManager.liveMoveRecent: no database", zap.String("resource", resource))
		return false, ""
	}
	rec, err := rm.controller.db.GetHaMigration(ctx, resource)
	// A move a stopped controller left unfinished is back in rm.live once
	// resumeLiveMigrations has picked it up, so only finished ones count here.
	recent = err == nil && rec != nil && rec.Done && time.Since(rec.UpdatedAt) < liveMoveAlertWindow
	if recent {
		endedOn = rec.ActiveNode
	}
	log.Debug("ResourceManager.liveMoveRecent: stored record", zap.String("resource", resource),
		zap.Bool("found", rec != nil), zap.Bool("recent", recent), zap.String("ended_on", endedOn), zap.Error(err))
	return recent, endedOn
}

// snapshot returns a copy of the job's record.
func (j *haLiveJob) snapshot() database.HaMigration {
	j.mu.Lock()
	defer j.mu.Unlock()
	rec := j.rec
	rec.Steps = append([]string(nil), j.rec.Steps...)
	return rec
}

// update changes the job's record and writes it to the database, which is
// what a controller starting after this one died reads.
func (j *haLiveJob) update(ctx context.Context, change func(*database.HaMigration)) {
	j.mu.Lock()
	change(&j.rec)
	rec := j.rec
	rec.Steps = append([]string(nil), j.rec.Steps...)
	j.mu.Unlock()
	if j.rm.controller.db == nil {
		return
	}
	// The record must be written even when the move's own context is gone.
	if err := j.rm.controller.db.SaveHaMigration(context.WithoutCancel(ctx), &rec); err != nil {
		j.rm.controller.logger.Warn("Failed to save the live move's record", zap.String("resource", rec.Resource), zap.Error(err))
	}
}

// step records one thing the move did.
func (j *haLiveJob) step(ctx context.Context, phase, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	j.rm.controller.logger.Info("Live move: "+msg, zap.String("resource", j.rec.Resource), zap.String("phase", phase))
	j.update(ctx, func(r *database.HaMigration) {
		r.Phase, r.Message = phase, msg
		r.Steps = append(r.Steps, msg)
	})
}

// GetHaMigration returns the live move of resource that is running, or the
// last one that ran; nil when there never was one.
func (rm *ResourceManager) GetHaMigration(ctx context.Context, resource string) (*database.HaMigration, error) {
	log := rm.controller.traceLogger()
	if j := rm.live.get(resource); j != nil {
		rec := j.snapshot()
		log.Debug("ResourceManager.GetHaMigration: running", zap.String("resource", resource), zap.String("phase", rec.Phase))
		return &rec, nil
	}
	if rm.controller.db == nil {
		log.Debug("ResourceManager.GetHaMigration: no database", zap.String("resource", resource))
		return nil, fmt.Errorf("database not available")
	}
	rec, err := rm.controller.db.GetHaMigration(ctx, resource)
	log.Debug("ResourceManager.GetHaMigration: stored", zap.String("resource", resource), zap.Bool("found", rec != nil), zap.Error(err))
	return rec, err
}

// StartLiveEvict checks that resource's guest can move live to target (or
// picks a target), then starts the move and returns its record. Everything
// that can be refused is refused here, before any node is changed.
func (rm *ResourceManager) StartLiveEvict(ctx context.Context, resource, target string, timeout time.Duration) (*database.HaMigration, error) {
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.StartLiveEvict: start", zap.String("resource", resource), zap.String("target", target),
		zap.Duration("timeout", timeout))
	if rm.deployment == nil || rm.controller.db == nil {
		log.Debug("ResourceManager.StartLiveEvict: no deployment client or database", zap.String("resource", resource))
		return nil, fmt.Errorf("deployment client or database not available")
	}
	if resource == SelfHaResource || !liveResourceRe.MatchString(resource) {
		log.Debug("ResourceManager.StartLiveEvict: not a resource a guest runs on", zap.String("resource", resource))
		return nil, fmt.Errorf("%s runs no libvirt guest; --live moves a guest made highly available with `ha create --vm`", resource)
	}
	switch {
	case timeout <= 0:
		timeout = liveDefaultTimeout
	case timeout > liveMaxTimeout:
		return nil, fmt.Errorf("a live move may take at most %s", liveMaxTimeout)
	}
	if m := rm.liveMigrationRunning(resource); m != "" {
		return nil, fmt.Errorf("%s", m)
	}
	if prev, err := rm.controller.db.GetHaMigration(ctx, resource); err == nil && prev != nil && !prev.Done {
		// A move the previous controller did not finish; resumeLiveMigrations
		// is settling it, or will at the next start.
		log.Debug("ResourceManager.StartLiveEvict: an earlier move is unsettled", zap.String("resource", resource), zap.String("phase", prev.Phase))
		return nil, fmt.Errorf("an earlier live move of %s was interrupted and is not settled yet; `haify ha migration %s` shows it", resource, resource)
	}

	cfg, err := rm.controller.db.GetHaConfig(ctx, resource)
	if err != nil || cfg == nil {
		log.Debug("ResourceManager.StartLiveEvict: no HA config", zap.String("resource", resource), zap.Error(err))
		return nil, fmt.Errorf("%s has no HA configuration; --live moves a guest made highly available with `ha create --vm`", resource)
	}
	g := vmGuestOf(cfg)
	if g == nil {
		log.Debug("ResourceManager.StartLiveEvict: HA config runs no libvirt guest", zap.String("resource", resource))
		return nil, fmt.Errorf("%s runs no libvirt guest, so it cannot move live: its services stop on one node and start on "+
			"another. Use `haify ha evict %s` without --live", resource, resource)
	}
	if dbRes, err := rm.controller.db.GetResource(ctx, resource); err == nil && dbRes != nil && dbRes.WANMode {
		log.Debug("ResourceManager.StartLiveEvict: WAN resource refused", zap.String("resource", resource))
		return nil, fmt.Errorf("%s replicates over WAN; a live move needs two Primaries for a moment, which asynchronous replication cannot do safely", resource)
	}
	hosts, err := rm.failoverHosts(ctx, resource)
	if err != nil {
		log.Debug("ResourceManager.StartLiveEvict: no failover hosts", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	if len(hosts) < 2 {
		return nil, fmt.Errorf("%s has one replica that can run the guest; there is nowhere to move it", resource)
	}

	// The chain must be the guest and nothing else: a mount or an address in
	// it could not follow a live migration.
	_, content, err := rm.GetHaToml(ctx, resource)
	if err != nil {
		log.Debug("ResourceManager.StartLiveEvict: promoter config unreadable", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	chain, err := gateway.PromoterChain(content)
	if err != nil {
		return nil, fmt.Errorf("HA config of %s: %w", resource, err)
	}
	if len(chain) != 1 || path.Base(chain[0].AgentPath) != "VirtualDomain" || chain[0].Unit != g.agentUnit(resource) {
		log.Debug("ResourceManager.StartLiveEvict: chain is not the guest alone", zap.String("resource", resource), zap.Int("chain", len(chain)))
		return nil, fmt.Errorf("the HA configuration of %s starts more than the guest %s; only a chain that is the guest alone can move live",
			resource, g.Domain)
	}
	agentPath := chain[0].AgentPath

	nodes := rm.probeLiveNodes(ctx, resource, g, agentPath, hosts)
	src, err := liveSource(g.Domain, resource, nodes)
	if err != nil {
		log.Debug("ResourceManager.StartLiveEvict: no usable source", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	if target != "" {
		target = rm.nodeLabel(rm.controller.ResolveHost(target))
	}
	// Definitions first: a replica that merely lacks the guest's definition
	// is given it, so it can be a target.
	must := []string{src.Node}
	if target != "" {
		must = append(must, target)
	}
	if err := rm.checkVMDefinitionBeforeMove(ctx, resource, g, hosts, must); err != nil {
		log.Debug("ResourceManager.StartLiveEvict: definition check failed", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	defState := map[string]VMDefinitionNode{}
	if st, err := rm.GetVMDefinition(ctx, resource); err == nil {
		for _, n := range st.Nodes {
			defState[n.Node] = n
		}
	}
	tgt, err := chooseLiveTarget(src, nodes, target, cfg.PreferredNodes, defState, rm.controller.nodes.GetDRBDNameByRef)
	if err != nil {
		log.Debug("ResourceManager.StartLiveEvict: no usable target", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	// libvirt on the source opens a connection to libvirt on the target.
	res, err := rm.runNodeScript(ctx, []string{src.Host}, liveConnectScript(tgt.Host))
	out, _ := appHostOutput(res, src.Host)
	if kv := keyValues(out); err != nil || kv["HAIFY_CONNECTED"] != "1" {
		reason := kv["HAIFY_ERR"]
		if err != nil {
			reason = err.Error()
		}
		dest, _ := liveMoveURIs(tgt.Host)
		log.Debug("ResourceManager.StartLiveEvict: source cannot reach libvirt on the target", zap.String("source", src.Node),
			zap.String("target", tgt.Node), zap.String("reason", reason))
		return nil, fmt.Errorf("root on %s cannot reach libvirt on %s (%s): %s. A live migration needs that connection: "+
			"give root on %s an SSH key that %s accepts, and its host key", src.Node, tgt.Node, dest, reason, src.Node, tgt.Node)
	}

	now := time.Now()
	job := &haLiveJob{rm: rm, guest: g, agentPath: agentPath, hosts: hosts, sourceHost: src.Host, targetHost: tgt.Host,
		rec: database.HaMigration{Resource: resource, Domain: g.Domain, SourceNode: src.Node, TargetNode: tgt.Node,
			AgentUnit: g.agentUnit(resource), Phase: livePhaseChecking, StartedAt: now, Deadline: now.Add(timeout),
			Message: fmt.Sprintf("moving %s from %s to %s", g.Domain, src.Node, tgt.Node)}}
	if !rm.live.reserve(resource, job) {
		return nil, fmt.Errorf("a live move of %s is already in progress", g.Domain)
	}
	job.step(ctx, livePhaseChecking, "moving %s from %s to %s; the copy may take until %s", g.Domain, src.Node, tgt.Node,
		job.rec.Deadline.Format("15:04:05"))
	// The move outlives the request that started it.
	go func() {
		defer rm.live.release(resource)
		job.run(rm.liveContext())
	}()
	rec := job.snapshot()
	return &rec, nil
}

// liveContext is the context a live move runs under: the controller's, so a
// controller that is shutting down stops driving the move (and leaves the
// record for the next one), not the context of the request that started it.
func (rm *ResourceManager) liveContext() context.Context {
	if rm.controller.ctx != nil {
		return rm.controller.ctx
	}
	return context.Background()
}

// publish raises the event for how the move ended.
func (j *haLiveJob) publish(rec database.HaMigration) {
	c := j.rm.controller
	if c.events == nil {
		c.traceLogger().Debug("haLiveJob.publish: no event bus", zap.String("resource", rec.Resource))
		return
	}
	ev := event.Event{Type: event.TypeHAGuestMoved, Severity: event.SeverityInfo, Status: event.StatusInfo,
		Resource: rec.Resource, Node: rec.ActiveNode, Message: rec.Message,
		Details: map[string]string{"domain": rec.Domain, "from": rec.SourceNode, "to": rec.TargetNode, "outcome": rec.Outcome}}
	switch rec.Outcome {
	case liveOutcomeMoved:
	case liveOutcomeAttention:
		ev.Type, ev.Severity = event.TypeHAGuestMoveFailed, event.SeverityCritical
	default:
		ev.Type, ev.Severity = event.TypeHAGuestMoveFailed, event.SeverityWarning
	}
	c.traceLogger().Debug("haLiveJob.publish: publishing", zap.String("resource", rec.Resource), zap.String("type", string(ev.Type)),
		zap.String("outcome", rec.Outcome))
	c.events.Publish(ev)
}

// resumeLiveMigrations picks up the live moves a previous controller left
// unfinished. It does not continue them: it waits for a migration that is
// still running to end, then puts back what the move had taken apart.
func (rm *ResourceManager) resumeLiveMigrations(ctx context.Context) {
	log := rm.controller.traceLogger()
	if rm.controller.db == nil || rm.deployment == nil {
		log.Debug("ResourceManager.resumeLiveMigrations: no database or deployment client")
		return
	}
	recs, err := rm.controller.db.ListHaMigrations(ctx)
	if err != nil {
		log.Debug("ResourceManager.resumeLiveMigrations: cannot list records", zap.Error(err))
		return
	}
	for _, rec := range recs {
		if rec.Done {
			continue
		}
		log.Debug("ResourceManager.resumeLiveMigrations: unfinished move", zap.String("resource", rec.Resource), zap.String("phase", rec.Phase))
		hosts, err := rm.failoverHosts(ctx, rec.Resource)
		if err != nil {
			rm.controller.logger.Warn("An interrupted live move cannot be settled: the resource's replicas are unknown",
				zap.String("resource", rec.Resource), zap.Error(err))
			continue
		}
		g := &vmGuest{Domain: rec.Domain, ConfigPath: "/etc/libvirt/qemu/" + rec.Domain + ".xml", Unit: rec.AgentUnit}
		job := &haLiveJob{rm: rm, guest: g, agentPath: "/usr/lib/ocf/resource.d/heartbeat/VirtualDomain", hosts: hosts,
			sourceHost: rm.controller.ResolveHost(rec.SourceNode), targetHost: rm.controller.ResolveHost(rec.TargetNode), rec: *rec}
		if !rm.live.reserve(rec.Resource, job) {
			continue
		}
		resource := rec.Resource
		go func() {
			defer rm.live.release(resource)
			job.step(ctx, livePhaseSettling, "the controller restarted during the move; waiting for the migration to end, then settling")
			job.finish(ctx, job.waitForMove(ctx))
		}()
	}
}
