package csi

import (
	"context"
	"errors"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// labelBackend is the fake backend plus the controller's label merge: an
// empty value deletes the key.
type labelBackend struct {
	*fakeBackend
	setErr error
	sets   []map[string]string
}

func (b *labelBackend) SetNodeLabels(_ context.Context, node string, labels map[string]string, _ bool) (*haifypb.NodeInfo, error) {
	if b.setErr != nil {
		return nil, b.setErr
	}
	b.sets = append(b.sets, labels)
	for _, n := range b.nodes {
		if n.GetName() != node {
			continue
		}
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		for k, v := range labels {
			if v == "" {
				delete(n.Labels, k)
			} else {
				n.Labels[k] = v
			}
		}
		return n, nil
	}
	return nil, errors.New("node not found: " + node)
}

// k8sLabels is a Node's labels as a reader the test can change or break.
type k8sLabels struct {
	labels map[string]string
	err    error
}

func (k *k8sLabels) read(context.Context) (map[string]string, error) { return k.labels, k.err }

func TestNodeGetInfoPublishesZoneAndRegion(t *testing.T) {
	k := &k8sLabels{labels: map[string]string{TopologyKeyZone: "east-1a", TopologyKeyRegion: "east-1", "other": "x"}}
	ns := newTestNode(newFakeBackend("n1"), newRecordingMounter())
	EnableNodeTopology(ns, NewNodeTopology("n1", k.read, &labelBackend{fakeBackend: newFakeBackend("n1")}, zap.NewNop()))
	resp, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{TopologyKeyNode: "n1", TopologyKeyZone: "east-1a", TopologyKeyRegion: "east-1"},
		resp.AccessibleTopology.Segments)

	// The label changed: the next registration reports what the node carries
	// now, which is what the kubelet compares it with.
	k.labels = map[string]string{TopologyKeyZone: "east-1b"}
	resp, err = ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{TopologyKeyNode: "n1", TopologyKeyZone: "east-1b"}, resp.AccessibleTopology.Segments)

	// The API went away: what was read last still stands.
	k.err = errors.New("apiserver unreachable")
	resp, err = ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	require.NoError(t, err)
	assert.Equal(t, "east-1b", resp.AccessibleTopology.Segments[TopologyKeyZone])
}

// A cluster without zones, or a plugin that may not read its Node, registers
// exactly as before: with the node key alone.
func TestNodeGetInfoWithoutZones(t *testing.T) {
	for name, k := range map[string]*k8sLabels{
		"no zone label": {labels: map[string]string{"kubernetes.io/hostname": "n1"}},
		"read refused":  {err: errors.New("nodes \"n1\" is forbidden")},
	} {
		t.Run(name, func(t *testing.T) {
			ns := newTestNode(newFakeBackend("n1"), newRecordingMounter())
			EnableNodeTopology(ns, NewNodeTopology("n1", k.read, &labelBackend{fakeBackend: newFakeBackend("n1")}, nil))
			resp, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
			require.NoError(t, err)
			assert.Equal(t, map[string]string{TopologyKeyNode: "n1"}, resp.AccessibleTopology.Segments)
		})
	}
}

func TestNodeTopologySyncFollowsKubernetes(t *testing.T) {
	b := &labelBackend{fakeBackend: newFakeBackend("n1", "n2")}
	k := &k8sLabels{labels: map[string]string{TopologyKeyZone: "a", TopologyKeyRegion: "r"}}
	topo := NewNodeTopology("n1", k.read, b, nil)

	require.NoError(t, topo.Sync(context.Background()))
	assert.Equal(t, map[string]string{TopologyKeyZone: "a", HaifyZoneLabel: "a", TopologyKeyRegion: "r", HaifyRegionLabel: "r"},
		b.nodes[0].Labels)
	assert.Empty(t, b.nodes[1].Labels, "only this node's labels are written")

	require.NoError(t, topo.Sync(context.Background()))
	assert.Len(t, b.sets, 1, "nothing to write when in step")

	k.labels = map[string]string{TopologyKeyZone: "b"}
	require.NoError(t, topo.Sync(context.Background()))
	assert.Equal(t, map[string]string{TopologyKeyZone: "b", HaifyZoneLabel: "b"}, b.nodes[0].Labels, "zone follows, region is gone")
}

func TestNodeTopologySyncKeepsAHandSetLabel(t *testing.T) {
	b := &labelBackend{fakeBackend: newFakeBackend("n1")}
	b.nodes[0].Labels = map[string]string{HaifyZoneLabel: "rack-7", "host": "h1"}
	k := &k8sLabels{labels: map[string]string{TopologyKeyZone: "a"}}
	require.NoError(t, NewNodeTopology("n1", k.read, b, nil).Sync(context.Background()))
	assert.Equal(t, map[string]string{HaifyZoneLabel: "rack-7", "host": "h1", TopologyKeyZone: "a"}, b.nodes[0].Labels)
}

// Not knowing the node's labels is not the labels being gone: a failed read
// must never strip the zone off the Haify node.
func TestNodeTopologySyncWritesNothingWithoutARead(t *testing.T) {
	b := &labelBackend{fakeBackend: newFakeBackend("n1")}
	b.nodes[0].Labels = map[string]string{TopologyKeyZone: "a", HaifyZoneLabel: "a"}
	k := &k8sLabels{err: errors.New("forbidden")}
	require.Error(t, NewNodeTopology("n1", k.read, b, nil).Sync(context.Background()))
	assert.Empty(t, b.sets)

	// Nor does a node the controller does not know yet fail anything else.
	k = &k8sLabels{labels: map[string]string{TopologyKeyZone: "a"}}
	require.Error(t, NewNodeTopology("elsewhere", k.read, b, nil).Sync(context.Background()))
	assert.Empty(t, b.sets)
}
