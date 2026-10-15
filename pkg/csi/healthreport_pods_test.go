package csi

import (
	"context"
	"fmt"
	"strings"
	"testing"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// objectRecorder records which object each event went to.
type objectRecorder struct{ events []string }

func (r *objectRecorder) Event(obj runtime.Object, eventtype, reason, message string) {
	ref, _ := obj.(*corev1.ObjectReference)
	r.events = append(r.events, fmt.Sprintf("%s %s/%s %s %s: %s", ref.Kind, ref.Namespace, ref.Name, eventtype, reason, message))
}

func (r *objectRecorder) Eventf(obj runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	r.Event(obj, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (r *objectRecorder) AnnotatedEventf(obj runtime.Object, _ map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	r.Event(obj, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (r *objectRecorder) drain() []string {
	out := r.events
	r.events = nil
	return out
}

func csiPV(name, handle, namespace, claim string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: DriverName, VolumeHandle: handle}},
			ClaimRef:               &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: namespace, Name: claim},
		},
	}
}

func podWithClaim(namespace, name, claim string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID("uid-" + name)},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}}}},
		Status: corev1.PodStatus{Phase: phase},
	}
}

type podHealthHarness struct {
	b    *fakeBackend
	kube *kubefake.Clientset
	rep  *HealthReporter
	rec  *objectRecorder
}

// Two volumes: "data" in default, used by two running Pods (and one that has
// finished, and a same-named claim in another namespace), and the generic
// ephemeral volume "scratch" of Pod web-0.
func newPodHealthHarness(t *testing.T) *podHealthHarness {
	t.Helper()
	b := newFakeBackend("n1", "n2")
	for _, r := range []string{"pvc_data", "pvc_scratch"} {
		require.NoError(t, b.CreateResourceWithPoolAndType(context.Background(), r, 0, []string{"n1", "n2"}, "C", 1, "vg0", "lvm", nil))
	}
	ephemeral := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-0"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{
			Ephemeral: &corev1.EphemeralVolumeSource{}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	kube := kubefake.NewClientset(
		csiPV("pv-data", "pvc_data", "default", "data"),
		csiPV("pv-scratch", "pvc_scratch", "default", "web-0-scratch"),
		podWithClaim("default", "db-0", "data", corev1.PodRunning),
		podWithClaim("default", "backup", "data", corev1.PodPending),
		podWithClaim("default", "old-job", "data", corev1.PodSucceeded),
		podWithClaim("other", "elsewhere", "data", corev1.PodRunning),
		ephemeral,
	)
	rec := &objectRecorder{}
	return &podHealthHarness{b: b, kube: kube, rep: NewHealthReporter(b, kube, rec, 0, zap.NewNop()), rec: rec}
}

func (h *podHealthHarness) set(resource string, states map[string]*haifypb.NodeResourceState) {
	h.b.resources[resource].NodeStates = states
}

func (h *podHealthHarness) podLists() int {
	n := 0
	for _, a := range h.kube.Actions() {
		if a.GetVerb() == "list" && a.GetResource().Resource == "pods" {
			n++
		}
	}
	return n
}

var standAlone = map[string]*haifypb.NodeResourceState{
	"n1": {Role: "Primary", DiskState: "UpToDate", Node: "n1"},
	"n2": {Connection: "StandAlone", Node: "n2"},
}

func TestReporterPostsTransitionsOnThePodsUsingTheClaim(t *testing.T) {
	h := newPodHealthHarness(t)
	ctx := context.Background()
	h.set("pvc_data", healthy)
	h.set("pvc_scratch", healthy)
	h.rep.Sweep(ctx)
	assert.Empty(t, h.rec.drain())
	assert.Zero(t, h.podLists(), "a sweep with nothing to post lists no Pods")

	h.set("pvc_data", standAlone)
	h.rep.Sweep(ctx)
	evts := h.rec.drain()
	require.Len(t, evts, 3, "%v", evts)
	assert.True(t, strings.HasPrefix(evts[0], "PersistentVolumeClaim default/data Warning VolumeDegraded: "), evts[0])
	assert.Contains(t, evts, "Pod default/db-0 Warning VolumeDegraded: PVC data: "+strings.SplitN(evts[0], ": ", 2)[1])
	var podsTold []string
	for _, e := range evts[1:] {
		podsTold = append(podsTold, strings.Fields(e)[1])
	}
	assert.ElementsMatch(t, []string{"default/db-0", "default/backup"}, podsTold,
		"not the finished Pod, and not a same-named claim in another namespace")

	h.rep.Sweep(ctx)
	assert.Empty(t, h.rec.drain(), "transitions only, on Pods as on the PVC")

	h.set("pvc_data", healthy)
	h.rep.Sweep(ctx)
	evts = h.rec.drain()
	require.Len(t, evts, 3)
	for _, e := range evts {
		assert.Contains(t, e, "Normal VolumeRecovered")
	}
}

// Two volumes changing in one sweep still cost one Pod list.
func TestReporterListsPodsOncePerSweep(t *testing.T) {
	h := newPodHealthHarness(t)
	ctx := context.Background()
	h.set("pvc_data", standAlone)
	h.set("pvc_scratch", standAlone)
	h.rep.Sweep(ctx)
	assert.Equal(t, 1, h.podLists())
	evts := h.rec.drain()
	assert.Contains(t, strings.Join(evts, "\n"), "Pod default/web-0 Warning VolumeDegraded: PVC web-0-scratch: ",
		"an ephemeral volume's claim is <pod>-<volume>")
}

// Without the Pod list the PVC still gets its event.
func TestReporterPostsOnThePVCWhenPodsCannotBeListed(t *testing.T) {
	h := newPodHealthHarness(t)
	h.kube.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("forbidden")
	})
	h.set("pvc_data", standAlone)
	h.set("pvc_scratch", standAlone)
	h.rep.Sweep(context.Background())
	evts := h.rec.drain()
	require.Len(t, evts, 2)
	for _, e := range evts {
		assert.True(t, strings.HasPrefix(e, "PersistentVolumeClaim "), e)
	}
	assert.Equal(t, 1, h.podLists(), "a failed list is not retried for every volume")
}
