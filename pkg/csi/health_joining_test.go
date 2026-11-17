package csi

import (
	"context"
	"testing"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// joiningResource is a volume with replicas on n1 and n2 and a third being
// added on n3, as the Primary n1 reports it.
func joiningResource(committed uint32, n3 *haifypb.NodeResourceState, mut func(map[string]*haifypb.NodeResourceState)) *haifypb.ResourceInfo {
	states := map[string]*haifypb.NodeResourceState{
		"n1": {Role: "Primary", DiskState: "UpToDate", Node: "n1"},
		"n2": {DiskState: "UpToDate", Connection: "Connected", Node: "n2"},
		"n3": n3,
	}
	if mut != nil {
		mut(states)
	}
	r := resourceWith(states)
	r.CommittedReplicas = committed
	if n3 != nil && n3.GetJoining() {
		r.JoiningNodes = []string{"n3"}
	}
	return r
}

func syncing(joining bool) *haifypb.NodeResourceState {
	return &haifypb.NodeResourceState{DiskState: "Inconsistent", ReplicationState: "SyncSource", SyncPercent: 40,
		Connection: "Connected", Node: "n3", Joining: joining}
}

// A replica being added on top of the committed ones is not a degraded
// volume: a replica moving to its user, a rebalance, a raised replica count.
func TestHealthLeavesAJoiningReplicaAlone(t *testing.T) {
	abnormal, msg, _ := volumeHealthDetail(joiningResource(2, syncing(true), nil))
	assert.False(t, abnormal)
	assert.Contains(t, msg, "a replica being added is syncing")
	assert.Contains(t, msg, "n3: resyncing (40.0%)")
}

// Every way the rule could hide a real loss of redundancy is still reported.
func TestHealthReportsAJoiningReplicaWhenRedundancyIsShort(t *testing.T) {
	for name, r := range map[string]*haifypb.ResourceInfo{
		// The same sync without the controller's word that it is a new copy.
		"not marked joining": joiningResource(2, syncing(false), nil),
		// A replacement for a lost replica: committed to three, two there.
		"fewer UpToDate than committed": joiningResource(3, syncing(true), nil),
		// A controller that does not say what the volume is committed to.
		"no committed count": joiningResource(0, syncing(true), nil),
		"another replica disconnected": joiningResource(1, syncing(true), func(s map[string]*haifypb.NodeResourceState) {
			s["n2"] = &haifypb.NodeResourceState{Connection: "Connecting", Node: "n2"}
		}),
		"another replica Outdated": joiningResource(1, syncing(true), func(s map[string]*haifypb.NodeResourceState) {
			s["n2"] = &haifypb.NodeResourceState{DiskState: "Outdated", Connection: "Connected", Node: "n2"}
		}),
		"another replica lost its disk": joiningResource(1, syncing(true), func(s map[string]*haifypb.NodeResourceState) {
			s["n2"] = &haifypb.NodeResourceState{DiskState: "Diskless", Connection: "Connected", Node: "n2"}
		}),
		"another replica resyncing": joiningResource(1, syncing(true), func(s map[string]*haifypb.NodeResourceState) {
			s["n2"] = &haifypb.NodeResourceState{DiskState: "Inconsistent", ReplicationState: "SyncSource", Connection: "Connected", Node: "n2"}
		}),
		"the joining replica disconnected": joiningResource(2,
			&haifypb.NodeResourceState{Connection: "StandAlone", Node: "n3", Joining: true}, nil),
		"the joining replica Inconsistent and not syncing": joiningResource(2,
			&haifypb.NodeResourceState{DiskState: "Inconsistent", ReplicationState: "Established", Connection: "Connected", Node: "n3", Joining: true}, nil),
		"the joining replica Outdated": joiningResource(2,
			&haifypb.NodeResourceState{DiskState: "Outdated", Connection: "Connected", Node: "n3", Joining: true}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			abnormal, msg, _ := volumeHealthDetail(r)
			assert.True(t, abnormal, msg)
			assert.Contains(t, msg, "n3", "the joining replica is named with the rest")
		})
	}

	// A tiebreaker or a diskless client is not a copy and does not count
	// towards the committed ones.
	r := joiningResource(2, syncing(true), func(s map[string]*haifypb.NodeResourceState) {
		s["n2"] = &haifypb.NodeResourceState{DiskState: "Diskless", Connection: "Connected", Node: "n2"}
	})
	r.DisklessNodes = []string{"n2"}
	abnormal, _, _ := volumeHealthDetail(r)
	assert.True(t, abnormal)
}

// On the PVC: no VolumeDegraded while a replica joins an established volume,
// and one as soon as the same sync is no longer covered.
func TestReporterStaysQuietWhileAReplicaJoins(t *testing.T) {
	h := newHealthHarness(t)
	ctx := context.Background()
	h.setStates(healthy)
	h.rep.Sweep(ctx)
	require.Empty(t, h.drain())

	joining := joiningResource(2, syncing(true), nil)
	h.b.resources["pvc_x"].NodeStates = joining.NodeStates
	h.b.resources["pvc_x"].CommittedReplicas = 2
	h.rep.Sweep(ctx)
	h.rep.Sweep(ctx)
	assert.Empty(t, h.drain(), "a replica being added to an established volume is not a degraded volume")

	// A committed replica drops out while the new one is still syncing.
	short := joiningResource(2, syncing(true), func(s map[string]*haifypb.NodeResourceState) {
		s["n2"] = &haifypb.NodeResourceState{Connection: "Connecting", Node: "n2"}
	})
	h.b.resources["pvc_x"].NodeStates = short.NodeStates
	h.rep.Sweep(ctx)
	evts := h.drain()
	require.Len(t, evts, 1)
	assert.Contains(t, evts[0], ReasonVolumeDegraded)
	assert.Contains(t, evts[0], "n2: not connected")
	assert.Contains(t, evts[0], "n3: resyncing")

	// Back, and the new replica finished: one recovery.
	done := joiningResource(0, &haifypb.NodeResourceState{DiskState: "UpToDate", Connection: "Connected", Node: "n3"}, nil)
	h.b.resources["pvc_x"].NodeStates = done.NodeStates
	h.b.resources["pvc_x"].CommittedReplicas = 0
	h.rep.Sweep(ctx)
	evts = h.drain()
	require.Len(t, evts, 1)
	assert.Contains(t, evts[0], ReasonVolumeRecovered)
}
