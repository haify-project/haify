package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
)

// nodeReferences lists what still depends on a node, one entry per resource
// as "<resource> (<role>, <role>)": a diskful replica, a diskless quorum
// tiebreaker, a diskless client, the DR node of a WAN resource, or the node a
// gateway is active on. Records name nodes by name, but older and adopted
// ones may carry an address or hostname, so every identity of the node counts.
// An empty result means the node can be unregistered without orphaning
// anything the controller knows about.
func nodeReferences(resources []*database.Resource, gateways []*database.Gateway, idents ...string) []string {
	log := zap.L()
	log.Debug("nodeReferences: start", zap.Int("resources", len(resources)), zap.Int("gateways", len(gateways)),
		zap.Strings("idents", idents))
	is := make(map[string]bool, len(idents))
	for _, id := range idents {
		if id = strings.TrimSpace(id); id != "" {
			is[id] = true
		}
	}
	inList := func(list string) bool {
		for _, n := range strings.Split(list, ",") {
			if is[strings.TrimSpace(n)] {
				return true
			}
		}
		return false
	}

	roles := make(map[string][]string)
	add := func(resource, role string) {
		for _, r := range roles[resource] {
			if r == role {
				return
			}
		}
		roles[resource] = append(roles[resource], role)
	}
	replicaHere := make(map[string]bool)
	for _, r := range resources {
		if r == nil {
			continue
		}
		if inList(r.Nodes) {
			log.Debug("nodeReferences: holds a replica", zap.String("resource", r.Name))
			add(r.Name, "replica")
			replicaHere[r.Name] = true
		}
		if inList(r.DisklessNodes) {
			log.Debug("nodeReferences: is a tiebreaker", zap.String("resource", r.Name))
			add(r.Name, "tiebreaker")
		}
		if inList(r.DisklessClients) {
			log.Debug("nodeReferences: is a diskless client", zap.String("resource", r.Name))
			add(r.Name, "diskless client")
		}
		if r.WANMode && is[strings.TrimSpace(r.DRNode)] {
			log.Debug("nodeReferences: is the DR node", zap.String("resource", r.Name))
			add(r.Name, "DR node")
		}
	}
	for _, g := range gateways {
		if g == nil {
			continue
		}
		// A gateway's promoter runs on its resource's diskful replicas (never
		// a tiebreaker), so it is listed where the resource has a replica and
		// on whatever node it was last recorded active on.
		if replicaHere[g.Resource] || is[strings.TrimSpace(g.ActiveNode)] {
			log.Debug("nodeReferences: serves a gateway", zap.String("resource", g.Resource),
				zap.String("type", string(g.Type)), zap.String("active_node", g.ActiveNode))
			add(g.Resource, gatewayRole(g.Type))
		}
	}

	names := make([]string, 0, len(roles))
	for name := range roles {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, fmt.Sprintf("%s (%s)", name, strings.Join(roles[name], ", ")))
	}
	log.Debug("nodeReferences: done", zap.Int("count", len(out)))
	return out
}

func gatewayRole(t database.GatewayType) string {
	switch t {
	case database.GatewayTypeNFS:
		return "NFS gateway"
	case database.GatewayTypeISCSI:
		return "iSCSI gateway"
	case database.GatewayTypeNVMEOF:
		return "NVMe-oF gateway"
	case database.GatewayTypeSMB:
		return "SMB gateway"
	}
	return fmt.Sprintf("%s gateway", t)
}

// checkNodeUnreferenced refuses to unregister a node that resources or
// gateways still place on it. Dropping it from the registry would leave those
// records naming a node the controller can no longer resolve: its replica
// stays in every peer's config, and repair, failover and delete stop reaching
// it.
func (nm *NodeManager) checkNodeUnreferenced(ctx context.Context, node *NodeInfo, address string) error {
	log := loggerOfController(nm.controller)
	log.Debug("NodeManager.checkNodeUnreferenced: start", zap.String("node", node.Name), zap.String("address", address))
	db := nm.controller.db
	if db == nil {
		log.Debug("NodeManager.checkNodeUnreferenced: no database, nothing can reference it", zap.String("node", node.Name))
		return nil
	}
	start := time.Now()
	resources, err := db.ListResources(ctx)
	if err != nil {
		log.Debug("NodeManager.checkNodeUnreferenced: listing resources failed", zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("check what still uses node %s: list resources: %w", node.Name, err)
	}
	log.Debug("NodeManager.checkNodeUnreferenced: listed resources", zap.Int("count", len(resources)), zap.Duration("took", time.Since(start)))
	start = time.Now()
	gateways, err := db.ListGateways(ctx)
	if err != nil {
		log.Debug("NodeManager.checkNodeUnreferenced: listing gateways failed", zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("check what still uses node %s: list gateways: %w", node.Name, err)
	}
	log.Debug("NodeManager.checkNodeUnreferenced: listed gateways", zap.Int("count", len(gateways)), zap.Duration("took", time.Since(start)))
	refs := nodeReferences(resources, gateways, node.Name, node.Hostname, node.Address, address)
	if len(refs) == 0 {
		log.Debug("NodeManager.checkNodeUnreferenced: node is unreferenced", zap.String("node", node.Name))
		return nil
	}
	log.Debug("NodeManager.checkNodeUnreferenced: node still referenced", zap.String("node", node.Name), zap.Strings("references", refs))
	label := node.Name
	if label == "" {
		label = node.Address
	}
	return fmt.Errorf("node %s is still in use by: %s; move or remove these first",
		label, strings.Join(refs, "; "))
}
