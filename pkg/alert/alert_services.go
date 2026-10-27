package alert

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/event"
)

// Managed services that are down or restarting.
//
// DRBD is not where an HA chain fails first. The replicas are UpToDate, a
// Primary exists, the mount and the service IP are up, and the one unit that
// does the work is not running: nothing is wrong that the resource checks can
// see. A gateway sat like that for five days (2026-10-22 to 2026-10-27) while
// its database was damaged. It was restarted a hundred times a day and had a
// listening socket on none of them, and no alert existed that read the unit.
//
// Two things are read, because the failure shows itself in two ways. A unit
// that is stopped, or failed, is simply not running, and is raised once it has
// stayed that way for a grace period. A unit that crashes at once on every
// start is not seen as stopped at all: systemd restarts it within seconds, so
// a poll finds it running about as often as not. Its restart counter is what
// shows it, so the counter is watched between polls.

// serviceTrack is what the monitor remembers about one unit between polls.
type serviceTrack struct {
	// node and restarts are the last reading. The counter is the unit
	// instance's own on that node, so a reading is only compared with an
	// earlier one from the same node.
	node     string
	restarts int
	known    bool
	// downSince is when the unit was first seen not running, zero while it runs.
	downSince time.Time
	// restartedAt has one entry per restart seen inside the window.
	restartedAt []time.Time
}

// serviceKey identifies a unit in serviceTracks. It does not include the node:
// the unit moves with the chain, and its history with it.
func serviceKey(info ServiceStatusInfo) string { return info.Resource + "|" + info.Unit }

// checkServices reads every HA chain's services and raises or clears one
// condition per unit.
func (m *Monitor) checkServices(ctx context.Context, sc *pollScope) {
	if m.services == nil {
		m.log.Debug("Monitor.checkServices: no service lister, service checks skipped")
		return
	}
	start := time.Now()
	infos, err := m.services.GetServiceStatusList(ctx)
	m.log.Debug("Monitor.checkServices: listed services",
		zap.Int("count", len(infos)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		// A listing that failed says nothing about any service. Reading it as
		// "all fine" would clear every alert on one SSH hiccup.
		m.log.Warn("alert monitor: list services failed", zap.Error(err))
		return
	}
	sc.servicesOK = true

	listed := make(map[string]bool, len(infos))
	for _, info := range infos {
		listed[serviceKey(info)] = true
		m.checkService(info, sc)
	}

	// Forget units that are no longer part of any chain; resolveVanished clears
	// their alert.
	m.mu.Lock()
	for key := range m.serviceTracks {
		if !listed[key] {
			m.log.Debug("Monitor.checkServices: unit no longer listed, forgetting it", zap.String("unit", key))
			delete(m.serviceTracks, key)
		}
	}
	m.mu.Unlock()
}

// checkService judges one unit.
func (m *Monitor) checkService(info ServiceStatusInfo, sc *pollScope) {
	tmpl := event.Event{
		Type:     event.TypeHAServiceFailing,
		Severity: event.SeverityCritical,
		Resource: info.Resource,
		Node:     info.Unit, // the unit is the subject; the host is in Details
	}
	if info.Node == "" {
		// The chain runs nowhere, so there is nothing to say about its service,
		// and above all nothing to clear: the Primary moving is when this alert
		// matters most, and clearing on every move would only be noise. Mark the
		// condition seen so it is not mistaken for one whose subject vanished.
		sc.mark(tmpl.Key())
		m.log.Debug("Monitor.checkService: chain runs on no node, condition left as it is",
			zap.String("resource", info.Resource), zap.String("unit", info.Unit))
		return
	}

	now := m.now()
	m.mu.Lock()
	t := m.serviceTracks[serviceKey(info)]
	if t == nil {
		t = &serviceTrack{}
		m.serviceTracks[serviceKey(info)] = t
	}
	if t.known && t.node == info.Node && info.Restarts > t.restarts {
		// Bounded: a counter that jumped by thousands (the monitor was off for
		// a day) must not become thousands of entries.
		for range min(info.Restarts-t.restarts, 4*m.serviceRestartCount) {
			t.restartedAt = append(t.restartedAt, now)
		}
	}
	t.node, t.restarts, t.known = info.Node, info.Restarts, true

	cutoff := now.Add(-m.serviceRestartWindow)
	kept := t.restartedAt[:0]
	for _, at := range t.restartedAt {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	t.restartedAt = kept

	if info.Active {
		t.downSince = time.Time{}
	} else if t.downSince.IsZero() {
		t.downSince = now
	}
	downFor := time.Duration(0)
	if !info.Active {
		downFor = now.Sub(t.downSince)
	}
	restarts := len(t.restartedAt)
	m.mu.Unlock()

	down := !info.Active && downFor >= m.serviceDownGrace
	looping := restarts >= m.serviceRestartCount
	m.log.Debug("Monitor.checkService: unit judged",
		zap.String("resource", info.Resource), zap.String("unit", info.Unit), zap.String("node", info.Node),
		zap.Bool("active", info.Active), zap.String("sub_state", info.SubState),
		zap.Duration("down_for", downFor), zap.Duration("grace", m.serviceDownGrace), zap.Bool("down", down),
		zap.Int("restarts_in_window", restarts), zap.Int("threshold", m.serviceRestartCount), zap.Bool("looping", looping))

	tmpl.Details = map[string]string{
		"unit":     info.Unit,
		"node":     info.Node,
		"state":    info.SubState,
		"restarts": strconv.Itoa(restarts),
	}
	m.level(tmpl, sc, sourceServices, down || looping,
		m.serviceFailingMessage(info, down, downFor, looping, restarts),
		fmt.Sprintf("service %s of resource %s is running steadily again on %s", serviceName(info), info.Resource, info.Node))
}

// serviceFailingMessage says what is wrong with the unit and where to look.
func (m *Monitor) serviceFailingMessage(info ServiceStatusInfo, down bool, downFor time.Duration, looping bool, restarts int) string {
	var reasons []string
	if down {
		reasons = append(reasons, fmt.Sprintf("has not been running for %s (state %s)",
			describeWindow(downFor.Round(time.Second)), stateOrUnknown(info.SubState)))
	}
	if looping {
		reasons = append(reasons, fmt.Sprintf("restarted %d times in the last %s",
			restarts, describeWindow(m.serviceRestartWindow)))
	}
	return fmt.Sprintf("service %s of resource %s %s on %s: DRBD and the Primary are fine and nothing is being served; "+
		"read the unit's journal on %s (journalctl -u %s) before failing over again, because if what it reads is damaged "+
		"the next node will fail the same way",
		serviceName(info), info.Resource, strings.Join(reasons, " and "), info.Node, info.Node, info.Unit)
}

// serviceName is how messages name the unit: what it is for, then its name.
func serviceName(info ServiceStatusInfo) string {
	if info.Label == "" {
		return info.Unit
	}
	return fmt.Sprintf("%s (%s)", info.Label, info.Unit)
}

func stateOrUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
