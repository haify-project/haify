package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/event"
)

// Moving a replica.
//
// A replica moves by adding the new one, waiting for it to be UpToDate, and
// only then removing the old one: the resource has one copy more for the
// duration, never one fewer. The removal is recorded on the resource
// (MoveFrom/MoveTo) before the add, so a controller that restarts or fails
// over mid-sync picks the move up again (resumeMoves) instead of leaving the
// extra replica behind.

const (
	movePollInterval = 15 * time.Second
	moveTimeout      = 48 * time.Hour
)

// MoveReplica starts moving resource's replica from one node to another.
// It returns once the new replica is added; the old one goes when the new
// one is in sync.
func (rm *ResourceManager) MoveReplica(ctx context.Context, resource, from, to string) error {
	log := rm.resourcesLog()
	log.Debug("MoveReplica: start", zap.String("resource", resource), zap.String("from", from), zap.String("to", to))
	dbRes, err := rm.controller.db.GetResource(ctx, resource)
	if err != nil || dbRes == nil {
		log.Debug("MoveReplica: resource not found", zap.String("resource", resource), zap.Error(err))
		return fmt.Errorf("resource %q not found", resource)
	}
	var refusal error
	switch {
	case dbRes.MoveFrom != "":
		refusal = fmt.Errorf("%s is already moving its replica from %s to %s", resource, dbRes.MoveFrom, dbRes.MoveTo)
	case !contains(splitCSV(dbRes.Nodes), from):
		refusal = fmt.Errorf("%s holds no replica of %s", from, resource)
	case dbRes.WANMode && from == dbRes.DRNode:
		refusal = fmt.Errorf("%s is the DR node of %s; moving it is `resource remove-dr` and `add-dr`", from, resource)
	case resource == SelfHaResource:
		refusal = fmt.Errorf("the controller's own metadata is moved with `ha self`, not here")
	}
	if refusal != nil {
		log.Debug("MoveReplica: refused", zap.String("resource", resource), zap.Error(refusal))
		return refusal
	}
	// Refused now rather than once the new replica has synced: removing the
	// old one deletes its locked snapshots.
	if err := rm.controller.assertResourceUnlocked(ctx, resource, "moving the replica off "+from); err != nil {
		log.Debug("MoveReplica: resource is locked", zap.String("resource", resource), zap.Error(err))
		return err
	}
	if info, err := rm.GetResource(ctx, resource); err == nil {
		for node, st := range info.NodeStates {
			if strings.EqualFold(st.Role, "Primary") && (node == from || rm.controller.ResolveHost(node) == rm.controller.ResolveHost(from)) {
				log.Debug("MoveReplica: source is Primary; refusing", zap.String("resource", resource), zap.String("node", from))
				return fmt.Errorf("%s is Primary on %s; move the role first (`haify node drain %s`)", resource, from, from)
			}
		}
	} else {
		log.Debug("MoveReplica: status unavailable; not checking roles", zap.String("resource", resource), zap.Error(err))
	}
	dbRes.MoveFrom, dbRes.MoveTo = from, to
	err = rm.controller.db.SaveResource(ctx, dbRes)
	log.Debug("MoveReplica: recorded move", zap.String("resource", resource), zap.Error(err))
	if err != nil {
		return err
	}
	if err := rm.AddReplica(ctx, resource, to); err != nil {
		log.Debug("MoveReplica: adding new replica failed; clearing move", zap.String("resource", resource), zap.Error(err))
		rm.clearMove(context.WithoutCancel(ctx), resource)
		return err
	}
	log.Debug("MoveReplica: new replica added; waiting for it in the background", zap.String("resource", resource), zap.String("to", to))
	go rm.finishMove(rm.controller.ctx, resource, from, to)
	return nil
}

// resumeMoves picks up moves a previous controller left half-done.
func (rm *ResourceManager) resumeMoves(ctx context.Context) {
	log := rm.resourcesLog()
	if rm.controller.db == nil {
		log.Debug("resumeMoves: no database")
		return
	}
	resources, err := rm.controller.db.ListResources(ctx)
	if err != nil {
		log.Debug("resumeMoves: cannot list resources", zap.Error(err))
		return
	}
	for _, r := range resources {
		if r.MoveFrom != "" {
			log.Debug("resumeMoves: resuming move", zap.String("resource", r.Name), zap.String("from", r.MoveFrom), zap.String("to", r.MoveTo))
			go rm.finishMove(ctx, r.Name, r.MoveFrom, r.MoveTo)
		}
	}
}

// finishMove waits for to's replica to be UpToDate, then removes from's.
func (rm *ResourceManager) finishMove(ctx context.Context, resource, from, to string) {
	log := rm.resourcesLog()
	log.Debug("finishMove: waiting for new replica", zap.String("resource", resource), zap.String("from", from), zap.String("to", to))
	deadline := time.Now().Add(moveTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			log.Debug("finishMove: context done; move stays recorded", zap.String("resource", resource), zap.Error(ctx.Err()))
			return
		case <-time.After(movePollInterval):
		}
		upToDate := rm.replicaUpToDate(ctx, resource, to)
		log.Debug("finishMove: poll", zap.String("resource", resource), zap.String("to", to), zap.Bool("up_to_date", upToDate))
		if !upToDate {
			continue
		}
		err := rm.RemoveReplica(ctx, resource, from)
		log.Debug("finishMove: removed old replica", zap.String("resource", resource), zap.String("from", from), zap.Error(err))
		if err != nil {
			rm.moveEvent(resource, event.SeverityWarning, fmt.Sprintf("the new replica of %s on %s is in sync, but removing "+
				"the one on %s failed: %v; remove it with `haify resource remove-replica %s --node %s`", resource, to, from, err, resource, from))
		} else {
			rm.moveEvent(resource, event.SeverityInfo, fmt.Sprintf("the replica of %s moved from %s to %s", resource, from, to))
		}
		rm.clearMove(ctx, resource)
		return
	}
	log.Debug("finishMove: timed out", zap.String("resource", resource), zap.Duration("timeout", moveTimeout))
	rm.moveEvent(resource, event.SeverityWarning, fmt.Sprintf("the new replica of %s on %s did not reach UpToDate in %s; "+
		"the one on %s is kept", resource, to, moveTimeout, from))
	rm.clearMove(ctx, resource)
}

func (rm *ResourceManager) replicaUpToDate(ctx context.Context, resource, node string) bool {
	log := rm.resourcesLog()
	info, err := rm.GetResource(ctx, resource)
	if err != nil {
		log.Debug("replicaUpToDate: status unavailable", zap.String("resource", resource), zap.Error(err))
		return false
	}
	addr := rm.controller.ResolveHost(node)
	for name, st := range info.NodeStates {
		if (name == node || rm.controller.ResolveHost(name) == addr) && st.DiskState == "UpToDate" {
			return true
		}
	}
	return false
}

func (rm *ResourceManager) clearMove(ctx context.Context, resource string) {
	r, err := rm.controller.db.GetResource(ctx, resource)
	rm.resourcesLog().Debug("clearMove: clearing recorded move", zap.String("resource", resource), zap.Bool("found", r != nil), zap.Error(err))
	if err == nil && r != nil {
		r.MoveFrom, r.MoveTo = "", ""
		if err := rm.controller.db.SaveResource(ctx, r); err != nil {
			rm.controller.logger.Warn("Could not clear a finished move", zap.String("resource", resource), zap.Error(err))
		}
	}
}

func (rm *ResourceManager) moveEvent(resource string, sev event.Severity, msg string) {
	rm.controller.logger.Info(msg)
	if rm.controller.events != nil {
		rm.controller.events.Publish(event.Event{Type: event.TypeReplicaMoved, Severity: sev, Status: event.StatusInfo,
			Resource: resource, Message: msg})
	}
}
