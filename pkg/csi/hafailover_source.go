package csi

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// haClusterSource is where the HA controller reads nodes, volumes and Pods from.
// Run uses informer caches, so a sweep every few seconds costs the API server
// nothing; tests use the API directly, so what a sweep changed is visible to
// the next one at once.
type haClusterSource interface {
	nodes(ctx context.Context) ([]*corev1.Node, error)
	persistentVolumes(ctx context.Context) ([]*corev1.PersistentVolume, error)
	pods(ctx context.Context) ([]*corev1.Pod, error)
}

// haAPISource lists straight from the API server.
type haAPISource struct{ kube kubernetes.Interface }

func (s haAPISource) nodes(ctx context.Context) ([]*corev1.Node, error) {
	l, err := s.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]*corev1.Node, 0, len(l.Items))
	for i := range l.Items {
		out = append(out, &l.Items[i])
	}
	return out, nil
}

func (s haAPISource) persistentVolumes(ctx context.Context) ([]*corev1.PersistentVolume, error) {
	l, err := s.kube.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]*corev1.PersistentVolume, 0, len(l.Items))
	for i := range l.Items {
		out = append(out, &l.Items[i])
	}
	return out, nil
}

func (s haAPISource) pods(ctx context.Context) ([]*corev1.Pod, error) {
	l, err := s.kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: haScheduledPodsSelector})
	if err != nil {
		return nil, err
	}
	out := make([]*corev1.Pod, 0, len(l.Items))
	for i := range l.Items {
		out = append(out, &l.Items[i])
	}
	return out, nil
}

// haScheduledPodsSelector leaves out Pods not yet bound to a node, which the HA
// controller has no use for.
const haScheduledPodsSelector = "spec.nodeName!="

// haInformerSource reads from informer caches.
type haInformerSource struct {
	nodeLister corelisters.NodeLister
	pvLister   corelisters.PersistentVolumeLister
	podLister  corelisters.PodLister
}

func (s haInformerSource) nodes(context.Context) ([]*corev1.Node, error) {
	return s.nodeLister.List(labels.Everything())
}

func (s haInformerSource) persistentVolumes(context.Context) ([]*corev1.PersistentVolume, error) {
	return s.pvLister.List(labels.Everything())
}

func (s haInformerSource) pods(context.Context) ([]*corev1.Pod, error) {
	return s.podLister.List(labels.Everything())
}

// haStartInformers starts the caches the HA controller reads and waits until
// they are filled. The caches stop with ctx.
func haStartInformers(ctx context.Context, kube kubernetes.Interface, log *zap.Logger) (haClusterSource, error) {
	start := time.Now()
	log.Debug("haStartInformers: starting node, PV and pod informers")
	all := informers.NewSharedInformerFactory(kube, 0)
	scheduled := informers.NewSharedInformerFactoryWithOptions(kube, 0,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.FieldSelector = haScheduledPodsSelector }))
	src := haInformerSource{
		nodeLister: all.Core().V1().Nodes().Lister(),
		pvLister:   all.Core().V1().PersistentVolumes().Lister(),
		podLister:  scheduled.Core().V1().Pods().Lister(),
	}
	all.Start(ctx.Done())
	scheduled.Start(ctx.Done())
	synced := cache.WaitForCacheSync(ctx.Done(),
		all.Core().V1().Nodes().Informer().HasSynced,
		all.Core().V1().PersistentVolumes().Informer().HasSynced,
		scheduled.Core().V1().Pods().Informer().HasSynced)
	log.Debug("haStartInformers: caches", zap.Bool("synced", synced), zap.Duration("took", time.Since(start)))
	if !synced {
		return nil, fmt.Errorf("informer caches did not sync")
	}
	return src, nil
}

// haClusterView is one sweep's snapshot of the Kubernetes side.
type haClusterView struct {
	nodes map[string]*corev1.Node
	// pvByClaim maps namespace/name of a claim to the Haify PV bound to it.
	pvByClaim map[string]*corev1.PersistentVolume
	// handles is every Haify volume handle with a PV.
	handles map[string]*corev1.PersistentVolume
	// uses lists, per node, the running Pods there that mount a Haify volume.
	uses map[string][]haPodUse
}

// haPodUse is a Pod and the Haify PVs it mounts.
type haPodUse struct {
	pod *corev1.Pod
	pvs []*corev1.PersistentVolume
}

// haReadCluster takes the snapshot a sweep works from.
func haReadCluster(ctx context.Context, src haClusterSource, log *zap.Logger) (*haClusterView, error) {
	start := time.Now()
	nodes, err := src.nodes(ctx)
	if err != nil {
		log.Debug("haReadCluster: list nodes failed", zap.Error(err))
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	pvs, err := src.persistentVolumes(ctx)
	if err != nil {
		log.Debug("haReadCluster: list persistent volumes failed", zap.Error(err))
		return nil, fmt.Errorf("list persistent volumes: %w", err)
	}
	pods, err := src.pods(ctx)
	if err != nil {
		log.Debug("haReadCluster: list pods failed", zap.Error(err))
		return nil, fmt.Errorf("list pods: %w", err)
	}

	v := &haClusterView{nodes: map[string]*corev1.Node{}, pvByClaim: map[string]*corev1.PersistentVolume{},
		handles: map[string]*corev1.PersistentVolume{}, uses: map[string][]haPodUse{}}
	for _, n := range nodes {
		v.nodes[n.Name] = n
	}
	for _, pv := range pvs {
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != DriverName {
			continue
		}
		v.handles[pv.Spec.CSI.VolumeHandle] = pv
		if ref := pv.Spec.ClaimRef; ref != nil {
			v.pvByClaim[ref.Namespace+"/"+ref.Name] = pv
		}
	}
	for _, pod := range pods {
		if pod.Spec.NodeName == "" || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		var mine []*corev1.PersistentVolume
		for _, claim := range haPodClaims(pod) {
			if pv := v.pvByClaim[claim]; pv != nil {
				mine = append(mine, pv)
			}
		}
		if len(mine) > 0 {
			v.uses[pod.Spec.NodeName] = append(v.uses[pod.Spec.NodeName], haPodUse{pod: pod, pvs: mine})
		}
	}
	log.Debug("haReadCluster: snapshot", zap.Int("nodes", len(v.nodes)), zap.Int("haify_pvs", len(v.handles)),
		zap.Int("nodes_with_haify_pods", len(v.uses)), zap.Duration("took", time.Since(start)))
	return v, nil
}
