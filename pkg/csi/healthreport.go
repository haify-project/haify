package csi

import (
	"context"
	"regexp"
	"sync"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
)

// Event reasons the health reporter posts on a PVC.
const (
	ReasonVolumeDegraded  = "VolumeDegraded"
	ReasonVolumeRecovered = "VolumeRecovered"
)

// HealthReporter puts the health of each volume's DRBD replicas where a
// Kubernetes user looks: as events on the PVC.
//
// CSI had a channel for this, VolumeCondition, and spec v1.13.0 removed it. The
// state it would have carried still matters — a volume whose replica went
// StandAlone is running on fewer copies than its StorageClass promised, and
// nothing in `kubectl describe pvc` said so — so the controller plugin reports
// it directly.
//
// Events are posted on transitions only. Every sweep would otherwise repeat the
// same warning, and a PVC with fifty identical events is one people stop reading.
type HealthReporter struct {
	backend  HaifyBackend
	kube     kubernetes.Interface
	recorder record.EventRecorder
	interval time.Duration
	log      *zap.Logger

	mu   sync.Mutex
	last map[string]string // PV name -> last reported message ("" = healthy)
	// established records PVs seen with every replica up to date at least
	// once. Until then a resync is the volume's initial sync, not a fault.
	established map[string]bool
}

// NewHealthReporter builds a reporter. interval is how often volumes are swept.
func NewHealthReporter(b HaifyBackend, kube kubernetes.Interface, recorder record.EventRecorder, interval time.Duration, log *zap.Logger) *HealthReporter {
	if log == nil {
		log = zap.NewNop()
	}
	log.Debug("NewHealthReporter: building", zap.Duration("interval", interval))
	return &HealthReporter{backend: b, kube: kube, recorder: recorder, interval: interval, log: log,
		last: map[string]string{}, established: map[string]bool{}}
}

// Run sweeps until ctx is cancelled.
func (h *HealthReporter) Run(ctx context.Context) {
	h.log.Debug("HealthReporter.Run: starting", zap.Duration("interval", h.interval))
	t := time.NewTicker(h.interval)
	defer t.Stop()
	for {
		h.Sweep(ctx)
		select {
		case <-ctx.Done():
			h.log.Debug("HealthReporter.Run: stopping", zap.Error(ctx.Err()))
			return
		case <-t.C:
			h.log.Debug("HealthReporter.Run: tick")
		}
	}
}

// Sweep checks every volume this driver provisioned once.
func (h *HealthReporter) Sweep(ctx context.Context) {
	sweepStart := time.Now()
	pvs, err := h.kube.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		h.log.Warn("health sweep: list persistent volumes", zap.Error(err))
		return
	}
	h.log.Debug("HealthReporter.Sweep: listed persistent volumes", zap.Int("count", len(pvs.Items)),
		zap.Duration("took", time.Since(sweepStart)))
	seen := map[string]bool{}
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != DriverName || pv.Spec.ClaimRef == nil {
			continue
		}
		seen[pv.Name] = true
		h.check(ctx, pv)
	}
	// Forget PVs that are gone, so a recreated one with the same name starts
	// from nothing rather than from a stranger's history.
	forgotten := 0
	h.mu.Lock()
	for name := range h.last {
		if !seen[name] {
			h.log.Debug("HealthReporter.Sweep: forgetting PV that is gone", zap.String("pv", name))
			delete(h.last, name)
			forgotten++
		}
	}
	for name := range h.established {
		if !seen[name] {
			delete(h.established, name)
		}
	}
	h.mu.Unlock()
	h.log.Debug("HealthReporter.Sweep: done", zap.Int("checked", len(seen)), zap.Int("forgotten", forgotten),
		zap.Duration("took", time.Since(sweepStart)))
}

func (h *HealthReporter) check(ctx context.Context, pv *corev1.PersistentVolume) {
	start := time.Now()
	r, err := h.backend.GetResource(ctx, pv.Spec.CSI.VolumeHandle)
	h.log.Debug("HealthReporter.check: got resource", zap.String("pv", pv.Name), zap.String("resource", pv.Spec.CSI.VolumeHandle),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		// The controller not answering is not the volume being unhealthy. A
		// VolumeDegraded posted on every PVC in the cluster because the
		// controller failed over for four seconds would be exactly the false
		// alarm this reporter exists to replace.
		h.log.Debug("health sweep: get resource", zap.String("pv", pv.Name), zap.Error(err))
		return
	}
	abnormal, msg, onlyResync := volumeHealthDetail(r)
	current := ""
	if abnormal {
		current = healthSignature(msg)
	}

	h.mu.Lock()
	if !abnormal {
		h.established[pv.Name] = true
	}
	// A new volume's replicas start empty and sync once. Posting a warning for
	// that put a VolumeDegraded on every PVC the moment it was created, which
	// teaches people that the event means nothing. A disconnected, diskless or
	// outdated replica is still reported whatever the volume's age.
	if abnormal && onlyResync && !h.established[pv.Name] {
		h.mu.Unlock()
		h.log.Debug("HealthReporter.check: initial sync of a new volume, not reported", zap.String("pv", pv.Name))
		return
	}
	prev, known := h.last[pv.Name]
	h.last[pv.Name] = current
	h.mu.Unlock()

	claim := pv.Spec.ClaimRef
	switch {
	case abnormal && (!known || prev != current):
		// First sight counts: a volume already degraded when the plugin starts
		// is the one an operator most needs to hear about. So does a change of
		// message, since "resyncing 40%" becoming "StandAlone" is news.
		h.log.Debug("HealthReporter.check: posting VolumeDegraded", zap.String("pv", pv.Name),
			zap.String("namespace", claim.Namespace), zap.String("pvc", claim.Name), zap.Bool("first_sight", !known), zap.String("message", msg))
		h.recorder.Event(claim, corev1.EventTypeWarning, ReasonVolumeDegraded, msg)
	case !abnormal && known && prev != "":
		h.log.Debug("HealthReporter.check: posting VolumeRecovered", zap.String("pv", pv.Name),
			zap.String("namespace", claim.Namespace), zap.String("pvc", claim.Name))
		h.recorder.Event(claim, corev1.EventTypeNormal, ReasonVolumeRecovered, msg)
	default:
		h.log.Debug("HealthReporter.check: no change", zap.String("pv", pv.Name), zap.Bool("abnormal", abnormal),
			zap.Bool("known", known))
	}
}

// resyncPercent matches the progress figure volumeHealth puts in a resyncing
// replica's message.
var resyncPercent = regexp.MustCompile(`\(\d+(\.\d+)?%\)`)

// healthSignature is what decides whether a message is news. A resync reports
// a new percentage every sweep, and treating 40% -> 41% as a change would post a
// warning a minute for the length of the resync.
func healthSignature(msg string) string {
	return resyncPercent.ReplaceAllString(msg, "(…)")
}
