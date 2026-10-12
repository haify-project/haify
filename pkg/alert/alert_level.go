package alert

import (
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/event"
)

// Condition bookkeeping: which level conditions are raised, raising and
// clearing them, and publishing what changed.

// pollScope is what one poll observed: the condition keys it evaluated, the
// resources it saw, and whether each source answered at all.
type pollScope struct {
	seen map[string]bool
	live map[string]bool
	// livePools holds the pools seen this poll, keyed "<pool>@<node>", which is
	// what makes a deleted pool distinguishable from a pool on a node that
	// dropped out of the listing.
	livePools map[string]bool
	// resourcesOK / nodesOK / poolsOK are false when that source could not be
	// listed. A source that failed reports nothing, which must never be
	// mistaken for "every condition it owns has cleared".
	resourcesOK bool
	nodesOK     bool
	poolsOK     bool
}

func (s *pollScope) mark(key string) {
	if s != nil {
		s.seen[key] = true
	}
}

// resolveVanished clears alerts whose subject no longer exists.
//
// A level condition only resolves when something evaluates it and finds it
// false. If the thing it describes disappears, nothing evaluates it again and
// the alert stays raised forever. Three ways that happens, all reachable from
// normal operations:
//
//   - the resource is deleted;
//   - a replica is removed from a resource that still exists (remove-replica) —
//     and a replica being degraded is a common reason to remove it, so the
//     stuck alert lands on exactly the workflow that provokes it;
//   - a node is unregistered while unreachable.
//
// Keying this off "the resource is gone" only covered the first. Keying it off
// "nothing evaluated this condition" covers all three, and needs no separate
// knowledge of what removal looks like.
//
// The safety condition is that silence must come from a source that actually
// answered. A failed list produces no keys at all, which would otherwise read
// as every condition it owns having cleared — resolving every alert in the
// cluster on a transient database error.
func (m *Monitor) resolveVanished(sc *pollScope) {
	type vanished struct{ key, message string }

	m.mu.Lock()
	m.log.Debug("Monitor.resolveVanished: looking for conditions whose subject vanished",
		zap.Int("firing", len(m.firing)), zap.Int("seen", len(sc.seen)),
		zap.Bool("resources_ok", sc.resourcesOK), zap.Bool("nodes_ok", sc.nodesOK), zap.Bool("pools_ok", sc.poolsOK))
	var stale []vanished
	for key, on := range m.firing {
		if !on || sc.seen[key] {
			continue
		}
		// Key layout is "<type>|<resource>|<node>".
		parts := strings.Split(key, "|")
		if len(parts) != 3 {
			m.log.Debug("Monitor.resolveVanished: malformed condition key, skipped", zap.String("key", key))
			continue
		}
		resource, node := parts[1], parts[2]

		// Only prune from a source that reported. Which source owns a condition
		// is recorded when it starts firing rather than guessed from the key:
		// pool conditions carry both a name and a node, so the old "empty
		// resource means node" rule would have misattributed every one of them.
		var msg string
		switch m.owners[key] {
		case sourceNodes:
			if !sc.nodesOK {
				m.log.Debug("Monitor.resolveVanished: node source did not answer, keeping condition", zap.String("key", key))
				continue
			}
			msg = fmt.Sprintf("node %s is no longer registered; clearing its outstanding alert", node)
		case sourcePools:
			if !sc.poolsOK {
				m.log.Debug("Monitor.resolveVanished: pool source did not answer, keeping condition", zap.String("key", key))
				continue
			}
			if sc.livePools[poolKey(resource, node)] {
				msg = fmt.Sprintf("pool %s on %s no longer holds a thin pool; clearing its outstanding alert", resource, node)
			} else {
				msg = fmt.Sprintf("pool %s on %s no longer exists; clearing its outstanding alert", resource, node)
			}
		default:
			if !sc.resourcesOK {
				m.log.Debug("Monitor.resolveVanished: resource source did not answer, keeping condition", zap.String("key", key))
				continue
			}
			switch {
			case !sc.live[resource]:
				msg = fmt.Sprintf("resource %s no longer exists; clearing its outstanding alert", resource)
			default:
				msg = fmt.Sprintf("%s is no longer part of resource %s; clearing its outstanding alert", node, resource)
			}
		}
		m.log.Debug("Monitor.resolveVanished: condition's subject vanished, clearing",
			zap.String("key", key), zap.String("resource", resource), zap.String("node", node))
		stale = append(stale, vanished{key: key, message: msg})
	}
	for _, v := range stale {
		delete(m.firing, v.key)
		delete(m.owners, v.key)
		delete(m.raised, v.key)
	}
	m.mu.Unlock()
	m.log.Debug("Monitor.resolveVanished: vanished conditions cleared", zap.Int("count", len(stale)))

	sort.Slice(stale, func(i, j int) bool { return stale[i].key < stale[j].key })
	for _, v := range stale {
		parts := strings.Split(v.key, "|")
		m.publish(event.Event{
			Type:     event.Type(parts[0]),
			Severity: event.SeverityInfo,
			Status:   event.StatusResolved,
			Resource: parts[1],
			Node:     parts[2],
			Message:  v.message,
		})
	}
}

// level reports a condition that is either true or false at each poll, emitting
// exactly one firing event when it starts and one resolved event when it ends.
// The severity on tmpl applies to the firing event; a recovery is informational.
//
// src names the lister that owns the condition, and is recorded for as long as
// it fires so that resolveVanished can tell "the subject is gone" from "the
// source that would have reported it did not answer".
func (m *Monitor) level(tmpl event.Event, sc *pollScope, src source, active bool, firingMsg, resolvedMsg string) {
	key := tmpl.Key()
	sc.mark(key)

	m.mu.Lock()
	was := m.firing[key]
	if !active {
		delete(m.pending, key)
	}
	if active && !was && !m.heldLongEnough(key, tmpl.Severity) {
		m.mu.Unlock()
		m.log.Debug("Monitor.level: condition active but still within its hold, not raised yet",
			zap.String("key", key), zap.String("severity", string(tmpl.Severity)), zap.Duration("hold", m.hold))
		return
	}
	switch {
	case active && !was:
		m.firing[key] = true
		m.owners[key] = src
		m.raised[key] = FiringCondition{Type: string(tmpl.Type), Severity: string(tmpl.Severity)}
		m.log.Debug("Monitor.level: condition started, raising",
			zap.String("key", key), zap.String("severity", string(tmpl.Severity)), zap.Int("source", int(src)))
	case !active && was:
		delete(m.firing, key)
		delete(m.owners, key)
		delete(m.raised, key)
		m.log.Debug("Monitor.level: condition cleared, resolving", zap.String("key", key))
	default:
		m.mu.Unlock()
		m.log.Debug("Monitor.level: condition unchanged", zap.String("key", key), zap.Bool("active", active))
		return
	}
	m.mu.Unlock()

	if active {
		tmpl.Status = event.StatusFiring
		tmpl.Message = firingMsg
	} else {
		tmpl.Status = event.StatusResolved
		tmpl.Severity = event.SeverityInfo
		tmpl.Message = resolvedMsg
	}
	m.publish(tmpl)
}

func (m *Monitor) publish(e event.Event) {
	if m.bus == nil {
		m.log.Debug("Monitor.publish: no bus, event dropped",
			zap.String("type", string(e.Type)), zap.String("status", string(e.Status)),
			zap.String("resource", e.Resource), zap.String("node", e.Node))
		return
	}
	m.log.Debug("Monitor.publish: publishing event",
		zap.String("type", string(e.Type)), zap.String("severity", string(e.Severity)),
		zap.String("status", string(e.Status)), zap.String("resource", e.Resource), zap.String("node", e.Node))
	published := m.bus.Publish(e)
	m.log.Info("alert",
		zap.Uint64("id", published.ID),
		zap.String("type", string(published.Type)),
		zap.String("severity", string(published.Severity)),
		zap.String("status", string(published.Status)),
		zap.String("message", published.Message))
}
