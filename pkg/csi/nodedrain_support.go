package csi

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// drainWanted reports whether a node asks to be drained in Haify, and what
// says so.
//
// Kubernetes has no mark for "this node is being drained": `kubectl drain`
// cordons the node and evicts its Pods, and a cordon alone looks the same
// whether the node is about to be switched off or is only being kept free
// while someone looks at it. So the signals that count are the ones that mean
// maintenance and nothing else: the node's own DrainRequestKey, and the
// taints maintenance tooling sets. A bare cordon counts only when the operator
// opted in with OnCordon.
func drainWanted(node *corev1.Node, cfg NodeDrainConfig) (bool, string) {
	request, where := node.Annotations[DrainRequestKey], "annotation"
	if request == "" {
		request, where = node.Labels[DrainRequestKey], "label"
	}
	switch {
	case strings.EqualFold(request, "false"):
		return false, ""
	case strings.EqualFold(request, "true"):
		return true, fmt.Sprintf("%s %s=true", where, DrainRequestKey)
	}
	for _, t := range node.Spec.Taints {
		for _, key := range cfg.Taints {
			if key != "" && t.Key == key {
				return true, "taint " + key
			}
		}
	}
	if cfg.OnCordon && node.Spec.Unschedulable {
		return true, "the node is cordoned"
	}
	return false, ""
}

// drainHaifyNode finds the Haify node a Kubernetes node is, or nil when it is
// none. The node plugin registers a node under its Kubernetes name, so that
// matches first; a node registered some other way is recognised by its
// hostname or address.
func drainHaifyNode(node *corev1.Node, haifyNodes []*haifypb.NodeInfo) *haifypb.NodeInfo {
	for _, hn := range haifyNodes {
		if hn.GetName() == node.Name {
			return hn
		}
	}
	id := haNodeIdentity{}
	id.add(node.Name)
	id.add(node.Labels[corev1.LabelHostname])
	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeHostName || a.Type == corev1.NodeInternalIP {
			id.add(a.Address)
		}
	}
	for _, hn := range haifyNodes {
		for _, known := range []string{hn.GetName(), hn.GetHostname(), hn.GetAddress()} {
			if known = strings.ToLower(strings.TrimSpace(known)); known != "" && id[known] {
				return hn
			}
		}
	}
	return nil
}

// drainedElsewhere names the other nodes that are drained in Haify right now:
// the Haify nodes in maintenance, whoever drained them, and the Kubernetes
// nodes this controller has a drain under way on.
func drainedElsewhere(self *corev1.Node, nodes []*corev1.Node, haifyNodes []*haifypb.NodeInfo) []string {
	own := drainHaifyNode(self, haifyNodes).GetName()
	seen := map[string]bool{}
	for _, hn := range haifyNodes {
		if hn.GetState() == nodeStateMaintenance && hn.GetName() != own {
			seen[hn.GetName()] = true
		}
	}
	for _, n := range nodes {
		if n.Name == self.Name || n.Annotations[DrainStateAnnotation] == "" {
			continue
		}
		if hn := drainHaifyNode(n, haifyNodes); hn != nil && hn.GetName() != own {
			seen[hn.GetName()] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// drainComplete reports whether the drain controller finished draining the
// node in Haify and the node is still cordoned.
//
// The HA controller leaves such a node alone. Its Pods that used Haify volumes
// were gone before it was drained, and the cordon has kept others from being
// scheduled there since, so when it is then switched off there is nothing to
// fail over: what is left on it tolerates the cordon, as a DaemonSet's Pods
// do, and deleting such a Pod only has it recreated on the same node.
func drainComplete(node *corev1.Node) bool {
	return node.Annotations[DrainStateAnnotation] == drainStateDrained && node.Spec.Unschedulable
}

// haifyPodsOn lists, as namespace/name, the Pods on a node that hold a Haify
// volume there. Shared volumes do not count: their Pods mount NFS, and the
// volume is Primary where its gateway runs, which the drain moves.
//
// It asks the API server rather than a cache. It is called only for a node
// that is about to be drained, and that decision should rest on what is on
// the node now.
func (d *NodeDrain) haifyPodsOn(ctx context.Context, nodeName string) ([]string, error) {
	start := time.Now()
	pvs, err := d.kube.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	d.log.Debug("NodeDrain.haifyPodsOn: listed persistent volumes", zap.Int("count", len(pvs.Items)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("list persistent volumes: %w", err)
	}
	claims := map[string]bool{}
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != DriverName || pv.Spec.ClaimRef == nil ||
			isSharedContext(pv.Spec.CSI.VolumeAttributes) {
			continue
		}
		claims[pv.Spec.ClaimRef.Namespace+"/"+pv.Spec.ClaimRef.Name] = true
	}
	start = time.Now()
	pods, err := d.kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + nodeName})
	d.log.Debug("NodeDrain.haifyPodsOn: listed pods", zap.String("node", nodeName), zap.Int("count", len(pods.Items)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("list pods on %s: %w", nodeName, err)
	}
	var out []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != nodeName || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, claim := range haPodClaims(pod) {
			if claims[claim] {
				out = append(out, pod.Namespace+"/"+pod.Name)
				break
			}
		}
	}
	sort.Strings(out)
	d.log.Debug("NodeDrain.haifyPodsOn: done", zap.String("node", nodeName), zap.Int("haify_pods", len(out)))
	return out, nil
}

// setState writes the drain record on a node, or removes it when state is "".
// A merge patch touches that one annotation and nothing else on the Node.
func (d *NodeDrain) setState(ctx context.Context, nodeName, state string) error {
	var value any
	if state != "" {
		value = state
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{DrainStateAnnotation: value}}})
	if err != nil {
		return fmt.Errorf("encode annotation patch: %w", err)
	}
	start := time.Now()
	_, err = d.kube.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{})
	d.log.Debug("NodeDrain.setState: patched node", zap.String("node", nodeName), zap.String("state", state),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("patch node %s: %w", nodeName, err)
	}
	return nil
}

// waitingMessage is the event for a node whose first step is done: what
// changed in Haify, and what the rest of the drain waits for. moved is what
// the step moved.
func (d *NodeDrain) waitingMessage(why string, pods, moved []string) string {
	msg := fmt.Sprintf("Takes no new replicas in Haify (%s)", why)
	if len(moved) > 0 {
		msg += fmt.Sprintf("; the Primary role of %d resource(s) Haify runs there moved to other nodes (%s)", len(moved), drainNameList(moved))
	}
	return msg + fmt.Sprintf(". The drain finishes when %d Pod(s) using Haify volumes have left the node: %s",
		len(pods), drainNameList(pods))
}

// drainNameList joins names for an event, cut short so one long list does not
// push the rest of the message out of the event.
func drainNameList(names []string) string {
	const show = 10
	if len(names) <= show {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:show], ", "), len(names)-show)
}

// drainStartInformer starts the node cache the drain controller reads and
// waits until it is filled. The cache stops with ctx.
func drainStartInformer(ctx context.Context, kube kubernetes.Interface, log *zap.Logger) (func(context.Context) ([]*corev1.Node, error), error) {
	start := time.Now()
	log.Debug("drainStartInformer: starting node informer")
	factory := informers.NewSharedInformerFactory(kube, 0)
	lister := factory.Core().V1().Nodes().Lister()
	factory.Start(ctx.Done())
	synced := cache.WaitForCacheSync(ctx.Done(), factory.Core().V1().Nodes().Informer().HasSynced)
	log.Debug("drainStartInformer: cache", zap.Bool("synced", synced), zap.Duration("took", time.Since(start)))
	if !synced {
		return nil, fmt.Errorf("node informer cache did not sync")
	}
	return func(context.Context) ([]*corev1.Node, error) { return lister.List(labels.Everything()) }, nil
}
