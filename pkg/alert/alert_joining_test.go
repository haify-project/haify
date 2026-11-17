package alert

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haify-project/haify/pkg/event"
)

func joiningState() NodeStateInfo {
	return NodeStateInfo{DiskState: "Inconsistent", ReplicationState: "SyncSource", Role: "Secondary", Connection: "Connected", Joining: true}
}

func peer(disk string) NodeStateInfo {
	return NodeStateInfo{DiskState: disk, ReplicationState: "Established", Role: "Secondary", Connection: "Connected"}
}

// A replica added on top of the committed ones syncs without the resource
// being raised as degraded: a move, a rebalance, a raised replica count.
func TestJoiningReplicaIsNotDegraded(t *testing.T) {
	lister := &mockLister{list: []ResourceStatusInfo{{
		Name: "res1", CommittedReplicas: 2,
		NodeStates: map[string]NodeStateInfo{"n1": healthy("Primary"), "n2": peer("UpToDate"), "n3": joiningState()},
	}}}
	mon, drain := newHarness(t, Options{Resources: lister})
	ctx := context.Background()
	mon.Poll(ctx)
	assert.Empty(t, drain())

	// A committed replica goes away while the new one syncs: both are raised.
	lister.list[0].NodeStates["n2"] = NodeStateInfo{Connection: "Connecting"}
	mon.Poll(ctx)
	evts := drain()
	require.Len(t, evts, 2)
	nodes := map[string]bool{}
	for _, e := range evts {
		assert.Equal(t, event.TypeResourceDegraded, e.Type)
		assert.Equal(t, event.StatusFiring, e.Status)
		nodes[e.Node] = true
	}
	assert.True(t, nodes["n2"] && nodes["n3"], "the missing replica and the one still syncing")

	// It comes back: the joining replica is covered again and resolves.
	lister.list[0].NodeStates["n2"] = peer("UpToDate")
	mon.Poll(ctx)
	evts = drain()
	require.Len(t, evts, 2)
	for _, e := range evts {
		assert.Equal(t, event.StatusResolved, e.Status)
	}
}

// What the rule must never cover.
func TestJoiningSyncIsNarrow(t *testing.T) {
	res := func(committed int, others ...NodeStateInfo) ResourceStatusInfo {
		r := ResourceStatusInfo{Name: "res1", CommittedReplicas: committed, NodeStates: map[string]NodeStateInfo{}}
		for i, o := range others {
			r.NodeStates[string(rune('a'+i))] = o
		}
		return r
	}
	two := []NodeStateInfo{healthy("Primary"), peer("UpToDate")}
	assert.True(t, joiningSync(res(2, two...), joiningState()))

	plain := joiningState()
	plain.Joining = false
	assert.False(t, joiningSync(res(2, two...), plain), "a replica that fell behind")
	assert.False(t, joiningSync(res(3, two...), joiningState()), "a replacement for a lost replica: one short of the commitment")
	assert.False(t, joiningSync(res(0, two...), joiningState()), "no commitment on record")
	assert.False(t, joiningSync(res(2, healthy("Primary"), peer("Outdated")), joiningState()), "an Outdated replica is not a copy")
	assert.False(t, joiningSync(res(2, healthy("Primary"), NodeStateInfo{Connection: "StandAlone"}), joiningState()), "a disconnected one neither")
	tie := NodeStateInfo{DiskState: "Diskless", Connection: "Connected", ExpectedDiskless: true}
	assert.False(t, joiningSync(res(2, healthy("Primary"), tie), joiningState()), "a tiebreaker holds no data")
	// Another joining replica that happens to be UpToDate already is not counted either.
	other := peer("UpToDate")
	other.Joining = true
	assert.False(t, joiningSync(res(2, healthy("Primary"), other), joiningState()))

	for name, st := range map[string]NodeStateInfo{
		"disconnected":             {Connection: "Connecting", Joining: true},
		"Outdated":                 {DiskState: "Outdated", Connection: "Connected", Joining: true},
		"Failed":                   {DiskState: "Failed", Connection: "Connected", Joining: true},
		"Inconsistent, no sync":    {DiskState: "Inconsistent", ReplicationState: "Established", Connection: "Connected", Joining: true},
		"Inconsistent, standalone": {DiskState: "Inconsistent", ReplicationState: "StandAlone", Connection: "Connected", Joining: true},
	} {
		assert.False(t, joiningSync(res(2, two...), st), name)
	}
	paused := joiningState()
	paused.ReplicationState = "PausedSyncT"
	assert.True(t, joiningSync(res(2, two...), paused), "a sync paused behind another is still the first sync")
}
