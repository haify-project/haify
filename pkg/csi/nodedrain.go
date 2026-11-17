package csi

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
)

const (
	// DrainRequestKey, as an annotation or a label on a Node, says what the
	// drain controller is to do with it: "true" asks for the node to be
	// drained in Haify, "false" keeps the controller away from it whatever
	// else the node shows. The annotation wins over the label.
	DrainRequestKey = "haify.csi/drain"
	// DrainStateAnnotation is the controller's own record on the Node of a
	// drain it started (drainStateDraining) or finished (drainStateDrained).
	// It is what makes the controller undrain only what it drained, and what
	// a restarted or newly leading replica picks up from.
	DrainStateAnnotation = "haify.csi/drain-state"

	drainStateDraining = "draining"
	drainStateDrained  = "drained"

	// drainRequester is who Haify records as having drained the node, so
	// that `haify node list` says the maintenance came from Kubernetes.
	drainRequester = "kubernetes drain"
)

// Event reasons the drain controller posts on a Node.
const (
	ReasonNodeDrained       = "HaifyNodeDrained"
	ReasonNodeDrainWaiting  = "HaifyNodeDrainWaiting"
	ReasonNodeDrainFailed   = "HaifyNodeDrainFailed"
	ReasonNodeUndrained     = "HaifyNodeUndrained"
	ReasonNodeUndrainFailed = "HaifyNodeUndrainFailed"
)

// DefaultDrainTaints are the taints maintenance tooling puts on a node it is
// emptying: the node-maintenance operator's, and the one KubeVirt reads.
var DefaultDrainTaints = []string{"medik8s.io/drain", "kubevirt.io/drain"}

// NodeDrainConfig tunes the drain controller. Zero values take the defaults.
type NodeDrainConfig struct {
	// Interval is how often the controller looks. Default 10s.
	Interval time.Duration
	// Delay is how long a node must keep asking to be drained before the
	// controller acts. Default 30s; negative means none.
	Delay time.Duration
	// Retry is the pause between attempts at a drain or undrain that failed.
	// Default 1m.
	Retry time.Duration
	// OnCordon also treats any cordoned node as one to drain. Off by default:
	// nodes are cordoned for reasons that have nothing to do with taking them
	// down.
	OnCordon bool
	// Taints are the taint keys that mark a node as under maintenance.
	Taints []string
	// MaxNodes is how many nodes may be drained in Haify at once, counting
	// ones an operator drained there by hand. Default 1.
	MaxNodes int
	// CallTimeout bounds one drain or undrain call. A drain moves services
	// between nodes and can take minutes. Default 10m.
	CallTimeout time.Duration
}

func (c NodeDrainConfig) withDefaults() NodeDrainConfig {
	if c.Interval <= 0 {
		c.Interval = 10 * time.Second
	}
	if c.Delay == 0 {
		c.Delay = 30 * time.Second
	}
	if c.Delay < 0 {
		c.Delay = 0
	}
	if c.Retry <= 0 {
		c.Retry = time.Minute
	}
	if c.MaxNodes <= 0 {
		c.MaxNodes = 1
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = 10 * time.Minute
	}
	return c
}

// nodeDrainBackend is the part of the Haify controller the drain controller
// needs. *client.HaifyClient satisfies it.
type nodeDrainBackend interface {
	ListNodes(ctx context.Context) ([]*haifypb.NodeInfo, error)
	// DrainNodeWith returns the reply alongside an error when the controller
	// sent one: a drain that could not move everything says what it moved.
	DrainNodeWith(ctx context.Context, req *haifypb.DrainNodeRequest) (*haifypb.DrainNodeResponse, error)
	UndrainNode(ctx context.Context, name string) error
}

// NodeDrain carries a Kubernetes node's maintenance over to Haify.
//
// `kubectl drain` moves the Pods off a node and tells Haify nothing: the node
// goes on being offered new replicas, and whatever Haify itself runs there —
// an NFS, iSCSI or NVMe-oF gateway, an HA service, the controller's own
// metadata — is still Primary on it when it is switched off, and only then
// fails over. This controller drains the node in Haify as well (`haify node
// drain`), and returns it to service when the maintenance is over.
//
// A Haify drain moves Primary roles, so it is not something to do on a
// guess. The controller acts only on a node that asks for it (drainWanted),
// and only after it has asked for Delay. It then drains in two steps.
//
// The first comes at once, while Pods are still leaving: the node goes into
// maintenance in Haify, so it is offered no new replica from that moment, and
// what Haify itself keeps Primary there moves away. The volumes of the Pods
// still on the node are left exactly as they are (a keep_in_use drain): such
// a volume is Primary where its Pod runs, and moving that role from under a
// running Pod is the eviction's job, not this one's. Without this step a node
// whose Pods take an hour to leave went on receiving new replicas for that
// hour, each of which then had to be moved or left behind on a machine about
// to be switched off.
//
// The second comes when no Pod on the node uses a Haify volume any more: the
// drain is repeated without the exception, which moves whatever is still
// Primary, and the node is recorded as drained.
//
// What it has done is recorded on the Node (DrainStateAnnotation), never in
// memory, so a restart repeats nothing and a node an operator drained in
// Haify by hand is never undrained here.
//
// Only one replica of the controller plugin runs it, the one holding its Lease.
type NodeDrain struct {
	backend  nodeDrainBackend
	kube     kubernetes.Interface
	recorder record.EventRecorder
	cfg      NodeDrainConfig
	log      *zap.Logger
	now      func() time.Time
	// nodes lists the cluster's nodes: straight from the API until Run
	// switches it to an informer cache.
	nodes func(ctx context.Context) ([]*corev1.Node, error)

	// asking is when each node was first seen asking to be drained, reset as
	// soon as a sweep sees it not asking.
	asking map[string]time.Time
	// attempted is when a drain or undrain of each node was last tried.
	attempted map[string]time.Time
	// said is the last event posted on each node, so a node that waits or
	// keeps failing for the same reason gets one event, not one per sweep.
	said map[string]string
	// held has the nodes whose first step is done: in maintenance, with
	// only the volumes of their remaining Pods still Primary. The value is
	// what that step moved, so the event that says so stays the same event
	// while the node waits. It is memory only; a replica that starts
	// without it repeats the step once, which moves nothing that has
	// already moved.
	held map[string][]string
}

// NewNodeDrain builds the drain controller.
func NewNodeDrain(b nodeDrainBackend, kube kubernetes.Interface, recorder record.EventRecorder, cfg NodeDrainConfig, log *zap.Logger) *NodeDrain {
	if log == nil {
		log = zap.NewNop()
	}
	cfg = cfg.withDefaults()
	log.Debug("NewNodeDrain: building", zap.Duration("interval", cfg.Interval), zap.Duration("delay", cfg.Delay),
		zap.Duration("retry", cfg.Retry), zap.Bool("on_cordon", cfg.OnCordon), zap.Strings("taints", cfg.Taints),
		zap.Int("max_nodes", cfg.MaxNodes))
	return &NodeDrain{backend: b, kube: kube, recorder: recorder, cfg: cfg, log: log, now: time.Now,
		nodes:  haAPISource{kube: kube}.nodes,
		asking: map[string]time.Time{}, attempted: map[string]time.Time{}, said: map[string]string{}, held: map[string][]string{}}
}

// Run sweeps until ctx is cancelled, which happens when this replica stops
// leading.
func (d *NodeDrain) Run(ctx context.Context) {
	d.log.Debug("NodeDrain.Run: starting", zap.Duration("interval", d.cfg.Interval))
	lister, err := drainStartInformer(ctx, d.kube, d.log)
	if err != nil {
		d.log.Warn("node drain: informer cache", zap.Error(err))
		return
	}
	d.nodes = lister
	d.log.Info("node drain running", zap.Duration("interval", d.cfg.Interval), zap.Duration("delay", d.cfg.Delay),
		zap.Bool("on_cordon", d.cfg.OnCordon), zap.Strings("taints", d.cfg.Taints), zap.Int("max_nodes", d.cfg.MaxNodes))
	t := time.NewTicker(d.cfg.Interval)
	defer t.Stop()
	for {
		d.Sweep(ctx)
		select {
		case <-ctx.Done():
			d.log.Debug("NodeDrain.Run: stopping", zap.Error(ctx.Err()))
			return
		case <-t.C:
		}
	}
}

// Sweep looks once and acts on what it finds, one node after another: two
// nodes are never drained at the same moment. It is not safe to call from two
// goroutines at once.
func (d *NodeDrain) Sweep(ctx context.Context) {
	start := time.Now()
	if ctx.Err() != nil {
		d.log.Debug("NodeDrain.Sweep: not leading, nothing done", zap.Error(ctx.Err()))
		return
	}
	nodes, err := d.nodes(ctx)
	if err != nil {
		d.log.Debug("NodeDrain.Sweep: cannot list nodes", zap.Error(err))
		return
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	// Nothing to do is the usual case, and it should not cost the Haify
	// controller a call every few seconds.
	busy := false
	present := map[string]bool{}
	for _, n := range nodes {
		present[n.Name] = true
		if want, _ := drainWanted(n, d.cfg); want || n.Annotations[DrainStateAnnotation] != "" {
			busy = true
		}
	}
	d.forget(present)
	if !busy {
		d.log.Debug("NodeDrain.Sweep: no node asks to be drained or is drained", zap.Int("nodes", len(nodes)))
		return
	}

	callStart := time.Now()
	haifyNodes, err := d.backend.ListNodes(ctx)
	d.log.Debug("NodeDrain.Sweep: listed Haify nodes", zap.Int("count", len(haifyNodes)),
		zap.Duration("took", time.Since(callStart)), zap.Error(err))
	if err != nil {
		// Without the controller nothing can be drained, and nothing is
		// known about what already is.
		d.log.Warn("node drain: cannot reach the Haify controller", zap.Error(err))
		return
	}

	for _, n := range nodes {
		if ctx.Err() != nil {
			d.log.Debug("NodeDrain.Sweep: stopped leading")
			return
		}
		want, why := drainWanted(n, d.cfg)
		state := n.Annotations[DrainStateAnnotation]
		hn := drainHaifyNode(n, haifyNodes)
		d.log.Debug("NodeDrain.Sweep: node", zap.String("node", n.Name), zap.Bool("wanted", want), zap.String("why", why),
			zap.String("state", state), zap.String("haify_node", hn.GetName()), zap.String("haify_state", hn.GetState()))
		switch {
		case want && state != drainStateDrained:
			d.drain(ctx, n, hn, why, nodes, haifyNodes)
		case !want:
			delete(d.asking, n.Name)
			if state != "" {
				d.undrain(ctx, n, hn)
			} else {
				delete(d.said, n.Name)
			}
		}
	}
	d.log.Debug("NodeDrain.Sweep: done", zap.Int("nodes", len(nodes)), zap.Duration("took", time.Since(start)))
}

// forget drops what is remembered about nodes that no longer exist.
func (d *NodeDrain) forget(present map[string]bool) {
	for _, m := range []map[string]time.Time{d.asking, d.attempted} {
		for name := range m {
			if !present[name] {
				delete(m, name)
			}
		}
	}
	for name := range d.said {
		if !present[name] {
			delete(d.said, name)
		}
	}
	for name := range d.held {
		if !present[name] {
			delete(d.held, name)
		}
	}
}

// drain takes one node that asks to be drained a step further: through the
// delay, past the checks that hold a drain back, and into Haify.
func (d *NodeDrain) drain(ctx context.Context, node *corev1.Node, hn *haifypb.NodeInfo, why string,
	nodes []*corev1.Node, haifyNodes []*haifypb.NodeInfo) {
	name := node.Name
	state := node.Annotations[DrainStateAnnotation]
	now := d.now()
	if _, ok := d.asking[name]; !ok {
		d.asking[name] = now
	}
	if state == "" {
		if waited := now.Sub(d.asking[name]); waited < d.cfg.Delay {
			d.log.Debug("NodeDrain.drain: inside the delay", zap.String("node", name), zap.Duration("asking_for", waited),
				zap.Duration("delay", d.cfg.Delay))
			return
		}
	}
	if hn == nil {
		d.log.Debug("NodeDrain.drain: not a Haify node, nothing to drain", zap.String("node", name))
		return
	}
	// A node that is down cannot hand anything over: what it was Primary for
	// has failed over already, or will, and the HA controller moves its Pods.
	// Draining resumes if it comes back still asking.
	if !haNodeReady(node) || haHasTaint(node) {
		d.log.Debug("NodeDrain.drain: node is down, left to failover", zap.String("node", name),
			zap.Bool("ready", haNodeReady(node)), zap.Bool("failover_taint", haHasTaint(node)))
		return
	}
	if state == "" && hn.GetState() == nodeStateMaintenance {
		// Drained in Haify by someone else. It stays theirs: recording it
		// as this controller's would have an uncordon undo their drain.
		d.log.Debug("NodeDrain.drain: already drained in Haify by an operator, left alone", zap.String("node", name))
		return
	}
	if state == "" {
		if others := drainedElsewhere(node, nodes, haifyNodes); len(others) >= d.cfg.MaxNodes {
			d.say(name, corev1.EventTypeNormal, ReasonNodeDrainWaiting, fmt.Sprintf(
				"Not drained in Haify yet (%s): %s still drained there, and at most %d node(s) are drained at a time",
				why, strings.Join(others, ", "), d.cfg.MaxNodes))
			return
		}
	}
	pods, err := d.haifyPodsOn(ctx, name)
	if err != nil {
		d.log.Warn("node drain: cannot tell which Pods on the node use Haify volumes; not draining", zap.String("node", name),
			zap.Error(err))
		return
	}
	if moved, held := d.held[name]; held && len(pods) > 0 {
		d.say(name, corev1.EventTypeNormal, ReasonNodeDrainWaiting, d.waitingMessage(why, pods, moved))
		return
	}
	if last, tried := d.attempted[name]; tried && now.Sub(last) < d.cfg.Retry {
		d.log.Debug("NodeDrain.drain: waiting to retry", zap.String("node", name), zap.Duration("since_last", now.Sub(last)))
		return
	}

	// The record goes on the Node before the call: a replica that dies in
	// between leaves a node marked "draining" for the next one to finish,
	// rather than a node drained in Haify that nobody remembers draining.
	if state == "" {
		if err := d.setState(ctx, name, drainStateDraining); err != nil {
			d.log.Warn("node drain: record the drain on the node; not draining", zap.String("node", name), zap.Error(err))
			return
		}
	}
	d.attempted[name] = now
	// While Pods with Haify volumes are still on the node, their volumes
	// stay where they are; once they are gone, nothing is excepted.
	keep := len(pods) > 0
	callCtx, cancel := context.WithTimeout(ctx, d.cfg.CallTimeout)
	start := time.Now()
	resp, err := d.backend.DrainNodeWith(callCtx, &haifypb.DrainNodeRequest{Name: hn.GetName(), KeepInUse: keep,
		RequestedBy: drainRequester, Reason: why})
	cancel()
	moved := resp.GetResourcesMoved()
	d.log.Debug("NodeDrain.drain: drain returned", zap.String("node", name), zap.String("haify_node", hn.GetName()),
		zap.Bool("keep_in_use", keep), zap.Strings("moved", moved), zap.Int("left", len(resp.GetResourcesLeft())),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		d.log.Warn("node drain: Haify drain did not complete", zap.String("node", name), zap.Strings("moved", moved), zap.Error(err))
		d.say(name, corev1.EventTypeWarning, ReasonNodeDrainFailed, fmt.Sprintf(
			"Haify drain (%s) did not complete and is retried every %s: %v", why, d.cfg.Retry, err))
		return
	}
	if keep {
		// The first step: out of placement, with the Pods' volumes still on
		// it. The record on the Node stays "draining" until the second.
		d.held[name] = moved
		delete(d.attempted, name)
		d.log.Info("node drain: node takes no new replicas in Haify; waiting for its Pods", zap.String("node", name),
			zap.String("why", why), zap.Strings("moved", moved), zap.Int("pods", len(pods)))
		d.say(name, corev1.EventTypeNormal, ReasonNodeDrainWaiting, d.waitingMessage(why, pods, moved))
		return
	}
	if err := d.setState(ctx, name, drainStateDrained); err != nil {
		// The drain itself is done; the next attempt repeats it, which
		// moves nothing, and tries the record again.
		d.log.Warn("node drain: record the finished drain on the node", zap.String("node", name), zap.Error(err))
		return
	}
	delete(d.attempted, name)
	delete(d.held, name)
	what := "nothing was Primary on it"
	if len(moved) > 0 {
		what = fmt.Sprintf("the Primary role of %d resource(s) moved to other nodes (%s)", len(moved), drainNameList(moved))
	}
	d.log.Info("node drain: drained node in Haify", zap.String("node", name), zap.String("why", why), zap.Strings("moved", moved))
	d.say(name, corev1.EventTypeNormal, ReasonNodeDrained, fmt.Sprintf(
		"Drained in Haify (%s): it takes no new replicas, and %s. The replicas it holds stay on it.", why, what))
}

// undrain returns to service a node this controller drained, now that it no
// longer asks to be drained, and removes the record from the Node.
func (d *NodeDrain) undrain(ctx context.Context, node *corev1.Node, hn *haifypb.NodeInfo) {
	name := node.Name
	now := d.now()
	if last, tried := d.attempted[name]; tried && now.Sub(last) < d.cfg.Retry {
		d.log.Debug("NodeDrain.undrain: waiting to retry", zap.String("node", name), zap.Duration("since_last", now.Sub(last)))
		return
	}
	undrained := false
	if hn != nil && hn.GetState() == nodeStateMaintenance {
		d.attempted[name] = now
		callCtx, cancel := context.WithTimeout(ctx, d.cfg.CallTimeout)
		start := time.Now()
		err := d.backend.UndrainNode(callCtx, hn.GetName())
		cancel()
		d.log.Debug("NodeDrain.undrain: undrain returned", zap.String("node", name), zap.String("haify_node", hn.GetName()),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			d.log.Warn("node drain: Haify undrain failed", zap.String("node", name), zap.Error(err))
			d.say(name, corev1.EventTypeWarning, ReasonNodeUndrainFailed, fmt.Sprintf(
				"Still drained in Haify; returning it to service failed and is retried every %s: %v", d.cfg.Retry, err))
			return
		}
		undrained = true
	} else {
		d.log.Debug("NodeDrain.undrain: not drained in Haify, only the record to remove", zap.String("node", name),
			zap.Bool("haify_node", hn != nil))
	}
	if err := d.setState(ctx, name, ""); err != nil {
		d.log.Warn("node drain: remove the drain record from the node", zap.String("node", name), zap.Error(err))
		return
	}
	delete(d.attempted, name)
	delete(d.held, name)
	if undrained {
		d.log.Info("node drain: returned node to service in Haify", zap.String("node", name))
		d.say(name, corev1.EventTypeNormal, ReasonNodeUndrained,
			"Returned to service in Haify: it takes new replicas again. Nothing that moved off it was moved back.")
	}
	delete(d.said, name)
}

// say posts an event on the node unless it is the one posted there last.
func (d *NodeDrain) say(nodeName, eventType, reason, message string) {
	key := reason + "\x00" + message
	if d.said[nodeName] == key {
		d.log.Debug("NodeDrain.say: same as the last event, not repeated", zap.String("node", nodeName), zap.String("reason", reason))
		return
	}
	d.said[nodeName] = key
	d.log.Debug("NodeDrain.say: posting event", zap.String("node", nodeName), zap.String("reason", reason))
	d.recorder.Event(haNodeRef(nodeName), eventType, reason, message)
}
