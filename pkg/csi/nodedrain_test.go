package csi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
)

// drainBackend is the fake backend plus the controller's drain: a drained
// node reads "maintenance" until it is undrained.
type drainBackend struct {
	*fakeBackend
	drainErr   error
	undrainErr error
	moved      []string
	left       []*haifypb.DrainLeftResource
	drains     []string
	requests   []*haifypb.DrainNodeRequest
	undrains   []string
}

func (b *drainBackend) setState(name, state string) {
	for _, n := range b.nodes {
		if n.GetName() == name {
			n.State = state
		}
	}
}

func (b *drainBackend) DrainNodeWith(_ context.Context, req *haifypb.DrainNodeRequest) (*haifypb.DrainNodeResponse, error) {
	b.drains = append(b.drains, req.GetName())
	b.requests = append(b.requests, req)
	// The controller marks the node before it moves anything, so a drain
	// that fails half-way still leaves it in maintenance.
	b.setState(req.GetName(), nodeStateMaintenance)
	resp := &haifypb.DrainNodeResponse{Success: b.drainErr == nil, ResourcesMoved: b.moved}
	if req.GetKeepInUse() {
		resp.ResourcesLeft = b.left
	}
	return resp, b.drainErr
}

func (b *drainBackend) UndrainNode(_ context.Context, name string) error {
	b.undrains = append(b.undrains, name)
	if b.undrainErr != nil {
		return b.undrainErr
	}
	b.setState(name, "online")
	return nil
}

// drainFixture is the HA fixture's cluster (a StatefulSet Pod on node-a using
// the Haify volume pvc-1) with a drain controller watching it.
type drainFixture struct {
	t        *testing.T
	backend  *drainBackend
	kube     *fake.Clientset
	recorder *record.FakeRecorder
	drain    *NodeDrain
	clock    time.Time
}

func newDrainFixture(t *testing.T, cfg NodeDrainConfig) *drainFixture {
	t.Helper()
	ha := newHAFixture(t, corev1.ConditionTrue)
	f := &drainFixture{t: t, backend: &drainBackend{fakeBackend: ha.backend}, kube: ha.kube,
		recorder: record.NewFakeRecorder(100), clock: time.Unix(1_800_000_000, 0)}
	f.restart(cfg)
	return f
}

// restart replaces the controller with a new one that remembers nothing, as a
// restarted plugin or another replica taking the Lease would be.
func (f *drainFixture) restart(cfg NodeDrainConfig) {
	f.drain = NewNodeDrain(f.backend, f.kube, f.recorder, cfg, nil)
	f.drain.now = func() time.Time { return f.clock }
}

func (f *drainFixture) sweep()                  { f.drain.Sweep(context.Background()) }
func (f *drainFixture) advance(d time.Duration) { f.clock = f.clock.Add(d) }

func (f *drainFixture) node(name string) *corev1.Node {
	n, err := f.kube.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(f.t, err)
	return n
}

func (f *drainFixture) update(name string, change func(*corev1.Node)) {
	n := f.node(name)
	change(n)
	_, err := f.kube.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
	require.NoError(f.t, err)
}

func (f *drainFixture) request(name, value string) {
	f.update(name, func(n *corev1.Node) {
		if value == "" {
			delete(n.Annotations, DrainRequestKey)
			return
		}
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		n.Annotations[DrainRequestKey] = value
	})
}

func (f *drainFixture) evictPods() {
	require.NoError(f.t, f.kube.CoreV1().Pods("app").Delete(context.Background(), "db-0", metav1.DeleteOptions{}))
}

func (f *drainFixture) state(name string) string {
	return f.node(name).Annotations[DrainStateAnnotation]
}

func (f *drainFixture) events() []string {
	var out []string
	for {
		select {
		case e := <-f.recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestDrainWanted(t *testing.T) {
	cfg := NodeDrainConfig{Taints: DefaultDrainTaints}
	node := func(change func(*corev1.Node)) *corev1.Node {
		n := haTestNode("n", corev1.ConditionTrue)
		change(n)
		return n
	}
	cordon := func(n *corev1.Node) { n.Spec.Unschedulable = true }
	for name, tc := range map[string]struct {
		node *corev1.Node
		cfg  NodeDrainConfig
		want bool
	}{
		"plain node":             {node(func(*corev1.Node) {}), cfg, false},
		"cordon alone":           {node(cordon), cfg, false},
		"cordon, opted in":       {node(cordon), NodeDrainConfig{OnCordon: true}, true},
		"annotation":             {node(func(n *corev1.Node) { n.Annotations = map[string]string{DrainRequestKey: "true"} }), cfg, true},
		"label":                  {node(func(n *corev1.Node) { n.Labels[DrainRequestKey] = "true" }), cfg, true},
		"maintenance taint":      {node(func(n *corev1.Node) { n.Spec.Taints = []corev1.Taint{{Key: "medik8s.io/drain"}} }), cfg, true},
		"unschedulable taint":    {node(func(n *corev1.Node) { n.Spec.Taints = []corev1.Taint{{Key: "node.kubernetes.io/unschedulable"}} }), cfg, false},
		"taint, none configured": {node(func(n *corev1.Node) { n.Spec.Taints = []corev1.Taint{{Key: "medik8s.io/drain"}} }), NodeDrainConfig{}, false},
		"opted out beats cordon": {node(func(n *corev1.Node) { cordon(n); n.Annotations = map[string]string{DrainRequestKey: "false"} }),
			NodeDrainConfig{OnCordon: true}, false},
		"annotation beats label": {node(func(n *corev1.Node) {
			n.Labels[DrainRequestKey] = "true"
			n.Annotations = map[string]string{DrainRequestKey: "false"}
		}), cfg, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, why := drainWanted(tc.node, tc.cfg)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.want, why != "")
		})
	}
}

// The whole of a maintenance: asked for, held through the delay, taken out of
// placement at once with its Pods' volumes left alone, drained for good when
// the Pods are gone, and returned to service when it is over.
func TestNodeDrainFollowsAMaintenance(t *testing.T) {
	f := newDrainFixture(t, NodeDrainConfig{})
	f.backend.moved = []string{"nfs-share", "haify-meta"}
	f.backend.left = []*haifypb.DrainLeftResource{{Resource: "pvc-1", Reason: "the CSI driver decides where it is Primary"}}

	f.sweep()
	assert.Empty(t, f.backend.drains, "nothing asks")

	f.request("node-a", "true")
	f.sweep()
	f.advance(20 * time.Second)
	f.sweep()
	assert.Empty(t, f.backend.drains, "inside the delay")
	assert.Empty(t, f.events())

	// The delay is over and a Pod still uses a Haify volume there: the node
	// stops taking replicas now, and that Pod's volume is not touched.
	f.advance(20 * time.Second)
	f.sweep()
	f.sweep()
	require.Len(t, f.backend.requests, 1, "once, not at every sweep")
	first := f.backend.requests[0]
	assert.Equal(t, "node-a", first.GetName())
	assert.True(t, first.GetKeepInUse(), "what the Pods on the node use stays Primary there")
	assert.Equal(t, drainRequester, first.GetRequestedBy())
	assert.Contains(t, first.GetReason(), DrainRequestKey)
	assert.Equal(t, nodeStateMaintenance, f.backend.nodes[0].GetState(), "no new replica lands on it from here on")
	assert.Equal(t, drainStateDraining, f.state("node-a"), "not drained yet")
	ev := f.events()
	require.Len(t, ev, 1, "the wait is said once, not at every sweep")
	assert.Contains(t, ev[0], ReasonNodeDrainWaiting)
	assert.Contains(t, ev[0], "Takes no new replicas in Haify")
	assert.Contains(t, ev[0], "nfs-share, haify-meta")
	assert.Contains(t, ev[0], "app/db-0")

	f.evictPods()
	f.sweep()
	require.Len(t, f.backend.requests, 2)
	assert.False(t, f.backend.requests[1].GetKeepInUse(), "nothing on the node uses a volume any more")
	assert.Equal(t, drainStateDrained, f.state("node-a"))
	ev = f.events()
	require.Len(t, ev, 1)
	assert.Contains(t, ev[0], ReasonNodeDrained)
	assert.Contains(t, ev[0], "nfs-share, haify-meta")

	// Nothing is repeated: not by later sweeps, and not by a controller that
	// starts with no memory of having drained the node.
	f.sweep()
	f.restart(NodeDrainConfig{})
	f.advance(time.Hour)
	f.sweep()
	assert.Len(t, f.backend.drains, 2)
	assert.Empty(t, f.events())

	f.request("node-a", "")
	f.sweep()
	assert.Equal(t, []string{"node-a"}, f.backend.undrains)
	assert.Empty(t, f.state("node-a"))
	ev = f.events()
	require.Len(t, ev, 1)
	assert.Contains(t, ev[0], ReasonNodeUndrained)

	f.sweep()
	assert.Len(t, f.backend.undrains, 1)
}

// The first step survives what interrupts it: a failure is retried while the
// Pods are still there, and a controller that starts without memory of it
// repeats it once and no more.
func TestNodeDrainFirstStepIsRetriedAndIdempotent(t *testing.T) {
	f := newDrainFixture(t, NodeDrainConfig{Delay: -1})
	f.backend.drainErr = errors.New("drbd-reactor manages nfs-share and evicting it failed")
	f.request("node-a", "true")
	f.sweep()
	f.sweep()
	require.Len(t, f.backend.requests, 1, "not hammered: one attempt per retry period")
	assert.Equal(t, drainStateDraining, f.state("node-a"))
	ev := f.events()
	require.Len(t, ev, 1)
	assert.Contains(t, ev[0], ReasonNodeDrainFailed)

	f.backend.drainErr = nil
	f.advance(2 * time.Minute)
	f.sweep()
	require.Len(t, f.backend.requests, 2)
	assert.True(t, f.backend.requests[1].GetKeepInUse(), "the Pod is still there")
	assert.Equal(t, drainStateDraining, f.state("node-a"))
	f.advance(time.Hour)
	f.sweep()
	assert.Len(t, f.backend.requests, 2, "done: nothing more until the Pods leave")

	f.restart(NodeDrainConfig{Delay: -1})
	f.sweep()
	f.sweep()
	require.Len(t, f.backend.requests, 3, "a new leader repeats the step once")
	assert.True(t, f.backend.requests[2].GetKeepInUse())

	// The maintenance is called off before the Pods ever left: the node is
	// returned to service, since this controller put it in maintenance.
	f.request("node-a", "")
	f.sweep()
	assert.Equal(t, []string{"node-a"}, f.backend.undrains)
	assert.Empty(t, f.state("node-a"))
	assert.Equal(t, "online", f.backend.nodes[0].GetState())
}

func TestNodeDrainLeavesAnOperatorsDrainAlone(t *testing.T) {
	f := newDrainFixture(t, NodeDrainConfig{Delay: -1})
	f.evictPods()
	f.backend.setState("node-a", nodeStateMaintenance) // haify node drain node-a
	f.request("node-a", "true")
	f.sweep()
	assert.Empty(t, f.backend.drains)
	assert.Empty(t, f.state("node-a"), "not taken over")

	f.request("node-a", "")
	f.sweep()
	assert.Empty(t, f.backend.undrains, "the operator drained it, the operator undrains it")
}

func TestNodeDrainRetriesAFailedDrain(t *testing.T) {
	f := newDrainFixture(t, NodeDrainConfig{Delay: -1})
	f.evictPods()
	f.backend.drainErr = errors.New("node \"node-a\" is drained, but 1 resource(s) are still Primary on it")
	f.request("node-a", "true")
	f.sweep()
	f.sweep()
	assert.Len(t, f.backend.drains, 1, "not hammered: one attempt per retry period")
	assert.Equal(t, drainStateDraining, f.state("node-a"))
	ev := f.events()
	require.Len(t, ev, 1)
	assert.Contains(t, ev[0], ReasonNodeDrainFailed)
	assert.Contains(t, ev[0], "still Primary")

	f.backend.drainErr = nil
	f.advance(2 * time.Minute)
	f.sweep()
	assert.Len(t, f.backend.drains, 2)
	assert.Equal(t, drainStateDrained, f.state("node-a"))

	// Uncordoned while the undrain fails: the record stays until it works.
	f.backend.undrainErr = errors.New("database not available")
	f.request("node-a", "")
	f.sweep()
	assert.Equal(t, drainStateDrained, f.state("node-a"))
	f.backend.undrainErr = nil
	f.advance(2 * time.Minute)
	f.sweep()
	assert.Empty(t, f.state("node-a"))
	assert.Equal(t, "online", f.backend.nodes[0].GetState())
}

// A drain cut short between the record and the call is finished by whoever
// leads next; a node whose maintenance was called off before anything was
// drained only loses the record.
func TestNodeDrainPicksUpAnInterruptedDrain(t *testing.T) {
	f := newDrainFixture(t, NodeDrainConfig{})
	f.evictPods()
	f.update("node-a", func(n *corev1.Node) {
		n.Annotations = map[string]string{DrainRequestKey: "true", DrainStateAnnotation: drainStateDraining}
	})
	f.sweep()
	assert.Equal(t, []string{"node-a"}, f.backend.drains, "no delay again: the decision was already taken")
	assert.Equal(t, drainStateDrained, f.state("node-a"))

	f.update("node-b", func(n *corev1.Node) { n.Annotations = map[string]string{DrainStateAnnotation: drainStateDraining} })
	f.sweep()
	assert.Empty(t, f.state("node-b"))
	assert.Empty(t, f.backend.undrains, "node-b was never drained in Haify")
}

func TestNodeDrainLeavesADeadNodeToFailover(t *testing.T) {
	f := newDrainFixture(t, NodeDrainConfig{Delay: -1})
	f.evictPods()
	f.request("node-a", "true")
	f.update("node-a", func(n *corev1.Node) { n.Status.Conditions[0].Status = corev1.ConditionUnknown })
	f.sweep()
	assert.Empty(t, f.backend.drains, "a node that is down has nothing to hand over")

	f.update("node-a", func(n *corev1.Node) {
		n.Status.Conditions[0].Status = corev1.ConditionTrue
		n.Spec.Taints = []corev1.Taint{{Key: FailoverTaintKey, Effect: corev1.TaintEffectNoSchedule}}
	})
	f.sweep()
	assert.Empty(t, f.backend.drains, "not while the HA controller still holds it lost")

	f.update("node-a", func(n *corev1.Node) { n.Spec.Taints = nil })
	f.sweep()
	assert.Len(t, f.backend.drains, 1, "back and still asking")
}

func TestNodeDrainOneNodeAtATime(t *testing.T) {
	f := newDrainFixture(t, NodeDrainConfig{Delay: -1})
	f.evictPods()
	f.request("node-a", "true")
	f.request("node-b", "true")
	f.sweep()
	assert.Equal(t, []string{"node-a"}, f.backend.drains)
	ev := strings.Join(f.events(), "\n")
	assert.Contains(t, ev, "node-a still drained")

	f.request("node-a", "")
	f.sweep() // node-a returns to service
	f.sweep()
	assert.Equal(t, []string{"node-a", "node-b"}, f.backend.drains)
}

func TestNodeDrainIgnoresPodsOnSharedVolumes(t *testing.T) {
	f := newDrainFixture(t, NodeDrainConfig{Delay: -1})
	pv, err := f.kube.CoreV1().PersistentVolumes().Get(context.Background(), "pv-1", metav1.GetOptions{})
	require.NoError(t, err)
	pv.Spec.CSI.VolumeAttributes = map[string]string{paramSharedFilesystem: sharedNFS}
	_, err = f.kube.CoreV1().PersistentVolumes().Update(context.Background(), pv, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.request("node-a", "true")
	f.sweep()
	assert.Len(t, f.backend.drains, 1, "its Pod mounts NFS; the gateway is what the drain moves")
}

func TestNodeDrainSkipsNodesHaifyDoesNotKnow(t *testing.T) {
	f := newDrainFixture(t, NodeDrainConfig{Delay: -1, OnCordon: true})
	_, err := f.kube.CoreV1().Nodes().Create(context.Background(), haTestNode("compute-1", corev1.ConditionTrue), metav1.CreateOptions{})
	require.NoError(t, err)
	f.update("compute-1", func(n *corev1.Node) { n.Spec.Unschedulable = true })
	f.sweep()
	assert.Empty(t, f.backend.drains)
	assert.Empty(t, f.state("compute-1"))
}

func TestDrainHaifyNodeMatchesByAddress(t *testing.T) {
	n := haTestNode("worker-1.lab.example", corev1.ConditionTrue)
	n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.2"}}
	nodes := []*haifypb.NodeInfo{{Name: "stor-a", Address: "10.0.0.1"}, {Name: "stor-b", Address: "10.0.0.2"}}
	assert.Equal(t, "stor-b", drainHaifyNode(n, nodes).GetName())
	assert.Nil(t, drainHaifyNode(haTestNode("other", corev1.ConditionTrue), nodes))
}

// A node drained for maintenance and then switched off is not a node that
// died: the HA controller leaves what is still on it alone. Until the drain
// has completed it is, and its Pods are moved as from any lost node.
func TestHAFailoverSkipsANodeDrainedForMaintenance(t *testing.T) {
	f := newHAFixture(t, corev1.ConditionUnknown)
	n, err := f.kube.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	n.Spec.Unschedulable = true
	n.Annotations = map[string]string{DrainStateAnnotation: drainStateDrained}
	_, err = f.kube.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.sweep()
	assert.True(t, f.podExists("db-0"))
	assert.False(t, f.tainted("node-a"))

	n.Annotations[DrainStateAnnotation] = drainStateDraining
	_, err = f.kube.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.sweep()
	assert.False(t, f.podExists("db-0"), "died before the drain was through: a real loss")
	assert.True(t, f.tainted("node-a"))
}
