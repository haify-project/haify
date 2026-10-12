package controller

import (
	"context"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// TiebreakerLabel opts a node out of automatic diskless-tiebreaker selection
// when set to "false" (`haify node label <node> haify.tiebreaker=false`). Use it on
// WAN/DR nodes, which cannot join a LAN resource's DRBD connection mesh.
const TiebreakerLabel = "haify.tiebreaker"

// selectTiebreaker picks a registered node, not already part of the resource,
// to serve as a diskless quorum tiebreaker. A node outside every replica's
// fault domain is preferred over any node inside one: a tiebreaker beside a
// replica falls with it and takes the survivor's quorum along. Then selection
// prefers, in order: online storage nodes, online compute-only nodes, then
// offline nodes; within each tier it is deterministic (lowest node name) so
// repeated creations are stable. Returns "" when no spare node is available — the caller then keeps
// the resource as a bare 2-node configuration.
//
// The storage-node preference matters in a mixed cluster: a hypervisor
// registered only to attach volumes as a diskless client (e.g. a Proxmox node)
// has no storage pool of its own. Dragging such a compute-only node into every
// 2-replica resource's quorum mesh is wrong — it should stay a pure client — so
// a real storage node is chosen for the tiebreaker whenever one is free.
func (rm *ResourceManager) selectTiebreaker(ctx context.Context, nodes []string) string {
	log := rm.resourcesLog()
	log.Debug("selectTiebreaker: start", zap.Strings("nodes", nodes))
	inUse := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		inUse[n] = true
	}

	all, err := rm.controller.nodes.ListNodes(ctx)
	if err != nil {
		rm.controller.logger.Warn("Failed to list nodes for tiebreaker selection", zap.Error(err))
		return ""
	}

	hasPool := rm.storageNodeSet(ctx)

	// Four tiers, most-preferred first. Within a tier, lowest name wins.
	var onlineStorage, onlineCompute, offlineStorage, offlineCompute []string
	for _, n := range all {
		// A drained node takes nothing new, not even a diskless vote: it is
		// about to go down, and a tiebreaker that goes down costs quorum.
		if n == nil || inUse[n.Name] || n.State == NodeStateMaintenance {
			if n != nil {
				log.Debug("selectTiebreaker: candidate skipped", zap.String("node", n.Name), zap.Bool("member", inUse[n.Name]),
					zap.String("state", string(n.State)))
			}
			continue
		}
		// A tiebreaker joins the resource's DRBD connection mesh, so it must sit
		// on the replication network like any other peer. A remote/DR node does
		// not: it is reached over a WAN and, on a cloud instance, its public
		// address is not even configured on an interface, so `drbdadm up` fails
		// with "IP <addr> not found on this host" — after the volumes exist.
		// Nodes labelled haify.tiebreaker=false are therefore never auto-selected;
		// label DR sites that way. Same spirit as the compute-only rule below.
		if strings.EqualFold(n.Labels[TiebreakerLabel], "false") {
			rm.controller.logger.Debug("Skipping tiebreaker candidate opted out by label",
				zap.String("node", n.Name), zap.String("label", TiebreakerLabel))
			continue
		}
		storage := hasPool[n.Name]
		log.Debug("selectTiebreaker: candidate", zap.String("node", n.Name), zap.String("state", string(n.State)), zap.Bool("storage", storage))
		switch {
		case n.State == NodeStateOnline && storage:
			onlineStorage = append(onlineStorage, n.Name)
		case n.State == NodeStateOnline:
			onlineCompute = append(onlineCompute, n.Name)
		case storage:
			offlineStorage = append(offlineStorage, n.Name)
		default:
			offlineCompute = append(offlineCompute, n.Name)
		}
	}

	labels := make(map[string]map[string]string, len(all))
	for _, n := range all {
		if n != nil {
			labels[n.Name] = n.Labels
		}
	}
	key := rm.faultDomainKey()
	used := map[string]bool{}
	for _, r := range nodes {
		used[faultDomain(r, labels[r], key)] = true
	}
	tiers := [][]string{onlineStorage, onlineCompute, offlineStorage, offlineCompute}
	for _, apart := range []bool{true, false} {
		for _, tier := range tiers {
			sort.Strings(tier)
			for _, n := range tier {
				if !apart || !used[faultDomain(n, labels[n], key)] {
					log.Debug("selectTiebreaker: chose", zap.String("node", n), zap.Bool("separate_fault_domain", apart))
					return n
				}
			}
		}
	}
	log.Debug("selectTiebreaker: no spare node", zap.Strings("nodes", nodes))
	return ""
}

// storageNodeSet returns the set of node names that host at least one storage
// pool, i.e. real storage nodes as opposed to compute-only clients. It uses the
// StorageManager's authoritative pool view (live discovery with a persisted
// fallback), because pools are not always mirrored into the resource DB — a
// direct db.ListPools can come back empty even when nodes clearly have pools.
// Pools record their node as either a name or an address, so both forms are
// mapped back to the node name. An empty set (no storage manager, or no pools
// anywhere) makes selectTiebreaker fall back to name order across all
// candidates — the pre-existing behavior.
func (rm *ResourceManager) storageNodeSet(ctx context.Context) map[string]bool {
	set := make(map[string]bool)
	if rm.controller.storage == nil {
		rm.resourcesLog().Debug("storageNodeSet: no storage manager")
		return set
	}
	defer func() {
		rm.resourcesLog().Debug("storageNodeSet: storage nodes", zap.Int("count", len(set)))
	}()
	pools, err := rm.controller.storage.ListPools(ctx)
	if err != nil {
		rm.controller.logger.Warn("Failed to list pools for tiebreaker selection", zap.Error(err))
		return set
	}
	for _, p := range pools {
		if p == nil || strings.TrimSpace(p.Node) == "" {
			continue
		}
		// p.Node may be a name or an address; record the canonical node name.
		if name := rm.controller.nodes.GetNodeNameByAddress(p.Node); name != "" {
			set[name] = true
		} else {
			set[p.Node] = true
		}
	}
	return set
}
