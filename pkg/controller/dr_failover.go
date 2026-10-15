package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// drPrimarySiteProbeTimeout bounds the check that the primary site is really
// gone. A live node answers a status read in well under a second; a lost site
// does not answer at all.
const drPrimarySiteProbeTimeout = 5 * time.Second

// isDRPromoteRefusal reports whether a forced promote of a DR node failed for
// one of the two reasons a DR failover expects: the DR node still believes
// the old primary is Primary (its proxy on loopback stays up when the far side
// dies, until the ping timeout), or it has no quorum, which is what it has
// once it knows the primary site is gone and only itself is left.
func isDRPromoteRefusal(output string) bool {
	return strings.Contains(output, "Multiple primaries not allowed") || strings.Contains(output, "No quorum")
}

// drFailoverPromote promotes a WAN resource's DR node for a DR failover, the
// operator having declared the primary site lost.
//
// A resource with quorum enabled (every one Haify creates) cannot be promoted
// on a DR node that is alone: DRBD needs a majority of the resource's nodes,
// the DR node is one of three or more, and `primary --force` does not override
// quorum. So the DR node is cut loose from the primary site and quorum is
// turned off on it, at runtime only, before the forced promote. The
// configuration keeps quorum, so `dr-failback`, which runs `drbdadm adjust`
// once the sites reconnect, brings it back.
//
// With quorum off nothing stops a second Primary, which is the split brain
// quorum exists to prevent. That is only acceptable when the primary site
// really is gone, so the primary site's nodes are asked first: if one answers
// and is Primary, the failover is refused.
func (rm *ResourceManager) drFailoverPromote(ctx context.Context, resource, node, address string) error {
	log := rm.resourcesLog()
	log.Debug("drFailoverPromote: start", zap.String("resource", resource), zap.String("dr_node", node))
	if live := rm.primarySiteStillPrimary(ctx, resource, node); live != "" {
		log.Debug("drFailoverPromote: primary site still up, refusing", zap.String("resource", resource), zap.String("primary", live))
		return fmt.Errorf("DR failover refused: %s is still Primary for %s at the primary site and answers, so the site is not lost; "+
			"promoting the DR node now would leave two Primaries writing different data. Stop the workload there first "+
			"(the volume is demoted when it is unmounted), then run dr-failover again", live, resource)
	}
	rm.controller.logger.Warn("DR failover: disconnecting the DR node from the primary site and turning quorum off there before promoting",
		zap.String("resource", resource), zap.String("node", node))
	if err := rm.execAllSuccess(ctx, []string{address}, "sudo drbdadm disconnect --force "+resource,
		"disconnect the DR node from the primary site"); err != nil {
		log.Debug("drFailoverPromote: disconnect failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	if err := rm.execAllSuccess(ctx, []string{address}, "sudo drbdsetup resource-options "+resource+" --quorum=off",
		"turn quorum off on the DR node"); err != nil {
		log.Debug("drFailoverPromote: quorum off failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	start := time.Now()
	result, err := rm.deployment.DRBDPrimary(ctx, address, resource, true)
	log.Debug("drFailoverPromote: forced promote", zap.String("resource", resource), zap.String("host", address),
		zap.Bool("success", err == nil && result.Success), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("failed to promote %s on DR node %s: %w", resource, node, err)
	}
	if !result.Success {
		return fmt.Errorf("failed to promote %s on DR node %s: %s", resource, node, strings.TrimSpace(result.Output))
	}
	return nil
}

// primarySiteStillPrimary returns the first node of resource other than the DR
// node that answers within drPrimarySiteProbeTimeout and is Primary, or "".
// A node that does not answer is what a lost site looks like.
func (rm *ResourceManager) primarySiteStillPrimary(ctx context.Context, resource, drNode string) string {
	log := rm.resourcesLog()
	if rm.controller.db == nil {
		return ""
	}
	r, err := rm.controller.db.GetResource(ctx, resource)
	if err != nil || r == nil {
		log.Debug("primarySiteStillPrimary: no resource record", zap.String("resource", resource), zap.Error(err))
		return ""
	}
	drHost := rm.controller.ResolveHost(drNode)
	for _, n := range strings.Split(r.Nodes, ",") {
		n = strings.TrimSpace(n)
		if n == "" || n == drNode || rm.controller.ResolveHost(n) == drHost {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, drPrimarySiteProbeTimeout)
		st, err := rm.readDrbdStatus(probeCtx, rm.controller.ResolveHost(n), resource)
		cancel()
		log.Debug("primarySiteStillPrimary: probed", zap.String("resource", resource), zap.String("node", n),
			zap.Bool("answered", err == nil), zap.Error(err))
		if err == nil && st.Role == "Primary" {
			return n
		}
	}
	return ""
}
