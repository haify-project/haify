package csi

import (
	"context"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
)

// labelSetter is the part of the Haify client that changes a resource's
// labels.
type labelSetter interface {
	SetResourceLabels(ctx context.Context, resource string, set map[string]string, remove []string) (map[string]string, error)
}

// syncClaimLabel keeps the haify.csi/pvc label on the claim the volume is
// bound to now. The label is written at provisioning from the claim that asked
// for the volume, and CDI provisions for a claim of its own (prime-<uid> for an
// import, tmp-pvc-<uid> for a clone) and then rebinds the volume to the
// DataVolume's claim: every KubeVirt and Harvester disk would otherwise be
// named after a claim that no longer exists, in the web UI and in alerts.
func (h *HealthReporter) syncClaimLabel(ctx context.Context, pv *corev1.PersistentVolume, r *haifypb.ResourceInfo) {
	if r.GetLabels()[managedByLabel] != managedByValue {
		h.log.Debug("HealthReporter.syncClaimLabel: not a volume the driver made, left alone", zap.String("pv", pv.Name))
		return
	}
	want := pv.Spec.ClaimRef.Namespace + "/" + pv.Spec.ClaimRef.Name
	have := r.GetLabels()[pvcLabel]
	if have == want {
		return
	}
	setter, ok := h.backend.(labelSetter)
	if !ok {
		h.log.Debug("HealthReporter.syncClaimLabel: backend cannot set labels", zap.String("pv", pv.Name))
		return
	}
	start := time.Now()
	_, err := setter.SetResourceLabels(ctx, r.GetName(), map[string]string{pvcLabel: want}, nil)
	h.log.Debug("HealthReporter.syncClaimLabel: relabelled", zap.String("pv", pv.Name), zap.String("resource", r.GetName()),
		zap.String("from", have), zap.String("to", want), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		h.log.Warn("health sweep: update the claim label", zap.String("resource", r.GetName()), zap.Error(err))
	}
}
