package controller

import (
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// Auto-placement: when a resource is created without an explicit node list, the
// controller picks where its replicas go. This is the Haify equivalent of
// LINSTOR's autoplace. Placement is capacity-first (most free space in the
// target pool wins) and honours a set of label/affinity constraints:
//
//   - replicas-on-different <key>: each replica lands in a distinct value of the
//     node label <key> (fault-domain spread, e.g. rack).
//   - replicas-on-same <key>: every replica shares one value of <key> (e.g. keep
//     all copies in one zone for latency).
//   - do-not-place-with <resource>: never place a replica on a node that already
//     holds a replica of <resource> (cross-resource anti-affinity).
//
// All are composable: multiple on-different / on-same keys and multiple
// do-not-place-with resources apply together.
//
// This file holds the pure solver; placement_capacity.go decides which pools
// may take a volume, placement_candidates.go gathers the candidates from the
// cluster, and placement_domain.go spreads replicas across fault domains.

// placementNode is a candidate node with the free capacity of its copy of the
// target pool and its label set (for evaluating constraints).
type placementNode struct {
	node string
	// freeGB only ranks candidates; whether a node may host the volume at all
	// was already decided by poolCapacity.admits before the node got here. Zero
	// is therefore not a rejection: it is a pool that could not report its
	// capacity, or a thin pool with little left that can still take writes, and
	// either one simply sorts behind every node able to prove it has room.
	freeGB uint64
	labels map[string]string
	// thin marks an over-provisioned pool, where freeGB does not cap a volume.
	thin bool
	// freeBytes is the exact figure behind freeGB, which is rounded to the
	// nearest GiB — fine for ranking, wrong for "the largest volume that fits".
	freeBytes uint64
}

// placementConstraints are the label-based rules a placement must satisfy.
// do-not-place-with is applied by the caller (it removes nodes before solving),
// so it does not appear here.
type placementConstraints struct {
	onDifferent []string // label keys whose values must differ across replicas
	onSame      []string // label keys whose value must be identical across replicas
}

func (c placementConstraints) empty() bool {
	return len(c.onDifferent) == 0 && len(c.onSame) == 0
}

// referencedKeys is the set of label keys the constraints mention. A node must
// carry every one of them (non-empty) to be eligible — you asked to place by
// these labels, so a node lacking one cannot be reasoned about.
func (c placementConstraints) referencedKeys() []string {
	seen := make(map[string]bool)
	var keys []string
	for _, k := range append(append([]string{}, c.onDifferent...), c.onSame...) {
		if k != "" && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return keys
}

// selectConstrained picks `replicas` nodes from `nodes` satisfying the
// constraints, maximising total free space. Approach: keep only nodes carrying
// every referenced label; partition them into groups that agree on all on-same
// keys; within each group greedily take the most-free nodes that stay pairwise
// distinct on every on-different key; return the best group's selection (most
// total free space, deterministic tie-break by node names). Pure and
// side-effect-free (never mutates `nodes`), so it is unit-testable without a
// live cluster.
func selectConstrained(nodes []placementNode, replicas int, c placementConstraints) ([]string, error) {
	log := zap.L()
	log.Debug("selectConstrained: start", zap.Int("candidates", len(nodes)), zap.Int("replicas", replicas),
		zap.Strings("on_different", c.onDifferent), zap.Strings("on_same", c.onSame))
	if replicas < 1 {
		log.Debug("selectConstrained: refusing, replica count below one", zap.Int("replicas", replicas))
		return nil, fmt.Errorf("replicas must be >= 1, got %d", replicas)
	}

	// Eligibility: every node must carry all referenced label keys.
	keys := c.referencedKeys()
	eligible := make([]placementNode, 0, len(nodes))
	for _, n := range nodes {
		ok := true
		missing := ""
		for _, k := range keys {
			if n.labels[k] == "" {
				ok = false
				missing = k
				break
			}
		}
		log.Debug("selectConstrained: candidate eligibility", zap.String("node", n.node),
			zap.Bool("eligible", ok), zap.String("missing_label", missing), zap.Uint64("free_gb", n.freeGB))
		if ok {
			eligible = append(eligible, n)
		}
	}

	// Partition into on-same groups (one group when no on-same keys).
	groups := make(map[string][]placementNode)
	var groupOrder []string
	for _, n := range eligible {
		gk := onSameKey(n, c.onSame)
		if _, seen := groups[gk]; !seen {
			groupOrder = append(groupOrder, gk)
		}
		groups[gk] = append(groups[gk], n)
	}
	log.Debug("selectConstrained: partitioned into on-same groups",
		zap.Int("eligible", len(eligible)), zap.Int("groups", len(groupOrder)))

	// Solve each group; keep the best full selection across groups.
	var bestNodes []string
	var bestTotal uint64
	for _, gk := range groupOrder {
		picked, total := pickWithinGroup(groups[gk], replicas, c.onDifferent)
		if len(picked) < replicas {
			log.Debug("selectConstrained: group cannot host every replica",
				zap.Int("group_size", len(groups[gk])), zap.Strings("picked", picked), zap.Int("replicas", replicas))
			continue
		}
		if bestNodes == nil || total > bestTotal || (total == bestTotal && lexLess(picked, bestNodes)) {
			log.Debug("selectConstrained: group is the best so far",
				zap.Strings("picked", picked), zap.Uint64("total_free_gb", total))
			bestNodes, bestTotal = picked, total
		} else {
			log.Debug("selectConstrained: group loses to the best so far",
				zap.Strings("picked", picked), zap.Uint64("total_free_gb", total), zap.Uint64("best_free_gb", bestTotal))
		}
	}

	if bestNodes == nil {
		err := placementError(replicas, c, len(eligible))
		log.Debug("selectConstrained: no group satisfies the constraints", zap.Error(err))
		return nil, err
	}
	log.Debug("selectConstrained: selected", zap.Strings("nodes", bestNodes), zap.Uint64("total_free_gb", bestTotal))
	return bestNodes, nil
}

// pickWithinGroup greedily selects up to `replicas` most-free nodes from one
// on-same group such that no two share a value on any on-different key. Returns
// the chosen node names and their total free space.
func pickWithinGroup(group []placementNode, replicas int, onDifferent []string) ([]string, uint64) {
	log := zap.L()
	log.Debug("pickWithinGroup: start", zap.Int("group_size", len(group)), zap.Int("replicas", replicas),
		zap.Strings("on_different", onDifferent))
	sorted := append([]placementNode(nil), group...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].freeGB != sorted[j].freeGB {
			return sorted[i].freeGB > sorted[j].freeGB
		}
		return sorted[i].node < sorted[j].node
	})

	used := make(map[string]map[string]bool, len(onDifferent))
	for _, k := range onDifferent {
		used[k] = make(map[string]bool)
	}

	var picked []string
	var total uint64
	for _, n := range sorted {
		collides := false
		collidesOn := ""
		for _, k := range onDifferent {
			if used[k][n.labels[k]] {
				collides = true
				collidesOn = k
				break
			}
		}
		if collides {
			log.Debug("pickWithinGroup: skipping node, label value already taken",
				zap.String("node", n.node), zap.String("key", collidesOn), zap.String("value", n.labels[collidesOn]))
			continue
		}
		for _, k := range onDifferent {
			used[k][n.labels[k]] = true
		}
		log.Debug("pickWithinGroup: picked node", zap.String("node", n.node), zap.Uint64("free_gb", n.freeGB))
		picked = append(picked, n.node)
		total += n.freeGB
		if len(picked) == replicas {
			break
		}
	}
	log.Debug("pickWithinGroup: done", zap.Strings("picked", picked), zap.Uint64("total_free_gb", total))
	return picked, total
}

// onSameKey builds a group key from a node's values for the on-same label keys.
// The empty string (no on-same keys) puts every node in one group.
func onSameKey(n placementNode, onSame []string) string {
	if len(onSame) == 0 {
		return ""
	}
	parts := make([]string, len(onSame))
	for i, k := range onSame {
		parts[i] = n.labels[k]
	}
	return strings.Join(parts, "\x1f")
}

// lexLess reports whether a sorts before b lexicographically element-wise, used
// only as a deterministic tie-break between equally-good selections.
func lexLess(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// placementError explains why no selection was found, naming the binding constraint.
func placementError(replicas int, c placementConstraints, eligible int) error {
	log := zap.L()
	switch {
	case len(c.onSame) > 0:
		log.Debug("placementError: on-same constraint is binding", zap.Strings("on_same", c.onSame), zap.Int("eligible", eligible))
		return fmt.Errorf("no group of nodes sharing %v can host %d replica(s) satisfying on-different %v (%d eligible nodes)", c.onSame, replicas, c.onDifferent, eligible)
	case len(c.onDifferent) > 0:
		log.Debug("placementError: on-different constraint is binding", zap.Strings("on_different", c.onDifferent), zap.Int("eligible", eligible))
		return fmt.Errorf("insufficient fault domains for replicas-on-different %v: cannot place %d replica(s) on distinct values (%d eligible nodes)", c.onDifferent, replicas, eligible)
	default:
		log.Debug("placementError: capacity is binding", zap.Int("replicas", replicas), zap.Int("eligible", eligible))
		return fmt.Errorf("insufficient capacity for auto-placement: need %d node(s), only %d have enough free space", replicas, eligible)
	}
}
