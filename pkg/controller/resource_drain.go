package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
)

// DrainNode takes a node out of service ahead of maintenance. It first marks
// the node maintenance — from then on placement, CSI provisioning and
// tiebreaker selection skip it, and the health check leaves the state alone —
// and then moves every resource the node is Primary for onto another replica.
//
// A resource that cannot be moved safely is left where it is and reported; the
// rest are still moved, and the node stays drained. It returns the resources
// that were moved.
func (rm *ResourceManager) DrainNode(ctx context.Context, nodeName string) ([]string, error) {
	moved, _, err := rm.DrainNodeWith(ctx, nodeName, DrainOptions{})
	return moved, err
}

// DrainOptions are DrainNodeWith's.
type DrainOptions struct {
	// KeepInUse moves only what Haify itself keeps Primary and leaves alone
	// what something else does (drainLeaves).
	KeepInUse bool
	// By and Reason say who drains the node and why, for the node list.
	By, Reason string
}

// DrainLeft is a resource a KeepInUse drain left Primary on the node.
type DrainLeft struct {
	Resource, Reason string
}

// DrainNodeWith is DrainNode with options. With KeepInUse it also returns
// the resources it left Primary on the node on purpose; those are not
// failures, and the node is drained with them still on it.
func (rm *ResourceManager) DrainNodeWith(ctx context.Context, nodeName string, opts DrainOptions) ([]string, []DrainLeft, error) {
	log := rm.resourcesLog()
	log.Debug("DrainNode: start", zap.String("node", nodeName), zap.Bool("keep_in_use", opts.KeepInUse),
		zap.String("by", opts.By), zap.String("reason", opts.Reason))
	if rm.controller.db == nil {
		log.Debug("DrainNode: no database", zap.String("node", nodeName))
		return nil, nil, fmt.Errorf("database not available")
	}
	addr := rm.controller.nodes.GetNodeAddressByName(nodeName)
	if addr == "" {
		log.Debug("DrainNode: node not registered", zap.String("node", nodeName))
		return nil, nil, fmt.Errorf("node %q is not registered", nodeName)
	}
	if err := rm.controller.nodes.setMaintenanceBy(ctx, addr, true, opts.By, opts.Reason); err != nil {
		log.Debug("DrainNode: could not mark maintenance", zap.String("node", nodeName), zap.Error(err))
		return nil, nil, fmt.Errorf("mark %q maintenance: %w", nodeName, err)
	}
	log.Debug("DrainNode: marked maintenance", zap.String("node", nodeName), zap.String("host", addr))

	allResources, err := rm.controller.db.ListResources(ctx)
	log.Debug("DrainNode: listed resources", zap.Int("count", len(allResources)), zap.Error(err))
	if err != nil {
		return nil, nil, fmt.Errorf("list resources: %w", err)
	}
	// The controller's own metadata goes last: evicting it moves this process
	// to another node, and anything still queued behind it would not be moved.
	sort.SliceStable(allResources, func(i, j int) bool {
		return allResources[j].Name == SelfHaResource && allResources[i].Name != SelfHaResource
	})

	var moved, refused []string
	var left []DrainLeft
	for _, dbRes := range allResources {
		if !containsString(splitCSV(dbRes.Nodes), nodeName) {
			continue
		}
		info, err := rm.GetResource(ctx, dbRes.Name)
		if err != nil {
			log.Debug("DrainNode: cannot read status", zap.String("resource", dbRes.Name), zap.Error(err))
			refused = append(refused, fmt.Sprintf("%s: cannot read its status: %v", dbRes.Name, err))
			continue
		}
		ns := info.NodeStates[nodeName]
		if ns == nil {
			ns = info.NodeStates[addr]
		}
		if ns == nil || ns.Role != "Primary" {
			log.Debug("DrainNode: not Primary on drained node; leaving", zap.String("resource", dbRes.Name),
				zap.Bool("has_state", ns != nil))
			continue
		}
		if opts.KeepInUse {
			if why := rm.drainLeaves(ctx, dbRes, nodeName, addr); why != "" {
				log.Debug("DrainNode: left Primary on the drained node", zap.String("resource", dbRes.Name), zap.String("reason", why))
				left = append(left, DrainLeft{Resource: dbRes.Name, Reason: why})
				continue
			}
		}
		log.Debug("DrainNode: moving Primary off drained node", zap.String("resource", dbRes.Name), zap.String("node", nodeName))
		how, err := rm.moveOffDrainedNode(ctx, info, nodeName)
		if err != nil {
			log.Debug("DrainNode: could not move", zap.String("resource", dbRes.Name), zap.Error(err))
			refused = append(refused, fmt.Sprintf("%s: %v", dbRes.Name, err))
			continue
		}
		rm.controller.logger.Info("drain: moved primary",
			zap.String("resource", dbRes.Name), zap.String("from", nodeName), zap.String("how", how))
		moved = append(moved, dbRes.Name)
	}

	log.Debug("DrainNode: done", zap.String("node", nodeName), zap.Strings("moved", moved), zap.Int("refused", len(refused)),
		zap.Int("left", len(left)))
	if len(refused) > 0 {
		return moved, left, fmt.Errorf("node %q is drained, but %d resource(s) are still Primary on it:\n  %s",
			nodeName, len(refused), strings.Join(refused, "\n  "))
	}
	return moved, left, nil
}

// drainLeaves says why a KeepInUse drain leaves resource Primary on the
// drained node, or "" when the drain moves it.
//
// What Haify moves is what Haify decided: a resource with a drbd-reactor
// promoter (a gateway, an HA service, an app, the controller's own metadata)
// is Primary where the promoter put it, and evicting it there is a planned
// switchover of a service built to switch over. Everything else is Primary
// because something uses it. A hypervisor or the CSI driver made it Primary
// for a VM or a Pod that is still running on it, and moving the role from
// under that means the I/O of a running workload fails; the system that put
// the workload there is the one to move it. A resource nobody is on record
// for is left as well while its device is open, or while that cannot be
// told: a device nobody has open is the only one a demotion cannot hurt.
func (rm *ResourceManager) drainLeaves(ctx context.Context, r *database.Resource, node, addr string) string {
	log := rm.resourcesLog()
	if rm.reactorManaged(ctx, r.Name) {
		log.Debug("drainLeaves: a promoter decides its Primary; moved", zap.String("resource", r.Name))
		return ""
	}
	if owner := externallyPromoted(r.Name, r.Labels); owner != "" {
		what := owner + " decides where it is Primary"
		if of := resourceOwner(r.Labels); of != "" {
			what += " (" + of + ")"
		}
		return what + "; it moves when what uses it leaves " + node
	}
	open, known, err := rm.deviceOpenKnown(ctx, addr, r.Name)
	switch {
	case err != nil:
		return fmt.Sprintf("whether its device is in use on %s could not be read (%v)", node, err)
	case !known:
		return fmt.Sprintf("the DRBD on %s does not report whether its device is in use", node)
	case open:
		return fmt.Sprintf("its device is open on %s (mounted, or in use by a VM or a service)", node)
	}
	log.Debug("drainLeaves: Primary with its device closed; moved", zap.String("resource", r.Name))
	return ""
}

// deviceOpenKnown is deviceOpen, and also says whether DRBD answered the
// question at all. `drbdsetup status --verbose` prints open:yes or open:no
// per volume from DRBD 9.2 on; an older one prints neither, and reading that
// silence as "closed" would have a drain demote a volume that is in use.
func (rm *ResourceManager) deviceOpenKnown(ctx context.Context, address, resource string) (open, known bool, err error) {
	log := rm.resourcesLog()
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, []string{address}, "sudo drbdsetup status "+resource+" --verbose")
	if err != nil {
		log.Debug("deviceOpenKnown: status failed", zap.String("host", address), zap.String("resource", resource),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return false, false, err
	}
	out, ok := appHostOutput(res, address)
	if !ok {
		log.Debug("deviceOpenKnown: status failed on host", zap.String("host", address), zap.String("resource", resource),
			zap.Duration("took", time.Since(start)))
		return false, false, fmt.Errorf("%s", res.FailureDetails())
	}
	open = strings.Contains(out, "open:yes")
	known = open || strings.Contains(out, "open:no")
	log.Debug("deviceOpenKnown: checked", zap.String("host", address), zap.String("resource", resource), zap.Bool("open", open),
		zap.Bool("known", known), zap.Duration("took", time.Since(start)))
	return open, known, nil
}

// moveOffDrainedNode moves one resource's Primary role off the drained node
// and says how it did.
//
// A resource drbd-reactor manages (an HA config, a gateway, the controller's
// own metadata) goes through the promoter's eviction, exactly as `haify ha
// evict` does: a raw demote fails while the promoter's mount holds the device,
// and even when it succeeds the promoter promotes it straight back.
//
// Anything else is demoted and then promoted on a target chosen by
// drainTarget — demote first because a single-primary resource refuses a
// second Primary. If that promote fails, the drained node is promoted again so
// the resource is not left without a Primary.
func (rm *ResourceManager) moveOffDrainedNode(ctx context.Context, info *ResourceInfo, drained string) (string, error) {
	log := rm.resourcesLog()
	name := info.Name
	if rm.reactorManaged(ctx, name) {
		log.Debug("moveOffDrainedNode: reactor-managed; evicting", zap.String("resource", name), zap.String("node", drained))
		if err := rm.EvictHa(ctx, name); err != nil {
			log.Debug("moveOffDrainedNode: evict failed", zap.String("resource", name), zap.Error(err))
			return "", fmt.Errorf("drbd-reactor manages it and evicting it failed: %v; fix that, then run `haify ha evict %s`", err, name)
		}
		return "drbd-reactor evict", nil
	}

	target, err := rm.drainTarget(info, drained)
	if err != nil {
		log.Debug("moveOffDrainedNode: no target", zap.String("resource", name), zap.Error(err))
		return "", err
	}
	log.Debug("moveOffDrainedNode: demoting then promoting", zap.String("resource", name), zap.String("from", drained), zap.String("to", target))
	if err := rm.SetSecondary(ctx, name, drained); err != nil {
		log.Debug("moveOffDrainedNode: demote failed", zap.String("resource", name), zap.Error(err))
		return "", fmt.Errorf("demote on %s: %w", drained, err)
	}
	if err := rm.SetPrimary(ctx, name, target, false); err != nil {
		log.Debug("moveOffDrainedNode: promote on target failed; promoting drained node back", zap.String("resource", name),
			zap.String("to", target), zap.Error(err))
		if rerr := rm.SetPrimary(ctx, name, drained, false); rerr != nil {
			log.Debug("moveOffDrainedNode: re-promote failed too; no Primary", zap.String("resource", name), zap.Error(rerr))
			return "", fmt.Errorf("promote on %s failed (%v) and re-promoting %s failed too (%v): the resource has no Primary",
				target, err, drained, rerr)
		}
		return "", fmt.Errorf("promote on %s failed (%v); %s was promoted back and is Primary again", target, err, drained)
	}
	return "promoted on " + target, nil
}

// reactorManaged reports whether a drbd-reactor promoter owns the resource's
// Primary role, so the controller must not demote or promote it directly.
func (rm *ResourceManager) reactorManaged(ctx context.Context, resource string) bool {
	log := rm.resourcesLog()
	if resource == SelfHaResource {
		log.Debug("reactorManaged: controller metadata resource", zap.String("resource", resource))
		return true
	}
	if cfg, err := rm.controller.db.GetHaConfig(ctx, resource); err == nil && cfg != nil {
		log.Debug("reactorManaged: has HA config", zap.String("resource", resource))
		return true
	}
	if gw, err := rm.controller.db.GetGatewayByResource(ctx, resource); err == nil && gw != nil {
		log.Debug("reactorManaged: has gateway", zap.String("resource", resource))
		return true
	}
	if app, err := rm.controller.db.GetAppByResource(ctx, resource); err == nil && app != nil {
		log.Debug("reactorManaged: has app", zap.String("resource", resource))
		return true
	}
	log.Debug("reactorManaged: not managed by drbd-reactor", zap.String("resource", resource))
	return false
}

// drainTarget picks the replica to take over from the drained node: the first
// diskful node in the resource's node order whose disk is UpToDate and whose
// connection is up. A WAN resource's DR node never qualifies — it takes over
// only when an operator declares the primary site lost — and neither does a
// node that is itself drained or offline. When nothing qualifies, the error
// names every candidate and why it was passed over.
func (rm *ResourceManager) drainTarget(info *ResourceInfo, drained string) (string, error) {
	log := rm.resourcesLog()
	diskless := make(map[string]bool)
	for _, n := range append(append([]string(nil), info.DisklessNodes...), info.DisklessClients...) {
		diskless[strings.TrimSpace(n)] = true
	}

	var why []string
	for _, n := range info.Nodes {
		n = strings.TrimSpace(n)
		if n == "" || n == drained || diskless[n] {
			continue
		}
		reason := rm.drainTargetUnfit(info, n)
		log.Debug("drainTarget: candidate", zap.String("resource", info.Name), zap.String("node", n), zap.String("unfit", reason))
		if reason == "" {
			return n, nil
		}
		why = append(why, fmt.Sprintf("%s (%s)", n, reason))
	}
	if len(why) == 0 {
		log.Debug("drainTarget: no other diskful replica", zap.String("resource", info.Name))
		return "", fmt.Errorf("no other diskful replica to take over")
	}
	log.Debug("drainTarget: no candidate fit", zap.String("resource", info.Name), zap.Strings("candidates", why))
	return "", fmt.Errorf("no replica can take over safely: %s", strings.Join(why, ", "))
}

// drainTargetUnfit says why node n cannot take over, or "" when it can.
func (rm *ResourceManager) drainTargetUnfit(info *ResourceInfo, n string) string {
	if info.WANMode && n == info.DRNode {
		return "DR node, never takes over automatically"
	}
	if st, ok := rm.controller.nodes.nodeStateByName(n); ok && st != NodeStateOnline {
		return "node is " + string(st)
	}
	ns := info.NodeStates[n]
	if ns == nil {
		ns = info.NodeStates[rm.controller.ResolveHost(n)]
	}
	if ns == nil {
		return "no DRBD status"
	}
	if ns.DiskState != "UpToDate" {
		disk := ns.DiskState
		if disk == "" {
			disk = "unknown"
		}
		return "disk " + disk
	}
	// Connection is empty only for the node whose status was read, which
	// therefore answered; any peer must be Connected.
	if ns.Connection != "" && ns.Connection != "Connected" {
		return "connection " + ns.Connection
	}
	return ""
}

// UndrainNode returns a drained node to service: placement may use it again.
// It moves nothing back.
func (rm *ResourceManager) UndrainNode(ctx context.Context, nodeName string) error {
	log := rm.resourcesLog()
	log.Debug("UndrainNode: start", zap.String("node", nodeName))
	if rm.controller.db == nil {
		log.Debug("UndrainNode: no database", zap.String("node", nodeName))
		return fmt.Errorf("database not available")
	}
	addr := rm.controller.nodes.GetNodeAddressByName(nodeName)
	if addr == "" {
		log.Debug("UndrainNode: node not registered", zap.String("node", nodeName))
		return fmt.Errorf("node %q is not registered", nodeName)
	}
	err := rm.controller.nodes.setMaintenance(ctx, addr, false)
	log.Debug("UndrainNode: cleared maintenance", zap.String("node", nodeName), zap.String("host", addr), zap.Error(err))
	return err
}
