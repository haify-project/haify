package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// setMaintenance drains (on=true) or undrains a node by its address, in the
// in-memory registry and in the database alike. The registry is what placement
// and tiebreaker selection read, so writing only the database — as drain once
// did — left a drained node taking new replicas until the controller restarted.
func (nm *NodeManager) setMaintenance(ctx context.Context, address string, on bool) error {
	return nm.setMaintenanceBy(ctx, address, on, "", "")
}

// setMaintenanceBy is setMaintenance, and records who drained the node and
// why, for `haify node list` and the web UI: a node that takes no replicas
// should say whose doing that is.
//
// The first drain's word stands. A node already in maintenance keeps the
// name and the time it was drained with, so that a second drain (the CSI
// driver finishing the one it started, an operator draining a node an
// automation already drained) does not rewrite who took it out of service.
func (nm *NodeManager) setMaintenanceBy(ctx context.Context, address string, on bool, by, reason string) error {
	state := NodeStateOnline
	if on {
		state = NodeStateMaintenance
	}
	log := nm.controller.traceLogger()
	log.Debug("NodeManager.setMaintenanceBy: start", zap.String("address", address), zap.Bool("on", on),
		zap.String("state", string(state)), zap.String("by", by), zap.String("reason", reason))
	if on {
		nm.mu.Lock()
		if n := nm.nodes[address]; n != nil {
			if n.State != NodeStateMaintenance || n.MaintenanceSince.IsZero() {
				n.MaintenanceBy, n.MaintenanceReason, n.MaintenanceSince = by, reason, time.Now()
			} else if n.MaintenanceBy == "" && n.MaintenanceReason == "" {
				// Drained before, by nobody on record: this drain is the
				// first to say.
				n.MaintenanceBy, n.MaintenanceReason = by, reason
			} else {
				log.Debug("NodeManager.setMaintenanceBy: already drained, keeping who drained it", zap.String("address", address),
					zap.String("by", n.MaintenanceBy))
			}
		}
		nm.mu.Unlock()
	}
	return nm.SetNodeState(ctx, address, state)
}

// SetNodeState sets a node's state by its address, in the registry and the
// database alike; maintenance and eviction (self_heal_evict.go) use it.
func (nm *NodeManager) SetNodeState(ctx context.Context, address string, state NodeState) error {
	log := nm.controller.traceLogger()
	log.Debug("NodeManager.SetNodeState: start", zap.String("address", address), zap.String("state", string(state)))

	nm.mu.Lock()
	n := nm.nodes[address]
	if n == nil {
		nm.mu.Unlock()
		log.Debug("NodeManager.SetNodeState: node not found", zap.String("address", address))
		return fmt.Errorf("node %s not found", address)
	}
	prev := n.State
	n.State = state
	if state != NodeStateMaintenance {
		// Whatever takes a node out of maintenance ends the record of who
		// put it there.
		n.MaintenanceBy, n.MaintenanceReason, n.MaintenanceSince = "", "", time.Time{}
	}
	rec := nodeRecord(n)
	nm.mu.Unlock()
	log.Debug("NodeManager.SetNodeState: registry updated", zap.String("address", address),
		zap.String("previous_state", string(prev)), zap.String("state", string(state)))

	if nm.controller.db == nil {
		log.Debug("NodeManager.SetNodeState: no database, registry only", zap.String("address", address))
		return nil
	}
	start := time.Now()
	dbNode, getErr := nm.controller.db.GetNode(ctx, address)
	log.Debug("NodeManager.SetNodeState: read node record", zap.String("address", address),
		zap.Bool("found", getErr == nil && dbNode != nil), zap.Duration("took", time.Since(start)), zap.Error(getErr))
	if getErr == nil && dbNode != nil {
		dbNode.State = string(state)
		dbNode.MaintenanceBy, dbNode.MaintenanceReason, dbNode.MaintenanceSince = rec.MaintenanceBy, rec.MaintenanceReason, rec.MaintenanceSince
		rec = dbNode
	}
	start = time.Now()
	if err := nm.controller.db.SaveNode(ctx, rec); err != nil {
		nm.mu.Lock()
		if cur := nm.nodes[address]; cur != nil {
			cur.State = prev
		}
		nm.mu.Unlock()
		log.Debug("NodeManager.SetNodeState: saving node record failed, registry state rolled back",
			zap.String("address", address), zap.String("state", string(prev)), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("save node state: %w", err)
	}
	log.Debug("NodeManager.SetNodeState: saved node record", zap.String("address", address),
		zap.String("state", string(state)), zap.Duration("took", time.Since(start)))
	return nil
}

// nodeStateByName returns the registry state of a node named by name,
// hostname or address, and false when the controller does not know it.
func (nm *NodeManager) nodeStateByName(name string) (NodeState, bool) {
	log := nm.controller.traceLogger()
	addr := nm.GetNodeAddressByName(name)
	if addr == "" {
		log.Debug("NodeManager.nodeStateByName: unknown node", zap.String("node", name))
		return "", false
	}
	nm.mu.RLock()
	defer nm.mu.RUnlock()
	if n := nm.nodes[addr]; n != nil {
		log.Debug("NodeManager.nodeStateByName: found", zap.String("node", name), zap.String("state", string(n.State)))
		return n.State, true
	}
	log.Debug("NodeManager.nodeStateByName: address no longer registered", zap.String("node", name), zap.String("address", addr))
	return "", false
}
