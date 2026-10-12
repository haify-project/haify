package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Gathering placement candidates from the cluster and running the solver
// (placement.go) over them, for a new resource and for replicas added to an
// existing one.

// selectPlacementNodes auto-picks `replicas` node names to host a new resource
// whose volumes need sizeGB total on pool. Candidates are the online nodes
// whose copy of pool can take the volume (see poolCapacity.admits), minus any
// node holding a replica of a do-not-place-with resource. The constraints then
// shape the selection. Used by CreateResource when the caller supplies no
// explicit nodes.
//
// warning is set when the replicas could not be spread across fault domains
// (see placement_domain.go).
func (rm *ResourceManager) selectPlacementNodes(ctx context.Context, pool string, sizeGB uint32, replicas int, onDifferent, onSame, doNotPlaceWith []string) ([]string, string, error) {
	log := loggerOfController(rm.controller)
	log.Debug("ResourceManager.selectPlacementNodes: start", zap.String("pool", pool), zap.Uint32("size_gb", sizeGB),
		zap.Int("replicas", replicas), zap.Strings("on_different", onDifferent), zap.Strings("on_same", onSame),
		zap.Strings("do_not_place_with", doNotPlaceWith))
	if replicas < 1 {
		log.Debug("ResourceManager.selectPlacementNodes: refusing, replica count below one", zap.Int("replicas", replicas))
		return nil, "", fmt.Errorf("replicas must be >= 1, got %d", replicas)
	}
	pool = normalizeManagedName(pool)
	cands, err := rm.placementCandidates(ctx, pool, uint64(sizeGB), doNotPlaceWith)
	if err != nil {
		log.Debug("ResourceManager.selectPlacementNodes: gathering candidates failed", zap.String("pool", pool), zap.Error(err))
		return nil, "", err
	}

	constraints := placementConstraints{onDifferent: onDifferent, onSame: onSame}
	picked, warning, err := spreadAcrossDomains(cands, replicas, constraints, rm.faultDomainKey())
	if err != nil {
		log.Debug("ResourceManager.selectPlacementNodes: no placement satisfies the request",
			zap.String("pool", pool), zap.Int("candidates", len(cands)), zap.Error(err))
		// "%dGB per replica" rather than "%dGB free per replica": on a thin pool
		// the volume is not required to fit in what is free, so naming free
		// space as the requirement would misdescribe why the placement failed.
		return nil, "", fmt.Errorf("%w (pool %q, %dGB per replica)", err, pool, sizeGB)
	}
	if warning != "" {
		rm.controller.logger.Warn("auto-placed replicas share a fault domain", zap.String("warning", warning))
	}
	rm.controller.logger.Info("auto-placed resource replicas",
		zap.Strings("nodes", picked), zap.String("pool", pool),
		zap.Uint32("size_gb", sizeGB), zap.Int("replicas", replicas),
		zap.Strings("on_different", onDifferent), zap.Strings("on_same", onSame),
		zap.Strings("do_not_place_with", doNotPlaceWith))
	return picked, warning, nil
}

// placementCandidates lists the online nodes whose copy of pool admits a
// volume of sizeGB, minus every node holding a diskful replica of a
// doNotPlaceWith resource. Zero admits any size.
func (rm *ResourceManager) placementCandidates(ctx context.Context, pool string, sizeGB uint64, doNotPlaceWith []string) ([]placementNode, error) {
	log := loggerOfController(rm.controller)
	log.Debug("ResourceManager.placementCandidates: start", zap.String("pool", pool), zap.Uint64("size_gb", sizeGB),
		zap.Strings("do_not_place_with", doNotPlaceWith))
	start := time.Now()
	pools, err := rm.controller.storage.ListPools(ctx)
	if err != nil {
		log.Debug("ResourceManager.placementCandidates: listing pools failed", zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, fmt.Errorf("list pools for placement: %w", err)
	}
	log.Debug("ResourceManager.placementCandidates: listed pools", zap.Int("count", len(pools)), zap.Duration("took", time.Since(start)))
	nodes, err := rm.controller.nodes.ListNodes(ctx)
	if err != nil {
		log.Debug("ResourceManager.placementCandidates: listing nodes failed", zap.Error(err))
		return nil, fmt.Errorf("list nodes for placement: %w", err)
	}

	// Nodes to avoid: every diskful node of each do-not-place-with resource.
	excluded, err := rm.nodesHostingResources(ctx, doNotPlaceWith)
	if err != nil {
		log.Debug("ResourceManager.placementCandidates: resolving do-not-place-with failed", zap.Error(err))
		return nil, err
	}

	// Pools report their host as an address; map it back to a node name (raw and
	// resolved), and capture online state + labels by name.
	nameByAddr := make(map[string]string, len(nodes)*2)
	online := make(map[string]bool, len(nodes))
	labelsByName := make(map[string]map[string]string, len(nodes))
	for _, n := range nodes {
		nameByAddr[n.Address] = n.Name
		nameByAddr[rm.controller.ResolveHost(n.Address)] = n.Name
		online[n.Name] = n.State == NodeStateOnline
		labelsByName[n.Name] = n.Labels
	}

	// One lookup for the pool's recorded type, not one per node: the record is
	// keyed by pool name and every node's copy of a pool shares it.
	recordedThin := rm.poolRecordedThin(ctx, pool)

	var cands []placementNode
	seen := make(map[string]bool)
	for _, p := range pools {
		if normalizeManagedName(p.Name) != pool {
			continue
		}
		capacity := poolPlacementCapacity(p, recordedThin)
		capacity.maxOvercommit = rm.maxOvercommit()
		if !capacity.admits(sizeGB) {
			log.Debug("ResourceManager.placementCandidates: pool copy rejected, no room",
				zap.String("pool", pool), zap.String("host", p.Node), zap.Uint64("free_gb", capacity.freeGB))
			continue
		}
		name := nameByAddr[p.Node]
		if name == "" {
			name = p.Node // fall back to whatever the pool recorded
		}
		if !online[name] || seen[name] || excluded[name] {
			log.Debug("ResourceManager.placementCandidates: node skipped", zap.String("node", name),
				zap.Bool("online", online[name]), zap.Bool("duplicate", seen[name]), zap.Bool("excluded", excluded[name]))
			continue
		}
		seen[name] = true
		log.Debug("ResourceManager.placementCandidates: node is a candidate", zap.String("node", name),
			zap.Uint64("free_gb", capacity.freeGB), zap.Bool("thin", capacity.thin))
		cands = append(cands, placementNode{node: name, freeGB: capacity.freeGB, labels: labelsByName[name],
			thin: capacity.thin, freeBytes: capacity.freeBytes})
	}
	log.Debug("ResourceManager.placementCandidates: done", zap.String("pool", pool), zap.Int("count", len(cands)))
	return cands, nil
}

// selectAdditionalReplicas picks count more nodes for a resource that already
// has replicas on existing, keeping the constraints true of the whole set: a
// new node may not repeat an existing replica's value for an onDifferent key,
// and must share the existing replicas' value for an onSame key. Nodes in
// barred are never picked: a resource's tiebreaker or diskless client, which
// AddReplica refuses, for a caller that cannot turn one into a replica.
func (rm *ResourceManager) selectAdditionalReplicas(ctx context.Context, pool string, sizeGB uint64, count int, existing, barred, onDifferent, onSame []string) ([]string, error) {
	log := loggerOfController(rm.controller)
	log.Debug("ResourceManager.selectAdditionalReplicas: start", zap.String("pool", pool), zap.Uint64("size_gb", sizeGB),
		zap.Int("count", count), zap.Strings("existing", existing), zap.Strings("barred", barred),
		zap.Strings("on_different", onDifferent), zap.Strings("on_same", onSame))
	pool = normalizeManagedName(pool)
	cands, err := rm.placementCandidates(ctx, pool, sizeGB, nil)
	if err != nil {
		log.Debug("ResourceManager.selectAdditionalReplicas: gathering candidates failed", zap.Error(err))
		return nil, err
	}
	nodes, err := rm.controller.nodes.ListNodes(ctx)
	if err != nil {
		log.Debug("ResourceManager.selectAdditionalReplicas: listing nodes failed", zap.Error(err))
		return nil, err
	}
	labelsByName := make(map[string]map[string]string, len(nodes))
	for _, n := range nodes {
		labelsByName[n.Name] = n.Labels
	}
	have := make(map[string]bool, len(existing)+len(barred))
	for _, n := range existing {
		have[n] = true
	}
	for _, n := range barred {
		have[n] = true
	}

	var filtered, apart []placementNode
	key := rm.faultDomainKey()
	usedDomains := map[string]bool{}
	for _, e := range existing {
		usedDomains[faultDomain(e, labelsByName[e], key)] = true
	}
	for _, c := range cands {
		if have[c.node] || !fitsExistingReplicas(c.labels, existing, labelsByName, onDifferent, onSame) {
			log.Debug("ResourceManager.selectAdditionalReplicas: candidate excluded",
				zap.String("node", c.node), zap.Bool("already_holds_or_barred", have[c.node]))
			continue
		}
		filtered = append(filtered, c)
		newDomain := !usedDomains[faultDomain(c.node, c.labels, key)]
		log.Debug("ResourceManager.selectAdditionalReplicas: candidate kept",
			zap.String("node", c.node), zap.Bool("new_fault_domain", newDomain))
		if newDomain {
			apart = append(apart, c)
		}
	}
	constraints := placementConstraints{onDifferent: onDifferent, onSame: onSame}
	// A new replica in a fault domain the resource does not use yet is worth
	// more than one with more free space beside an existing copy.
	if picked, warning, err := spreadAcrossDomains(apart, count, constraints, key); err == nil && warning == "" {
		log.Debug("ResourceManager.selectAdditionalReplicas: placed in unused fault domains", zap.Strings("nodes", picked))
		return picked, nil
	}
	log.Debug("ResourceManager.selectAdditionalReplicas: unused fault domains cannot host them, using every eligible node",
		zap.Int("apart", len(apart)), zap.Int("filtered", len(filtered)))
	picked, err := selectConstrained(filtered, count, constraints)
	if err != nil {
		log.Debug("ResourceManager.selectAdditionalReplicas: no placement satisfies the request", zap.Error(err))
		return nil, fmt.Errorf("%w (pool %q, %d more replica(s) beside %s)", err, pool, count, strings.Join(existing, ", "))
	}
	log.Debug("ResourceManager.selectAdditionalReplicas: selected", zap.Strings("nodes", picked))
	return picked, nil
}

// fitsExistingReplicas reports whether a node with these labels may join the
// existing replicas under the constraints.
func fitsExistingReplicas(labels map[string]string, existing []string, labelsByName map[string]map[string]string, onDifferent, onSame []string) bool {
	for _, k := range onDifferent {
		for _, e := range existing {
			if v, ok := labelsByName[e][k]; ok && labels[k] == v {
				zap.L().Debug("fitsExistingReplicas: repeats an existing replica's on-different value",
					zap.String("existing", e), zap.String("key", k), zap.String("value", v))
				return false
			}
		}
	}
	for _, k := range onSame {
		for _, e := range existing {
			if v, ok := labelsByName[e][k]; ok && labels[k] != v {
				zap.L().Debug("fitsExistingReplicas: differs from an existing replica's on-same value",
					zap.String("existing", e), zap.String("key", k), zap.String("value", labels[k]), zap.String("want", v))
				return false
			}
		}
	}
	return true
}

// nodesHostingResources returns the set of node names holding a diskful replica
// of any of the named resources, used to enforce do-not-place-with. Unknown
// resource names are reported as an error so a typo does not silently weaken the
// anti-affinity constraint.
func (rm *ResourceManager) nodesHostingResources(ctx context.Context, resources []string) (map[string]bool, error) {
	log := loggerOfController(rm.controller)
	excluded := make(map[string]bool)
	if len(resources) == 0 || rm.controller.db == nil {
		log.Debug("ResourceManager.nodesHostingResources: nothing to exclude",
			zap.Int("resources", len(resources)), zap.Bool("has_db", rm.controller.db != nil))
		return excluded, nil
	}
	for _, name := range resources {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		start := time.Now()
		dbRes, err := rm.controller.db.GetResource(ctx, name)
		if err != nil || dbRes == nil {
			log.Debug("ResourceManager.nodesHostingResources: do-not-place-with resource not found",
				zap.String("resource", name), zap.Duration("took", time.Since(start)), zap.Error(err))
			return nil, fmt.Errorf("do-not-place-with resource %q not found", name)
		}
		nodes := splitCSV(dbRes.Nodes)
		log.Debug("ResourceManager.nodesHostingResources: excluding the nodes of a resource",
			zap.String("resource", name), zap.Strings("nodes", nodes), zap.Duration("took", time.Since(start)))
		for _, n := range nodes {
			excluded[n] = true
		}
	}
	return excluded, nil
}
