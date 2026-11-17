package csi

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// zonedBackend is a cluster of the named nodes, each in the zone given for it
// ("" for a node with no zone), every node hosting the test pool.
func zonedBackend(zones map[string]string, names ...string) *fakeBackend {
	b := newFakeBackend(names...)
	for _, n := range b.nodes {
		if z := zones[n.GetName()]; z != "" {
			n.Labels = map[string]string{TopologyKeyZone: z, HaifyZoneLabel: z}
		}
	}
	return b
}

func zoneTopology(zone string) *csi.Topology {
	return &csi.Topology{Segments: map[string]string{TopologyKeyZone: zone}}
}

func TestZoneTermsIgnoreTopologiesThatNameANode(t *testing.T) {
	terms := zoneTerms(&csi.TopologyRequirement{
		Preferred: []*csi.Topology{zoneTopology("b")},
		Requisite: []*csi.Topology{
			{Segments: map[string]string{TopologyKeyNode: "n1", TopologyKeyZone: "a"}},
			zoneTopology("a"), zoneTopology("b"),
		},
	})
	require.Len(t, terms, 2)
	assert.Equal(t, "b", terms[0][TopologyKeyZone], "preferred first")
	assert.Equal(t, "a", terms[1][TopologyKeyZone])
	assert.Nil(t, zoneTerms(nil))
}

// The node plugins publish the zone beside the node, so under
// --strict-topology the requisite topology is the scheduler's node with its
// zone. That must stay a pin on the node and nothing more: the second replica
// goes wherever the fault domain and free space say, another zone included.
func TestCreateVolumeWithNodeAndZoneSegmentsPinsTheNodeOnly(t *testing.T) {
	b := zonedBackend(map[string]string{"n1": "a", "n2": "a", "n3": "b"}, "n1", "n2", "n3")
	req := validCreateReq("pvc-strict")
	req.Parameters["faultDomainLabel"] = "zone"
	req.AccessibilityRequirements = &csi.TopologyRequirement{
		Requisite: []*csi.Topology{{Segments: map[string]string{TopologyKeyNode: "n2", TopologyKeyZone: "a"}}},
		Preferred: []*csi.Topology{{Segments: map[string]string{TopologyKeyNode: "n2", TopologyKeyZone: "a"}}},
	}
	resp, err := newTestController(b).CreateVolume(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, []string{"n2", "n3"}, b.createCalls[0].nodes, "scheduler's node, then the other zone")
	for _, top := range resp.Volume.AccessibleTopology {
		assert.Len(t, top.Segments, 1, "the volume's topology names nodes, never zones")
		assert.NotEmpty(t, top.Segments[TopologyKeyNode])
	}
}

func TestCreateVolumeSeatsAReplicaInARequestedZone(t *testing.T) {
	b := zonedBackend(map[string]string{"n1": "a", "n2": "a", "n3": "b", "n4": "c"}, "n1", "n2", "n3", "n4")
	// n4, outside the requested zones, is by far the emptiest.
	b.pools = []*haifypb.PoolInfo{thinPool("10.0.0.1", 20, 50), thinPool("10.0.0.2", 20, 60), thinPool("10.0.0.3", 20, 70),
		thinPool("10.0.0.4", 20, 0)}
	req := validCreateReq("pvc-zone")
	req.AccessibilityRequirements = &csi.TopologyRequirement{Requisite: []*csi.Topology{zoneTopology("a"), zoneTopology("b")}}
	_, err := newTestController(b).CreateVolume(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, []string{"n1", "n2"}, b.createCalls[0].nodes, "inside the zones, most free space first, before n4 outside")
}

func TestCreateVolumeSpillsOutsideTheZonesOnlyWhenTheyAreFull(t *testing.T) {
	b := zonedBackend(map[string]string{"n1": "a", "n2": "b", "n3": "b"}, "n1", "n2", "n3")
	req := validCreateReq("pvc-spill")
	req.AccessibilityRequirements = &csi.TopologyRequirement{Requisite: []*csi.Topology{zoneTopology("a")}}
	_, err := newTestController(b).CreateVolume(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, b.createCalls[0].nodes, 2)
	assert.Equal(t, "n1", b.createCalls[0].nodes[0], "one replica in the requested zone")
}

func TestCreateVolumeFailsWhenNoNodeOfTheZoneHasThePool(t *testing.T) {
	b := zonedBackend(map[string]string{"n1": "a", "n2": "a", "n3": "b"}, "n1", "n2", "n3")
	b.onlyPoolOnNodes("n1", "n2")
	req := validCreateReq("pvc-nozone")
	req.AccessibilityRequirements = &csi.TopologyRequirement{Requisite: []*csi.Topology{zoneTopology("b")}}
	_, err := newTestController(b).CreateVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err), "the code external-provisioner reschedules on")
	assert.Empty(t, b.createCalls)

	// A remote-access volume is usable in the zone without a replica there.
	req.Parameters[paramAllowRemoteVolumeAccess] = "true"
	_, err = newTestController(b).CreateVolume(context.Background(), req)
	require.NoError(t, err)
}

func TestPlaceInZonesWithoutTermsChangesNothing(t *testing.T) {
	cands := []replicaCandidate{{node: "n1"}, {node: "n2"}}
	out, pinned, err := placeInZones(cands, nil, nil, nil, 2, 0, true)
	require.NoError(t, err)
	assert.Equal(t, cands, out, "no terms: candidates untouched")
	assert.Nil(t, pinned)
}

func TestPlaceInZonesAcceptsAPinnedNodeInside(t *testing.T) {
	nodes := zonedBackend(map[string]string{"n1": "a", "n2": "b"}, "n1", "n2").nodes
	cands := []replicaCandidate{{node: "n1"}, {node: "n2"}}
	_, pinned, err := placeInZones(cands, []string{"n1"}, []map[string]string{{TopologyKeyZone: "a"}}, nodes, 1, 0, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"n1"}, pinned)

	// A clone whose only replica must sit with its source, outside the zone.
	_, _, err = placeInZones(cands, []string{"n2"}, []map[string]string{{TopologyKeyZone: "a"}}, nodes, 1, 0, true)
	require.Error(t, err)
}

func TestCapacityOfAZone(t *testing.T) {
	b := zonedBackend(map[string]string{"n1": "a", "n2": "a", "n3": "b"}, "n1", "n2", "n3")
	b.pools = []*haifypb.PoolInfo{thinPool("10.0.0.1", 20, 50), thinPool("10.0.0.2", 20, 75), thinPool("10.0.0.3", 20, 90)}
	get := func(params map[string]string, segs map[string]string) (*csi.GetCapacityResponse, error) {
		return newTestController(b).GetCapacity(context.Background(), &csi.GetCapacityRequest{
			Parameters: params, AccessibleTopology: &csi.Topology{Segments: segs}})
	}
	resp, err := get(map[string]string{"pool": "vg0", "replicas": "2"}, map[string]string{TopologyKeyZone: "a"})
	require.NoError(t, err)
	assert.Equal(t, int64(15*giB), resp.AvailableCapacity, "n1 and n2")
	assert.Equal(t, int64(5*giB), resp.MaximumVolumeSize.GetValue(), "the second replica's node bounds it")

	resp, err = get(map[string]string{"pool": "vg0", "replicas": "1"}, map[string]string{TopologyKeyZone: "a"})
	require.NoError(t, err)
	assert.Equal(t, int64(10*giB), resp.MaximumVolumeSize.GetValue(), "the zone's roomiest node")

	// What external-provisioner asks once nodes publish zones: node and zone.
	resp, err = get(map[string]string{"pool": "vg0"}, map[string]string{TopologyKeyNode: "n3", TopologyKeyZone: "b"})
	require.NoError(t, err)
	assert.Equal(t, int64(2*giB), resp.AvailableCapacity, "the node's own figure")

	resp, err = get(map[string]string{"pool": "vg0"}, map[string]string{TopologyKeyZone: "nowhere"})
	require.NoError(t, err)
	assert.Zero(t, resp.AvailableCapacity)

	resp, err = get(map[string]string{"pool": "vg0", paramAllowRemoteVolumeAccess: "true"}, map[string]string{TopologyKeyZone: "nowhere"})
	require.NoError(t, err)
	assert.Equal(t, int64(17*giB), resp.AvailableCapacity, "remote access: the pool as a whole")
}

func TestTopologyLabelChanges(t *testing.T) {
	for _, tc := range []struct {
		name      string
		current   map[string]string
		zone      string
		set       map[string]string
		conflicts int
	}{
		{"first sync", nil, "a", map[string]string{TopologyKeyZone: "a", HaifyZoneLabel: "a"}, 0},
		{"in step", map[string]string{TopologyKeyZone: "a", HaifyZoneLabel: "a"}, "a", map[string]string{}, 0},
		{"kubernetes changed", map[string]string{TopologyKeyZone: "a", HaifyZoneLabel: "a"}, "b",
			map[string]string{TopologyKeyZone: "b", HaifyZoneLabel: "b"}, 0},
		{"hand-set before the first sync is kept", map[string]string{HaifyZoneLabel: "rack-7"}, "a",
			map[string]string{TopologyKeyZone: "a"}, 1},
		{"hand-set later is kept", map[string]string{TopologyKeyZone: "a", HaifyZoneLabel: "rack-7"}, "b",
			map[string]string{TopologyKeyZone: "b"}, 1},
		{"kubernetes label removed", map[string]string{TopologyKeyZone: "a", HaifyZoneLabel: "a"}, "",
			map[string]string{TopologyKeyZone: "", HaifyZoneLabel: ""}, 0},
		{"removed, hand-set stays", map[string]string{TopologyKeyZone: "a", HaifyZoneLabel: "rack-7"}, "",
			map[string]string{TopologyKeyZone: ""}, 0},
		{"no zone anywhere", map[string]string{"host": "h1"}, "", map[string]string{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set, conflicts := topologyLabelChanges(tc.current, tc.zone, "")
			assert.Equal(t, tc.set, set)
			assert.Len(t, conflicts, tc.conflicts)
		})
	}
}
