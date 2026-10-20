package csi

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
)

func boundPV(name, handle, claim string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: DriverName, VolumeHandle: handle}},
			ClaimRef:               &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: "default", Name: claim},
		},
	}
}

// CDI provisions a clone for tmp-pvc-<uid> and then rebinds the volume to the
// DataVolume's claim; the label follows the claim the volume is bound to.
func TestSweepRelabelsAVolumeCDIRebound(t *testing.T) {
	f := newModifyFake("n1", "n2")
	ctx := context.Background()
	require.NoError(t, f.CreateResourceWithPoolAndType(ctx, "pvc_a", 0, []string{"n1", "n2"}, "C", 1, "vg0", "lvm", nil))
	f.resources["pvc_a"].Labels = map[string]string{managedByLabel: managedByValue, pvcLabel: "default/tmp-pvc-84d56"}
	require.NoError(t, f.CreateResourceWithPoolAndType(ctx, "share", 0, []string{"n1", "n2"}, "C", 1, "vg0", "lvm", nil))

	kube := kubefake.NewSimpleClientset(boundPV("pv-a", "pvc_a", "vm1-root"), boundPV("pv-static", "share", "static"))
	rep := NewHealthReporter(f, kube, record.NewFakeRecorder(10), 0, zap.NewNop())

	rep.Sweep(ctx)
	assert.Equal(t, "default/vm1-root", f.resources["pvc_a"].Labels[pvcLabel])
	assert.Empty(t, f.resources["share"].Labels[pvcLabel], "a resource the driver did not make is left alone")
	require.Len(t, f.labelCalls, 1)

	rep.Sweep(ctx)
	assert.Len(t, f.labelCalls, 1, "a label already right is not written again")
}
