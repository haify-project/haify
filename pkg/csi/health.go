package csi

import (
	"fmt"
	"sort"
	"strings"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// volumeHealth judges a volume from the controller's view of its DRBD
// resource. The health reporter turns its verdict into events on the PVC.
//
// CSI used to carry this as VolumeCondition on ListVolumes/NodeGetVolumeStats;
// spec v1.13.0 removed that alpha API outright, so the verdict is delivered to
// Kubernetes directly instead of through the CSI response.
//
// Abnormal means the volume is not protected the way its StorageClass promised,
// not merely that I/O fails: a replica that is disconnected, out of date or
// resyncing is a volume running on fewer copies than it should, and that is the
// thing an operator looking at a PVC needs to hear about. The rules are the same
// ones pkg/alert applies, including the one that took a production split brain to
// learn — a peer whose link is down carries no disk state at all, so the
// connection has to be checked before anything else.
func volumeHealth(r *haifypb.ResourceInfo) (abnormal bool, message string) {
	abnormal, message, _ = volumeHealthDetail(r)
	return abnormal, message
}

// volumeHealthDetail is volumeHealth plus whether every problem found is a
// resync in progress. A resync heals itself, and the first one a volume ever
// runs — the initial sync of a replica that has never held data — is not news
// at all; the reporter uses this to stay quiet about it.
func volumeHealthDetail(r *haifypb.ResourceInfo) (abnormal bool, message string, onlyResync bool) {
	log := zap.L()
	if r == nil {
		log.Debug("volumeHealthDetail: no resource, volume not found")
		return true, "volume not found", false
	}
	expectedDiskless := map[string]bool{}
	for _, n := range r.GetDisklessNodes() {
		expectedDiskless[n] = true
	}
	for _, n := range r.GetDisklessClients() {
		expectedDiskless[n] = true
	}

	var problems []string
	onlyResync = true
	for host, st := range r.GetNodeStates() {
		name := st.GetNode()
		if name == "" {
			name = host
		}
		p := replicaProblem(st, expectedDiskless[name] || expectedDiskless[host])
		log.Debug("volumeHealthDetail: replica", zap.String("resource", r.GetName()), zap.String("node", name),
			zap.String("connection", st.GetConnection()), zap.String("disk_state", st.GetDiskState()),
			zap.String("replication_state", st.GetReplicationState()), zap.String("problem", p))
		if p != "" {
			problems = append(problems, fmt.Sprintf("%s: %s", name, p))
			if !strings.HasPrefix(p, "resyncing") {
				onlyResync = false
			}
		}
	}
	if len(problems) == 0 {
		log.Debug("volumeHealthDetail: healthy", zap.String("resource", r.GetName()), zap.Int("replicas", len(r.GetNodeStates())))
		return false, "all replicas up to date", false
	}
	sort.Strings(problems)
	log.Debug("volumeHealthDetail: abnormal", zap.String("resource", r.GetName()), zap.Int("problems", len(problems)),
		zap.Bool("only_resync", onlyResync))
	return true, "replica " + strings.Join(problems, "; "), onlyResync
}

// replicaProblem describes what is wrong with one replica, or "" when nothing
// is. expectedDiskless marks a tiebreaker or diskless client, for which having no
// local disk is the healthy steady state.
func replicaProblem(st *haifypb.NodeResourceState, expectedDiskless bool) string {
	if c := st.GetConnection(); c != "" && !strings.EqualFold(c, "Connected") {
		if strings.EqualFold(c, "StandAlone") {
			return "disconnected (StandAlone; DRBD does not reconnect it by itself, often a split brain)"
		}
		return "not connected (" + c + ")"
	}
	switch st.GetDiskState() {
	case "", "UpToDate":
		return ""
	case "Diskless":
		if expectedDiskless {
			return ""
		}
		return "lost its local disk (Diskless)"
	case "Inconsistent":
		// Which name the replication state carries depends on which node
		// answered: the node receiving data reports SyncTarget, and the Primary
		// sending it reports its peer as SyncSource. Both are the same resync.
		if r := st.GetReplicationState(); strings.HasPrefix(r, "Sync") || strings.HasPrefix(r, "PausedSync") {
			return fmt.Sprintf("resyncing (%.1f%%)", st.GetSyncPercent())
		}
		return "Inconsistent"
	default:
		return st.GetDiskState()
	}
}

// poolCapacity is how much room a pool has left on its node, and whether that
// figure is one the controller actually reported.
//
// It is poolFreeBytes with the ambiguity taken out. Placement can afford to read
// zero as "unknown" and admit the node, because a wrong guess there costs one
// retried CreateVolume. GetCapacity cannot: the scheduler reads a published zero
// as "this node is full" and stops placing Pods there, so a pool that is merely
// unreported must not be published as empty — and a thin pool that is genuinely
// full must be.
func poolCapacity(p *haifypb.PoolInfo) (uint64, bool) {
	log := zap.L()
	if p.GetThinPoolLv() != "" && p.GetThinSizeBytes() > 0 {
		used := float64(p.GetThinSizeBytes()) * p.GetThinDataPercent() / 100
		log.Debug("poolCapacity: from thin pool utilisation", zap.String("pool", p.GetName()), zap.String("node", p.GetNode()),
			zap.Uint64("thin_size_bytes", p.GetThinSizeBytes()), zap.Float64("data_percent", p.GetThinDataPercent()))
		if free := float64(p.GetThinSizeBytes()) - used; free > 0 {
			return uint64(free), true
		}
		return 0, true
	}
	if b := p.GetFreeBytes(); b > 0 {
		log.Debug("poolCapacity: from free bytes", zap.String("pool", p.GetName()), zap.String("node", p.GetNode()))
		return b, true
	}
	if g := p.GetFreeGb(); g > 0 {
		log.Debug("poolCapacity: from free GiB", zap.String("pool", p.GetName()), zap.String("node", p.GetNode()))
		return g * giB, true
	}
	// A thick pool with every extent allocated reports zeros in both fields,
	// which is indistinguishable from reporting nothing. Its total size says
	// which: a pool the controller measured has one.
	if p.GetTotalBytes() > 0 || p.GetTotalGb() > 0 {
		log.Debug("poolCapacity: measured and full", zap.String("pool", p.GetName()), zap.String("node", p.GetNode()))
		return 0, true
	}
	log.Debug("poolCapacity: no figure reported", zap.String("pool", p.GetName()), zap.String("node", p.GetNode()))
	return 0, false
}
