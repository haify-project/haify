package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	pb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/wanproxy"
)

// RepairWanProxy reconciles a WAN resource's replication tunnels with the
// controller's current view of its nodes.
//
// Leg names used to embed a node's address, so renumbering a node orphaned its
// tunnel: it kept replicating under the old name while everything that
// recomputed the name looked for one that did not exist — status reported a
// phantom outage, and deprovisioning would have missed it entirely. Legs are
// named after node names now, which do not move, but deployments created before
// that still carry address-named legs. This converges them.
//
// Re-provisioning restarts the tunnels, so DryRun exists to show what would
// change first.
func (s *Server) RepairWanProxy(ctx context.Context, req *pb.RepairWanProxyRequest) (*pb.RepairWanProxyResponse, error) {
	log := s.serverLog()
	name := req.GetName()
	log.Debug("RepairWanProxy: repairing", zap.String("resource", name), zap.Bool("dry_run", req.GetDryRun()))
	if name == "" {
		log.Debug("RepairWanProxy: no resource name")
		return &pb.RepairWanProxyResponse{Success: false, Message: "resource name is required"}, nil
	}
	if s.ctrl.db == nil {
		log.Debug("RepairWanProxy: no database")
		return &pb.RepairWanProxyResponse{Success: false, Message: "database not available"}, nil
	}

	dbRes, err := s.ctrl.db.GetResource(ctx, name)
	if err != nil || dbRes == nil {
		log.Debug("RepairWanProxy: resource not found", zap.String("resource", name), zap.Error(err))
		return &pb.RepairWanProxyResponse{Success: false, Message: fmt.Sprintf("resource %q not found", name)}, nil
	}
	if !dbRes.WANMode {
		log.Debug("RepairWanProxy: not a WAN resource", zap.String("resource", name))
		return &pb.RepairWanProxyResponse{
			Success: false,
			Message: fmt.Sprintf("resource %q is not a WAN resource", name),
		}, nil
	}
	return s.repairWanLegs(ctx, dbRes, req.GetDryRun(), false), nil
}

// repairWanLegs converges a WAN resource's tunnels on its database record.
// skipCheck provisions without probing that the DR endpoint answers — for an
// endpoint the operator knows is not reachable yet.
func (s *Server) repairWanLegs(ctx context.Context, dbRes *database.Resource, dryRun, skipCheck bool) *pb.RepairWanProxyResponse {
	log := s.serverLog()
	name := dbRes.Name
	log.Debug("repairWanLegs: converging", zap.String("resource", name), zap.String("dr_endpoint", dbRes.DREndpoint),
		zap.Bool("dry_run", dryRun), zap.Bool("skip_check", skipCheck))
	rm := s.ctrl.resources
	multi := rm.wanMultiSpecFor(dbRes)
	deploy := rm.wanproxyDeployClient()

	resp := &pb.RepairWanProxyResponse{Success: true}
	for _, leg := range multi.Legs() {
		log.Debug("repairWanLegs: expected leg", zap.String("host", leg.PrimaryNodeAddr), zap.String("leg", leg.Resource))
		resp.ExpectedLegs = append(resp.ExpectedLegs,
			fmt.Sprintf("%s -> %s", leg.PrimaryNodeAddr, leg.Resource))
	}

	start := time.Now()
	stale, err := wanproxy.FindStaleLegs(ctx, deploy, multi)
	if err != nil {
		log.Debug("repairWanLegs: stale legs not found", zap.Duration("took", time.Since(start)), zap.Error(err))
		return &pb.RepairWanProxyResponse{Success: false, Message: err.Error()}
	}
	for _, st := range stale {
		log.Debug("repairWanLegs: stale leg", zap.String("host", st.Host), zap.String("leg", st.LegID))
		resp.RemovedLegs = append(resp.RemovedLegs, fmt.Sprintf("%s: %s", st.Host, st.LegID))
	}
	log.Debug("repairWanLegs: legs compared", zap.Int("expected", len(resp.ExpectedLegs)), zap.Int("stale", len(stale)),
		zap.Duration("took", time.Since(start)))

	if dryRun {
		log.Debug("repairWanLegs: dry run, nothing changed", zap.String("resource", name))
		resp.AlreadyConsistent = len(stale) == 0
		resp.Message = fmt.Sprintf("dry run: %d leg(s) expected, %d stale instance(s) would be removed",
			len(resp.ExpectedLegs), len(stale))
		return resp
	}

	multi.SkipReachabilityCheck = skipCheck
	multi.BinaryFor = rm.wanproxyBinaryResolver(ctx,
		append(append([]string{}, multi.PrimaryNodeAddrs...), multi.DRNodeAddr))
	s.ctrl.logger.Info("Repairing WAN proxy legs",
		zap.String("resource", name),
		zap.Strings("expected", resp.ExpectedLegs),
		zap.Int("stale", len(stale)))

	// Stale legs go first, before their replacements are provisioned.
	//
	// The tempting order is the other one — stand the new tunnel up, then sweep
	// the old — so there is never a moment without a path to the DR. It does not
	// work for the case this exists for. Renaming a leg keeps its ports and
	// changes only its name, so the orphan is still holding the very loopback
	// port its replacement needs; provisioning first just fails to bind, and the
	// repair leaves both the old and new units broken.
	//
	// The gap this opens is one provisioning round on an asynchronous DR leg,
	// which DRBD closes by reconnecting and catching up. That is the cheaper
	// failure by a wide margin.
	start = time.Now()
	if err := wanproxy.RemoveStaleLegs(ctx, deploy, stale); err != nil {
		log.Debug("repairWanLegs: stale legs not removed", zap.Duration("took", time.Since(start)), zap.Error(err))
		return &pb.RepairWanProxyResponse{
			Success: false,
			Message: fmt.Sprintf("remove stale legs for %s: %v", name, err),
		}
	}
	log.Debug("repairWanLegs: stale legs removed, provisioning", zap.Duration("took", time.Since(start)))
	start = time.Now()
	if err := wanproxy.ProvisionMulti(ctx, deploy, multi); err != nil {
		log.Debug("repairWanLegs: provisioning failed", zap.Duration("took", time.Since(start)), zap.Error(err))
		return &pb.RepairWanProxyResponse{
			Success: false,
			Message: fmt.Sprintf("provision legs for %s: %v", name, err),
		}
	}

	resp.AlreadyConsistent = len(stale) == 0
	resp.Message = fmt.Sprintf("repaired %d leg(s); removed %d stale instance(s)",
		len(resp.ExpectedLegs), len(stale))
	log.Debug("repairWanLegs: repaired", zap.String("resource", name), zap.Int("legs", len(resp.ExpectedLegs)),
		zap.Int("removed", len(stale)), zap.Duration("took", time.Since(start)))
	return resp
}
