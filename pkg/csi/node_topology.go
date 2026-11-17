package csi

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// NodeLabelReader returns the labels of this node's Kubernetes Node object.
type NodeLabelReader func(ctx context.Context) (map[string]string, error)

// nodeLabelBackend is the part of the Haify controller the zone sync needs.
type nodeLabelBackend interface {
	ListNodes(ctx context.Context) ([]*haifypb.NodeInfo, error)
	SetNodeLabels(ctx context.Context, node string, labels map[string]string, replace bool) (*haifypb.NodeInfo, error)
}

// nodeTopologyReadTimeout bounds the read of the Node object inside
// NodeGetInfo. The kubelet registers the plugin on the answer, so the read
// must never hold it up for long; without an answer the plugin reports what
// it read last.
const nodeTopologyReadTimeout = 3 * time.Second

// nodeTopologyRetry is how soon a failed sync is tried again.
const nodeTopologyRetry = 30 * time.Second

// NodeTopology knows this node's Kubernetes zone and region. It hands them to
// NodeGetInfo, and keeps the Haify node's labels in step with them so replicas
// can be spread across zones.
type NodeTopology struct {
	nodeName string
	read     NodeLabelReader
	backend  nodeLabelBackend
	log      *zap.Logger

	mu     sync.Mutex
	known  bool // a read has succeeded; zone and region are what it found
	zone   string
	region string
	// reported is the conflict last logged, so a hand-set label that stays
	// different is warned about once and not at every sync.
	reported string
}

// NewNodeTopology builds the zone reader and sync for nodeName.
func NewNodeTopology(nodeName string, read NodeLabelReader, backend nodeLabelBackend, log *zap.Logger) *NodeTopology {
	if log == nil {
		log = zap.NewNop()
	}
	return &NodeTopology{nodeName: nodeName, read: read, backend: backend, log: log}
}

// refresh reads the node's labels and remembers its zone and region. A failed
// read changes nothing: not knowing is not the same as the label being gone.
func (t *NodeTopology) refresh(ctx context.Context) error {
	start := time.Now()
	labels, err := t.read(ctx)
	t.log.Debug("NodeTopology.refresh: read node labels", zap.String("node", t.nodeName), zap.Int("labels", len(labels)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("read the labels of node %s: %w", t.nodeName, err)
	}
	zone, region := labels[TopologyKeyZone], labels[TopologyKeyRegion]
	t.mu.Lock()
	changed := !t.known || zone != t.zone || region != t.region
	t.known, t.zone, t.region = true, zone, region
	t.mu.Unlock()
	if changed {
		t.log.Info("node topology", zap.String("node", t.nodeName), zap.String("zone", zone), zap.String("region", region))
	}
	return nil
}

// segments returns the zone and region to publish, keyed by topology key, or
// an empty map for a node that has neither.
//
// The labels are read again on every call rather than once at start. The
// kubelet refuses to register a plugin whose topology disagrees with a label
// the node already carries under the same key, and the keys here are the
// node's own labels, so reporting a value read before someone changed the
// label would stop the plugin registering at all.
func (t *NodeTopology) segments(ctx context.Context) map[string]string {
	readCtx, cancel := context.WithTimeout(ctx, nodeTopologyReadTimeout)
	err := t.refresh(readCtx)
	cancel()
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		t.log.Warn("zone topology: using what was read last", zap.String("node", t.nodeName), zap.Bool("known", t.known),
			zap.Error(err))
	}
	out := map[string]string{}
	if t.zone != "" {
		out[TopologyKeyZone] = t.zone
	}
	if t.region != "" {
		out[TopologyKeyRegion] = t.region
	}
	return out
}

// Sync reads the node's zone and region and brings the Haify node's labels in
// line (topologyLabelChanges has the rule).
func (t *NodeTopology) Sync(ctx context.Context) error {
	if err := t.refresh(ctx); err != nil {
		return err
	}
	t.mu.Lock()
	zone, region := t.zone, t.region
	t.mu.Unlock()

	start := time.Now()
	nodes, err := t.backend.ListNodes(ctx)
	t.log.Debug("NodeTopology.Sync: listed Haify nodes", zap.Int("count", len(nodes)), zap.Duration("took", time.Since(start)),
		zap.Error(err))
	if err != nil {
		return fmt.Errorf("list Haify nodes: %w", err)
	}
	var self *haifypb.NodeInfo
	for _, n := range nodes {
		if n.GetName() == t.nodeName {
			self = n
			break
		}
	}
	if self == nil {
		t.log.Debug("NodeTopology.Sync: node not registered with Haify yet", zap.String("node", t.nodeName))
		return fmt.Errorf("node %s is not registered with Haify yet", t.nodeName)
	}

	set, conflicts := topologyLabelChanges(self.GetLabels(), zone, region)
	t.reportConflicts(conflicts)
	if len(set) == 0 {
		t.log.Debug("NodeTopology.Sync: labels already in step", zap.String("node", t.nodeName), zap.String("zone", zone),
			zap.String("region", region))
		return nil
	}
	start = time.Now()
	_, err = t.backend.SetNodeLabels(ctx, t.nodeName, set, false)
	t.log.Debug("NodeTopology.Sync: set labels", zap.String("node", t.nodeName), zap.Strings("labels", sortedLabelKeys(set)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("set labels on Haify node %s: %w", t.nodeName, err)
	}
	for _, k := range sortedLabelKeys(set) {
		t.log.Info("zone topology: Haify node label follows Kubernetes", zap.String("node", t.nodeName), zap.String("label", k),
			zap.String("was", self.GetLabels()[k]), zap.String("now", set[k]))
	}
	return nil
}

// reportConflicts warns about short labels an operator set differently, once
// per distinct set of them.
func (t *NodeTopology) reportConflicts(conflicts []topologyLabelConflict) {
	desc := ""
	for _, c := range conflicts {
		desc += fmt.Sprintf("%s=%s (Kubernetes: %s) ", c.label, c.kept, c.kubernetes)
	}
	t.mu.Lock()
	again := desc == t.reported
	t.reported = desc
	t.mu.Unlock()
	if again {
		return
	}
	for _, c := range conflicts {
		t.log.Warn("zone topology: Haify node label was set by hand and is kept; it does not follow Kubernetes",
			zap.String("node", t.nodeName), zap.String("label", c.label), zap.String("kept", c.kept),
			zap.String("kubernetes", c.kubernetes))
	}
}

// Run syncs now and then every interval, so a label changed in Kubernetes, or
// a controller that was unreachable, is caught up with. A failed sync is tried
// again sooner. With an interval of 0 it stops after the first sync that
// succeeds. It returns when ctx ends, and nothing waits on it: the node plugin
// serves volumes whether or not the sync gets through.
func (t *NodeTopology) Run(ctx context.Context, interval time.Duration) {
	t.log.Debug("NodeTopology.Run: starting", zap.String("node", t.nodeName), zap.Duration("interval", interval))
	failed := ""
	for {
		wait := interval
		if err := t.Sync(ctx); err != nil {
			wait = nodeTopologyRetry
			if interval > 0 && interval < wait {
				wait = interval
			}
			// Logged when it changes: a controller that stays away would
			// otherwise repeat the same line for as long as it does.
			if err.Error() != failed {
				failed = err.Error()
				t.log.Warn("zone topology: sync to the Haify node failed; will retry", zap.String("node", t.nodeName),
					zap.Duration("retry_in", wait), zap.Error(err))
			}
		} else {
			failed = ""
			if interval <= 0 {
				t.log.Debug("NodeTopology.Run: synced once, periodic sync disabled", zap.String("node", t.nodeName))
				return
			}
		}
		select {
		case <-ctx.Done():
			t.log.Debug("NodeTopology.Run: stopping", zap.Error(ctx.Err()))
			return
		case <-time.After(wait):
		}
	}
}

// EnableNodeTopology makes the node service publish the zone and region t
// reads. ns must come from NewNodeServer; without this call it publishes the
// node alone.
func EnableNodeTopology(ns csi.NodeServer, t *NodeTopology) {
	s, ok := ns.(*nodeServer)
	if !ok {
		zap.L().Debug("EnableNodeTopology: not a Haify node server")
		return
	}
	s.log.Debug("EnableNodeTopology: zone and region will be published", zap.String("node", s.nodeName))
	s.topology = t
}

// nodeSegments is the topology NodeGetInfo reports: the node, and its zone and
// region when it has them.
func (s *nodeServer) nodeSegments(ctx context.Context) map[string]string {
	segs := map[string]string{TopologyKeyNode: s.nodeName}
	if s.topology != nil {
		for k, v := range s.topology.segments(ctx) {
			segs[k] = v
		}
	}
	return segs
}
