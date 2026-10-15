package csi

import (
	"net"
	"sort"
	"strings"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
)

// haNodeIdentity is every name a Kubernetes node can appear under in the
// controller's view of a DRBD resource.
//
// The view is keyed by two different things at once: the node that answered is
// filed under the name the resource was created with (the Kubernetes node name,
// since csi-node registers itself by it), and each peer under the host name in
// the DRBD configuration, which is the machine's real hostname and need not be
// the node name. Matching on the node name alone would miss exactly the peer
// that died.
type haNodeIdentity map[string]bool

// haIdentityFor collects the names node is known by: its own name, its hostname
// label and addresses, and what the Haify controller registered for it.
func haIdentityFor(node *corev1.Node, haifyNodes []*haifypb.NodeInfo) haNodeIdentity {
	id := haNodeIdentity{}
	if node == nil {
		return id
	}
	id.add(node.Name)
	id.add(node.Labels[corev1.LabelHostname])
	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeHostName || a.Type == corev1.NodeInternalIP {
			id.add(a.Address)
		}
	}
	for _, hn := range haifyNodes {
		if !id[strings.ToLower(hn.GetName())] && !id[strings.ToLower(hn.GetHostname())] {
			continue
		}
		id.add(hn.GetName())
		id.add(hn.GetHostname())
		id.add(hn.GetAddress())
		id.add(hn.GetReplicationAddress())
	}
	zap.L().Debug("haIdentityFor: node identity", zap.String("node", node.Name), zap.Int("names", len(id)))
	return id
}

// add records name, and for a DNS name its first label too: one side may know
// the machine as "node-a" and the other as "node-a.lab.example".
func (id haNodeIdentity) add(name string) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return
	}
	id[name] = true
	if net.ParseIP(name) == nil {
		if short, _, found := strings.Cut(name, "."); found && short != "" {
			id[short] = true
		}
	}
}

// matches reports whether the view entry filed under key describes this node.
func (id haNodeIdentity) matches(key string, st *haifypb.NodeResourceState) bool {
	probe := haNodeIdentity{}
	probe.add(key)
	probe.add(st.GetNode())
	for name := range probe {
		if id[name] {
			return true
		}
	}
	return false
}

// haLossVerdict is what one volume's view says about one node.
type haLossVerdict struct {
	// lost is true only when every condition for moving the volume away from
	// the node holds; reason says which one failed when it is false.
	lost      bool
	reason    string
	answering string   // the node whose DRBD status the view is
	survivors []string // nodes the answering node is connected to, itself included
	voters    int
}

// haJudgeNodeLoss decides from the controller's view of a volume whether the
// node identified by id has dropped out of it while the rest carry on.
//
// The view is one surviving node's DRBD status: its own state, and one
// connection per peer. Everything the HA controller does rests on DRBD quorum —
// a node cut off from the majority cannot write, and the majority may promote —
// so the verdict demands that the side it can see holds that majority:
//
//   - exactly one entry is the answering node (the only one without a
//     connection state); a view where that cannot be told apart is no evidence;
//   - the node is a peer of the answering node and its connection is down. A
//     node that answered for itself is reachable. A StandAlone connection is
//     DRBD refusing to reconnect, usually after a split brain, not a node that
//     died, and moving a workload in the middle of that is how data is lost;
//   - the answering node and the peers connected to it are more than half of
//     every node in the view, diskless tiebreakers included, which is DRBD's own
//     quorum rule;
//   - one of them holds an UpToDate copy, so there is somewhere to move to.
func haJudgeNodeLoss(r *haifypb.ResourceInfo, id haNodeIdentity) haLossVerdict {
	log := zap.L()
	if r == nil {
		log.Debug("haJudgeNodeLoss: no resource")
		return haLossVerdict{reason: "no view of the volume"}
	}
	states := r.GetNodeStates()
	v := haLossVerdict{voters: len(states)}

	var locals []string
	target := ""
	for key, st := range states {
		if st.GetConnection() == "" {
			locals = append(locals, key)
		}
		if id.matches(key, st) {
			target = key
		}
	}
	if len(locals) != 1 {
		log.Debug("haJudgeNodeLoss: cannot tell which node answered", zap.String("resource", r.GetName()),
			zap.Strings("candidates", locals))
		v.reason = "the view does not say which node answered"
		return v
	}
	v.answering = locals[0]
	switch target {
	case "":
		log.Debug("haJudgeNodeLoss: node not in view", zap.String("resource", r.GetName()))
		v.reason = "the node is not in the volume's view"
		return v
	case v.answering:
		log.Debug("haJudgeNodeLoss: node answered for itself", zap.String("resource", r.GetName()), zap.String("node", target))
		v.reason = "the node answered for the volume itself"
		return v
	}
	conn := states[target].GetConnection()
	switch {
	case strings.EqualFold(conn, "Connected"):
		log.Debug("haJudgeNodeLoss: node connected", zap.String("resource", r.GetName()), zap.String("node", target))
		v.reason = "the node is connected"
		return v
	case strings.EqualFold(conn, "StandAlone"):
		log.Debug("haJudgeNodeLoss: StandAlone, not a lost node", zap.String("resource", r.GetName()), zap.String("node", target))
		v.reason = "the connection is StandAlone (split brain or disconnected by hand), not a lost node"
		return v
	}

	upToDate := strings.EqualFold(states[v.answering].GetDiskState(), "UpToDate")
	v.survivors = append(v.survivors, v.answering)
	for key, st := range states {
		if key == v.answering || key == target || !strings.EqualFold(st.GetConnection(), "Connected") {
			continue
		}
		v.survivors = append(v.survivors, key)
		if strings.EqualFold(st.GetDiskState(), "UpToDate") {
			upToDate = true
		}
	}
	sort.Strings(v.survivors)
	if len(v.survivors)*2 <= v.voters {
		log.Debug("haJudgeNodeLoss: survivors lack quorum", zap.String("resource", r.GetName()),
			zap.Int("survivors", len(v.survivors)), zap.Int("voters", v.voters))
		v.reason = "the surviving replicas do not hold quorum"
		return v
	}
	if !upToDate {
		log.Debug("haJudgeNodeLoss: no UpToDate survivor", zap.String("resource", r.GetName()))
		v.reason = "no surviving replica is UpToDate"
		return v
	}
	v.lost = true
	v.reason = "connection " + conn
	log.Debug("haJudgeNodeLoss: node lost", zap.String("resource", r.GetName()), zap.String("node", target),
		zap.String("connection", conn), zap.Strings("survivors", v.survivors), zap.Int("voters", v.voters))
	return v
}

// haNodeReady reports whether the node's Ready condition is True. Unknown, which
// is what the node lifecycle controller sets when the kubelet stops reporting,
// counts as not ready.
func haNodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// haHasTaint reports whether node carries the taint the HA controller
// puts on a lost node.
func haHasTaint(node *corev1.Node) bool {
	for _, t := range node.Spec.Taints {
		if t.Key == FailoverTaintKey {
			return true
		}
	}
	return false
}

// haOptedOut reports whether a Pod asked not to be moved by the HA controller,
// with the annotation or the label FailoverOptOutKey set to "false".
func haOptedOut(pod *corev1.Pod) bool {
	return strings.EqualFold(pod.Annotations[FailoverOptOutKey], "false") ||
		strings.EqualFold(pod.Labels[FailoverOptOutKey], "false")
}

// haHasController reports whether something recreates the Pod once it is
// deleted. Deleting a bare Pod would only throw it away.
func haHasController(pod *corev1.Pod) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

// haPodClaims returns the namespace/name of every PVC the Pod mounts, including
// the claim of a generic ephemeral volume, which is named <pod>-<volume>.
func haPodClaims(pod *corev1.Pod) []string {
	var out []string
	for _, vol := range pod.Spec.Volumes {
		switch {
		case vol.PersistentVolumeClaim != nil:
			out = append(out, pod.Namespace+"/"+vol.PersistentVolumeClaim.ClaimName)
		case vol.Ephemeral != nil:
			out = append(out, pod.Namespace+"/"+pod.Name+"-"+vol.Name)
		}
	}
	return out
}
