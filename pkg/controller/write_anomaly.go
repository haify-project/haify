package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/alert"
	"github.com/haify-project/haify/pkg/config"
	"github.com/haify-project/haify/pkg/event"
)

// Write-anomaly detection ([alert.write_anomaly]).
//
// Ransomware that encrypts a volume rewrites it, block by block, as fast as
// it can. A block device cannot see file names or entropy, but it can see
// that: DRBD counts what it writes to each node's backing disk, the health
// poll reads that counter, and a resource writing many times faster than it
// usually does at that hour of the week is worth stopping for.
//
// When it fires, the detector does what keeps the clean history: it freezes
// the resource's snapshot schedule (snapshot_freeze.go), so neither retention
// nor a filling thin pool removes the snapshots from before the rewrite and
// nobody can delete them through haify, and it takes one more snapshot now. It
// raises resource.write_anomaly at critical severity. It does not claim an
// attack: a bulk import, a reindex or a restore look the same. Encryption
// that is slow, paused or throttled to look normal does not trip it, and a
// volume whose guest encrypts it (or an haify LUKS resource) changes nothing
// about the signal: it is the rate, not the content.
//
// Each resource learns its own normal, from the polls that were not
// anomalous: an overall average, and one per hour of the week once that hour
// has been seen a few times. Until the overall average has enough samples the
// detector only learns. Resync traffic is not the application's and is
// skipped.

const (
	// writeLearnSamples is how many normal polls a resource needs before its
	// rate is judged.
	writeLearnSamples = 30
	// writeHourSamples is how often an hour of the week must have been seen
	// before its own average replaces the overall one.
	writeHourSamples = 3
	// writeConfirmPolls is how many anomalous polls in a row fire: one burst
	// is not a rewrite.
	writeConfirmPolls = 2
	// writeCalmPolls is how many normal polls in a row resolve it.
	writeCalmPolls   = 3
	writeGlobalAlpha = 0.05
	writeHourAlpha   = 0.3
	writeSaveEvery   = 10 * time.Minute
)

// ewma is an exponentially weighted moving average of bytes per second.
type ewma struct {
	Mean float64 `json:"mean"`
	N    int     `json:"n"`
}

func (e *ewma) add(x, alpha float64) {
	if e.N == 0 {
		e.Mean = x
	} else {
		e.Mean += alpha * (x - e.Mean)
	}
	e.N++
}

// writeBaseline is what a resource has learned to be normal.
type writeBaseline struct {
	Global ewma      `json:"global"`
	Hours  [168]ewma `json:"hours"`
}

func hourOfWeek(t time.Time) int {
	t = t.UTC()
	return int(t.Weekday())*24 + t.Hour()
}

// expected is the normal rate at t, and whether enough has been learned.
func (b *writeBaseline) expected(t time.Time) (float64, bool) {
	if b.Global.N < writeLearnSamples {
		return 0, false
	}
	if h := b.Hours[hourOfWeek(t)]; h.N >= writeHourSamples {
		return h.Mean, true
	}
	return b.Global.Mean, true
}

// writeTrack is one resource's reading and state.
type writeTrack struct {
	node   string
	kib    uint64
	at     time.Time
	base   writeBaseline
	loaded bool
	over   int
	calm   int
	firing bool
}

// anomalyActions is what the detector does on firing; ScheduleManager in
// production.
type anomalyActions interface {
	FreezeSchedule(ctx context.Context, resource string, d time.Duration, reason string) (time.Time, error)
	SnapshotNow(ctx context.Context, resource string) error
}

// baselineStore keeps baselines across restarts; the database in production.
type baselineStore interface {
	SaveWriteBaseline(ctx context.Context, resource string, data []byte) error
	LoadWriteBaseline(ctx context.Context, resource string) ([]byte, error)
}

type writeAnomalyDetector struct {
	cfg     config.WriteAnomalyConfig
	actions anomalyActions
	store   baselineStore
	events  *event.Bus
	log     *zap.Logger
	ctx     context.Context
	now     func() time.Time

	mu       sync.Mutex
	tracks   map[string]*writeTrack
	lastSave time.Time
}

func newWriteAnomalyDetector(c *Controller) *writeAnomalyDetector {
	d := &writeAnomalyDetector{cfg: c.config.Alert.WriteAnomaly, events: c.events, log: c.logger, ctx: c.ctx,
		now: time.Now, tracks: map[string]*writeTrack{}}
	if c.schedules != nil {
		d.actions = c.schedules
	}
	if c.db != nil {
		d.store = c.db
	}
	c.traceLogger().Debug("newWriteAnomalyDetector: built", zap.Bool("enabled", d.cfg.Enabled), zap.Float64("factor", d.cfg.Factor),
		zap.Float64("min_mbps", d.cfg.MinMBps), zap.Int("freeze_hours", d.cfg.FreezeHours),
		zap.Bool("has_actions", d.actions != nil), zap.Bool("has_store", d.store != nil))
	return d
}

// Observed implements alert.Observer.
func (d *writeAnomalyDetector) Observed(obs alert.Observation) {
	res := obs.Resources
	if !res.Enabled || !res.OK {
		d.log.Debug("writeAnomalyDetector.Observed: no resource reading this poll", zap.Bool("enabled", res.Enabled), zap.Bool("ok", res.OK))
		return
	}
	now := d.now()
	d.log.Debug("writeAnomalyDetector.Observed: poll", zap.Int("resources", len(res.Items)), zap.Time("at", now))
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, item := range res.Items {
		node, kib, ok := writtenReading(item)
		if !ok {
			d.log.Debug("writeAnomalyDetector.Observed: no usable reading (resyncing or no counter)", zap.String("resource", item.Name))
			continue
		}
		t := d.track(item.Name)
		rate, ok := t.sample(node, kib, now)
		if !ok {
			d.log.Debug("writeAnomalyDetector.Observed: first or restarted reading, no rate yet", zap.String("resource", item.Name),
				zap.String("node", node), zap.Uint64("written_kib", kib))
			continue
		}
		d.judge(item.Name, t, rate, now)
	}
	if now.Sub(d.lastSave) >= writeSaveEvery {
		d.log.Debug("writeAnomalyDetector.Observed: saving baselines", zap.Int("resources", len(d.tracks)))
		d.save()
		d.lastSave = now
	}
}

// writtenReading is the answering node's written counter, unless a resync is
// running: its writes are DRBD's, not the application's.
func writtenReading(item alert.ResourceStatusInfo) (string, uint64, bool) {
	node, kib, found := "", uint64(0), false
	for name, st := range item.NodeStates {
		if strings.HasPrefix(st.ReplicationState, "Sync") || strings.HasPrefix(st.ReplicationState, "PausedSync") {
			return "", 0, false
		}
		if st.WrittenKiB != nil {
			node, kib, found = name, *st.WrittenKiB, true
		}
	}
	return node, kib, found
}

func (d *writeAnomalyDetector) track(resource string) *writeTrack {
	t, ok := d.tracks[resource]
	if !ok {
		t = &writeTrack{}
		d.tracks[resource] = t
	}
	if !t.loaded && d.store != nil {
		t.loaded = true
		if data, err := d.store.LoadWriteBaseline(d.ctx, resource); err == nil && len(data) > 0 {
			uerr := json.Unmarshal(data, &t.base)
			d.log.Debug("writeAnomalyDetector.track: baseline loaded", zap.String("resource", resource),
				zap.Int("samples", t.base.Global.N), zap.Error(uerr))
		} else {
			d.log.Debug("writeAnomalyDetector.track: no stored baseline", zap.String("resource", resource), zap.Error(err))
		}
	}
	return t
}

// sample turns a new counter reading into bytes per second. A different
// answering node, or a counter that went down (the resource came up again),
// starts over rather than inventing a rate.
func (t *writeTrack) sample(node string, kib uint64, now time.Time) (float64, bool) {
	defer func() { t.node, t.kib, t.at = node, kib, now }()
	if t.at.IsZero() || t.node != node || kib < t.kib {
		return 0, false
	}
	dt := now.Sub(t.at).Seconds()
	if dt < 5 {
		return 0, false
	}
	return float64(kib-t.kib) * 1024 / dt, true
}

func (d *writeAnomalyDetector) judge(resource string, t *writeTrack, rate float64, now time.Time) {
	expected, trained := t.base.expected(now)
	limit := max(d.cfg.Factor*expected, d.cfg.MinMBps*1024*1024)
	d.log.Debug("writeAnomalyDetector.judge: rate", zap.String("resource", resource), zap.Float64("bytes_per_sec", rate),
		zap.Float64("expected", expected), zap.Float64("limit", limit), zap.Bool("trained", trained),
		zap.Int("samples", t.base.Global.N), zap.Bool("firing", t.firing))
	if trained && rate > limit {
		t.over++
		t.calm = 0
		d.log.Debug("writeAnomalyDetector.judge: over the limit", zap.String("resource", resource), zap.Int("over", t.over))
		if t.over >= writeConfirmPolls && !t.firing {
			t.firing = true
			d.fire(resource, rate, expected)
		}
		// An anomalous rate is not learned: the attack must not become the
		// normal it is measured against.
		return
	}
	t.over = 0
	t.base.Global.add(rate, writeGlobalAlpha)
	t.base.Hours[hourOfWeek(now)].add(rate, writeHourAlpha)
	if t.firing {
		d.log.Debug("writeAnomalyDetector.judge: calm poll while firing", zap.String("resource", resource), zap.Int("calm", t.calm+1))
		if t.calm++; t.calm >= writeCalmPolls {
			t.firing, t.calm = false, 0
			d.publish(resource, event.StatusResolved, fmt.Sprintf("%s writes at its usual rate again (%s); "+
				"its snapshot schedule stays frozen until the freeze ends or is lifted", resource, mbps(rate)))
		}
	}
}

func (d *writeAnomalyDetector) fire(resource string, rate, expected float64) {
	freeze := time.Duration(d.cfg.FreezeHours) * time.Hour
	msg := fmt.Sprintf("%s is being written at %s, %.0fx its usual %s for this hour: a volume being rewritten "+
		"wholesale, which is what encryption by ransomware looks like (a bulk import or a reindex looks the same)",
		resource, mbps(rate), rate/max(expected, 1), mbps(expected))
	d.log.Debug("writeAnomalyDetector.fire: firing", zap.String("resource", resource), zap.Float64("bytes_per_sec", rate),
		zap.Float64("expected", expected), zap.Duration("freeze", freeze), zap.Bool("has_actions", d.actions != nil))
	if d.actions == nil {
		d.publish(resource, event.StatusFiring, msg)
		return
	}
	until, err := d.actions.FreezeSchedule(d.ctx, resource, freeze, "write anomaly at "+d.now().UTC().Format(time.RFC3339))
	if err != nil {
		d.log.Debug("writeAnomalyDetector.fire: freeze failed", zap.String("resource", resource), zap.Error(err))
		msg += fmt.Sprintf(". Its snapshots could not be frozen: %v", err)
		d.publish(resource, event.StatusFiring, msg)
		return
	}
	msg += fmt.Sprintf(". Its snapshot schedule is frozen until %s — nothing prunes or deletes its snapshots — "+
		"and a snapshot is being taken now", until.UTC().Format(time.RFC3339))
	d.publish(resource, event.StatusFiring, msg)
	go func() {
		ctx, cancel := context.WithTimeout(d.ctx, 10*time.Minute)
		defer cancel()
		if err := d.actions.SnapshotNow(ctx, resource); err != nil {
			d.log.Warn("Write anomaly: snapshot failed", zap.String("resource", resource), zap.Error(err))
		} else {
			d.log.Debug("writeAnomalyDetector.fire: snapshot taken", zap.String("resource", resource))
		}
	}()
}

func (d *writeAnomalyDetector) publish(resource string, st event.Status, msg string) {
	d.log.Warn(msg, zap.String("resource", resource))
	d.log.Debug("writeAnomalyDetector.publish: event", zap.String("resource", resource), zap.String("status", string(st)),
		zap.Bool("has_bus", d.events != nil))
	if d.events != nil {
		d.events.Publish(event.Event{Type: event.TypeResourceWriteAnomaly, Severity: event.SeverityCritical,
			Status: st, Resource: resource, Message: msg})
	}
}

func (d *writeAnomalyDetector) save() {
	if d.store == nil {
		d.log.Debug("writeAnomalyDetector.save: no store, baselines kept in memory only")
		return
	}
	for resource, t := range d.tracks {
		data, err := json.Marshal(t.base)
		if err != nil {
			d.log.Debug("writeAnomalyDetector.save: baseline not encoded", zap.String("resource", resource), zap.Error(err))
			continue
		}
		if err := d.store.SaveWriteBaseline(d.ctx, resource, data); err != nil {
			d.log.Debug("Saving a write baseline failed", zap.String("resource", resource), zap.Error(err))
		} else {
			d.log.Debug("writeAnomalyDetector.save: baseline saved", zap.String("resource", resource), zap.Int("samples", t.base.Global.N))
		}
	}
}

func mbps(bytesPerSec float64) string {
	return fmt.Sprintf("%.1f MB/s", bytesPerSec/1024/1024)
}

// observers hands each poll to several observers, in order.
type observers []alert.Observer

func (o observers) Observed(obs alert.Observation) {
	for _, x := range o {
		x.Observed(obs)
	}
}
