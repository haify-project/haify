// Package alert watches cluster health and turns what it sees into events on
// an event.Bus.
//
// It only detects; it does not deliver. Webhooks and watch streams are
// subscribers on the bus, so adding a new way to be notified requires no change
// here, and every consumer sees the same event stream in the same order.
//
// Two kinds of condition are handled differently on purpose:
//
//   - Level conditions (a replica is degraded, a node is unreachable, a WAN
//     link is broken) are either true or false at every poll. These fire the
//     first time they are seen — including on the very first poll after a
//     controller restart, because a problem that predates the restart is still a
//     problem — and resolve when they clear.
//   - Edge conditions (the Primary moved) exist only as a transition between two
//     polls. These are suppressed on a resource's first observation: after a
//     restart the controller has no idea where the Primary used to be, and
//     announcing "failed over to node-a" every time it starts would train
//     operators to ignore the one alert that most needs attention.
package alert

import (
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/event"
)

// Monitor polls cluster health and publishes state changes to a bus.
type Monitor struct {
	interval  time.Duration
	resources ResourceLister
	nodes     NodeLister
	pools     PoolLister
	nearFull  float64
	full      float64
	bus       *event.Bus
	observer  Observer
	log       *zap.Logger

	hold time.Duration
	now  func() time.Time
	mu   sync.Mutex
	// firing tracks which level conditions are currently raised, keyed by
	// event.Event.Key(), so each is reported once when it starts and once when
	// it clears rather than on every poll.
	firing map[string]bool
	// owners records which lister raised each firing condition. Kept alongside
	// firing rather than derived from the key, so resolveVanished can tell a
	// subject that disappeared from a source that failed to answer.
	// pending holds when each warning condition was first seen, while it
	// waits out the hold.
	// raised is what each firing condition is, for the metrics.
	raised  map[string]FiringCondition
	pending map[string]time.Time
	owners  map[string]source
	// primaries remembers each resource's Primary set between polls. A resource
	// absent from this map has not been observed yet, which is what suppresses
	// failover alerts on the first poll.
	primaries map[string]string
	// lastPrimary remembers the last node that actually held the Primary role,
	// and unlike primaries it is not cleared when the role goes away. It is what
	// makes "no Primary" expressible as a condition: the alert has to keep
	// naming the node that was demoted for as long as it stays raised, and
	// primaries is "" on every poll after the first one that noticed.
	//
	// Its presence is also the test for whether a resource is supposed to have a
	// Primary at all. Most resources sit Secondary on every node by design, so
	// treating that as an outage would raise a critical for each of them.
	lastPrimary map[string]string

	stop     chan struct{}
	stopOnce sync.Once

	// kick asks for a poll now, when something reports a DRBD state change;
	// see Kick. Buffered by one so a burst of changes collapses into a poll.
	kick chan struct{}
	// eventDriven is set while every node's DRBD state changes are being
	// received as they happen. Then a quiet, healthy cluster needs no
	// thirty-second polling to be noticed breaking, and it is polled on
	// idleInterval instead; see nextInterval.
	eventDriven  atomic.Bool
	idleInterval time.Duration
	// steady is whether the last poll found every resource healthy. A resync
	// or a degraded replica emits no event as it progresses, so the interval
	// stays short until it is over.
	steady atomic.Bool

	pollsMu sync.Mutex
	polls   int
}

// Options configures a Monitor.
type Options struct {
	// Interval between polls. Zero uses 30s.
	Interval time.Duration
	// IdleInterval is the interval while the monitor is event-driven and the
	// last poll found nothing wrong. Below Interval uses DefaultIdleInterval.
	IdleInterval time.Duration
	// Resources is required.
	Resources ResourceLister
	// Nodes is optional; nil disables node reachability alerts.
	Nodes NodeLister
	// Pools is optional; nil disables thin pool capacity alerts.
	Pools PoolLister
	// NearFullPercent and FullPercent are the thin pool utilisation thresholds.
	// Zero or out-of-range values fall back to the defaults, and a NearFull at
	// or above Full is corrected rather than rejected — a misconfigured
	// threshold must not silently disable the alerting it configures.
	NearFullPercent float64
	FullPercent     float64
	// Observer is optional; nil means each poll's snapshot is used for events
	// only.
	Observer Observer
	// Logger is optional.
	Logger *zap.Logger
	// WarningHold is how long a warning condition must last before it is
	// raised; zero raises it on the first poll that sees it. See alert_hold.go.
	WarningHold time.Duration
}

// Default thin pool utilisation thresholds, in percent.
const (
	DefaultNearFullPercent = 85.0
	DefaultFullPercent     = 95.0
)

// DefaultIdleInterval is how often a healthy cluster is polled while its DRBD
// state changes arrive as events. It still bounds how late a condition no
// event reports — a thin pool filling up — is noticed.
const DefaultIdleInterval = 5 * time.Minute

// kickSettle is how long a kick waits for the rest of a burst: one failover
// is a dozen state changes across several nodes within a second or two.
var kickSettle = 2 * time.Second

// NewMonitor creates a monitor publishing to bus.
func NewMonitor(bus *event.Bus, opts Options) *Monitor {
	if opts.Logger == nil {
		opts.Logger = zap.NewNop()
	}
	log := opts.Logger
	if opts.Interval <= 0 {
		log.Debug("NewMonitor: interval not set, using 30s", zap.Duration("requested", opts.Interval))
		opts.Interval = 30 * time.Second
	}
	if opts.IdleInterval < opts.Interval {
		log.Debug("NewMonitor: idle interval below poll interval, raising it",
			zap.Duration("requested", opts.IdleInterval), zap.Duration("interval", opts.Interval))
		opts.IdleInterval = max(opts.Interval, DefaultIdleInterval)
	}
	near, full := normalizeThresholds(opts.NearFullPercent, opts.FullPercent)
	log.Debug("NewMonitor: creating alert monitor",
		zap.Duration("interval", opts.Interval), zap.Duration("idle_interval", opts.IdleInterval),
		zap.Bool("has_resources", opts.Resources != nil), zap.Bool("has_nodes", opts.Nodes != nil),
		zap.Bool("has_pools", opts.Pools != nil), zap.Bool("has_observer", opts.Observer != nil),
		zap.Bool("has_bus", bus != nil), zap.Float64("near_full_percent", near),
		zap.Float64("full_percent", full), zap.Duration("warning_hold", opts.WarningHold))
	return &Monitor{
		interval:     opts.Interval,
		resources:    opts.Resources,
		nodes:        opts.Nodes,
		pools:        opts.Pools,
		nearFull:     near,
		full:         full,
		bus:          bus,
		observer:     opts.Observer,
		log:          opts.Logger,
		firing:       make(map[string]bool),
		owners:       make(map[string]source),
		primaries:    make(map[string]string),
		lastPrimary:  make(map[string]string),
		stop:         make(chan struct{}),
		kick:         make(chan struct{}, 1),
		idleInterval: opts.IdleInterval,
		hold:         opts.WarningHold,
		pending:      make(map[string]time.Time),
		raised:       make(map[string]FiringCondition),
		now:          time.Now,
	}
}

// normalizeThresholds keeps the two thin pool thresholds usable whatever the
// configuration says.
//
// A percentage outside (0, 100] is meaningless and falls back to the default. A
// near-full at or above full would make the warning unreachable — every pool
// crossing it would already be critical — so it is pulled below rather than
// rejected: a bad threshold should degrade the alert, not remove it.
func normalizeThresholds(near, full float64) (float64, float64) {
	if near <= 0 || near > 100 {
		zap.L().Debug("normalizeThresholds: near-full out of range, using default",
			zap.Float64("requested", near), zap.Float64("near_full_percent", DefaultNearFullPercent))
		near = DefaultNearFullPercent
	}
	if full <= 0 || full > 100 {
		zap.L().Debug("normalizeThresholds: full out of range, using default",
			zap.Float64("requested", full), zap.Float64("full_percent", DefaultFullPercent))
		full = DefaultFullPercent
	}
	if near >= full {
		corrected := full * DefaultNearFullPercent / DefaultFullPercent
		zap.L().Debug("normalizeThresholds: near-full at or above full, pulling it below",
			zap.Float64("near_full_percent", near), zap.Float64("full_percent", full), zap.Float64("corrected", corrected))
		near = corrected
	}
	return near, full
}
