package csi

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// setFailoverTaint adds (add=true) or removes the failover taint on a node and
// reports whether it changed anything.
//
// The node is read fresh rather than from the cache, and the patch is a JSON
// patch that first tests the resourceVersion read: the taint list is replaced
// as a whole, so without that test a taint someone else added in between would
// be wiped out. A lost race fails the patch and the next sweep tries again.
func (h *HAFailover) setFailoverTaint(ctx context.Context, nodeName string, add bool) (bool, error) {
	start := time.Now()
	node, err := h.kube.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	h.log.Debug("HAFailover.setFailoverTaint: read node", zap.String("node", nodeName), zap.Bool("add", add),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return false, fmt.Errorf("get node %s: %w", nodeName, err)
	}
	if haHasTaint(node) == add {
		h.log.Debug("HAFailover.setFailoverTaint: already as wanted", zap.String("node", nodeName), zap.Bool("tainted", add))
		return false, nil
	}
	taints := make([]corev1.Taint, 0, len(node.Spec.Taints)+1)
	for _, t := range node.Spec.Taints {
		if t.Key != FailoverTaintKey {
			taints = append(taints, t)
		}
	}
	if add {
		now := metav1.Now()
		taints = append(taints, corev1.Taint{Key: FailoverTaintKey, Value: "true", Effect: corev1.TaintEffectNoSchedule, TimeAdded: &now})
	}
	var ops []map[string]any
	if node.ResourceVersion != "" {
		ops = append(ops, map[string]any{"op": "test", "path": "/metadata/resourceVersion", "value": node.ResourceVersion})
	}
	// "add" on an existing member replaces it, and also works on a node that
	// has no taints field at all, where "replace" would fail.
	ops = append(ops, map[string]any{"op": "add", "path": "/spec/taints", "value": taints})
	patch, err := json.Marshal(ops)
	if err != nil {
		return false, fmt.Errorf("encode taint patch: %w", err)
	}
	start = time.Now()
	_, err = h.kube.CoreV1().Nodes().Patch(ctx, nodeName, types.JSONPatchType, patch, metav1.PatchOptions{})
	h.log.Debug("HAFailover.setFailoverTaint: patched node", zap.String("node", nodeName), zap.Bool("add", add),
		zap.Int("taints", len(taints)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return false, fmt.Errorf("patch node %s taints: %w", nodeName, err)
	}
	return true, nil
}

// forceDeletePod deletes a Pod with no grace period, which is what lets its
// StatefulSet or Deployment replace it while the node it ran on cannot confirm
// that it stopped. The UID precondition makes sure the Pod deleted is the one
// judged: a StatefulSet recreates its Pod under the same name, and that new Pod
// must never be the one removed. It reports whether the Pod was deleted, false
// when it was already gone.
func (h *HAFailover) forceDeletePod(ctx context.Context, pod *corev1.Pod) (bool, error) {
	zero := int64(0)
	uid := pod.UID
	opts := metav1.DeleteOptions{GracePeriodSeconds: &zero}
	if uid != "" {
		opts.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	start := time.Now()
	err := h.kube.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, opts)
	h.log.Debug("HAFailover.forceDeletePod: delete returned", zap.String("namespace", pod.Namespace), zap.String("pod", pod.Name),
		zap.String("uid", string(uid)), zap.Duration("took", time.Since(start)), zap.Error(err))
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		// Gone, or replaced by a Pod with a new UID: either way there is
		// nothing of the judged Pod left to delete.
		h.log.Debug("HAFailover.forceDeletePod: already gone or replaced", zap.String("namespace", pod.Namespace),
			zap.String("pod", pod.Name))
		return false, nil
	default:
		return false, fmt.Errorf("delete pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
}

// deleteVolumeAttachments removes the VolumeAttachments that tie the given PVs
// to the lost node. The driver's CSIDriver sets attachRequired: false, so
// Kubernetes normally creates none; a cluster whose CSIDriver object was
// changed by hand would have them, and a VolumeAttachment left on a dead node
// keeps the volume from attaching anywhere else for six minutes.
func (h *HAFailover) deleteVolumeAttachments(ctx context.Context, nodeName string, pvNames map[string]bool) int {
	start := time.Now()
	list, err := h.kube.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
	h.log.Debug("HAFailover.deleteVolumeAttachments: listed", zap.String("node", nodeName),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		h.log.Warn("HA failover: list volume attachments", zap.String("node", nodeName), zap.Error(err))
		return 0
	}
	deleted := 0
	for i := range list.Items {
		va := &list.Items[i]
		pv := va.Spec.Source.PersistentVolumeName
		if va.Spec.Attacher != DriverName || va.Spec.NodeName != nodeName || pv == nil || !pvNames[*pv] {
			continue
		}
		if va.DeletionTimestamp != nil {
			h.log.Debug("HAFailover.deleteVolumeAttachments: already being deleted", zap.String("volume_attachment", va.Name))
			continue
		}
		start := time.Now()
		err := h.kube.StorageV1().VolumeAttachments().Delete(ctx, va.Name, metav1.DeleteOptions{})
		h.log.Debug("HAFailover.deleteVolumeAttachments: delete returned", zap.String("volume_attachment", va.Name),
			zap.String("pv", *pv), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil && !apierrors.IsNotFound(err) {
			h.log.Warn("HA failover: delete volume attachment", zap.String("volume_attachment", va.Name), zap.Error(err))
			continue
		}
		if err == nil {
			deleted++
			h.log.Info("HA failover: deleted volume attachment", zap.String("node", nodeName),
				zap.String("volume_attachment", va.Name), zap.String("pv", *pv))
		}
	}
	return deleted
}

// haNodeRef is the object reference an event on a Node needs; a Node is
// cluster-scoped, and its events are filed by its name as UID.
func haNodeRef(name string) *corev1.ObjectReference {
	return &corev1.ObjectReference{Kind: "Node", Name: name, UID: types.UID(name)}
}
