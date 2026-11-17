package alert

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haify-project/haify/pkg/event"
)

// A failover loop is the Primary moving again and again. A service that fails
// on every node because the data it reads is bad does exactly that, and the
// cluster dutifully reports each move as one more harmless warning.

// pairResource is a two-replica resource whose Primary is on node n1.
func pairResource(name string) *mockLister {
	return &mockLister{list: []ResourceStatusInfo{{
		Name:       name,
		NodeStates: map[string]NodeStateInfo{"n1": healthy("Primary"), "n2": healthy("Secondary")},
	}}}
}

// movePrimary makes to the Primary and the other replica the Secondary.
func movePrimary(l *mockLister, to string) {
	for node := range l.list[0].NodeStates {
		if node == to {
			l.list[0].NodeStates[node] = healthy("Primary")
		} else {
			l.list[0].NodeStates[node] = healthy("Secondary")
		}
	}
}

// loopHarness builds a monitor with an injected clock that has already seen
// the resource once, so the next move counts as a failover.
func loopHarness(t *testing.T, l *mockLister, window time.Duration) (*Monitor, func() []event.Event, *time.Time) {
	t.Helper()
	mon, drain := newHarness(t, Options{Resources: l, FailoverLoopCount: 3, FailoverLoopWindow: window})
	clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	mon.now = func() time.Time { return clock }
	mon.Poll(context.Background())
	drain()
	return mon, drain, &clock
}

// loopEvents keeps only the failover-loop events from a drained batch.
func loopEvents(evts []event.Event) []event.Event {
	var out []event.Event
	for _, e := range evts {
		if e.Type == event.TypeResourceFailoverLoop {
			out = append(out, e)
		}
	}
	return out
}

func TestRepeatedFailoversRaiseAFailoverLoop(t *testing.T) {
	l := pairResource("gw")
	mon, drain, clock := loopHarness(t, l, 10*time.Minute)
	ctx := context.Background()

	// Two moves are the cluster doing its job; they are not yet a pattern.
	for _, to := range []string{"n2", "n1"} {
		*clock = clock.Add(time.Minute)
		movePrimary(l, to)
		mon.Poll(ctx)
	}
	assert.Empty(t, loopEvents(drain()), "two failovers are not a loop")

	*clock = clock.Add(time.Minute)
	movePrimary(l, "n2")
	mon.Poll(ctx)

	loops := loopEvents(drain())
	require.Len(t, loops, 1)
	assert.Equal(t, event.SeverityCritical, loops[0].Severity)
	assert.Equal(t, event.StatusFiring, loops[0].Status)
	assert.Equal(t, "gw", loops[0].Resource)
	assert.Equal(t, "3", loops[0].Details["failovers"])
	assert.Equal(t, "n1,n2", loops[0].Details["nodes"])
	assert.Contains(t, loops[0].Message, "failed over 3 times",
		"the message has to say what was counted")
	assert.Contains(t, loops[0].Message, "another failover will not fix it",
		"the message has to say why moving again is not the answer")
}

func TestFailoverLoopResolvesWhenThePrimaryStopsMoving(t *testing.T) {
	l := pairResource("gw")
	mon, drain, clock := loopHarness(t, l, 10*time.Minute)
	ctx := context.Background()
	for _, to := range []string{"n2", "n1", "n2"} {
		*clock = clock.Add(time.Minute)
		movePrimary(l, to)
		mon.Poll(ctx)
	}
	raised := loopEvents(drain())
	require.Len(t, raised, 1)

	// Still inside the window: the moves are still being counted.
	*clock = clock.Add(5 * time.Minute)
	mon.Poll(ctx)
	assert.Empty(t, loopEvents(drain()))

	// Quiet for longer than the window: the loop is over.
	*clock = clock.Add(6 * time.Minute)
	mon.Poll(ctx)
	cleared := loopEvents(drain())
	require.Len(t, cleared, 1)
	assert.Equal(t, event.StatusResolved, cleared[0].Status)
	assert.Equal(t, event.SeverityInfo, cleared[0].Severity)
	assert.Equal(t, raised[0].Key(), cleared[0].Key(),
		"a receiver pairs firing with resolved by key, so the resolve must carry the same one")
}

func TestFailoversSpreadOverTimeAreNotALoop(t *testing.T) {
	l := pairResource("gw")
	mon, drain, clock := loopHarness(t, l, 10*time.Minute)
	ctx := context.Background()

	// Six failovers, six minutes apart: never more than two inside ten minutes.
	for _, to := range []string{"n2", "n1", "n2", "n1", "n2", "n1"} {
		*clock = clock.Add(6 * time.Minute)
		movePrimary(l, to)
		mon.Poll(ctx)
	}
	assert.Empty(t, loopEvents(drain()),
		"a node that fails once in a while is a node problem, not a loop")
}

func TestFailoverLoopIsAnnouncedOnceWhileItContinues(t *testing.T) {
	l := pairResource("gw")
	mon, drain, clock := loopHarness(t, l, 10*time.Minute)
	ctx := context.Background()

	for _, to := range []string{"n2", "n1", "n2", "n1", "n2", "n1"} {
		*clock = clock.Add(time.Minute)
		movePrimary(l, to)
		mon.Poll(ctx)
	}
	assert.Len(t, loopEvents(drain()), 1,
		"one condition, one firing event, however many more moves follow")
}

func TestExternallyManagedMovesAreNotCountedAsALoop(t *testing.T) {
	l := pairResource("pvc-1")
	l.list[0].IdleWithoutPrimary = true // Proxmox VE or Kubernetes decides where it runs
	mon, drain, clock := loopHarness(t, l, 10*time.Minute)
	ctx := context.Background()

	for _, to := range []string{"n2", "n1", "n2", "n1", "n2"} {
		*clock = clock.Add(time.Minute)
		movePrimary(l, to)
		mon.Poll(ctx)
	}
	assert.Empty(t, loopEvents(drain()),
		"live migration is a scheduler's everyday business, not a failure")
}

func TestLiveMovesAreNotFailovers(t *testing.T) {
	l := pairResource("web1")
	mon, drain, clock := loopHarness(t, l, 10*time.Minute)
	ctx := context.Background()
	step := func(primaries ...string) {
		for node := range l.list[0].NodeStates {
			l.list[0].NodeStates[node] = healthy("Secondary")
		}
		for _, node := range primaries {
			l.list[0].NodeStates[node] = healthy("Primary")
		}
		*clock = clock.Add(30 * time.Second)
		mon.Poll(ctx)
	}

	// `haify ha evict --live`: the target is promoted beside the source, then
	// one of them is demoted. Two that stayed and one that moved, as measured.
	l.list[0].LiveMove = true
	step("n1", "n2")
	step("n1")
	step("n1", "n2")
	step("n1")
	step("n1", "n2")
	for _, e := range drain() {
		assert.NotEqual(t, event.TypeResourceFailover, e.Type, "a live move is not a failover: %s", e.Message)
		assert.NotEqual(t, event.TypeResourceFailoverLoop, e.Type, "live moves are not a loop: %s", e.Message)
	}

	// The third move has ended on n2, and the monitor sees its last step only
	// now. A Primary that lands on n2 is still the move.
	l.list[0].LiveMoveEndedOn = "n2"
	step("n2")
	for _, e := range drain() {
		assert.NotEqual(t, event.TypeResourceFailover, e.Type, "the end of the move is not a failover: %s", e.Message)
	}

	// n2 loses power a minute after the move and n1 takes over: a failover,
	// even inside the window (measured 2026-11-17: reported as a move).
	step("n1")
	failovers := func() int {
		n := 0
		for _, e := range drain() {
			if e.Type == event.TypeResourceFailover {
				n++
			}
		}
		return n
	}
	assert.Equal(t, 1, failovers(), "a takeover elsewhere after a live move is a failover")

	// Once the move is forgotten, every move is a failover again.
	l.list[0].LiveMove, l.list[0].LiveMoveEndedOn = false, ""
	step("n2")
	assert.Equal(t, 1, failovers())
}

func TestFailoverLoopClearsWhenTheResourceIsDeleted(t *testing.T) {
	l := pairResource("gw")
	mon, drain, clock := loopHarness(t, l, 10*time.Minute)
	ctx := context.Background()
	for _, to := range []string{"n2", "n1", "n2"} {
		*clock = clock.Add(time.Minute)
		movePrimary(l, to)
		mon.Poll(ctx)
	}
	require.Len(t, loopEvents(drain()), 1)

	l.list = nil
	mon.Poll(ctx)
	cleared := loopEvents(drain())
	require.Len(t, cleared, 1, "an alert about a resource that no longer exists must not stay outstanding")
	assert.Equal(t, event.StatusResolved, cleared[0].Status)

	mon.mu.Lock()
	defer mon.mu.Unlock()
	assert.Empty(t, mon.failovers, "the deleted resource's history is forgotten")
}
