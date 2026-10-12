package alert

import (
	"context"
	"strings"
	"time"

	"go.uber.org/zap"
)

// The polling loop: when a poll runs, how often, and what one poll does.

// Start begins background polling until ctx is cancelled or Stop is called.
//
// The first poll runs immediately rather than after one interval: a controller
// starting up into an already-degraded cluster should say so now, not in thirty
// seconds.
func (m *Monitor) Start(ctx context.Context) {
	m.log.Debug("Monitor.Start: starting background polling",
		zap.Duration("interval", m.interval), zap.Duration("idle_interval", m.idleInterval))
	go func() {
		m.log.Debug("Monitor.Start: running the first poll immediately")
		m.Poll(ctx)
		next := m.nextInterval()
		m.log.Debug("Monitor.Start: next poll scheduled", zap.Duration("in", next))
		timer := time.NewTimer(next)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				m.log.Debug("Monitor.Start: context cancelled, polling stops")
				return
			case <-m.stop:
				m.log.Debug("Monitor.Start: stopped, polling stops")
				return
			case <-timer.C:
				m.log.Debug("Monitor.Start: tick, interval elapsed")
			case <-m.kick:
				m.log.Debug("Monitor.Start: kicked, settling before polling", zap.Duration("settle", kickSettle))
				select {
				case <-ctx.Done():
					m.log.Debug("Monitor.Start: context cancelled while settling, polling stops")
					return
				case <-m.stop:
					m.log.Debug("Monitor.Start: stopped while settling, polling stops")
					return
				case <-time.After(kickSettle):
				}
				select { // the kicks that came in while settling are this poll's
				case <-m.kick:
					m.log.Debug("Monitor.Start: absorbed kicks that arrived while settling")
				default:
				}
			}
			m.Poll(ctx)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			next = m.nextInterval()
			m.log.Debug("Monitor.Start: next poll scheduled", zap.Duration("in", next))
			timer.Reset(next)
		}
	}()
}

// Kick asks for a poll soon, because something changed. Never blocks.
func (m *Monitor) Kick() {
	select {
	case m.kick <- struct{}{}:
		m.log.Debug("Monitor.Kick: poll requested")
	default:
		m.log.Debug("Monitor.Kick: a poll is already pending, kick collapsed")
	}
}

// SetEventDriven says whether state changes are reaching Kick from every
// node. While they are, a healthy cluster is polled on the idle interval.
func (m *Monitor) SetEventDriven(on bool) {
	prev := m.eventDriven.Swap(on)
	m.log.Debug("Monitor.SetEventDriven: event-driven mode set", zap.Bool("event_driven", on), zap.Bool("was", prev))
	if prev != on && !on {
		m.log.Debug("Monitor.SetEventDriven: state changes no longer arriving, kicking a poll")
		m.Kick() // a node's changes are no longer arriving: look now
	}
}

func (m *Monitor) nextInterval() time.Duration {
	eventDriven, steady, holding := m.eventDriven.Load(), m.steady.Load(), m.holding()
	if eventDriven && steady && !holding {
		m.log.Debug("Monitor.nextInterval: event-driven and steady, using idle interval",
			zap.Duration("interval", m.idleInterval))
		return m.idleInterval
	}
	m.log.Debug("Monitor.nextInterval: using poll interval",
		zap.Duration("interval", m.interval), zap.Bool("event_driven", eventDriven),
		zap.Bool("steady", steady), zap.Bool("holding", holding))
	return m.interval
}

// Stop halts background polling. Safe to call more than once, and safe on a
// monitor that was never started.
func (m *Monitor) Stop() {
	m.stopOnce.Do(func() {
		m.log.Debug("Monitor.Stop: stopping background polling")
		close(m.stop)
	})
}

// Polls reports how many poll cycles have completed.
func (m *Monitor) Polls() int {
	m.pollsMu.Lock()
	defer m.pollsMu.Unlock()
	return m.polls
}

// Poll runs one health check cycle. Exported so a caller can force a check and
// so tests need not wait on a ticker.
func (m *Monitor) Poll(ctx context.Context) {
	// scope records every condition this cycle actually evaluated, so a firing
	// alert whose subject has since vanished can be cleared. See resolveVanished.
	start := time.Now()
	m.log.Debug("Monitor.Poll: poll starting")
	sc := &pollScope{seen: map[string]bool{}, live: map[string]bool{}, livePools: map[string]bool{}}
	var obs Observation
	m.checkResources(ctx, sc, &obs)
	m.checkNodes(ctx, sc, &obs)
	m.checkPools(ctx, sc, &obs)
	m.resolveVanished(sc)
	m.dropUnseenPending(sc)
	obs.Firing = m.firingConditions()
	steady := steadyState(obs)
	m.steady.Store(steady)

	// After the events, not before: an observer that panics or blocks must not
	// be able to stop a degradation from being reported.
	if m.observer != nil {
		obsStart := time.Now()
		m.observer.Observed(obs)
		m.log.Debug("Monitor.Poll: observer handed the observation", zap.Duration("took", time.Since(obsStart)))
	}

	m.pollsMu.Lock()
	m.polls++
	polls := m.polls
	m.pollsMu.Unlock()
	m.log.Debug("Monitor.Poll: poll finished",
		zap.Int("poll", polls), zap.Duration("took", time.Since(start)),
		zap.Bool("resources_ok", sc.resourcesOK), zap.Int("resources", len(obs.Resources.Items)),
		zap.Bool("nodes_ok", sc.nodesOK), zap.Int("nodes", len(obs.Nodes.Items)),
		zap.Bool("pools_ok", sc.poolsOK), zap.Int("pools", len(obs.Pools.Items)),
		zap.Int("conditions_seen", len(sc.seen)), zap.Int("firing", len(obs.Firing)),
		zap.Bool("steady", steady))
}

// steadyState is whether a poll found nothing that changes without DRBD saying
// so. A resync or a verify advances silently — events2 reports the start and
// the end, not the progress — so while one runs the monitor keeps polling.
// A replica stuck degraded is not in that set: an Outdated disk or a peer
// that stays away changes only by a state change, which is an event, and
// counting it would keep a cluster with one long-standing fault on the
// short interval for good.
func steadyState(obs Observation) bool {
	if !obs.Resources.OK {
		zap.L().Debug("steadyState: resources not listed, not steady")
		return false
	}
	for _, r := range obs.Resources.Items {
		for node, st := range r.NodeStates {
			if st.SyncPercent != nil && *st.SyncPercent < 100 {
				zap.L().Debug("steadyState: replica resyncing, not steady",
					zap.String("resource", r.Name), zap.String("node", node), zap.Float64("sync_percent", *st.SyncPercent))
				return false
			}
			switch {
			case strings.HasPrefix(st.ReplicationState, "Sync"),
				strings.HasPrefix(st.ReplicationState, "PausedSync"),
				strings.HasPrefix(st.ReplicationState, "Verify"):
				zap.L().Debug("steadyState: replica syncing or verifying, not steady",
					zap.String("resource", r.Name), zap.String("node", node), zap.String("repl_state", st.ReplicationState))
				return false
			}
		}
	}
	return true
}
