package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/inspect"
)

// gatherGuests adds every HA guest's definition, as each replica holds it,
// to an inspection. It asks the nodes: the database only knows what was
// defined at the last sync, not what a node holds today.
func (im *InspectionManager) gatherGuests(ctx context.Context, in *inspect.Input) {
	c := im.controller
	log := c.traceLogger()
	if c.db == nil || c.resources == nil || c.resources.deployment == nil {
		log.Debug("InspectionManager.gatherGuests: no database or deployment client")
		return
	}
	rm := c.resources
	start := time.Now()
	cfgs, err := c.db.ListHaConfigs(ctx)
	log.Debug("InspectionManager.gatherGuests: listed HA configs", zap.Int("count", len(cfgs)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return
	}
	for _, cfg := range cfgs {
		g := vmGuestOf(cfg)
		if g == nil {
			continue
		}
		guest := inspect.GuestDefinition{Resource: cfg.Resource, Domain: g.Domain}
		if rec, err := c.db.GetHaMigration(ctx, cfg.Resource); err == nil && rec != nil && !rec.Done && rm.live.get(cfg.Resource) == nil {
			log.Debug("InspectionManager.gatherGuests: unsettled live move", zap.String("resource", cfg.Resource), zap.String("phase", rec.Phase))
			guest.Unsettled = fmt.Sprintf("it stopped in phase %q moving from %s to %s; the promoter may be disabled on the replicas",
				rec.Phase, rec.SourceNode, rec.TargetNode)
		}
		def, err := c.db.GetVMDefinition(ctx, cfg.Resource)
		if err != nil || def == nil || def.Domain != g.Domain {
			log.Debug("InspectionManager.gatherGuests: no definition kept", zap.String("resource", cfg.Resource), zap.Error(err))
			in.Guests = append(in.Guests, guest)
			continue
		}
		hosts, err := rm.failoverHosts(ctx, cfg.Resource)
		if err != nil {
			log.Debug("InspectionManager.gatherGuests: no failover hosts", zap.String("resource", cfg.Resource), zap.Error(err))
			continue
		}
		st := assessVMDefinition(def, rm.nodeNames(hosts), rm.vmFactsByNode(rm.probeVMNodes(ctx, g, hosts, "")))
		guest.Kept, guest.UUID = true, def.UUID
		for _, n := range st.Nodes {
			guest.Nodes = append(guest.Nodes, inspect.GuestNode{Node: n.Node, State: n.State, UUID: n.UUID,
				Running: n.Running, Detail: n.Detail})
		}
		log.Debug("InspectionManager.gatherGuests: guest", zap.String("resource", cfg.Resource), zap.String("domain", g.Domain),
			zap.Bool("consistent", st.Consistent))
		in.Guests = append(in.Guests, guest)
	}
}
