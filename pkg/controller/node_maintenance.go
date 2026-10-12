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
	state := NodeStateOnline
	if on {
		state = NodeStateMaintenance
	}
	nm.controller.traceLogger().Debug("NodeManager.setMaintenance: start",
		zap.String("address", address), zap.Bool("on", on), zap.String("state", string(state)))
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
