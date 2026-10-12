package alert

import (
	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/event"
)

// A warning is raised only once its condition has outlasted the hold. A
// replica link that drops for ten seconds and reconnects on its own — a
// hypervisor's bridged network stalling, a peer's ping arriving late — used to
// produce a warning and a recovery each time, dozens a day for something
// nobody had to act on. Critical conditions are never held: a full pool or a
// lost Primary is reported on the poll that sees it.

// heldLongEnough reports whether a condition that is active now may be raised.
// Called with m.mu held.
func (m *Monitor) heldLongEnough(key string, severity event.Severity) bool {
	if m.hold <= 0 || severity != event.SeverityWarning {
		m.log.Debug("Monitor.heldLongEnough: no hold applies",
			zap.String("key", key), zap.String("severity", string(severity)), zap.Duration("hold", m.hold))
		return true
	}
	since, seen := m.pending[key]
	if !seen {
		m.log.Debug("Monitor.heldLongEnough: warning first seen, starting its hold",
			zap.String("key", key), zap.Duration("hold", m.hold))
		m.pending[key] = m.now()
		return false
	}
	if held := m.now().Sub(since); held < m.hold {
		m.log.Debug("Monitor.heldLongEnough: warning still within its hold",
			zap.String("key", key), zap.Duration("held", held), zap.Duration("hold", m.hold))
		return false
	}
	m.log.Debug("Monitor.heldLongEnough: warning outlasted its hold, may be raised", zap.String("key", key))
	delete(m.pending, key)
	return true
}

// holding reports whether any warning is waiting out its hold, which keeps the
// monitor on its short interval so the warning is raised close to the hold
// rather than at the next idle poll.
func (m *Monitor) holding() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending) > 0
}

// dropUnseenPending forgets held conditions this poll did not evaluate: their
// subject is gone, or its source did not answer and will be asked again.
func (m *Monitor) dropUnseenPending(sc *pollScope) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dropped := 0
	for key := range m.pending {
		if !sc.seen[key] {
			m.log.Debug("Monitor.dropUnseenPending: held condition not evaluated this poll, forgetting it", zap.String("key", key))
			delete(m.pending, key)
			dropped++
		}
	}
	m.log.Debug("Monitor.dropUnseenPending: pending holds reviewed", zap.Int("dropped", dropped), zap.Int("pending", len(m.pending)))
}

// firingConditions lists what is raised now.
func (m *Monitor) firingConditions() []FiringCondition {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]FiringCondition, 0, len(m.raised))
	for _, c := range m.raised {
		out = append(out, c)
	}
	return out
}
