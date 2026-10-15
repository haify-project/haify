package csi

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Changing the number of diskful replicas of a provisioned volume.
//
// A PV's node affinity is written once, at provisioning, and Kubernetes never
// changes it. For a volume that allows remote access it is empty, so a Pod may
// run anywhere and replicas can come and go freely. For a pinned volume it
// names the replica nodes the volume was created on: a Pod can only be
// scheduled there, and a node plugin refuses to stage the volume on a node
// that holds no replica. So a pinned volume's original nodes are never
// removed (a Pod scheduled to one would be stuck), and a replica added to it
// makes the data safer but gives Pods nowhere new to run.

const (
	// placementTopologyLabel records where the PV lets Pods run: "any" for a
	// volume that allows remote access, else its original replica nodes
	// joined by "+" (a label value may not hold a comma).
	placementTopologyLabel = "haify.csi/topology"
	topologyAnywhere       = "any"
	// faultDomainLabelKey records the StorageClass's faultDomainLabel, so new
	// replicas are spread by the same rule the first ones were.
	faultDomainLabelKey = "haify.csi/fault-domain-label"
	// minKeptReplicas is what the controller's RemoveReplica keeps: it refuses
	// to leave a replicated resource with a single diskful copy.
	minKeptReplicas = 2
)

// labelValueRE mirrors the controller's rule for resource label values.
var labelValueRE = regexp.MustCompile(`^[A-Za-z0-9._:@+/-]{0,253}$`)

// placementLabels adds the labels ControllerModifyVolume later needs to
// change the volume's replicas without breaking its PV's node affinity.
func placementLabels(labels map[string]string, allowRemote bool, replicaNodes []string, faultDomainLabel string) {
	topo := topologyAnywhere
	if !allowRemote {
		topo = strings.Join(replicaNodes, "+")
	}
	if labelValueRE.MatchString(topo) {
		labels[placementTopologyLabel] = topo
	} else {
		// Without the label the volume is treated as pinned to whatever
		// replicas it has, which never removes a node a Pod may need.
		zap.L().Debug("placementLabels: topology does not fit a label, not recorded", zap.Int("length", len(topo)))
	}
	if faultDomainLabel != "" && labelValueRE.MatchString(faultDomainLabel) {
		labels[faultDomainLabelKey] = faultDomainLabel
	}
	zap.L().Debug("placementLabels: added", zap.Bool("allow_remote", allowRemote), zap.Strings("nodes", replicaNodes),
		zap.String("fault_domain_label", faultDomainLabel))
}

// volumeTopology reads placementTopologyLabel: anywhere for a remote-access
// volume, else the nodes Pods are pinned to. recorded is false for a volume
// provisioned before the label existed.
func volumeTopology(r *haifypb.ResourceInfo) (anywhere bool, pinned []string, recorded bool) {
	v, ok := r.GetLabels()[placementTopologyLabel]
	if !ok || v == "" {
		return false, nil, false
	}
	if v == topologyAnywhere {
		return true, nil, true
	}
	return false, strings.Split(v, "+"), true
}

// diskfulReplicas lists the nodes holding a local copy of the volume. The
// off-site DR of a WAN resource is a member but not a copy Pods can use.
func diskfulReplicas(r *haifypb.ResourceInfo) []string {
	var out []string
	for _, n := range r.GetNodes() {
		if r.GetWanMode() && n == r.GetDrNode() {
			continue
		}
		out = append(out, n)
	}
	return out
}

// modifyReplicas grows or shrinks the volume to want diskful replicas.
func (s *controllerServer) modifyReplicas(ctx context.Context, mod volumeModifier, r *haifypb.ResourceInfo, want int) error {
	current := diskfulReplicas(r)
	s.log.Debug("controllerServer.modifyReplicas: start", zap.String("resource", r.GetName()), zap.Strings("current", current),
		zap.Int("want", want))
	if len(current) == want && !needsTiebreaker(r, want, nil) {
		s.log.Debug("controllerServer.modifyReplicas: already at the wanted count", zap.String("resource", r.GetName()))
		return nil
	}
	start := time.Now()
	nodes, err := s.backend.ListNodes(ctx)
	s.log.Debug("controllerServer.modifyReplicas: listed nodes", zap.Int("count", len(nodes)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return status.Errorf(codes.Unavailable, "list nodes: %v", err)
	}
	start = time.Now()
	pools, err := s.backend.ListPools(ctx)
	s.log.Debug("controllerServer.modifyReplicas: listed pools", zap.Int("count", len(pools)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return status.Errorf(codes.Unavailable, "list pools: %v", err)
	}
	if len(current) == want {
		// An earlier shrink left it without a tiebreaker and failed to add one.
		return s.ensureTiebreaker(ctx, mod, r, want, nodes, nil)
	}
	if len(current) < want {
		return s.growReplicas(ctx, mod, r, current, want, nodes, pools)
	}
	return s.shrinkReplicas(ctx, mod, r, current, want, nodes, pools)
}

// volumePoolAndSize is the pool the volume's first volume lives in and the
// room a new replica of it needs.
func volumePoolAndSize(r *haifypb.ResourceInfo) (string, uint64) {
	if len(r.GetVolumes()) == 0 {
		return "", 0
	}
	v := r.GetVolumes()[0]
	if v.GetSizeBytes() > 0 {
		return v.GetPool(), v.GetSizeBytes()
	}
	return v.GetPool(), v.GetSizeGb() * giB
}

func domainLabelOf(r *haifypb.ResourceInfo) string {
	if v := r.GetLabels()[faultDomainLabelKey]; v != "" {
		return v
	}
	return "host"
}

// domainOf names a node's fault domain the way nodesWithPool does.
func domainOf(n *haifypb.NodeInfo, label string) string {
	if v := n.GetLabels()[label]; v != "" && label != "" {
		return label + "=" + v
	}
	return "node:" + n.GetName()
}

// growReplicas adds want-len(current) replicas, placed the way CreateVolume
// places them: on nodes that host the volume's pool and have room for it,
// in fault domains no replica uses yet first, the emptiest nodes first.
func (s *controllerServer) growReplicas(ctx context.Context, mod volumeModifier, r *haifypb.ResourceInfo, current []string,
	want int, nodes []*haifypb.NodeInfo, pools []*haifypb.PoolInfo) error {
	name := r.GetName()
	if _, _, recorded := volumeTopology(r); !recorded {
		// Remember the nodes the PV is pinned to before there are more
		// replicas than that, so a later shrink knows which ones to keep.
		if err := s.recordTopology(ctx, mod, name, current); err != nil {
			return err
		}
	}
	pool, need := volumePoolAndSize(r)
	label := domainLabelOf(r)
	candidates := nodesWithPool(nodes, pools, pool, label)
	byName := map[string]*haifypb.NodeInfo{}
	for _, n := range nodes {
		byName[n.GetName()] = n
	}
	// The existing replicas are seated first and need no room: their copy is
	// already there. Zero free space is "fits" to selectReplicaNodes.
	seated := map[string]bool{}
	for i := range candidates {
		if containsNode(current, candidates[i].node) {
			candidates[i].freeBytes = 0
			seated[candidates[i].node] = true
		}
	}
	for _, n := range current {
		if seated[n] {
			continue
		}
		domain := "node:" + n
		if info := byName[n]; info != nil {
			domain = domainOf(info, label)
		}
		s.log.Debug("controllerServer.growReplicas: existing replica not a pool candidate, seating it anyway",
			zap.String("resource", name), zap.String("node", n), zap.String("domain", domain))
		candidates = append(candidates, replicaCandidate{node: n, domain: domain})
	}
	pinned := append([]string{}, current...)
	// A node running the volume as a diskless client gets the first new
	// copy when it has room: its Pod then reads locally.
	for _, n := range r.GetDisklessClients() {
		for _, c := range candidates {
			if c.node == n && !containsNode(pinned, n) && c.fits(need) {
				s.log.Debug("controllerServer.growReplicas: preferring the diskless client's node", zap.String("resource", name),
					zap.String("node", n))
				pinned = append(pinned, n)
			}
		}
	}
	picked, err := selectReplicaNodes(candidates, pinned, want, need)
	if err != nil {
		s.log.Debug("controllerServer.growReplicas: placement failed", zap.String("resource", name), zap.Error(err))
		return status.Errorf(codes.ResourceExhausted, "add replicas to %q (pool %q): %v", name, pool, err)
	}
	for _, n := range picked {
		if containsNode(current, n) {
			continue
		}
		start := time.Now()
		err := mod.AddReplica(ctx, name, n)
		s.log.Debug("controllerServer.growReplicas: added replica", zap.String("resource", name), zap.String("node", n),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return status.Errorf(codes.Internal, "add a replica of %q on %s: %v", name, n, err)
		}
	}
	return nil
}

// recordTopology stores the nodes a pre-existing volume's PV is pinned to.
func (s *controllerServer) recordTopology(ctx context.Context, mod volumeModifier, name string, nodes []string) error {
	v := strings.Join(nodes, "+")
	if !labelValueRE.MatchString(v) {
		s.log.Debug("controllerServer.recordTopology: does not fit a label, not recorded", zap.String("resource", name))
		return nil
	}
	start := time.Now()
	_, err := mod.SetResourceLabels(ctx, name, map[string]string{placementTopologyLabel: v}, nil)
	s.log.Debug("controllerServer.recordTopology: recorded", zap.String("resource", name), zap.String("topology", v),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return status.Errorf(codes.Internal, "record the node affinity of %q: %v", name, err)
	}
	return nil
}

// replicaView is one diskful replica as shrinkReplicas weighs it.
type replicaView struct {
	node      string
	known     bool // the controller reported a state for it
	primary   bool
	upToDate  bool
	domain    string
	freeBytes uint64
}

// replicaViews pairs every current replica with its DRBD state. GetResource
// keys the states by DRBD host, which is often not the node name, so the
// node list resolves names, addresses and host names alike.
func replicaViews(r *haifypb.ResourceInfo, current []string, nodes []*haifypb.NodeInfo, pools []*haifypb.PoolInfo) []replicaView {
	alias := map[string]string{}
	byName := map[string]*haifypb.NodeInfo{}
	for _, n := range nodes {
		byName[n.GetName()] = n
		for _, a := range []string{n.GetName(), n.GetAddress(), n.GetHostname(), n.GetReplicationAddress()} {
			if a != "" {
				alias[a] = n.GetName()
			}
		}
	}
	states := map[string]*haifypb.NodeResourceState{}
	for key, st := range r.GetNodeStates() {
		name := st.GetNode()
		if name == "" {
			name = alias[key]
		}
		if name == "" {
			name = key
		}
		states[name] = st
	}
	pool, _ := volumePoolAndSize(r)
	label := domainLabelOf(r)
	free := map[string]uint64{}
	for _, c := range nodesWithPool(nodes, pools, pool, label) {
		free[c.node] = c.freeBytes
	}
	out := make([]replicaView, 0, len(current))
	for _, n := range current {
		v := replicaView{node: n, domain: "node:" + n, freeBytes: free[n]}
		if info := byName[n]; info != nil {
			v.domain = domainOf(info, label)
		}
		if st, ok := states[n]; ok {
			v.known = true
			v.primary = strings.EqualFold(st.GetRole(), "Primary")
			v.upToDate = st.GetDiskState() == "UpToDate"
		}
		zap.L().Debug("replicaViews: replica", zap.String("resource", r.GetName()), zap.String("node", n),
			zap.Bool("known", v.known), zap.Bool("primary", v.primary), zap.Bool("up_to_date", v.upToDate),
			zap.String("domain", v.domain))
		out = append(out, v)
	}
	return out
}

// pickRemovals chooses which replicas to drop. Never a Primary (the volume is
// in use there), never one whose state is unknown, never a node the PV pins
// Pods to. Of the rest: a copy that is not UpToDate goes first (it protects
// nothing yet), then one sharing its fault domain with another copy, then the
// one on the fullest pool. The reason a removal is impossible says whether
// waiting can help (a Primary, an unknown state) or not (the pinned nodes).
func pickRemovals(views []replicaView, count int, anywhere bool, pinned []string) (remove []string, retryable bool, why string) {
	perDomain := map[string]int{}
	for _, v := range views {
		perDomain[v.domain]++
	}
	var eligible []replicaView
	var busy, unknown, kept []string
	for _, v := range views {
		// Pinned first: it is the one reason waiting cannot fix.
		switch {
		case !anywhere && containsNode(pinned, v.node):
			kept = append(kept, v.node)
		case !v.known:
			unknown = append(unknown, v.node)
		case v.primary:
			busy = append(busy, v.node)
		default:
			eligible = append(eligible, v)
		}
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		if a.upToDate != b.upToDate {
			return !a.upToDate
		}
		if perDomain[a.domain] != perDomain[b.domain] {
			return perDomain[a.domain] > perDomain[b.domain]
		}
		if a.freeBytes != b.freeBytes {
			return a.freeBytes < b.freeBytes
		}
		return a.node < b.node
	})
	zap.L().Debug("pickRemovals: weighed", zap.Int("count", count), zap.Int("eligible", len(eligible)),
		zap.Strings("primary", busy), zap.Strings("unknown", unknown), zap.Strings("pinned", kept))
	if len(eligible) >= count {
		for _, v := range eligible[:count] {
			remove = append(remove, v.node)
		}
		return remove, false, ""
	}
	if len(eligible)+len(busy)+len(unknown) >= count {
		return nil, true, "not enough replicas can go now: in use (Primary) on " + listOrNone(busy) +
			", state unknown on " + listOrNone(unknown) + "; retrying"
	}
	return nil, false, "the PersistentVolume's node affinity names " + strings.Join(kept, ", ") +
		" and Kubernetes cannot change it, so those replicas stay (a Pod scheduled there could not use the volume otherwise)"
}

func listOrNone(nodes []string) string {
	if len(nodes) == 0 {
		return "none"
	}
	return strings.Join(nodes, ", ")
}

// shrinkReplicas removes len(current)-want replicas.
func (s *controllerServer) shrinkReplicas(ctx context.Context, mod volumeModifier, r *haifypb.ResourceInfo, current []string,
	want int, nodes []*haifypb.NodeInfo, pools []*haifypb.PoolInfo) error {
	name := r.GetName()
	if want < minKeptReplicas {
		s.log.Debug("controllerServer.shrinkReplicas: below the controller's minimum", zap.String("resource", name), zap.Int("want", want))
		return status.Errorf(codes.InvalidArgument,
			"cannot shrink %q to %d replica(s): Haify keeps at least %d diskful copies of a replicated volume", name, want, minKeptReplicas)
	}
	anywhere, pinned, recorded := volumeTopology(r)
	if !recorded {
		// Provisioned before the label: the PV pins Pods to the replicas it
		// was created with, which as far as anything recorded goes are the
		// ones it has. Keeping them all is the only safe reading.
		s.log.Debug("controllerServer.shrinkReplicas: no recorded topology, keeping every current replica", zap.String("resource", name))
		pinned = current
	}
	views := replicaViews(r, current, nodes, pools)
	remove, retryable, why := pickRemovals(views, len(current)-want, anywhere, pinned)
	if remove == nil {
		s.log.Debug("controllerServer.shrinkReplicas: cannot pick replicas to remove", zap.String("resource", name),
			zap.Bool("retryable", retryable), zap.String("why", why))
		if retryable {
			return status.Errorf(codes.FailedPrecondition, "shrink %q to %d replicas: %s", name, want, why)
		}
		return status.Errorf(codes.InvalidArgument, "shrink %q to %d replicas: %s", name, want, why)
	}
	upToDateLeft := false
	for _, v := range views {
		if v.upToDate && !containsNode(remove, v.node) {
			upToDateLeft = true
		}
	}
	if !upToDateLeft {
		s.log.Debug("controllerServer.shrinkReplicas: no UpToDate copy would remain", zap.String("resource", name),
			zap.Strings("remove", remove))
		return status.Errorf(codes.FailedPrecondition,
			"shrink %q to %d replicas: no UpToDate copy would remain; waiting for the resync to finish", name, want)
	}
	for _, n := range remove {
		start := time.Now()
		err := mod.RemoveReplica(ctx, name, n)
		s.log.Debug("controllerServer.shrinkReplicas: removed replica", zap.String("resource", name), zap.String("node", n),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return status.Errorf(codes.Internal, "remove the replica of %q on %s: %v", name, n, err)
		}
	}
	return s.ensureTiebreaker(ctx, mod, r, want, nodes, remove)
}
