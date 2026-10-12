package csi

import (
	"fmt"
	"sort"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// managedPoolPrefix mirrors the controller's normalizeManagedName: Haify-managed
// pools are stored with an "haify_" prefix, so a StorageClass pool of "vg0"
// matches the stored pool "haify_vg0".
const managedPoolPrefix = "haify_"

// nodeStateMaintenance is the state the controller reports for a node an
// operator drained (controller.NodeStateMaintenance).
const nodeStateMaintenance = "maintenance"

// managedPoolName normalizes a StorageClass pool name the same way the
// controller does, so it compares equal to the names returned by ListPools.
func managedPoolName(pool string) string {
	pool = strings.TrimSpace(pool)
	if pool == "" || strings.HasPrefix(pool, managedPoolPrefix) {
		return pool
	}
	return managedPoolPrefix + pool
}

// replicaCandidate is a node that hosts the target pool, carrying the room that
// node's copy of the pool has left. Placement ranks on that figure so replicas
// drift towards the emptiest nodes — the same capacity-first rule the CLI path
// gets from pkg/controller/placement.go, which a PVC otherwise missed entirely.
type replicaCandidate struct {
	node string
	// freeBytes is 0 when the controller reported no capacity figure this code
	// can stand behind. Zero means "unknown", never "full"; see poolFreeBytes.
	freeBytes uint64
	// domain is the node's fault domain: its value of the fault-domain label,
	// or the node itself when it has none.
	domain string
}

// poolFreeBytes is how much room a pool has left on its node, or 0 when the
// controller reported nothing usable.
//
// Which field answers that depends on the pool. Haify builds a thin pool out of
// nearly all of its volume group, so free_bytes/free_gb read near zero for
// the whole life of a thin pool however empty it is — rank on those and every
// thin node looks equally full, and reject on them and every thin-provisioned
// StorageClass stops provisioning. The thin pool's own utilisation is the
// figure that says whether the next write lands, so it wins when present.
//
// free_bytes is preferred over free_gb because it is exact, but it is not
// always populated: a pool the controller serves from its database carries only
// the rounded gibibyte counts. Hence the fallback rather than a single field.
func poolFreeBytes(p *haifypb.PoolInfo) uint64 {
	if p.GetThinPoolLv() != "" && p.GetThinSizeBytes() > 0 {
		used := float64(p.GetThinSizeBytes()) * p.GetThinDataPercent() / 100
		free := float64(p.GetThinSizeBytes()) - used
		zap.L().Debug("poolFreeBytes: thin pool utilisation", zap.String("pool", p.GetName()), zap.String("node", p.GetNode()),
			zap.Uint64("thin_size_bytes", p.GetThinSizeBytes()), zap.Float64("data_percent", p.GetThinDataPercent()))
		if free > 0 {
			return uint64(free)
		}
		return 0
	}
	if b := p.GetFreeBytes(); b > 0 {
		zap.L().Debug("poolFreeBytes: exact free bytes", zap.String("pool", p.GetName()), zap.String("node", p.GetNode()),
			zap.Uint64("free_bytes", b))
		return b
	}
	zap.L().Debug("poolFreeBytes: rounded free GiB", zap.String("pool", p.GetName()), zap.String("node", p.GetNode()),
		zap.Uint64("free_gb", p.GetFreeGb()))
	return p.GetFreeGb() * giB
}

// nodesWithPool returns the nodes that host the given pool, preserving the
// order of nodes and pairing each with the pool's free space there. A node
// hosts the pool when a PoolInfo with the matching (managed) name reports that
// node — matched by address or name, since the controller reports pools keyed
// by node address. This keeps replica placement pool-aware so volumes never
// land on a node lacking the backing pool.
func nodesWithPool(nodes []*haifypb.NodeInfo, pools []*haifypb.PoolInfo, pool, domainLabel string) []replicaCandidate {
	log := zap.L()
	want := managedPoolName(pool)
	log.Debug("nodesWithPool: start", zap.String("pool", want), zap.Int("nodes", len(nodes)), zap.Int("pools", len(pools)),
		zap.String("fault_domain_label", domainLabel))
	freeByKey := map[string]uint64{}
	for _, p := range pools {
		key := p.GetNode()
		if p.GetName() != want || key == "" {
			continue
		}
		if _, dup := freeByKey[key]; !dup {
			freeByKey[key] = poolFreeBytes(p)
		}
	}
	var out []replicaCandidate
	for _, n := range nodes {
		// A drained node (haify node drain) takes no new replicas.
		if n.GetState() == nodeStateMaintenance {
			log.Debug("nodesWithPool: skipping drained node", zap.String("node", n.GetName()))
			continue
		}
		hosts := false
		for _, key := range []string{n.GetAddress(), n.GetName()} {
			if free, ok := freeByKey[key]; ok {
				domain := "node:" + n.GetName()
				if v := n.GetLabels()[domainLabel]; v != "" && domainLabel != "" {
					domain = domainLabel + "=" + v
				}
				log.Debug("nodesWithPool: candidate", zap.String("node", n.GetName()), zap.Uint64("free_bytes", free),
					zap.String("domain", domain))
				out = append(out, replicaCandidate{node: n.GetName(), freeBytes: free, domain: domain})
				hosts = true
				break
			}
		}
		if !hosts {
			log.Debug("nodesWithPool: node does not host the pool", zap.String("node", n.GetName()), zap.String("pool", want))
		}
	}
	log.Debug("nodesWithPool: done", zap.String("pool", want), zap.Int("candidates", len(out)))
	return out
}

// requisiteNodes extracts node names from a CSI topology requirement, preferring
// Preferred order then Requisite. Returns nil when there is no constraint.
//
// These are pins, not merely hints: the driver deploys external-provisioner
// with --strict-topology, so under WaitForFirstConsumer this list is the node
// the scheduler already picked for the Pod. A volume placed anywhere else
// leaves that Pod unschedulable, which is a worse outcome than a replica on a
// tight node — hence selectReplicaNodes seats these before it ranks anything.
func requisiteNodes(req *csi.TopologyRequirement) []string {
	if req == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(tops []*csi.Topology) {
		for _, t := range tops {
			if n := t.GetSegments()[TopologyKeyNode]; n != "" && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	add(req.GetPreferred())
	add(req.GetRequisite())
	zap.L().Debug("requisiteNodes: extracted", zap.Int("preferred", len(req.GetPreferred())),
		zap.Int("requisite", len(req.GetRequisite())), zap.Strings("nodes", out))
	return out
}

// fits reports whether a candidate has room for a volume of needBytes.
//
// A candidate reporting zero free space is admitted, because zero is what an
// unknown capacity looks like here (see poolFreeBytes) and refusing those would
// fail provisioning on clusters that work today. Capacity only ever removes a
// node the controller positively said is too small.
func (c replicaCandidate) fits(needBytes uint64) bool {
	return c.freeBytes == 0 || c.freeBytes >= needBytes
}

// selectReplicaNodes picks `replicas` nodes to host a volume of needBytes.
//
// Pinned nodes (the scheduler's chosen node, and the node holding the source of
// a clone or restore) are seated first, in the order given. The remaining slots
// go to the candidates with the most free space, so a fresh volume lands on the
// emptiest nodes instead of on whatever ListNodes happened to return first —
// the behaviour `haify resource create` has always had. Equal free space
// breaks on node name so two provisioners racing over identical cluster state
// reach the same answer. Nodes in a fault domain no replica uses yet are taken
// before any that would share one: two copies on one physical host are one
// failure away from none.
//
// Pass needBytes 0 to place without a capacity requirement.
func selectReplicaNodes(candidates []replicaCandidate, pinned []string, replicas int, needBytes uint64) ([]string, error) {
	log := zap.L()
	log.Debug("selectReplicaNodes: start", zap.Int("candidates", len(candidates)), zap.Strings("pinned", pinned),
		zap.Int("replicas", replicas), zap.Uint64("need_bytes", needBytes))
	byName := make(map[string]replicaCandidate, len(candidates))
	unique := make([]replicaCandidate, 0, len(candidates))
	for _, c := range candidates {
		if _, dup := byName[c.node]; dup {
			continue
		}
		byName[c.node] = c
		unique = append(unique, c)
	}

	var picked []string
	used := map[string]bool{}
	domains := map[string]bool{}
	for _, n := range pinned {
		if len(picked) >= replicas {
			break
		}
		c, hosts := byName[n]
		if !hosts || used[n] {
			log.Debug("selectReplicaNodes: pin skipped", zap.String("node", n), zap.Bool("hosts_pool", hosts),
				zap.Bool("already_picked", used[n]))
			continue
		}
		// A pin that cannot fit the volume is refused rather than quietly
		// dropped: the caller turns this into ResourceExhausted, on which
		// external-provisioner clears the PVC's selected-node annotation and
		// lets the scheduler try another node. Placing the volume elsewhere
		// instead would strand the Pod on a node with no replica.
		if !c.fits(needBytes) {
			log.Debug("selectReplicaNodes: pinned node too small", zap.String("node", c.node),
				zap.Uint64("free_bytes", c.freeBytes), zap.Uint64("need_bytes", needBytes))
			return nil, fmt.Errorf("node %q must hold a replica but its pool has only %dGiB free, need %dGiB",
				c.node, c.freeBytes/giB, needBytes/giB)
		}
		log.Debug("selectReplicaNodes: seated pinned node", zap.String("node", n), zap.String("domain", c.domain))
		picked = append(picked, n)
		used[n] = true
		domains[c.domain] = true
	}

	roomy := make([]replicaCandidate, 0, len(unique))
	for _, c := range unique {
		if !used[c.node] && c.fits(needBytes) {
			roomy = append(roomy, c)
		}
	}
	sort.Slice(roomy, func(i, j int) bool {
		if roomy[i].freeBytes != roomy[j].freeBytes {
			return roomy[i].freeBytes > roomy[j].freeBytes
		}
		return roomy[i].node < roomy[j].node
	})
	for _, apart := range []bool{true, false} {
		for _, c := range roomy {
			if len(picked) >= replicas {
				break
			}
			if used[c.node] || (apart && c.domain != "" && domains[c.domain]) {
				continue
			}
			log.Debug("selectReplicaNodes: picked by free space", zap.String("node", c.node),
				zap.Uint64("free_bytes", c.freeBytes), zap.String("domain", c.domain), zap.Bool("new_domain", apart))
			picked = append(picked, c.node)
			used[c.node] = true
			domains[c.domain] = true
		}
	}

	if len(picked) < replicas {
		log.Debug("selectReplicaNodes: not enough nodes", zap.Int("picked", len(picked)), zap.Int("replicas", replicas),
			zap.Int("roomy", len(roomy)), zap.Int("unique", len(unique)))
		return nil, fmt.Errorf("need %d replica node(s) with %dGiB free, only %d of %d node(s) hosting the pool qualify",
			replicas, needBytes/giB, len(picked), len(unique))
	}
	log.Debug("selectReplicaNodes: done", zap.Strings("nodes", picked[:replicas]))
	return picked[:replicas], nil
}

// accessibleTopology builds one CSI topology segment per replica node.
func accessibleTopology(nodes []string) []*csi.Topology {
	out := make([]*csi.Topology, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, &csi.Topology{Segments: map[string]string{TopologyKeyNode: n}})
	}
	return out
}
