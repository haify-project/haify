package alert

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/event"
)

// Failover loops: the Primary moving over and over.
//
// A failover is the cluster working as designed, and each one is reported as a
// warning. A service that fails on every node is a different thing that looks
// the same one move at a time. Say the data it reads is damaged: the service
// fails on the node that holds the Primary, the cluster moves the Primary to
// the next node as it should, the service fails there too, and the Primary
// moves back. That went on for hours in a measured case (2026-10-27, a gateway
// whose database was corrupt), more than twenty moves between two nodes, each
// a warning among the many other warnings, none of them a reason to look
// until somebody noticed the service was gone.
//
// The moves are not the fault and counting them does not find it. What it
// does is turn "a warning every few minutes" into the one statement that
// matters: moving again will not help, look at what the service reads.

// failoverMark is one failover as the loop detector remembers it.
type failoverMark struct {
	at       time.Time
	from, to string
}

// noteFailover records that resource's Primary just moved from one set of nodes
// to another.
func (m *Monitor) noteFailover(resource, from, to string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failovers[resource] = append(m.failovers[resource], failoverMark{at: m.now(), from: from, to: to})
	m.log.Debug("Monitor.noteFailover: failover recorded",
		zap.String("resource", resource), zap.String("from", from), zap.String("to", to),
		zap.Int("remembered", len(m.failovers[resource])))
}

// forgetFailovers drops what is remembered about a resource that no longer
// exists.
func (m *Monitor) forgetFailovers(resource string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.failovers, resource)
	m.log.Debug("Monitor.forgetFailovers: resource gone, failover history dropped", zap.String("resource", resource))
}

// recentFailovers returns the failovers of resource inside the loop window,
// dropping older ones from what is remembered.
func (m *Monitor) recentFailovers(resource string) []failoverMark {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := m.now().Add(-m.failoverLoopWindow)
	var recent []failoverMark
	for _, f := range m.failovers[resource] {
		if f.at.After(cutoff) {
			recent = append(recent, f)
		}
	}
	if len(recent) == 0 {
		delete(m.failovers, resource)
	} else {
		m.failovers[resource] = recent
	}
	return recent
}

// checkFailoverLoop raises resource.failover_loop while the resource's Primary
// has moved failoverLoopCount times or more within failoverLoopWindow, and
// resolves it when the moves fall out of the window.
//
// It is a level condition and not an edge: it stays raised while the loop
// goes on, so it is announced once rather than once per further move, and it
// ends on its own. Failovers of an externally managed resource are never
// recorded (see checkPrimary), so a Kubernetes pod or a Proxmox VE guest that
// is migrated back and forth does not count.
func (m *Monitor) checkFailoverLoop(res ResourceStatusInfo, sc *pollScope) {
	recent := m.recentFailovers(res.Name)
	active := len(recent) >= m.failoverLoopCount
	nodes := failoverNodes(recent)
	m.log.Debug("Monitor.checkFailoverLoop: counted recent failovers",
		zap.String("resource", res.Name), zap.Int("failovers", len(recent)),
		zap.Int("threshold", m.failoverLoopCount), zap.Duration("window", m.failoverLoopWindow),
		zap.Strings("nodes", nodes), zap.Bool("loop", active))

	m.level(event.Event{
		Type:     event.TypeResourceFailoverLoop,
		Severity: event.SeverityCritical,
		Resource: res.Name,
		Details: map[string]string{
			"failovers": strconv.Itoa(len(recent)),
			"window":    describeWindow(m.failoverLoopWindow),
			"nodes":     strings.Join(nodes, ","),
		},
	}, sc, sourceResources, active,
		fmt.Sprintf("resource %s failed over %d times in the last %s (the Primary moved between %s): "+
			"a service that fails on every node keeps moving for as long as the fault lasts, so another failover will not fix it; "+
			"look at the managed service and the data it reads (`haify ha status %s`, then the service's journal on the node that is Primary)",
			res.Title(), len(recent), describeWindow(m.failoverLoopWindow), strings.Join(nodes, ", "), res.Name),
		fmt.Sprintf("resource %s: its Primary has stopped moving (fewer than %d failovers in the last %s)",
			res.Title(), m.failoverLoopCount, describeWindow(m.failoverLoopWindow)))
}

// failoverNodes lists every node the Primary moved from or to, sorted and
// without repeats, so the message names the nodes a loop is bouncing between.
func failoverNodes(marks []failoverMark) []string {
	seen := map[string]bool{}
	for _, f := range marks {
		for _, set := range []string{f.from, f.to} {
			for node := range strings.SplitSeq(set, ",") {
				if node != "" {
					seen[node] = true
				}
			}
		}
	}
	nodes := make([]string, 0, len(seen))
	for node := range seen {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	return nodes
}

// describeWindow renders the loop window the way an operator would write it:
// "15m" for a whole number of minutes, the plain duration otherwise.
func describeWindow(d time.Duration) string {
	if d > 0 && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}
