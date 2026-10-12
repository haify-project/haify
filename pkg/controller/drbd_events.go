package controller

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// The alert monitor learns about DRBD by asking: every poll reads every
// resource's status from its nodes, several SSH sessions per node every thirty
// seconds — around seventeen a minute into each node of an idle cluster. That
// is both the load and the latency: a failover is seen up to a poll late.
//
// DRBD will say when something changes. `drbdsetup events2` prints the state
// of everything once and then one line per change, for as long as it runs.
// With one of those open to every node, a change asks the monitor to look now,
// and a quiet cluster needs polling only as a safety net for what DRBD does
// not report (pool usage, resync progress).

// drbdEventsCmd is bounded by timeout(1): an SSH session closed under a
// command started without a terminal does not stop it until its next write,
// which on a quiet node may be days. An hour's cap means a lost stream
// leaves at most an hour-old process behind; the watcher reconnects.
const drbdEventsCmd = "sudo timeout 3600 drbdsetup events2 --timestamps all"

// drbdEventsReconcile is how often the set of watched nodes is compared with
// the registered ones.
var drbdEventsReconcile = 30 * time.Second

// eventMonitor is what the watcher drives: *alert.Monitor.
type eventMonitor interface {
	Kick()
	SetEventDriven(bool)
}

type lineStreamer interface {
	StreamLines(ctx context.Context, host, cmd string, onLine func(string)) error
}

type drbdEventWatcher struct {
	streamer lineStreamer
	monitor  eventMonitor
	hosts    func() []string
	log      *zap.Logger

	mu      sync.Mutex
	streams map[string]*drbdEventStream
	// allLive is the last value given to the monitor, for logging the change.
	allLive bool
}

type drbdEventStream struct {
	cancel context.CancelFunc
	// live is set once the stream has printed its initial state, and cleared
	// when it ends: until then a change on that node could go unseen.
	live bool
}

// watchDRBDEvents starts watching every node's DRBD events on behalf of the
// monitor, for as long as ctx lives.
func (c *Controller) watchDRBDEvents(ctx context.Context, monitor eventMonitor) {
	streamer, ok := c.deployment.(lineStreamer)
	if !ok {
		c.traceLogger().Debug("Controller.watchDRBDEvents: deployment client cannot stream, not watching events")
		return
	}
	c.traceLogger().Debug("Controller.watchDRBDEvents: starting event watcher")
	w := &drbdEventWatcher{
		streamer: streamer,
		monitor:  monitor,
		hosts: func() []string {
			c.hostsLock.RLock()
			defer c.hostsLock.RUnlock()
			return append([]string(nil), c.hosts...)
		},
		log:     c.traceLogger(),
		streams: map[string]*drbdEventStream{},
	}
	go w.run(ctx)
}

func (w *drbdEventWatcher) run(ctx context.Context) {
	t := time.NewTicker(drbdEventsReconcile)
	defer t.Stop()
	for {
		w.log.Debug("drbdEventWatcher.run: reconcile tick")
		w.reconcile(ctx)
		select {
		case <-ctx.Done():
			w.log.Debug("drbdEventWatcher.run: context done, stopping")
			w.monitor.SetEventDriven(false)
			return
		case <-t.C:
		}
	}
}

// reconcile starts a stream for each node not yet watched and stops the
// streams of nodes no longer registered.
func (w *drbdEventWatcher) reconcile(ctx context.Context) {
	want := map[string]bool{}
	for _, h := range w.hosts() {
		if h != "" {
			want[h] = true
		}
	}
	w.mu.Lock()
	for h, s := range w.streams {
		if !want[h] {
			w.log.Debug("drbdEventWatcher.reconcile: node no longer registered, stopping stream", zap.String("host", h))
			s.cancel()
			delete(w.streams, h)
		}
	}
	for h := range want {
		if _, ok := w.streams[h]; ok {
			continue
		}
		w.log.Debug("drbdEventWatcher.reconcile: starting stream", zap.String("host", h))
		sctx, cancel := context.WithCancel(ctx)
		w.streams[h] = &drbdEventStream{cancel: cancel}
		go w.follow(sctx, h)
	}
	n := len(w.streams)
	w.mu.Unlock()
	w.log.Debug("drbdEventWatcher.reconcile: done", zap.Int("wanted", len(want)), zap.Int("streams", n))
	w.updateLive()
}

// follow keeps one node's stream open, reconnecting with backoff.
func (w *drbdEventWatcher) follow(ctx context.Context, host string) {
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		started := time.Now()
		w.log.Debug("drbdEventWatcher.follow: opening event stream", zap.String("host", host))
		var lines, changes int
		err := w.streamer.StreamLines(ctx, host, drbdEventsCmd, func(line string) {
			lines++
			switch drbdEventKind(line) {
			case eventsInitialDone:
				w.log.Debug("drbdEventWatcher.follow: initial state received, stream live",
					zap.String("host", host), zap.Int("lines", lines), zap.Duration("took", time.Since(started)))
				w.setLive(host, true)
			case eventsChange:
				changes++
				w.monitor.Kick()
			}
		})
		w.log.Debug("drbdEventWatcher.follow: event stream closed", zap.String("host", host),
			zap.Int("lines", lines), zap.Int("changes", changes), zap.Duration("took", time.Since(started)), zap.Error(err))
		if ctx.Err() != nil {
			return
		}
		w.setLive(host, false)
		// A stream that ends may be a node that went away: look now rather
		// than at the next poll.
		w.monitor.Kick()
		if time.Since(started) > time.Minute {
			backoff = 5 * time.Second
		}
		w.log.Debug("DRBD event stream ended; reconnecting",
			zap.String("host", host), zap.Duration("in", backoff), zap.Error(err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (w *drbdEventWatcher) setLive(host string, live bool) {
	w.mu.Lock()
	if s, ok := w.streams[host]; ok {
		s.live = live
	} else {
		w.log.Debug("drbdEventWatcher.setLive: stream no longer tracked", zap.String("host", host))
	}
	w.mu.Unlock()
	w.log.Debug("drbdEventWatcher.setLive: stream liveness", zap.String("host", host), zap.Bool("live", live))
	w.updateLive()
}

// updateLive tells the monitor whether every node's changes are arriving.
func (w *drbdEventWatcher) updateLive() {
	w.mu.Lock()
	all := len(w.streams) > 0
	var down []string
	for h, s := range w.streams {
		all = all && s.live
		if !s.live {
			down = append(down, h)
		}
	}
	changed := all != w.allLive
	w.allLive = all
	n := len(w.streams)
	w.mu.Unlock()
	w.log.Debug("drbdEventWatcher.updateLive: event-driven decision", zap.Bool("all_live", all),
		zap.Bool("changed", changed), zap.Int("streams", n), zap.Int("down", len(down)))
	w.monitor.SetEventDriven(all)
	if changed && all {
		w.log.Info("Receiving DRBD events from every node; polling a healthy cluster less often", zap.Int("nodes", n))
	} else if changed {
		w.log.Info("DRBD events not arriving from every node; polling at the normal rate", zap.Strings("waiting_for", down))
	}
}

type eventsLineKind int

const (
	eventsIgnore eventsLineKind = iota
	eventsInitialDone
	eventsChange
)

// drbdEventKind classifies one events2 line. With --timestamps each starts
// with the time, then the verb: "exists" lines are the initial state, closed
// by "exists -"; "create", "change", "destroy" and "call" (a handler such as
// split-brain being run) are changes. Without --statistics every change line
// is a state change, so none needs to be filtered out by what it touches.
func drbdEventKind(line string) eventsLineKind {
	f := strings.Fields(line)
	if len(f) > 0 && len(f[0]) > 0 && f[0][0] >= '0' && f[0][0] <= '9' {
		f = f[1:]
	}
	if len(f) == 0 {
		return eventsIgnore
	}
	switch f[0] {
	case "exists":
		if len(f) > 1 && f[1] == "-" {
			return eventsInitialDone
		}
	case "create", "change", "destroy", "call":
		return eventsChange
	}
	return eventsIgnore
}
