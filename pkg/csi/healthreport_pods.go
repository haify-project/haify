package csi

import (
	"context"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Health events on Pods.
//
// The PVC is where a volume's health belongs, but it is not where people look
// when something is slow or failing: they `kubectl describe pod`. Until CSI
// v1.13.0 the kubelet put a volume's condition there itself (VolumeCondition,
// through NodeGetVolumeStats); with that gone, the reporter posts the same
// transition on every Pod using the claim as well.

// podIndex finds the Pods that use a claim. It lists Pods at most once per
// sweep, and only once something in the sweep is worth posting: most sweeps
// post nothing, and a cluster-wide Pod list for nothing is load on the API
// server for no one.
type podIndex struct {
	kube   kubernetes.Interface
	log    *zap.Logger
	loaded bool
	// byClaim maps "<namespace>/<claim>" to the Pods using it. nil after a
	// failed list: the sweep then posts on PVCs only, rather than listing
	// again for every volume.
	byClaim map[string][]*corev1.ObjectReference
}

func newPodIndex(kube kubernetes.Interface, log *zap.Logger) *podIndex {
	return &podIndex{kube: kube, log: log}
}

// claimsOf names the claims a Pod uses: its persistentVolumeClaim volumes,
// and its generic ephemeral volumes, whose claim Kubernetes names
// "<pod>-<volume>".
func claimsOf(pod *corev1.Pod) []string {
	var out []string
	for _, v := range pod.Spec.Volumes {
		switch {
		case v.PersistentVolumeClaim != nil:
			out = append(out, v.PersistentVolumeClaim.ClaimName)
		case v.Ephemeral != nil:
			out = append(out, pod.Name+"-"+v.Name)
		}
	}
	return out
}

// load lists every Pod once. A Pod that has finished uses nothing, so it is
// left out; the field selector says so to the API server, and the check here
// covers a server (or fake) that ignores it.
func (p *podIndex) load(ctx context.Context) {
	p.loaded = true
	start := time.Now()
	pods, err := p.kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase!=Succeeded,status.phase!=Failed",
		// Served from the API server's cache: a health event does not need
		// a quorum read of every Pod in the cluster.
		ResourceVersion: "0",
	})
	if err != nil {
		p.log.Warn("health sweep: list pods; events go on the PVCs only", zap.Duration("took", time.Since(start)), zap.Error(err))
		return
	}
	p.byClaim = map[string][]*corev1.ObjectReference{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, claim := range claimsOf(pod) {
			key := pod.Namespace + "/" + claim
			p.byClaim[key] = append(p.byClaim[key], &corev1.ObjectReference{
				Kind: "Pod", APIVersion: "v1", Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID,
			})
		}
	}
	p.log.Debug("podIndex.load: listed pods", zap.Int("pods", len(pods.Items)), zap.Int("claims", len(p.byClaim)),
		zap.Duration("took", time.Since(start)))
}

// podsUsing returns the Pods using namespace/claim, listing Pods first if this
// sweep has not yet.
func (p *podIndex) podsUsing(ctx context.Context, namespace, claim string) []*corev1.ObjectReference {
	if !p.loaded {
		p.log.Debug("podIndex.podsUsing: first lookup this sweep, listing pods", zap.String("namespace", namespace),
			zap.String("pvc", claim))
		p.load(ctx)
	}
	pods := p.byClaim[namespace+"/"+claim]
	p.log.Debug("podIndex.podsUsing: found", zap.String("namespace", namespace), zap.String("pvc", claim), zap.Int("pods", len(pods)))
	return pods
}

// postOnPods posts a PVC's health event on every Pod using it. The message
// names the claim: a Pod can have several, and the event alone would not say
// which one is degraded.
func (h *HealthReporter) postOnPods(ctx context.Context, pods *podIndex, claim *corev1.ObjectReference, eventType, reason, msg string) {
	for _, pod := range pods.podsUsing(ctx, claim.Namespace, claim.Name) {
		h.log.Debug("HealthReporter.postOnPods: posting on pod", zap.String("namespace", pod.Namespace), zap.String("pod", pod.Name),
			zap.String("pvc", claim.Name), zap.String("reason", reason))
		h.recorder.Event(pod, eventType, reason, "PVC "+claim.Name+": "+msg)
	}
}
