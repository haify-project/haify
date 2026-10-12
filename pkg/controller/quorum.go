package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// QuorumInfo is the vote arithmetic that decides whether a resource keeps
// serving as nodes are lost.
//
// It was previously reported nowhere. Working it out by hand needs two facts
// that are easy to get wrong: DRBD counts EVERY configured node — diskless
// tiebreakers and the off-site DR included — and the threshold is a strict
// majority of that total, not of the diskful replicas. The consequence is
// counter-intuitive: attaching a DR raises the majority from 2 to 3, which can
// cancel out the tiebreaker that was added to reach 2 in the first place.
type QuorumInfo struct {
	// Members is every node in the resource config.
	Members int
	// Required is the majority DRBD needs: Members/2 + 1.
	Required int
	// Online is how many members the reporting node currently sees, itself
	// included.
	Online int
	// HasQuorum is what DRBD reports, not a recomputation of the fields above.
	// They can disagree while a connection is coming up, and DRBD is the one
	// that decides whether I/O proceeds.
	HasQuorum bool
	// Tolerated is how many further members can be lost before I/O suspends.
	// Zero means the next failure stops the resource.
	Tolerated int
}

// quorumProbe reports the resource's membership and connection state from one
// node's point of view, plus DRBD's own quorum verdict.
const quorumProbe = `drbdsetup status %[1]s --verbose 2>/dev/null`

// Quorum reports a resource's vote arithmetic as seen from a node that holds it.
//
// The view is deliberately one node's: quorum is a per-partition property, so
// there is no cluster-wide answer to give. Asking the node that is Primary (or
// any survivor) is what an operator actually wants to know — "will THIS node
// keep serving?".
func (rm *ResourceManager) Quorum(ctx context.Context, resource string) (*QuorumInfo, error) {
	log := rm.controller.traceLogger()
	log.Debug("Quorum: reading", zap.String("resource", resource))
	if rm.deployment == nil {
		log.Debug("Quorum: no deployment client")
		return nil, fmt.Errorf("deployment client not set")
	}
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		log.Debug("Quorum: resource hosts unknown", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	if len(hosts) == 0 {
		log.Debug("Quorum: resource has no hosts", zap.String("resource", resource))
		return nil, fmt.Errorf("resource %q has no reachable hosts", resource)
	}

	// Prefer the Primary's view: it is the one whose I/O is at stake. Fall back
	// to whichever node answers.
	var out string
	for _, h := range hosts {
		start := time.Now()
		res, rerr := rm.deployment.Exec(ctx, []string{h}, fmt.Sprintf(quorumProbe, resource))
		log.Debug("Quorum: drbdsetup status --verbose", zap.String("resource", resource), zap.String("host", h),
			zap.Duration("took", time.Since(start)), zap.Error(rerr))
		if rerr != nil {
			continue
		}
		text := ""
		for _, r := range res.Hosts {
			text = r.Output
			break
		}
		if strings.TrimSpace(text) == "" {
			log.Debug("Quorum: empty status from host", zap.String("host", h))
			continue
		}
		if out == "" {
			out = text
		}
		if strings.Contains(text, "role:Primary") && !strings.Contains(text, "peer-role:Primary") {
			log.Debug("Quorum: using the Primary's view", zap.String("host", h))
			out = text
			break
		}
	}
	if out == "" {
		log.Debug("Quorum: no node answered", zap.String("resource", resource), zap.Strings("hosts", hosts))
		return nil, fmt.Errorf("could not read DRBD status for %q from any node", resource)
	}
	return parseQuorum(out), nil
}

// parseQuorum reads `drbdsetup status --verbose` output.
//
// Members are the reporting node plus every peer the config gives it; a peer
// line appears whether or not the connection is up, which is what makes the
// total countable from a single node. Online counts the reporting node plus the
// peers that are actually Connected.
func parseQuorum(out string) *QuorumInfo {
	info := &QuorumInfo{}
	peers, connected := 0, 0
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "quorum:yes") {
			info.HasQuorum = true
		}
		// A peer line names a connection; the resource's own line does not.
		if !strings.Contains(trimmed, "connection:") {
			continue
		}
		peers++
		if strings.Contains(trimmed, "connection:Connected") {
			connected++
		}
	}
	info.Members = peers + 1 // + the reporting node
	info.Required = info.Members/2 + 1
	info.Online = connected + 1
	info.Tolerated = info.Online - info.Required
	if info.Tolerated < 0 {
		info.Tolerated = 0
	}
	zap.L().Debug("parseQuorum: parsed", zap.Int("members", info.Members), zap.Int("required", info.Required),
		zap.Int("online", info.Online), zap.Int("tolerated", info.Tolerated), zap.Bool("has_quorum", info.HasQuorum))
	return info
}
