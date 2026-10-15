package csi

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
)

const (
	// FailoverTaintKey is the NoSchedule taint the HA controller puts on a node
	// it moved Pods away from, so their replacements do not land back there.
	FailoverTaintKey = "haify.csi/lost-quorum"
	// FailoverOptOutKey, as an annotation or label with the value "false",
	// keeps the HA controller from deleting a Pod.
	FailoverOptOutKey = "haify.csi/ha-failover"
)

// Event reasons the HA controller posts.
const (
	ReasonFailoverPodDeleted   = "HaifyFailoverPodDeleted"
	ReasonFailoverNodeTainted  = "HaifyFailoverNodeTainted"
	ReasonFailoverNodeReturned = "HaifyFailoverNodeReturned"
)

// HAFailoverConfig tunes the HA controller. Zero values take the defaults.
type HAFailoverConfig struct {
	// Interval is how often the controller looks. Default 5s.
	Interval time.Duration
	// Grace is how long a node must look lost to Haify before the controller
	// acts while Kubernetes still reports it Ready. Default 10s.
	Grace time.Duration
	// MaxLostFraction: when more than this fraction of the cluster's nodes look
	// lost at once, the controller does nothing, because that is more likely a
	// network partition than that many machines dying. Default 0.5.
	MaxLostFraction float64
	// Concurrency bounds the volume status reads in flight. Default 8.
	Concurrency int
	// CallTimeout bounds one status read. Default 30s.
	CallTimeout time.Duration
}

func (c HAFailoverConfig) withDefaults() HAFailoverConfig {
	if c.Interval <= 0 {
		c.Interval = 5 * time.Second
	}
	if c.Grace < 0 {
		c.Grace = 0
	}
	if c.MaxLostFraction <= 0 || c.MaxLostFraction > 1 {
		c.MaxLostFraction = 0.5
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 8
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = 30 * time.Second
	}
	return c
}

// HAFailover moves Pods off a node that has died, without waiting for
// Kubernetes to notice.
//
// Kubernetes evicts Pods from an unreachable node only after the node has been
// unreachable for five minutes, and never replaces a StatefulSet Pod at all
// until the Pod is force-deleted, while Haify can bring the volume up on another
// replica in seconds. The controller closes that gap: when a node has dropped
// out of a volume's DRBD resource and the replicas left behind still hold
// quorum, it taints the node and force-deletes the Pods there that use the
// volume, and their StatefulSet or Deployment starts them on a node with a
// replica.
//
// DRBD quorum is what makes that safe: a node cut off from the majority can no
// longer write, so the Pod it ran is not writing anymore either. The controller
// therefore refuses whenever it cannot see that majority, and whenever so much
// of the cluster looks lost at once that the more likely story is a partition.
//
// Only one replica of the controller plugin runs it, the one holding its Lease.
type HAFailover struct {
	backend  HaifyBackend
	kube     kubernetes.Interface
	recorder record.EventRecorder
	cfg      HAFailoverConfig
	log      *zap.Logger
	now      func() time.Time
	source   haClusterSource

	// firstSeen is when each node was first seen lost, reset as soon as a
	// sweep does not see it lost.
	firstSeen map[string]time.Time
	// held is the reason the last sweep held back, so it is logged once
	// rather than every few seconds.
	held string
}

// NewHAFailover builds the HA controller. It reads the cluster straight from
// the API until Run switches it to informer caches.
func NewHAFailover(b HaifyBackend, kube kubernetes.Interface, recorder record.EventRecorder, cfg HAFailoverConfig, log *zap.Logger) *HAFailover {
	if log == nil {
		log = zap.NewNop()
	}
	cfg = cfg.withDefaults()
	log.Debug("NewHAFailover: building", zap.Duration("interval", cfg.Interval), zap.Duration("grace", cfg.Grace),
		zap.Float64("max_lost_fraction", cfg.MaxLostFraction), zap.Int("concurrency", cfg.Concurrency))
	return &HAFailover{backend: b, kube: kube, recorder: recorder, cfg: cfg, log: log, now: time.Now,
		source: haAPISource{kube: kube}, firstSeen: map[string]time.Time{}}
}

// Run sweeps until ctx is cancelled, which happens when this replica stops
// leading.
func (h *HAFailover) Run(ctx context.Context) {
	h.log.Debug("HAFailover.Run: starting", zap.Duration("interval", h.cfg.Interval))
	src, err := haStartInformers(ctx, h.kube, h.log)
	if err != nil {
		h.log.Warn("HA failover: informer caches", zap.Error(err))
		return
	}
	h.source = src
	h.log.Info("HA failover running", zap.Duration("interval", h.cfg.Interval), zap.Duration("grace", h.cfg.Grace),
		zap.Float64("max_lost_fraction", h.cfg.MaxLostFraction))
	t := time.NewTicker(h.cfg.Interval)
	defer t.Stop()
	for {
		h.Sweep(ctx)
		select {
		case <-ctx.Done():
			h.log.Debug("HAFailover.Run: stopping", zap.Error(ctx.Err()))
			return
		case <-t.C:
		}
	}
}

// haNodeLoss is everything one sweep found about one lost node.
type haNodeLoss struct {
	volumes map[string]haLossVerdict // PV name -> verdict
	pods    []haPodUse               // Pods on the node using a lost volume
}

// Sweep looks once and acts on what it finds. It is not safe to call from two
// goroutines at once.
func (h *HAFailover) Sweep(ctx context.Context) {
	start := time.Now()
	if ctx.Err() != nil {
		h.log.Debug("HAFailover.Sweep: not leading, nothing done", zap.Error(ctx.Err()))
		return
	}
	view, err := haReadCluster(ctx, h.source, h.log)
	if err != nil {
		h.hold(fmt.Sprintf("cannot read the cluster: %v", err))
		return
	}
	callStart := time.Now()
	haifyNodes, err := h.backend.ListNodes(ctx)
	h.log.Debug("HAFailover.Sweep: listed Haify nodes", zap.Int("count", len(haifyNodes)),
		zap.Duration("took", time.Since(callStart)), zap.Error(err))
	if err != nil {
		// Without the controller there is no view of any volume, and no view
		// is never evidence that a node is gone.
		h.hold(fmt.Sprintf("cannot reach the Haify controller: %v", err))
		return
	}

	losses := h.findLosses(ctx, view, haifyNodes)
	now := h.now()
	for name := range h.firstSeen {
		if losses[name] == nil {
			h.log.Debug("HAFailover.Sweep: node no longer looks lost", zap.String("node", name))
			delete(h.firstSeen, name)
		}
	}
	for name := range losses {
		if _, ok := h.firstSeen[name]; !ok {
			h.firstSeen[name] = now
		}
	}

	if reason := h.partitionReason(view, losses); reason != "" {
		h.hold(reason)
	} else {
		h.resume()
		for _, name := range haSortedKeys(losses) {
			node := view.nodes[name]
			ready := haNodeReady(node)
			lostFor := now.Sub(h.firstSeen[name])
			if ready && lostFor < h.cfg.Grace {
				h.log.Debug("HAFailover.Sweep: node lost but Ready, inside grace", zap.String("node", name),
					zap.Duration("lost_for", lostFor), zap.Duration("grace", h.cfg.Grace))
				continue
			}
			h.log.Debug("HAFailover.Sweep: failing over", zap.String("node", name), zap.Bool("ready", ready),
				zap.Duration("lost_for", lostFor))
			h.failover(ctx, name, ready, losses[name])
		}
	}
	h.releaseTaints(ctx, view, haifyNodes, losses)
	h.log.Debug("HAFailover.Sweep: done", zap.Int("lost_nodes", len(losses)), zap.Duration("took", time.Since(start)))
}

// findLosses reads every Haify volume a Pod is using and returns, per node,
// the volumes that node has dropped out of while the rest hold quorum.
func (h *HAFailover) findLosses(ctx context.Context, view *haClusterView, haifyNodes []*haifypb.NodeInfo) map[string]*haNodeLoss {
	h.log.Debug("HAFailover.findLosses: start", zap.Int("nodes_with_haify_pods", len(view.uses)), zap.Int("haify_nodes", len(haifyNodes)))
	handles := map[string]bool{}
	for nodeName, uses := range view.uses {
		if view.nodes[nodeName] == nil {
			// A deleted Node's Pods are the pod garbage collector's.
			continue
		}
		for _, u := range uses {
			for _, pv := range u.pvs {
				handles[pv.Spec.CSI.VolumeHandle] = true
			}
		}
	}
	resources := h.readVolumes(ctx, handles)

	losses := map[string]*haNodeLoss{}
	for nodeName, uses := range view.uses {
		node := view.nodes[nodeName]
		if node == nil {
			continue
		}
		id := haIdentityFor(node, haifyNodes)
		verdicts := map[string]haLossVerdict{}
		var affected []haPodUse
		for _, u := range uses {
			hit := false
			for _, pv := range u.pvs {
				v, seen := verdicts[pv.Name]
				if !seen {
					v = haJudgeNodeLoss(resources[pv.Spec.CSI.VolumeHandle], id)
					verdicts[pv.Name] = v
					h.log.Debug("HAFailover.findLosses: verdict", zap.String("node", nodeName), zap.String("pv", pv.Name),
						zap.Bool("lost", v.lost), zap.String("reason", v.reason))
				}
				hit = hit || v.lost
			}
			if hit {
				affected = append(affected, u)
			}
		}
		if len(affected) == 0 {
			continue
		}
		loss := &haNodeLoss{volumes: map[string]haLossVerdict{}, pods: affected}
		for pvName, v := range verdicts {
			if v.lost {
				loss.volumes[pvName] = v
			}
		}
		losses[nodeName] = loss
	}
	return losses
}

// readVolumes reads the controller's view of each volume, a few at a time. A
// volume whose read fails is left out: no view is no evidence.
func (h *HAFailover) readVolumes(ctx context.Context, handles map[string]bool) map[string]*haifypb.ResourceInfo {
	h.log.Debug("HAFailover.readVolumes: start", zap.Int("volumes", len(handles)), zap.Int("concurrency", h.cfg.Concurrency))
	out := map[string]*haifypb.ResourceInfo{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, h.cfg.Concurrency)
	for handle := range handles {
		wg.Add(1)
		sem <- struct{}{}
		go func(handle string) {
			defer wg.Done()
			defer func() { <-sem }()
			callCtx, cancel := context.WithTimeout(ctx, h.cfg.CallTimeout)
			defer cancel()
			start := time.Now()
			r, err := h.backend.GetResource(callCtx, handle)
			h.log.Debug("HAFailover.readVolumes: got resource", zap.String("resource", handle),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			if err != nil {
				return
			}
			mu.Lock()
			out[handle] = r
			mu.Unlock()
		}(handle)
	}
	wg.Wait()
	return out
}

// partitionReason says why the sweep must not act, or "" when it may. Nodes
// Kubernetes reports not Ready count toward the lost ones, because a partition
// shows up there too.
func (h *HAFailover) partitionReason(view *haClusterView, losses map[string]*haNodeLoss) string {
	if len(losses) == 0 {
		return ""
	}
	lost := map[string]bool{}
	for name := range losses {
		lost[name] = true
	}
	for name, n := range view.nodes {
		if !haNodeReady(n) {
			lost[name] = true
		}
	}
	total := len(view.nodes)
	h.log.Debug("HAFailover.partitionReason: counting", zap.Int("lost", len(lost)), zap.Int("nodes", total),
		zap.Float64("max_lost_fraction", h.cfg.MaxLostFraction))
	if total == 0 || float64(len(lost)) > h.cfg.MaxLostFraction*float64(total) {
		return fmt.Sprintf("%d of %d nodes look lost, more than %.0f%%; treating it as a partition and moving nothing",
			len(lost), total, h.cfg.MaxLostFraction*100)
	}
	return ""
}

// failover taints the node, then force-deletes the Pods on it that use a lost
// volume. The taint goes first: a replacement Pod scheduled before it could be
// placed right back on the dead node, which Kubernetes may still call Ready.
func (h *HAFailover) failover(ctx context.Context, nodeName string, ready bool, loss *haNodeLoss) {
	h.log.Debug("HAFailover.failover: start", zap.String("node", nodeName), zap.Bool("ready", ready),
		zap.Int("pods", len(loss.pods)), zap.Int("volumes", len(loss.volumes)))
	var pods []haPodUse
	for _, u := range loss.pods {
		switch {
		case haOptedOut(u.pod):
			h.log.Debug("HAFailover.failover: pod opted out", zap.String("namespace", u.pod.Namespace), zap.String("pod", u.pod.Name))
		case !haHasController(u.pod):
			h.log.Debug("HAFailover.failover: bare pod, nothing would replace it", zap.String("namespace", u.pod.Namespace),
				zap.String("pod", u.pod.Name))
		default:
			pods = append(pods, u)
		}
	}
	if len(pods) == 0 {
		h.log.Debug("HAFailover.failover: no pod to move", zap.String("node", nodeName))
		return
	}
	if ctx.Err() != nil {
		h.log.Debug("HAFailover.failover: stopped leading", zap.String("node", nodeName))
		return
	}

	why := haLossSummary(nodeName, ready, loss)
	added, err := h.setFailoverTaint(ctx, nodeName, true)
	if err != nil {
		// Deleting the Pods without the taint could see them rescheduled onto
		// the very node they are being moved off.
		h.log.Warn("HA failover: taint node, pods left in place", zap.String("node", nodeName), zap.Error(err))
		return
	}
	if added {
		h.log.Info("HA failover: tainted lost node", zap.String("node", nodeName), zap.String("taint", FailoverTaintKey),
			zap.String("why", why))
		h.recorder.Event(haNodeRef(nodeName), corev1.EventTypeWarning, ReasonFailoverNodeTainted,
			fmt.Sprintf("Tainted %s:NoSchedule: %s", FailoverTaintKey, why))
	}

	pvNames := map[string]bool{}
	for _, u := range pods {
		if ctx.Err() != nil {
			h.log.Debug("HAFailover.failover: stopped leading mid-way", zap.String("node", nodeName))
			return
		}
		deleted, err := h.forceDeletePod(ctx, u.pod)
		if err != nil {
			h.log.Warn("HA failover: force-delete pod", zap.String("namespace", u.pod.Namespace), zap.String("pod", u.pod.Name),
				zap.Error(err))
			continue
		}
		var lostPVs []string
		for _, pv := range u.pvs {
			if _, ok := loss.volumes[pv.Name]; ok {
				lostPVs = append(lostPVs, pv.Name)
				pvNames[pv.Name] = true
			}
		}
		if !deleted {
			continue
		}
		h.log.Info("HA failover: force-deleted pod", zap.String("node", nodeName), zap.String("namespace", u.pod.Namespace),
			zap.String("pod", u.pod.Name), zap.Strings("pvs", lostPVs), zap.String("why", why))
		msg := fmt.Sprintf("Force-deleted so its controller recreates it on a node with a replica: %s", why)
		h.recorder.Event(u.pod, corev1.EventTypeWarning, ReasonFailoverPodDeleted, msg)
		for _, pv := range u.pvs {
			if _, ok := loss.volumes[pv.Name]; ok && pv.Spec.ClaimRef != nil {
				h.recorder.Event(pv.Spec.ClaimRef, corev1.EventTypeWarning, ReasonFailoverPodDeleted,
					fmt.Sprintf("Pod %s/%s force-deleted to move this volume off %s: %s", u.pod.Namespace, u.pod.Name, nodeName, why))
			}
		}
	}
	h.deleteVolumeAttachments(ctx, nodeName, pvNames)
}

// haLossSummary is the one sentence an event or log line gives as the reason.
func haLossSummary(nodeName string, ready bool, loss *haNodeLoss) string {
	var parts []string
	for _, pvName := range haSortedKeys(loss.volumes) {
		v := loss.volumes[pvName]
		parts = append(parts, fmt.Sprintf("%s (%s; %s hold quorum)", pvName, v.reason, strings.Join(v.survivors, ", ")))
	}
	k8s := "Kubernetes reports it not Ready"
	if ready {
		k8s = "Kubernetes still reports it Ready"
	}
	return fmt.Sprintf("node %s lost its DRBD connection to the other replicas of %s; %s",
		nodeName, strings.Join(parts, ", "), k8s)
}

// hold records that the sweep held back, logging it once per reason.
func (h *HAFailover) hold(reason string) {
	if reason == h.held {
		h.log.Debug("HAFailover.hold: still holding back", zap.String("reason", reason))
		return
	}
	h.held = reason
	h.log.Warn("HA failover: holding back", zap.String("reason", reason))
}

// resume clears a hold, logging that it ended.
func (h *HAFailover) resume() {
	if h.held == "" {
		return
	}
	h.log.Info("HA failover: no longer holding back", zap.String("was", h.held))
	h.held = ""
}

func haSortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
