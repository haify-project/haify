package controller

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/alert"
	"github.com/haify-project/haify/pkg/wansizing"
)

// Write-rate history for WAN sizing (haify wan sizing).
//
// Sizing a WAN link wants the write rate measured over days, busiest periods
// included, before anything is replicated off-site. DRBD already counts what
// it writes to each node's backing disk and the health poll already reads that
// counter, so every poll turns the change since the previous one into a rate
// and adds it to the resource's history: per UTC day, the bytes and seconds
// (for the average), the busiest interval, and a histogram of the intervals
// (for percentiles). Fourteen days are kept, in the database, so the history
// survives a controller restart and moves with it on failover.
//
// It runs for every resource, not only WAN ones, because the question it
// answers first is whether a resource should go over the WAN at all. Resync
// traffic is DRBD's own, not the application's, and is left out.

// writeHistorySaveEvery bounds how much a controller crash loses.
const writeHistorySaveEvery = 10 * time.Minute

// historyStore persists histories; the database in production.
type historyStore interface {
	SaveWriteHistory(ctx context.Context, resource string, data []byte) error
	LoadWriteHistory(ctx context.Context, resource string) ([]byte, error)
}

type historyTrack struct {
	node   string
	kib    uint64
	at     time.Time
	hist   wansizing.History
	loaded bool
	dirty  bool
}

type writeHistory struct {
	store historyStore
	log   *zap.Logger
	ctx   context.Context
	now   func() time.Time

	mu       sync.Mutex
	tracks   map[string]*historyTrack
	lastSave time.Time
}

func newWriteHistory(c *Controller) *writeHistory {
	w := &writeHistory{log: c.traceLogger(), ctx: c.ctx, now: time.Now, tracks: map[string]*historyTrack{}}
	if w.ctx == nil {
		w.ctx = context.Background()
	}
	if c.db != nil {
		w.store = c.db
	}
	w.log.Debug("newWriteHistory: built", zap.Bool("has_store", w.store != nil))
	return w
}

// Observed implements alert.Observer.
func (w *writeHistory) Observed(obs alert.Observation) {
	res := obs.Resources
	if !res.Enabled || !res.OK {
		w.log.Debug("writeHistory.Observed: no resource reading this poll", zap.Bool("enabled", res.Enabled), zap.Bool("ok", res.OK))
		return
	}
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	seen := make(map[string]bool, len(res.Items))
	for _, item := range res.Items {
		seen[item.Name] = true
		node, kib, ok := writtenReading(item)
		if !ok {
			// A resync writes to the target's disk too. Start over afterwards,
			// so the interval spanning it is not taken for the application's.
			if t, tracked := w.tracks[item.Name]; tracked {
				t.at = time.Time{}
			}
			w.log.Debug("writeHistory.Observed: no usable reading (resyncing or no counter)", zap.String("resource", item.Name))
			continue
		}
		t := w.track(item.Name)
		w.sample(item.Name, t, node, kib, now)
	}
	// A resource gone from a complete poll was deleted; its stored history went
	// with its record, and the copy here must not be written back.
	for name := range w.tracks {
		if !seen[name] {
			w.log.Debug("writeHistory.Observed: resource gone, forgetting its history", zap.String("resource", name))
			delete(w.tracks, name)
		}
	}
	if now.Sub(w.lastSave) >= writeHistorySaveEvery {
		w.save()
		w.lastSave = now
	}
}

// sample turns a counter reading into one interval of history. A different
// answering node, or a counter that went down (the resource came up again),
// starts over rather than inventing a rate.
func (w *writeHistory) sample(resource string, t *historyTrack, node string, kib uint64, now time.Time) {
	defer func() { t.node, t.kib, t.at = node, kib, now }()
	if t.at.IsZero() || t.node != node || kib < t.kib {
		w.log.Debug("writeHistory.sample: first or restarted reading", zap.String("resource", resource), zap.String("node", node),
			zap.Uint64("written_kib", kib))
		return
	}
	dt := now.Sub(t.at).Seconds()
	if dt < 5 {
		w.log.Debug("writeHistory.sample: interval too short", zap.String("resource", resource), zap.Float64("secs", dt))
		return
	}
	bytes := float64(kib-t.kib) * 1024
	t.hist.Add(now, dt, bytes, node)
	t.dirty = true
	w.log.Debug("writeHistory.sample: recorded", zap.String("resource", resource), zap.String("node", node),
		zap.Float64("secs", dt), zap.Float64("bytes_per_sec", bytes/dt))
}

func (w *writeHistory) track(resource string) *historyTrack {
	t, ok := w.tracks[resource]
	if !ok {
		t = &historyTrack{}
		w.tracks[resource] = t
	}
	if !t.loaded {
		t.loaded = true
		if h := w.load(resource); h != nil {
			t.hist = *h
		}
	}
	return t
}

func (w *writeHistory) load(resource string) *wansizing.History {
	if w.store == nil {
		return nil
	}
	data, err := w.store.LoadWriteHistory(w.ctx, resource)
	if err != nil || len(data) == 0 {
		w.log.Debug("writeHistory.load: none stored", zap.String("resource", resource), zap.Error(err))
		return nil
	}
	var h wansizing.History
	if err := json.Unmarshal(data, &h); err != nil {
		w.log.Debug("writeHistory.load: stored history does not parse", zap.String("resource", resource), zap.Error(err))
		return nil
	}
	w.log.Debug("writeHistory.load: loaded", zap.String("resource", resource), zap.Int("days", len(h.Days)))
	return &h
}

func (w *writeHistory) save() {
	if w.store == nil {
		w.log.Debug("writeHistory.save: no store, history kept in memory only")
		return
	}
	for resource, t := range w.tracks {
		if !t.dirty {
			continue
		}
		data, err := json.Marshal(t.hist)
		if err != nil {
			w.log.Debug("writeHistory.save: not encoded", zap.String("resource", resource), zap.Error(err))
			continue
		}
		if err := w.store.SaveWriteHistory(w.ctx, resource, data); err != nil {
			w.log.Debug("Saving a write history failed", zap.String("resource", resource), zap.Error(err))
			continue
		}
		t.dirty = false
		w.log.Debug("writeHistory.save: saved", zap.String("resource", resource), zap.Int("days", len(t.hist.Days)))
	}
}

// Summary is the resource's write history over the last `days` days, from
// memory when the poll is tracking it and from the database otherwise (the
// health poll off, or nothing polled since the controller started). Nil when
// there is none.
func (w *writeHistory) Summary(resource string, days int) *wansizing.Window {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.tracks[resource]; ok && t.loaded {
		return t.hist.Summary(w.now(), days)
	}
	h := w.load(resource)
	if h == nil {
		return nil
	}
	return h.Summary(w.now(), days)
}
