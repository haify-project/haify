// Package event carries the controller's operational notifications — a replica
// going degraded, a resource's Primary moving to another node, a node dropping
// off the network — from the detectors that notice them to whoever wants to
// know.
//
// Everything goes through one in-process Bus. A Webhook is then just a
// subscriber that POSTs, and a watch stream is a subscriber that forwards; both
// see exactly the same events in the same order, so an operator can wire up
// either (or both) without the two disagreeing about what happened.
//
// The Bus is deliberately not persisted. These are notifications about what the
// cluster is doing right now; the durable record of who changed what is the
// audit trail, and the durable record of cluster state is the cluster itself. A
// bounded in-memory history exists only so a client that connects a moment late
// can still see the alert that made someone go looking.
package event

import (
	"slices"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Type identifies what happened. Values are dotted "<subject>.<condition>" so a
// consumer can filter on a prefix ("resource.") without enumerating every type.
type Type string

const (
	// TypeResourceDegraded fires when a replica's disk or replication state
	// leaves the healthy set (Diskless, Failed, StandAlone, ...).
	TypeResourceDegraded Type = "resource.degraded"
	// TypeChannelTest is the message `channel test` sends. It has a type of
	// its own so a receiver that acts on event types — opens a ticket on
	// resource.degraded — does not act on a test.
	TypeChannelTest Type = "channel.test"
	// TypeResourceFailover fires when a resource's Primary role moves from one
	// set of nodes to a different one — the event an operator most wants pushed
	// at them, because by the time it happens the cluster has already reacted.
	TypeResourceFailover Type = "resource.failover"
	// TypeResourceNoPrimary fires when a resource that had a Primary no longer
	// has one anywhere. Nothing is serving I/O for it.
	TypeResourceNoPrimary Type = "resource.no_primary"
	// TypeResourcePromoted fires when a resource gains a Primary, either for the
	// first time or after a TypeResourceNoPrimary.
	TypeResourcePromoted Type = "resource.promoted"
	// TypeNodeUnreachable fires when the controller cannot reach a registered
	// node over SSH — the transport every other operation depends on.
	TypeNodeUnreachable Type = "node.unreachable"
	// TypeNodeEvicted is published (as info) by self-healing: a replica of
	// an unreachable node replaced, a node evicted, or — in dry-run, or when
	// a guard says no — what would have been done and why it was not.
	TypeNodeEvicted Type = "node.evicted"
	// TypeReplicaMoved is published (as info) when a move-replica finishes,
	// or (as a warning) when it could not.
	TypeReplicaMoved Type = "resource.replica_moved"
	// TypeWANDegraded fires when a WAN-replicated resource's cross-site link is
	// broken, which local replica states cannot reveal on their own.
	TypeWANDegraded Type = "wan.degraded"
	// TypeResourceOutOfSync fires when two connected, fully replicating copies
	// of a resource hold different data — what an online verify finds. Nothing
	// else reports it: both replicas say UpToDate, and whichever one a read
	// lands on decides what the application sees.
	TypeResourceOutOfSync Type = "resource.out_of_sync"

	// Thin pool capacity. Data and metadata are separate types rather than one
	// "pool full" event because they are separate failure modes with unrelated
	// causes, and an operator's response differs: data exhaustion is fixed by
	// extending the pool or deleting snapshots, metadata exhaustion by
	// extending the metadata LV specifically.
	//
	// Near-full and full are likewise separate types rather than one type whose
	// severity changes. A level condition publishes its severity when it starts
	// firing, so a single type crossing from warning to critical would never
	// re-announce itself; as two conditions, crossing the higher threshold
	// resolves the warning and raises the critical, which is visible.
	//
	// These carry the pool name in Resource and the node in Node. A pool is the
	// subject of the event in the same way a DRBD resource is.

	// TypePoolDataNearFull fires when a thin pool's data utilisation crosses
	// the warning threshold: still serving writes, no longer with room to
	// spare.
	TypePoolDataNearFull Type = "pool.data_near_full"
	// TypePoolDataFull fires when a thin pool's data utilisation is high enough
	// that it may no longer be able to absorb a full DRBD resync of the volumes
	// it holds — a resync reallocates every block, so a pool that looks
	// comfortable by snapshot delta can still fail one.
	TypePoolDataFull Type = "pool.data_full"
	// TypePoolMetadataNearFull fires when a thin pool's metadata utilisation
	// crosses the warning threshold.
	TypePoolMetadataNearFull Type = "pool.metadata_near_full"
	// TypePoolMetadataFull fires when a thin pool's metadata utilisation is
	// critical. Metadata is sized once at pool creation and does not grow with
	// the pool, so this can fire on a pool whose data is nearly empty.
	TypePoolMetadataFull Type = "pool.metadata_full"
	// TypePoolOutOfSpace fires when LVM itself reports the pool as out of data
	// space. This is not a threshold: the pool has already refused writes, the
	// kernel has dropped the backing device, and any DRBD replica on it is
	// about to report Diskless.
	TypePoolOutOfSpace Type = "pool.out_of_space"
	// TypePoolVDOPhysicalNearFull and TypePoolVDOPhysicalFull are the
	// near-full and full conditions for the physical space under a VDO-backed
	// thin pool. Deduplication and compression make it fill at a rate the thin
	// pool's own figures do not show, and when it is gone VDO fails writes
	// while the thin pool above it still reports room.
	TypePoolVDOPhysicalNearFull Type = "pool.vdo_physical_near_full"
	TypePoolVDOPhysicalFull     Type = "pool.vdo_physical_full"
	// TypePoolSnapshotsRemoved is published (as info) each time a near-full
	// thin pool loses a scheduled snapshot to make room. Snapshot history
	// vanishing is worth knowing about on its own, and a sudden run of these
	// is what a volume being encrypted looks like.
	TypePoolSnapshotsRemoved Type = "pool.snapshots_removed"
	// TypePoolSnapshotsLocked is published (as info, at critical severity)
	// on every scheduled run that finds a thin pool still near full with only
	// locked (or the newest) snapshots left, so nothing more is removed: the
	// pool has to grow before its replica's writes fail.
	TypePoolSnapshotsLocked Type = "pool.snapshots_locked"

	// TypeAuditShippingFailed fires when the audit trail has not reached an
	// [audit] destination for several minutes, and resolves once it has
	// caught up. Until then the entries wait in the controller database.
	TypeAuditShippingFailed Type = "audit.shipping_failed"
	// TypeAuditTruncated is published (as info, at critical severity) when
	// [audit] max_entries made the controller drop entries younger than
	// retention_days: history was lost, or someone is flooding the API to
	// push an entry out of the trail.
	TypeAuditTruncated Type = "audit.truncated"
	// TypeApprovalRequested is published (as info) when a call needs a
	// second person's approval ([rbac.approval]): whoever may approve hears
	// of it, and an unexpected one is a stolen token at work.
	TypeApprovalRequested Type = "approval.requested"
	// TypeClockJumped fires when the controller host's clock moved away from
	// the time the controller has counted since it started — what moving the
	// clock forward to end snapshot and backup locks early looks like. The
	// locks keep using the counted time (pkg/controller/lock_clock.go).
	TypeClockJumped Type = "controller.clock_jumped"
	// TypeResourceWriteAnomaly fires when a resource is written many times
	// faster than it usually is at that hour of the week — a volume being
	// rewritten wholesale, which is what encryption by ransomware looks like.
	// Firing it freezes the resource's snapshot schedule.
	TypeResourceWriteAnomaly Type = "resource.write_anomaly"

	// TypeBackupFailed fires when a scheduled backup did not complete, and
	// resolves on the schedule's next completed run. A schedule that keeps
	// failing leaves the cluster with an ever older last good copy and no
	// other sign of it.
	TypeBackupFailed Type = "backup.failed"

	// TypeInspectionCompleted is published once per inspection run. Its
	// severity is the run's worst finding, so a channel filtered to warning
	// and above hears about an inspection only when it found something.
	TypeInspectionCompleted Type = "inspection.completed"

	// TypePoolTrimmed is published (as info) after a scheduled trim, with what
	// it returned to the thin pools, and as a warning when a filesystem would
	// not trim.
	TypePoolTrimmed Type = "pool.trimmed"
	// TypePoolExtended is published when the controller grew a thin pool (or
	// its metadata) into its volume group's free space before it filled, and
	// as a warning when it needed to and could not.
	TypePoolExtended Type = "pool.extended"
	// TypePoolDiskMoved reports a disk taken out of, or replaced in, a pool:
	// its data moved off with pvmove. Warning when the move failed.
	TypePoolDiskMoved Type = "pool.disk_moved"
	// TypeDiskHealth fires when a disk backing a pool reports SMART or NVMe
	// health trouble, before it fails.
	TypeDiskHealth Type = "disk.health"
	// TypeVolumeMoved reports a volume moved to another pool, node by node.
	TypeVolumeMoved Type = "resource.volume_moved"
)

// Severity ranks how much an event should interrupt someone.
type Severity string

const (
	// SeverityInfo is a state change worth recording, not worth waking anyone.
	SeverityInfo Severity = "info"
	// SeverityWarning means redundancy or reachability is reduced but service
	// continues.
	SeverityWarning Severity = "warning"
	// SeverityCritical means service is affected or one more failure ends it.
	SeverityCritical Severity = "critical"
)

// rank orders severities for threshold comparisons. An unknown value ranks with
// info so a typo in a config filter cannot silently suppress critical alerts.
func (s Severity) rank() int {
	switch s {
	case SeverityCritical:
		return 2
	case SeverityWarning:
		return 1
	default:
		return 0
	}
}

// AtLeast reports whether s is as severe as min.
func (s Severity) AtLeast(min Severity) bool { return s.rank() >= min.rank() }

// ParseSeverity maps a config string to a Severity, falling back to info (the
// most permissive threshold) for anything unrecognised.
func ParseSeverity(s string) Severity {
	switch Severity(s) {
	case SeverityCritical:
		return SeverityCritical
	case SeverityWarning:
		return SeverityWarning
	default:
		return SeverityInfo
	}
}

// Status distinguishes an alert being raised from the same alert clearing, so a
// receiver can pair them up instead of treating a recovery as a new problem.
type Status string

const (
	// StatusFiring means the condition is currently true.
	StatusFiring Status = "firing"
	// StatusResolved means a previously firing condition has cleared.
	StatusResolved Status = "resolved"
	// StatusInfo is for one-shot events that neither fire nor clear.
	StatusInfo Status = "info"
)

// Event is one notification. The JSON encoding here is the Webhook body and the
// watch-stream frame; both are the same shape on purpose, so a receiver written
// against one works against the other.
type Event struct {
	// ID is monotonic across the whole bus, starting at 1. A watcher that sees
	// its IDs skip knows it fell behind and lost events, which is otherwise
	// invisible.
	ID       uint64   `json:"id"`
	Type     Type     `json:"type"`
	Severity Severity `json:"severity"`
	Status   Status   `json:"status"`
	// Resource and Node scope the event. Either may be empty for events that do
	// not apply to one.
	Resource string `json:"resource,omitempty"`
	Node     string `json:"node,omitempty"`
	Message  string `json:"message"`
	// Details carries type-specific fields (disk_state, from/to nodes, ...)
	// without turning this struct into the union of every event's needs.
	Details   map[string]string `json:"details,omitempty"`
	Timestamp time.Time         `json:"timestamp"`
}

// Key identifies the condition an event is about, so firing and resolved
// notifications for the same problem can be matched. It is not unique per
// event — that is ID.
func (e Event) Key() string {
	return string(e.Type) + "|" + e.Resource + "|" + e.Node
}

// Filter narrows which events a subscriber or history query wants.
type Filter struct {
	// MinSeverity drops anything less severe. Zero value (empty) keeps all.
	MinSeverity Severity
	// Types keeps only these types. Empty keeps all.
	Types []Type
	// Resource keeps only events for this resource. Empty keeps all.
	Resource string
}

// Match reports whether e passes the filter.
func (f Filter) Match(e Event) bool {
	if !e.Severity.AtLeast(f.MinSeverity) {
		return false
	}
	if f.Resource != "" && e.Resource != f.Resource {
		return false
	}
	if len(f.Types) == 0 {
		return true
	}
	return slices.Contains(f.Types, e.Type)
}

// DefaultHistory is the number of past events kept for late-joining clients.
const DefaultHistory = 500

// subscriberBuffer is how many events a slow subscriber may fall behind before
// it starts losing them. Deep enough to ride out a burst (a node failing takes
// every resource on it degraded at once), shallow enough that a dead client
// cannot pin megabytes.
const subscriberBuffer = 256

type subscription struct {
	ch      chan Event
	filter  Filter
	dropped uint64
}

// Bus fans events out to subscribers and keeps a bounded history. It is safe
// for concurrent use.
type Bus struct {
	mu       sync.Mutex
	nextID   uint64
	hist     []Event
	histNext int
	histWrap bool
	subs     map[uint64]*subscription
	nextSub  uint64
	dropped  uint64
	// persist, when set, records each published event outside the process,
	// so the history outlives a controller restart. See SetPersister.
	persist func(Event)
}

// NewBus creates a bus retaining the last history events. history <= 0 uses
// DefaultHistory.
func NewBus(history int) *Bus {
	if history <= 0 {
		zap.L().Debug("NewBus: history not set, using default", zap.Int("requested", history), zap.Int("history", DefaultHistory))
		history = DefaultHistory
	}
	zap.L().Debug("NewBus: creating event bus", zap.Int("history", history))
	return &Bus{
		hist: make([]Event, history),
		subs: make(map[uint64]*subscription),
	}
}

// Publish stamps e with the next ID and the current time (unless already set),
// records it in the history, and delivers it to every matching subscriber. It
// returns the stamped event.
//
// Delivery never blocks: a subscriber whose buffer is full loses the event
// rather than stalling the detector that published it. Losing an alert is bad;
// wedging the health monitor behind one unresponsive Webhook receiver would
// lose all of them.
func (b *Bus) Publish(e Event) Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.nextID++
	e.ID = b.nextID
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	if e.Severity == "" {
		e.Severity = SeverityInfo
	}
	if e.Status == "" {
		e.Status = StatusInfo
	}

	b.hist[b.histNext] = e
	b.histNext++
	if b.histNext == len(b.hist) {
		b.histNext = 0
		b.histWrap = true
	}

	log := zap.L()
	log.Debug("Bus.Publish: publishing event",
		zap.Uint64("id", e.ID), zap.String("type", string(e.Type)),
		zap.String("severity", string(e.Severity)), zap.String("status", string(e.Status)),
		zap.String("resource", e.Resource), zap.String("node", e.Node),
		zap.Int("details", len(e.Details)), zap.Int("subscribers", len(b.subs)))

	delivered, skipped, lost := 0, 0, 0
	for id, s := range b.subs {
		if !s.filter.Match(e) {
			skipped++
			continue
		}
		select {
		case s.ch <- e:
			delivered++
		default:
			s.dropped++
			b.dropped++
			lost++
			log.Debug("Bus.Publish: subscriber buffer full, event dropped for it",
				zap.Uint64("id", e.ID), zap.Uint64("subscriber", id),
				zap.Uint64("subscriber_dropped", s.dropped), zap.Uint64("bus_dropped", b.dropped))
		}
	}
	if b.persist != nil {
		start := time.Now()
		b.persist(e)
		log.Debug("Bus.Publish: event persisted", zap.Uint64("id", e.ID), zap.Duration("took", time.Since(start)))
	} else {
		log.Debug("Bus.Publish: no persister, event kept in memory only", zap.Uint64("id", e.ID))
	}
	log.Debug("Bus.Publish: event delivered",
		zap.Uint64("id", e.ID), zap.Int("delivered", delivered),
		zap.Int("filtered_out", skipped), zap.Int("dropped", lost))
	return e
}

// SetPersister records every event published from now on with fn. It is
// called with the bus lock held, so fn must not publish.
func (b *Bus) SetPersister(fn func(Event)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	zap.L().Debug("Bus.SetPersister: setting event persister", zap.Bool("has_persister", fn != nil))
	b.persist = fn
}

// Restore loads earlier events into the history — what the controller that ran
// before this one saw — and continues numbering after the highest ID among
// them, so a client resuming with ?since= does not skip or repeat any.
// Subscribers are not told: these events were delivered when they happened.
func (b *Bus) Restore(events []Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	log := zap.L()
	log.Debug("Bus.Restore: restoring event history", zap.Int("count", len(events)), zap.Uint64("next_id", b.nextID))
	restored := 0
	for _, e := range events {
		if e.ID == 0 {
			log.Debug("Bus.Restore: skipping event without an ID", zap.String("type", string(e.Type)))
			continue
		}
		restored++
		b.hist[b.histNext] = e
		b.histNext++
		if b.histNext == len(b.hist) {
			b.histNext = 0
			b.histWrap = true
		}
		if e.ID > b.nextID {
			b.nextID = e.ID
		}
	}
	log.Debug("Bus.Restore: history restored", zap.Int("restored", restored), zap.Uint64("next_id", b.nextID))
}

// Subscribe returns a channel of events matching filter and a cancel function.
// Cancel must be called when the subscriber is done; it closes the channel, so
// a `for e := range ch` loop terminates. Calling cancel more than once is safe.
func (b *Bus) Subscribe(filter Filter) (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.nextSub++
	id := b.nextSub
	s := &subscription{ch: make(chan Event, subscriberBuffer), filter: filter}
	b.subs[id] = s
	zap.L().Debug("Bus.Subscribe: subscriber added",
		zap.Uint64("subscriber", id), zap.String("min_severity", string(filter.MinSeverity)),
		zap.Int("types", len(filter.Types)), zap.String("resource", filter.Resource),
		zap.Int("subscribers", len(b.subs)))

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if _, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(s.ch)
				zap.L().Debug("Bus.Subscribe: subscriber cancelled",
					zap.Uint64("subscriber", id), zap.Uint64("dropped", s.dropped),
					zap.Int("subscribers", len(b.subs)))
			}
		})
	}
	return s.ch, cancel
}

// Recent returns up to limit past events matching filter, oldest first. limit
// <= 0 returns everything retained. sinceID > 0 returns only events newer than
// that ID, which lets a reconnecting watcher resume without replaying what it
// already saw.
func (b *Bus) Recent(filter Filter, sinceID uint64, limit int) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []Event
	for i := 0; i < len(b.hist); i++ {
		e := b.hist[i]
		if e.ID == 0 || e.ID <= sinceID {
			continue // never written, or already seen
		}
		if !filter.Match(e) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	// Trim from the front: when more events match than the caller asked for,
	// the newest are the useful ones.
	if limit > 0 && len(out) > limit {
		zap.L().Debug("Bus.Recent: trimming to newest events", zap.Int("matched", len(out)), zap.Int("limit", limit))
		out = out[len(out)-limit:]
	}
	zap.L().Debug("Bus.Recent: history queried",
		zap.Uint64("since_id", sinceID), zap.Int("limit", limit),
		zap.String("min_severity", string(filter.MinSeverity)), zap.String("resource", filter.Resource),
		zap.Int("count", len(out)))
	return out
}

// Stats reports bus counters for diagnostics.
func (b *Bus) Stats() (published, dropped uint64, subscribers int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.nextID, b.dropped, len(b.subs)
}
