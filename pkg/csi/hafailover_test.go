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
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
)

// haFixture is a three-node cluster with one StatefulSet Pod on node-a using
// the Haify volume pvc-1, replicated on node-a, node-b and node-c.
type haFixture struct {
	t        *testing.T
	backend  *fakeBackend
	kube     *fake.Clientset
	recorder *record.FakeRecorder
	ha       *HAFailover
	clock    time.Time
}

func haTestNode(name string, ready corev1.ConditionStatus) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{corev1.LabelHostname: name}},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}}},
	}
}

func haTestPod(name, node string) *corev1.Pod {
	yes := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "app", UID: types.UID("uid-" + name),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "db", Controller: &yes}}},
		Spec: corev1.PodSpec{NodeName: node, Volumes: []corev1.Volume{{Name: "data",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-db-0"}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// survivorView is node-b's view after node-a dropped out: node-c still
// connected, node-a's link given by conn.
func survivorView(conn string) map[string]*haifypb.NodeResourceState {
	return map[string]*haifypb.NodeResourceState{
		"node-b": {Role: "Secondary", DiskState: "UpToDate"},
		"node-c": {Role: "Secondary", DiskState: "UpToDate", Connection: "Connected"},
		"node-a": {Connection: conn},
	}
}

func newHAFixture(t *testing.T, nodeAReady corev1.ConditionStatus) *haFixture {
	t.Helper()
	f := &haFixture{t: t, backend: newFakeBackend("node-a", "node-b", "node-c"), recorder: record.NewFakeRecorder(100),
		clock: time.Unix(1_800_000_000, 0)}
	f.backend.resources["pvc-1"] = &haifypb.ResourceInfo{Name: "pvc-1", Nodes: []string{"node-a", "node-b", "node-c"},
		NodeStates: survivorView("Connecting")}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: DriverName, VolumeHandle: "pvc-1"}},
			ClaimRef:               &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: "app", Name: "data-db-0"},
		},
	}
	f.kube = fake.NewClientset(
		haTestNode("node-a", nodeAReady), haTestNode("node-b", corev1.ConditionTrue), haTestNode("node-c", corev1.ConditionTrue),
		pv, haTestPod("db-0", "node-a"),
	)
	f.ha = NewHAFailover(f.backend, f.kube, f.recorder, HAFailoverConfig{Grace: 10 * time.Second}, nil)
	f.ha.now = func() time.Time { return f.clock }
	return f
}

func (f *haFixture) sweep() { f.ha.Sweep(context.Background()) }

func (f *haFixture) podExists(name string) bool {
	_, err := f.kube.CoreV1().Pods("app").Get(context.Background(), name, metav1.GetOptions{})
	return err == nil
}

func (f *haFixture) tainted(node string) bool {
	n, err := f.kube.CoreV1().Nodes().Get(context.Background(), node, metav1.GetOptions{})
	require.NoError(f.t, err)
	count := 0
	for _, tn := range n.Spec.Taints {
		if tn.Key == FailoverTaintKey {
			assert.Equal(f.t, corev1.TaintEffectNoSchedule, tn.Effect)
			count++
		}
	}
	assert.LessOrEqual(f.t, count, 1, "taint added more than once")
	return count == 1
}

func (f *haFixture) events() []string {
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

func countReason(events []string, reason string) int {
	n := 0
	for _, e := range events {
		if strings.Contains(e, " "+reason+" ") {
			n++
		}
	}
	return n
}

func TestHAFailoverActsOnLostNotReadyNode(t *testing.T) {
	f := newHAFixture(t, corev1.ConditionUnknown)
	f.sweep()

	assert.True(t, f.tainted("node-a"))
	assert.False(t, f.podExists("db-0"), "pod on the lost node should be force-deleted")
	ev := f.events()
	assert.Equal(t, 1, countReason(ev, ReasonFailoverNodeTainted), "%v", ev)
	// One on the Pod, one on its PVC.
	assert.Equal(t, 2, countReason(ev, ReasonFailoverPodDeleted), "%v", ev)
	assert.False(t, f.tainted("node-b"))
}

func TestHAFailoverWaitsGraceWhileNodeReady(t *testing.T) {
	f := newHAFixture(t, corev1.ConditionTrue)
	f.sweep()
	assert.True(t, f.podExists("db-0"), "a node Kubernetes calls Ready waits out the grace period")
	assert.False(t, f.tainted("node-a"))

	f.clock = f.clock.Add(5 * time.Second)
	f.sweep()
	assert.True(t, f.podExists("db-0"))

	f.clock = f.clock.Add(6 * time.Second)
	f.sweep()
	assert.False(t, f.podExists("db-0"))
	assert.True(t, f.tainted("node-a"))
}

func TestHAFailoverGraceRestartsWhenNodeReturns(t *testing.T) {
	f := newHAFixture(t, corev1.ConditionTrue)
	f.sweep()
	f.backend.resources["pvc-1"].NodeStates = survivorView("Connected")
	f.clock = f.clock.Add(5 * time.Second)
	f.sweep()
	f.backend.resources["pvc-1"].NodeStates = survivorView("Connecting")
	f.clock = f.clock.Add(6 * time.Second)
	f.sweep()
	assert.True(t, f.podExists("db-0"), "a blip resets the grace period")
}

func TestHAFailoverNeedsSurvivorQuorum(t *testing.T) {
	f := newHAFixture(t, corev1.ConditionUnknown)
	view := survivorView("Connecting")
	view["node-c"] = &haifypb.NodeResourceState{Connection: "Connecting"}
	f.backend.resources["pvc-1"].NodeStates = view
	f.sweep()
	assert.True(t, f.podExists("db-0"), "one of three replicas is no quorum")
	assert.False(t, f.tainted("node-a"))
	assert.Empty(t, f.events())
}

func TestHAFailoverIgnoresStandAlone(t *testing.T) {
	f := newHAFixture(t, corev1.ConditionUnknown)
	f.backend.resources["pvc-1"].NodeStates = survivorView("StandAlone")
	f.sweep()
	assert.True(t, f.podExists("db-0"))
}

func TestHAFailoverRespectsOptOut(t *testing.T) {
	for _, where := range []string{"annotation", "label"} {
		t.Run(where, func(t *testing.T) {
			f := newHAFixture(t, corev1.ConditionUnknown)
			pod, err := f.kube.CoreV1().Pods("app").Get(context.Background(), "db-0", metav1.GetOptions{})
			require.NoError(t, err)
			if where == "annotation" {
				pod.Annotations = map[string]string{FailoverOptOutKey: "false"}
			} else {
				pod.Labels = map[string]string{FailoverOptOutKey: "false"}
			}
			_, err = f.kube.CoreV1().Pods("app").Update(context.Background(), pod, metav1.UpdateOptions{})
			require.NoError(t, err)

			f.sweep()
			assert.True(t, f.podExists("db-0"))
			assert.False(t, f.tainted("node-a"), "nothing to move, so no taint")
		})
	}
}

func TestHAFailoverLeavesBarePods(t *testing.T) {
	f := newHAFixture(t, corev1.ConditionUnknown)
	pod, err := f.kube.CoreV1().Pods("app").Get(context.Background(), "db-0", metav1.GetOptions{})
	require.NoError(t, err)
	pod.OwnerReferences = nil
	_, err = f.kube.CoreV1().Pods("app").Update(context.Background(), pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.sweep()
	assert.True(t, f.podExists("db-0"))
}

func TestHAFailoverDoesNothingWhenNotLeading(t *testing.T) {
	f := newHAFixture(t, corev1.ConditionUnknown)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.ha.Sweep(ctx)
	assert.True(t, f.podExists("db-0"))
	assert.False(t, f.tainted("node-a"))
	assert.Empty(t, f.events())
}

func TestHAFailoverHoldsWhenHaifyUnreachable(t *testing.T) {
	f := newHAFixture(t, corev1.ConditionUnknown)
	f.backend.listNodesErr = errors.New("connection refused")
	f.sweep()
	assert.True(t, f.podExists("db-0"))
	assert.Contains(t, f.ha.held, "cannot reach the Haify controller")
}

func TestHAFailoverHoldsOnPartition(t *testing.T) {
	f := newHAFixture(t, corev1.ConditionUnknown)
	nc, err := f.kube.CoreV1().Nodes().Get(context.Background(), "node-c", metav1.GetOptions{})
	require.NoError(t, err)
	nc.Status.Conditions[0].Status = corev1.ConditionUnknown
	_, err = f.kube.CoreV1().Nodes().Update(context.Background(), nc, metav1.UpdateOptions{})
	require.NoError(t, err)

	f.sweep()
	assert.True(t, f.podExists("db-0"), "two of three nodes lost looks like a partition")
	assert.Contains(t, f.ha.held, "partition")

	f.ha.cfg.MaxLostFraction = 0.7
	f.sweep()
	assert.False(t, f.podExists("db-0"))
	assert.Empty(t, f.ha.held)
}

func TestHAFailoverIsIdempotent(t *testing.T) {
	f := newHAFixture(t, corev1.ConditionUnknown)
	f.sweep()
	require.False(t, f.podExists("db-0"))
	f.events()

	// The StatefulSet brings db-0 back under the same name on node-b.
	_, err := f.kube.CoreV1().Pods("app").Create(context.Background(), haTestPod("db-0", "node-b"), metav1.CreateOptions{})
	require.NoError(t, err)
	for range 3 {
		f.clock = f.clock.Add(5 * time.Second)
		f.sweep()
	}
	assert.True(t, f.podExists("db-0"), "the replacement on a healthy node is left alone")
	assert.True(t, f.tainted("node-a"), "node-a is still down, the taint stays")
	assert.Empty(t, f.events(), "re-runs post nothing new")
}

func TestHAFailoverRemovesTaintWhenNodeReturns(t *testing.T) {
	f := newHAFixture(t, corev1.ConditionUnknown)
	f.sweep()
	require.True(t, f.tainted("node-a"))
	f.events()

	na, err := f.kube.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	na.Status.Conditions[0].Status = corev1.ConditionTrue
	_, err = f.kube.CoreV1().Nodes().Update(context.Background(), na, metav1.UpdateOptions{})
	require.NoError(t, err)

	f.sweep()
	assert.True(t, f.tainted("node-a"), "Ready but DRBD still disconnected: keep the taint")

	f.backend.resources["pvc-1"].NodeStates = survivorView("Connected")
	f.sweep()
	assert.False(t, f.tainted("node-a"))
	assert.Equal(t, 1, countReason(f.events(), ReasonFailoverNodeReturned))

	f.sweep()
	assert.Empty(t, f.events(), "removing an absent taint posts nothing")
}

func TestHAFailoverKeepsOtherTaints(t *testing.T) {
	f := newHAFixture(t, corev1.ConditionUnknown)
	na, err := f.kube.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	na.Spec.Taints = []corev1.Taint{{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoExecute}}
	_, err = f.kube.CoreV1().Nodes().Update(context.Background(), na, metav1.UpdateOptions{})
	require.NoError(t, err)

	f.sweep()
	na, err = f.kube.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Len(t, na.Spec.Taints, 2)
}

func TestHAFailoverDeletesVolumeAttachments(t *testing.T) {
	f := newHAFixture(t, corev1.ConditionUnknown)
	pvName := "pv-1"
	other := "pv-other"
	for _, va := range []*storagev1.VolumeAttachment{
		{ObjectMeta: metav1.ObjectMeta{Name: "va-lost"}, Spec: storagev1.VolumeAttachmentSpec{Attacher: DriverName, NodeName: "node-a",
			Source: storagev1.VolumeAttachmentSource{PersistentVolumeName: &pvName}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "va-other"}, Spec: storagev1.VolumeAttachmentSpec{Attacher: DriverName, NodeName: "node-a",
			Source: storagev1.VolumeAttachmentSource{PersistentVolumeName: &other}}},
	} {
		_, err := f.kube.StorageV1().VolumeAttachments().Create(context.Background(), va, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	f.sweep()
	_, err := f.kube.StorageV1().VolumeAttachments().Get(context.Background(), "va-lost", metav1.GetOptions{})
	assert.Error(t, err, "the lost volume's attachment is deleted")
	_, err = f.kube.StorageV1().VolumeAttachments().Get(context.Background(), "va-other", metav1.GetOptions{})
	assert.NoError(t, err, "another volume's attachment is kept")
}

func TestHAFailoverIgnoresUnreadableVolume(t *testing.T) {
	f := newHAFixture(t, corev1.ConditionUnknown)
	delete(f.backend.resources, "pvc-1")
	f.sweep()
	assert.True(t, f.podExists("db-0"), "no view is no evidence")
}

func TestHAFailoverRunFromInformers(t *testing.T) {
	f := newHAFixture(t, corev1.ConditionUnknown)
	f.ha.cfg.Interval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.ha.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for f.podExists("db-0") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	assert.False(t, f.podExists("db-0"))
}
