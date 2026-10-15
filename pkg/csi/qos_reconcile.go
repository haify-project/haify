package csi

import (
	"context"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"go.uber.org/zap"
)

// Keeping the I/O limits of published volumes current.
//
// A volume's limits start out in its volume context, which Kubernetes fixes at
// provisioning. A VolumeAttributesClass changes them later by setting
// qosLabelPrefix labels on the resource (modify.go). The node plugin applies
// "label, else volume context" when it publishes a volume, and every interval
// reads each published volume's labels again — one GetResource per volume —
// and rewrites io.max where the result changed.

// qosOrder is the order io.max lists its keys in.
var qosOrder = []string{"readBytesPerSecond", "writeBytesPerSecond", "readIOPS", "writeIOPS"}

// effectiveQoS is the limits a volume should have now: a label set through a
// VolumeAttributesClass wins over the volume context, and "max" in either is
// no limit. Only limited keys are returned.
func effectiveQoS(volumeContext, labels map[string]string) map[string]string {
	out := map[string]string{}
	for _, param := range qosOrder {
		v := volumeContext[param]
		if lv, ok := labels[qosLabelPrefix+param]; ok {
			v = lv
		}
		if v != "" && !strings.EqualFold(v, qosUnlimited) {
			out[param] = v
		}
	}
	return out
}

// ioMaxLimits renders limits as io.max keys, every key present so that a
// lifted limit is written as "max" and actually goes away.
func ioMaxLimits(limits map[string]string) string {
	parts := make([]string, 0, len(qosOrder))
	for _, param := range qosOrder {
		v := limits[param]
		if v == "" {
			v = qosUnlimited
		}
		parts = append(parts, qosParams[param]+"="+v)
	}
	return strings.Join(parts, " ")
}

// qosRecord is one published target whose limits the node keeps current.
type qosRecord struct {
	VolumeID string `json:"volume_id"`
	Target   string `json:"target"`
	// Context holds the QoS keys of the volume context, the fallback for any
	// key no label sets.
	Context map[string]string `json:"context,omitempty"`
}

// qosTracker remembers the published targets and what was last written for
// each, so a pass writes io.max only where something changed. Its methods
// accept a nil tracker, which applies limits without remembering anything.
type qosTracker struct {
	mu      sync.Mutex
	records map[string]qosRecord // by target
	applied map[string]string    // target -> limits last written
	lastErr map[string]string    // target -> last write error, to warn once
	dir     string               // where records persist; "" keeps them in memory
	log     *zap.Logger
	write   func(device, target, limits string) error
}

func newQoSTracker(log *zap.Logger) *qosTracker {
	return &qosTracker{records: map[string]qosRecord{}, applied: map[string]string{}, lastErr: map[string]string{},
		log: log, write: writeIOMax}
}

// qosContext keeps only the QoS keys of a volume context.
func qosContext(volumeContext map[string]string) map[string]string {
	var out map[string]string
	for param := range qosParams {
		if v := volumeContext[param]; v != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[param] = v
		}
	}
	return out
}

func (t *qosTracker) track(rec qosRecord) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.records[rec.Target] = rec
	t.mu.Unlock()
	t.save(rec)
	t.log.Debug("qosTracker.track: tracking", zap.String("resource", rec.VolumeID), zap.String("target", rec.Target),
		zap.Int("context_limits", len(rec.Context)))
}

func (t *qosTracker) forget(target string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	_, known := t.records[target]
	delete(t.records, target)
	delete(t.applied, target)
	delete(t.lastErr, target)
	t.mu.Unlock()
	t.remove(target)
	t.log.Debug("qosTracker.forget: forgot", zap.String("target", target), zap.Bool("was_tracked", known))
}

// apply writes limits for target unless they are what was written last. A
// volume that never had a limit is left alone, so nodes without the io
// controller are not bothered for volumes that do not use it.
func (t *qosTracker) apply(device, target string, limits map[string]string) {
	log := zap.L()
	line := ioMaxLimits(limits)
	var last string
	if t != nil {
		log = t.log
		t.mu.Lock()
		last = t.applied[target]
		t.mu.Unlock()
	}
	if len(limits) == 0 && last == "" {
		log.Debug("qosTracker.apply: no limits, none written before", zap.String("target", target))
		return
	}
	if line == last {
		log.Debug("qosTracker.apply: unchanged", zap.String("target", target), zap.String("limits", line))
		return
	}
	write := writeIOMax
	if t != nil {
		write = t.write
	}
	err := write(device, target, line)
	log.Debug("qosTracker.apply: wrote io.max", zap.String("device", device), zap.String("target", target),
		zap.String("limits", line), zap.String("previous", last), zap.Error(err))
	if t == nil {
		if err != nil {
			log.Warn("I/O limits for the volume were not applied", zap.String("target", target), zap.Error(err))
		}
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		// Warn once per distinct failure: on a node without the io
		// controller the same error would otherwise repeat every pass.
		if t.lastErr[target] != err.Error() {
			t.log.Warn("I/O limits for the volume were not applied; it stays usable without them",
				zap.String("target", target), zap.String("device", device), zap.Error(err))
			t.lastErr[target] = err.Error()
		}
		return
	}
	if _, tracked := t.records[target]; tracked {
		t.applied[target] = line
	}
	delete(t.lastErr, target)
}

// publishQoS applies a volume's limits as it is published and starts keeping
// them current. device may be empty for a filesystem volume, whose device
// comes from the same GetResource that reads the labels. A failure here never
// fails the publish: a missing limit is no reason to keep a Pod from its data.
func (s *nodeServer) publishQoS(ctx context.Context, req *csi.NodePublishVolumeRequest, device string) {
	id, target := req.GetVolumeId(), req.GetTargetPath()
	rec := qosRecord{VolumeID: id, Target: target, Context: qosContext(req.GetVolumeContext())}
	start := time.Now()
	r, err := s.backend.GetResource(ctx, id)
	s.log.Debug("nodeServer.publishQoS: read volume labels", zap.String("resource", id), zap.String("target", target),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	s.qos.track(rec)
	if err != nil {
		if len(rec.Context) > 0 {
			s.log.Warn("I/O limits not applied at publish; the next pass retries", zap.String("resource", id), zap.Error(err))
		}
		return
	}
	if device == "" && len(r.GetVolumes()) > 0 {
		device = r.GetVolumes()[0].GetDevice()
	}
	if device == "" {
		s.log.Debug("nodeServer.publishQoS: no device", zap.String("resource", id))
		return
	}
	s.qos.apply(device, target, effectiveQoS(rec.Context, r.GetLabels()))
}

// EnableQoSReconcile keeps the I/O limits of the volumes this node plugin
// publishes current: records persist in stateDir (empty: memory only, lost on
// a restart) and every interval each volume's limits are read again. An
// interval of 0 only applies limits at publish. ns must come from
// NewNodeServer.
func EnableQoSReconcile(ctx context.Context, ns csi.NodeServer, stateDir string, interval time.Duration) {
	s, ok := ns.(*nodeServer)
	if !ok || s.qos == nil {
		zap.L().Debug("EnableQoSReconcile: not a Haify node server")
		return
	}
	s.log.Debug("EnableQoSReconcile: start", zap.String("state_dir", stateDir), zap.Duration("interval", interval))
	s.qos.dir = stateDir
	s.qos.load()
	if interval <= 0 {
		s.log.Debug("EnableQoSReconcile: periodic pass disabled")
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		s.reconcileQoS(ctx)
		for {
			select {
			case <-ctx.Done():
				s.log.Debug("EnableQoSReconcile: stopping", zap.Error(ctx.Err()))
				return
			case <-ticker.C:
				s.reconcileQoS(ctx)
			}
		}
	}()
}

// reconcileQoS is one pass: one GetResource per published volume, and io.max
// rewritten for each target whose limits changed. A target whose path is gone
// was unpublished while the plugin was not looking, and is dropped.
func (s *nodeServer) reconcileQoS(ctx context.Context) {
	s.qos.mu.Lock()
	byVolume := map[string][]qosRecord{}
	for _, rec := range s.qos.records {
		byVolume[rec.VolumeID] = append(byVolume[rec.VolumeID], rec)
	}
	s.qos.mu.Unlock()
	ids := make([]string, 0, len(byVolume))
	for id := range byVolume {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	s.log.Debug("nodeServer.reconcileQoS: pass", zap.Int("volumes", len(ids)))
	for _, id := range ids {
		var live []qosRecord
		for _, rec := range byVolume[id] {
			if _, err := os.Stat(rec.Target); os.IsNotExist(err) {
				s.log.Debug("nodeServer.reconcileQoS: target gone, forgetting it", zap.String("resource", id),
					zap.String("target", rec.Target))
				s.qos.forget(rec.Target)
				continue
			}
			live = append(live, rec)
		}
		if len(live) == 0 {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		start := time.Now()
		r, err := s.backend.GetResource(callCtx, id)
		cancel()
		s.log.Debug("nodeServer.reconcileQoS: read volume", zap.String("resource", id), zap.Int("targets", len(live)),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil || len(r.GetVolumes()) == 0 || r.GetVolumes()[0].GetDevice() == "" {
			continue
		}
		device := r.GetVolumes()[0].GetDevice()
		for _, rec := range live {
			s.qos.apply(device, rec.Target, effectiveQoS(rec.Context, r.GetLabels()))
		}
	}
}
