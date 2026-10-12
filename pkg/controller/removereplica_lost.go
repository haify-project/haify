package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Removing the replica of a node that is gone for good.
//
// A normal removal stops DRBD on the leaving node, rewrites the config on
// every node and deletes the leaver's volume, so the leaver has to answer.
// When it never will — a dead disk controller, a host that was scrapped — that
// left no way to take it out at all, and the dead member kept counting toward
// quorum, kept its bitmap slot on every survivor (so add-replica could run out
// of slots), and blocked every operation that adjusts all members.
//
// The lost path runs nothing on the node: it rewrites the survivors' configs,
// adjusts them, frees the dead peer's bitmap slot with forget-peer and drops it
// from the registry. Its own storage and config stay where they are, because
// nothing can reach them; they must be cleaned before the node ever rejoins.
//
// It refuses rather than guesses. "Unreachable from the controller" is not the
// same as "gone": a node can be cut off from the controller and still be
// replicating. So the node must not answer over SSH, and every survivor that
// answers must see its DRBD connection down; and the survivors must hold
// quorum and an UpToDate copy without it, so that removing it cannot be what
// lets a minority partition carry on as if it were the cluster.

// memberRef is a resource member as its DRBD config and the controller know it.
type memberRef struct {
	drbdName string // the name in its `on` stanza, which is also how peers list it
	addr     string // where the controller reaches it
}

// answers reports whether a node answers over SSH.
func (rm *ResourceManager) answers(ctx context.Context, addr string) bool {
	log := rm.resourcesLog()
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, []string{addr}, "true")
	ok := err == nil && res != nil && res.AllSuccess() && len(res.Hosts) > 0
	log.Debug("answers: probed node over SSH", zap.String("host", addr), zap.Bool("answers", ok),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return ok
}

// survivorView is what one surviving member's DRBD says.
type survivorView struct {
	answered      bool
	quorum        bool
	upToDate      bool
	seesConnected map[string]bool // peer DRBD name -> its connection is up
}

// readSurvivorView reads resource's DRBD status on addr.
func (rm *ResourceManager) readSurvivorView(ctx context.Context, addr, resource string) survivorView {
	log := rm.resourcesLog()
	start := time.Now()
	res, err := rm.deployment.DRBDStatus(ctx, []string{addr}, resource)
	log.Debug("readSurvivorView: read DRBD status", zap.String("resource", resource), zap.String("host", addr),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || res == nil {
		log.Debug("readSurvivorView: no status; survivor counts as not answering", zap.String("resource", resource), zap.String("host", addr))
		return survivorView{}
	}
	hr := res.Hosts[addr]
	if hr == nil || !hr.Success || localStatusLine(hr.Output) == "" {
		log.Debug("readSurvivorView: status failed or resource not up there; survivor counts as not answering",
			zap.String("resource", resource), zap.String("host", addr), zap.Bool("has_result", hr != nil))
		return survivorView{}
	}
	v := parseSurvivorView(hr.Output)
	log.Debug("readSurvivorView: survivor view", zap.String("resource", resource), zap.String("host", addr),
		zap.Bool("quorum", v.quorum), zap.Bool("up_to_date", v.upToDate), zap.Int("peers", len(v.seesConnected)))
	return v
}

// parseSurvivorView reads the local quorum and disk state and each peer's
// connection from `drbdadm status` output, verbose or not.
//
// The local section runs from the resource line to the first peer line; a
// peer line is an indented line whose first word is a name, not a key:value.
// A connected peer's line carries its role (and, verbose, connection:Connected);
// a disconnected one's carries connection:<state> and no role.
func parseSurvivorView(out string) survivorView {
	v := survivorView{answered: true, quorum: true, seesConnected: map[string]bool{}}
	inLocal := false
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		fields := strings.Fields(trimmed)
		if !isIndentedStatusLine(line) {
			inLocal = strings.Contains(trimmed, "role:")
			if inLocal && strings.Contains(trimmed, "quorum:no") {
				v.quorum = false
			}
			continue
		}
		if !strings.Contains(fields[0], ":") {
			inLocal = false
			conn := fieldValue(fields[1:], "connection:")
			v.seesConnected[fields[0]] = conn == "" || conn == "Connected"
			continue
		}
		if !inLocal || isPeerDiskLine(trimmed) {
			continue
		}
		if strings.Contains(trimmed, "quorum:no") {
			v.quorum = false
		}
		if fieldValue(fields, "disk:") == "UpToDate" {
			v.upToDate = true
		}
	}
	zap.L().Debug("parseSurvivorView: parsed status", zap.Bool("quorum", v.quorum), zap.Bool("up_to_date", v.upToDate),
		zap.Int("peers", len(v.seesConnected)))
	return v
}

// confirmGone checks that members the controller cannot reach over SSH are
// down for good as far as anyone can tell: no witness that answers still has a
// DRBD connection to them. It returns the witnesses' views.
func (rm *ResourceManager) confirmGone(ctx context.Context, resource string, gone []memberRef, witnesses []memberRef) (map[string]survivorView, error) {
	log := rm.resourcesLog()
	log.Debug("confirmGone: checking members are gone", zap.String("resource", resource),
		zap.Int("gone", len(gone)), zap.Int("witnesses", len(witnesses)))
	views := map[string]survivorView{}
	for _, w := range witnesses {
		v := rm.readSurvivorView(ctx, w.addr, resource)
		if !v.answered {
			log.Debug("confirmGone: witness did not answer", zap.String("resource", resource), zap.String("node", w.drbdName), zap.String("host", w.addr))
			continue
		}
		views[w.addr] = v
		for _, g := range gone {
			log.Debug("confirmGone: witness view of gone member", zap.String("resource", resource), zap.String("witness", w.drbdName),
				zap.String("node", g.drbdName), zap.Bool("connected", v.seesConnected[g.drbdName]))
			if v.seesConnected[g.drbdName] {
				log.Debug("confirmGone: gone member still connected; refusing", zap.String("resource", resource),
					zap.String("witness", w.drbdName), zap.String("node", g.drbdName))
				return nil, fmt.Errorf(
					"%s is still connected to %s over DRBD although the controller cannot reach it; it is cut off from the controller, not gone",
					rm.nodeLabel(w.addr), g.drbdName)
			}
		}
	}
	if len(views) == 0 {
		log.Debug("confirmGone: no witness answered", zap.String("resource", resource))
		return nil, fmt.Errorf("no surviving member of %s answered, so nothing can confirm the rest are gone", resource)
	}
	log.Debug("confirmGone: members confirmed gone", zap.String("resource", resource), zap.Int("answered", len(views)))
	return views, nil
}

// assertLostRemovable is the lost path's guard: the node is gone, and the
// diskful survivors hold quorum and an UpToDate copy without it.
func (rm *ResourceManager) assertLostRemovable(ctx context.Context, resource string, lost memberRef, diskful []memberRef) error {
	log := rm.resourcesLog()
	log.Debug("assertLostRemovable: checking lost member", zap.String("resource", resource),
		zap.String("node", lost.drbdName), zap.String("host", lost.addr), zap.Int("diskful", len(diskful)))
	if rm.answers(ctx, lost.addr) {
		log.Debug("assertLostRemovable: node answers; not lost", zap.String("resource", resource), zap.String("node", lost.drbdName))
		return fmt.Errorf("%s answers over SSH, so it is not lost; remove it without --lost", rm.nodeLabel(lost.addr))
	}
	views, err := rm.confirmGone(ctx, resource, []memberRef{lost}, diskful)
	if err != nil {
		log.Debug("assertLostRemovable: could not confirm node is gone", zap.String("resource", resource),
			zap.String("node", lost.drbdName), zap.Error(err))
		return err
	}
	quorum, upToDate := false, false
	for _, v := range views {
		quorum = quorum || v.quorum
		upToDate = upToDate || v.upToDate
	}
	log.Debug("assertLostRemovable: survivors' state", zap.String("resource", resource),
		zap.Bool("quorum", quorum), zap.Bool("up_to_date", upToDate))
	if !upToDate {
		log.Debug("assertLostRemovable: no UpToDate survivor; refusing", zap.String("resource", resource))
		return fmt.Errorf("no surviving replica of %s is UpToDate; removing %s would leave no complete copy", resource, lost.drbdName)
	}
	if !quorum {
		log.Debug("assertLostRemovable: survivors lack quorum; refusing", zap.String("resource", resource))
		return fmt.Errorf(
			"the surviving replicas of %s have no quorum without %s. That is the state in which another partition could "+
				"still hold the data, so its vote is not dropped blind; give the survivors a quorum first "+
				"(`haify ha set-tiebreaker %s <node>` works with %s down), then remove it", resource, lost.drbdName, resource, lost.drbdName)
	}
	log.Debug("assertLostRemovable: lost member may be removed", zap.String("resource", resource), zap.String("node", lost.drbdName))
	return nil
}

var nodeIDLineRE = regexp.MustCompile(`^\s*node-id\s+(\d+)\s*;`)

// nodeIDOf returns the node-id in name's `on` stanza of a resource config.
func nodeIDOf(content, name string) (string, bool) {
	inBlock, depth := false, 0
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		if !inBlock {
			if len(f) >= 2 && f[0] == "on" && f[1] == name {
				inBlock = true
				depth = strings.Count(line, "{") - strings.Count(line, "}")
			}
			continue
		}
		if m := nodeIDLineRE.FindStringSubmatch(line); m != nil {
			zap.L().Debug("nodeIDOf: found node-id", zap.String("node", name), zap.String("node_id", m[1]))
			return m[1], true
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if depth <= 0 {
			zap.L().Debug("nodeIDOf: on stanza has no node-id", zap.String("node", name))
			return "", false
		}
	}
	zap.L().Debug("nodeIDOf: node-id not found", zap.String("node", name), zap.Bool("in_block", inBlock))
	return "", false
}

// forgetPeer frees a removed peer's bitmap slot on the diskful survivors.
//
// A slot is allocated per peer when the metadata is created and is not given
// back when the peer leaves the config. Left alone it is a phantom that still
// counts for quorum on some DRBD versions and, permanently, one fewer peer the
// resource can ever have — add-replica refuses once the slots run out. Best
// effort: a survivor that refuses is named in the log with the command to run.
func (rm *ResourceManager) forgetPeer(ctx context.Context, resource, peerID, peerName string, survivors []string) {
	log := rm.resourcesLog()
	if peerID == "" || len(survivors) == 0 {
		log.Debug("forgetPeer: nothing to forget", zap.String("resource", resource), zap.String("peer", peerName),
			zap.String("peer_id", peerID), zap.Int("survivors", len(survivors)))
		return
	}
	cmd := fmt.Sprintf("sudo drbdsetup forget-peer %s %s", resource, peerID)
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, survivors, cmd)
	log.Debug("forgetPeer: ran forget-peer", zap.String("resource", resource), zap.String("peer", peerName),
		zap.String("peer_id", peerID), zap.Strings("hosts", survivors), zap.Array("failed_hosts", resourcesFailedHosts{res}),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		rm.controller.logger.Warn("forget-peer failed; the removed peer's bitmap slot stays allocated",
			zap.String("resource", resource), zap.String("peer", peerName), zap.String("run", cmd), zap.Error(err))
		return
	}
	for h, hr := range res.Hosts {
		if hr != nil && !hr.Success {
			rm.controller.logger.Warn("forget-peer failed; the removed peer's bitmap slot stays allocated there",
				zap.String("resource", resource), zap.String("peer", peerName), zap.String("node", rm.nodeLabel(h)),
				zap.String("run", cmd), zap.String("output", strings.TrimSpace(hr.Output)))
		}
	}
}

// LostReplicaCleanup is what has to happen on a node whose replica was removed
// with --lost, before it may rejoin anything: it still holds its old config,
// which names peers that no longer know it.
func LostReplicaCleanup(resource, node string) string {
	return fmt.Sprintf("if %[2]s ever comes back, before it rejoins run there: drbdadm down %[1]s; "+
		"rm /etc/drbd.d/%[1]s.res /etc/drbd-reactor.d/haify-*-%[1]s.toml*; and delete its volumes for %[1]s",
		resource, node)
}
