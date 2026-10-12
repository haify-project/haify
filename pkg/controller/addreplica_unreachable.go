package controller

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
)

// Adding a replica while a member is away.
//
// Adding a replica rewrites the config on every member and adjusts them all,
// so one member that does not answer used to block it — and that is exactly
// when a replica is most wanted: a node is down and the resource is short a
// copy. With allow_unreachable the add goes ahead on the members that answer,
// provided they are the majority of the resource's members (so the add cannot
// be what a minority partition does on its own). The members that were away
// keep their old config, which simply does not know the newcomer; they are
// recorded and repaired when they answer again (self_heal_return.go).
//
// A WAN resource's DR node is not skipped: its tunnel has to be rewritten
// for the newcomer, and that cannot wait.

// unreachableMembers returns the members (diskful and tiebreakers) that do
// not answer over SSH, or an error when that blocks the add.
func (rm *ResourceManager) unreachableMembers(ctx context.Context, dbRes *database.Resource, primaries, tiebreakers []string, allow bool) ([]string, error) {
	log := rm.controller.traceLogger()
	members := append(append([]string(nil), primaries...), tiebreakers...)
	log.Debug("unreachableMembers: probing members", zap.String("resource", dbRes.Name),
		zap.Strings("primaries", primaries), zap.Strings("tiebreakers", tiebreakers), zap.Bool("allow_unreachable", allow))
	var away []string
	for _, n := range members {
		ok := rm.answers(ctx, rm.controller.ResolveHost(n))
		log.Debug("unreachableMembers: member probed", zap.String("node", n), zap.Bool("answers", ok))
		if !ok {
			away = append(away, n)
		}
	}
	if dbRes.WANMode && dbRes.DRNode != "" && !rm.answers(ctx, rm.controller.ResolveHost(dbRes.DRNode)) {
		log.Debug("unreachableMembers: DR node does not answer", zap.String("node", dbRes.DRNode))
		return nil, fmt.Errorf("the DR node %s does not answer; its WAN tunnel has to be rewritten for a new replica", dbRes.DRNode)
	}
	if len(away) == 0 {
		log.Debug("unreachableMembers: every member answers", zap.String("resource", dbRes.Name))
		return nil, nil
	}
	if !allow {
		log.Debug("unreachableMembers: members away and not allowed", zap.Strings("away", away))
		return nil, fmt.Errorf("member(s) %s of %s do not answer; add the replica anyway with allow_unreachable "+
			"(--allow-unreachable), and they get the new config when they are back",
			strings.Join(away, ", "), dbRes.Name)
	}
	if 2*len(away) >= len(members) {
		log.Debug("unreachableMembers: no majority answers", zap.Int("away", len(away)), zap.Int("members", len(members)))
		return nil, fmt.Errorf("%d of the %d members of %s do not answer; adding a replica needs the majority",
			len(away), len(members), dbRes.Name)
	}
	if len(withoutAll(primaries, away)) == 0 {
		log.Debug("unreachableMembers: no replica answers to copy from", zap.Strings("away", away))
		return nil, fmt.Errorf("no replica of %s answers to copy from", dbRes.Name)
	}
	log.Debug("unreachableMembers: proceeding without the away members", zap.Strings("away", away))
	return away, nil
}

// withoutAll is list without any of drop.
func withoutAll(list, drop []string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if !contains(drop, v) {
			out = append(out, v)
		}
	}
	return out
}
