package alert

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haify-project/haify/pkg/event"
)

// A managed service that is down, or that restarts over and over, is the one
// failure DRBD says nothing about: every replica is UpToDate, a Primary exists,
// and nothing is serving. These tests pin when that is raised and, as much,
// when it is not.

type mockServiceLister struct {
	list []ServiceStatusInfo
	err  error
}

func (m *mockServiceLister) GetServiceStatusList(context.Context) ([]ServiceStatusInfo, error) {
	return m.list, m.err
}

// running is a healthy service on node, restarted restarts times since it
// started there.
func running(node string, restarts int) ServiceStatusInfo {
	return ServiceStatusInfo{Resource: "gw", Unit: "openclaw.service", Label: "OpenClaw Gateway",
		Node: node, Active: true, SubState: "running", Restarts: restarts}
}

// stopped is the same service not running.
func stopped(node, sub string, restarts int) ServiceStatusInfo {
	s := running(node, restarts)
	s.Active, s.SubState = false, sub
	return s
}

func serviceHarness(t *testing.T, l *mockServiceLister) (*Monitor, func() []event.Event, *time.Time) {
	t.Helper()
	mon, drain := newHarness(t, Options{
		Services:             l,
		ServiceDownGrace:     3 * time.Minute,
		ServiceRestartCount:  3,
		ServiceRestartWindow: 10 * time.Minute,
	})
	clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	mon.now = func() time.Time { return clock }
	return mon, drain, &clock
}

func serviceEvents(evts []event.Event) []event.Event {
	var out []event.Event
	for _, e := range evts {
		if e.Type == event.TypeHAServiceFailing {
			out = append(out, e)
		}
	}
	return out
}

func TestServiceDownLongerThanTheGraceIsRaised(t *testing.T) {
	l := &mockServiceLister{list: []ServiceStatusInfo{stopped("n1", "dead", 0)}}
	mon, drain, clock := serviceHarness(t, l)
	ctx := context.Background()

	mon.Poll(ctx)
	assert.Empty(t, serviceEvents(drain()), "a service that has only just been seen down may be starting")

	*clock = clock.Add(4 * time.Minute)
	mon.Poll(ctx)
	evts := serviceEvents(drain())
	require.Len(t, evts, 1)
	assert.Equal(t, event.SeverityCritical, evts[0].Severity)
	assert.Equal(t, event.StatusFiring, evts[0].Status)
	assert.Equal(t, "gw", evts[0].Resource)
	assert.Equal(t, "openclaw.service", evts[0].Node, "the condition is the unit, not the node it happens to run on")
	assert.Equal(t, "n1", evts[0].Details["node"])
	assert.Equal(t, "dead", evts[0].Details["state"])
	assert.Contains(t, evts[0].Message, "has not been running for")
}

// Starting takes real time: the OpenClaw gateway needs well over a minute
// before it listens. A service that is slow to come up is not a failure.
func TestServiceStartingWithinTheGraceIsNotAFailure(t *testing.T) {
	l := &mockServiceLister{list: []ServiceStatusInfo{stopped("n1", "start", 0)}}
	mon, drain, clock := serviceHarness(t, l)
	ctx := context.Background()

	mon.Poll(ctx)
	*clock = clock.Add(90 * time.Second)
	mon.Poll(ctx)
	l.list = []ServiceStatusInfo{running("n1", 0)}
	*clock = clock.Add(30 * time.Second)
	mon.Poll(ctx)
	assert.Empty(t, serviceEvents(drain()))
}

func TestServiceRecoveryResolvesUnderTheSameKey(t *testing.T) {
	l := &mockServiceLister{list: []ServiceStatusInfo{stopped("n1", "failed", 0)}}
	mon, drain, clock := serviceHarness(t, l)
	ctx := context.Background()
	mon.Poll(ctx)
	*clock = clock.Add(4 * time.Minute)
	mon.Poll(ctx)
	raised := serviceEvents(drain())
	require.Len(t, raised, 1)

	l.list = []ServiceStatusInfo{running("n1", 0)}
	*clock = clock.Add(time.Minute)
	mon.Poll(ctx)
	cleared := serviceEvents(drain())
	require.Len(t, cleared, 1)
	assert.Equal(t, event.StatusResolved, cleared[0].Status)
	assert.Equal(t, raised[0].Key(), cleared[0].Key())
}

// A crash loop is not seen as down: systemd restarts the unit within seconds,
// so a poll finds it running about as often as not. The restart counter is what
// shows it.
func TestRestartsInsideTheWindowRaiseACrashLoop(t *testing.T) {
	l := &mockServiceLister{list: []ServiceStatusInfo{running("n1", 0)}}
	mon, drain, clock := serviceHarness(t, l)
	ctx := context.Background()
	mon.Poll(ctx) // learns the baseline
	drain()

	*clock = clock.Add(time.Minute)
	l.list = []ServiceStatusInfo{running("n1", 2)}
	mon.Poll(ctx)
	assert.Empty(t, serviceEvents(drain()), "two restarts are below the threshold")

	*clock = clock.Add(time.Minute)
	l.list = []ServiceStatusInfo{running("n1", 5)}
	mon.Poll(ctx)
	evts := serviceEvents(drain())
	require.Len(t, evts, 1, "the service shows running, yet it restarted five times in two minutes")
	assert.Equal(t, event.SeverityCritical, evts[0].Severity)
	assert.Equal(t, "5", evts[0].Details["restarts"])
	assert.Contains(t, evts[0].Message, "restarted 5 times")
}

func TestCrashLoopEndsWhenTheRestartsStop(t *testing.T) {
	l := &mockServiceLister{list: []ServiceStatusInfo{running("n1", 0)}}
	mon, drain, clock := serviceHarness(t, l)
	ctx := context.Background()
	mon.Poll(ctx)
	l.list = []ServiceStatusInfo{running("n1", 6)}
	*clock = clock.Add(time.Minute)
	mon.Poll(ctx)
	require.Len(t, serviceEvents(drain()), 1)

	*clock = clock.Add(5 * time.Minute)
	mon.Poll(ctx)
	assert.Empty(t, serviceEvents(drain()), "still inside the window")

	*clock = clock.Add(6 * time.Minute)
	mon.Poll(ctx)
	cleared := serviceEvents(drain())
	require.Len(t, cleared, 1)
	assert.Equal(t, event.StatusResolved, cleared[0].Status)
}

// The counter belongs to the unit instance on one node. A failover starts the
// unit afresh on the next node, so a smaller number there is a different unit,
// and neither that nor an operator's manual restart is a crash loop.
func TestACounterThatMovesNodeOrDropsIsNotARestartBurst(t *testing.T) {
	l := &mockServiceLister{list: []ServiceStatusInfo{running("n1", 2)}}
	mon, drain, clock := serviceHarness(t, l)
	ctx := context.Background()
	mon.Poll(ctx)

	// Failed over. 9 is n2's own count and larger than n1's 2, which is the
	// case that matters: read as one counter it is seven restarts in a minute.
	*clock = clock.Add(time.Minute)
	l.list = []ServiceStatusInfo{running("n2", 9)}
	mon.Poll(ctx)
	*clock = clock.Add(time.Minute)
	l.list = []ServiceStatusInfo{running("n2", 0)} // stopped and started by hand
	mon.Poll(ctx)
	assert.Empty(t, serviceEvents(drain()))
}

// While no node runs the chain there is nothing to say about its service, and
// that must not clear what was said before: the Primary moving is exactly when
// this alert matters, and a clear-then-raise on every move would be noise.
func TestAChainRunningNowhereLeavesTheAlertAlone(t *testing.T) {
	l := &mockServiceLister{list: []ServiceStatusInfo{stopped("n1", "dead", 0)}}
	mon, drain, clock := serviceHarness(t, l)
	ctx := context.Background()
	mon.Poll(ctx)
	*clock = clock.Add(4 * time.Minute)
	mon.Poll(ctx)
	require.Len(t, serviceEvents(drain()), 1)

	l.list = []ServiceStatusInfo{{Resource: "gw", Unit: "openclaw.service", Node: ""}}
	*clock = clock.Add(time.Minute)
	mon.Poll(ctx)
	assert.Empty(t, serviceEvents(drain()), "no node runs the chain: nothing resolved, nothing raised again")

	l.list = []ServiceStatusInfo{stopped("n2", "dead", 0)}
	*clock = clock.Add(time.Minute)
	mon.Poll(ctx)
	assert.Empty(t, serviceEvents(drain()), "still the same condition, not announced a second time")
}

// A listing that failed says nothing about services. Reading it as "every
// service is fine" would clear every alert on one SSH hiccup.
func TestAFailedServiceListingKeepsTheAlertRaised(t *testing.T) {
	l := &mockServiceLister{list: []ServiceStatusInfo{stopped("n1", "dead", 0)}}
	mon, drain, clock := serviceHarness(t, l)
	ctx := context.Background()
	mon.Poll(ctx)
	*clock = clock.Add(4 * time.Minute)
	mon.Poll(ctx)
	require.Len(t, serviceEvents(drain()), 1)

	l.list, l.err = nil, errors.New("ssh: connection refused")
	*clock = clock.Add(time.Minute)
	mon.Poll(ctx)
	assert.Empty(t, serviceEvents(drain()))
}

func TestAServiceThatLeavesTheChainClearsItsAlert(t *testing.T) {
	l := &mockServiceLister{list: []ServiceStatusInfo{stopped("n1", "dead", 0)}}
	mon, drain, clock := serviceHarness(t, l)
	ctx := context.Background()
	mon.Poll(ctx)
	*clock = clock.Add(4 * time.Minute)
	mon.Poll(ctx)
	require.Len(t, serviceEvents(drain()), 1)

	l.list = nil // the HA config was removed: the unit is no longer part of any chain
	mon.Poll(ctx)
	cleared := serviceEvents(drain())
	require.Len(t, cleared, 1, "an alert about a service that is no longer managed must not stay outstanding")
	assert.Equal(t, event.StatusResolved, cleared[0].Status)

	mon.mu.Lock()
	defer mon.mu.Unlock()
	assert.Empty(t, mon.serviceTracks, "what was remembered about the removed unit is dropped")
}

func TestServicesAreNotCheckedWithoutALister(t *testing.T) {
	mon, drain := newHarness(t, Options{Resources: pairResource("gw")})
	mon.Poll(context.Background())
	assert.Empty(t, serviceEvents(drain()))
}
