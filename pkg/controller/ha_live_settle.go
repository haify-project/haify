package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/deployment"
	"go.uber.org/zap"
)

// Settling a live move: one Primary, one running guest, the promoter enabled.
//
// The same procedure ends every move — the one that worked, the one whose
// migration failed, the one a controller restart interrupted — because it
// does not ask what was done. It asks the nodes where the guest runs and
// builds the end state around that node:
//
//  1. A migration still running is given up, and waited for.
//  2. The guest runs on at most one node; call it K. On two, nothing is
//     touched and the move ends needing attention.
//  3. Every other Primary is demoted. The guest's node is never demoted.
//  4. Dual-primary is turned off.
//  5. On the nodes other than K, drbd-reactor's units for the resource are
//     stopped — only where that stops no guest and demotes nothing (see
//     liveRetireScript) — and the restart guard is removed.
//  6. The promoter is enabled on K first. drbd-reactor starts a resource it
//     finds Primary on its own node, so it starts the services target
//     there: the VirtualDomain agent's start finds the guest running and
//     reports it started, and the agent's monitor takes over. The guest is
//     checked to be the same one afterwards.
//  7. The promoter is enabled on the other replicas, each only once it is
//     Secondary with dual-primary off: an enabled promoter on a node that is
//     still Primary starts the guest there.
//  8. The hook's marker is removed and the result is checked once more.
//
// What cannot be done is said, not skipped silently: the move then ends as
// "attention" with the list of what is left.

var (
	// liveSettlePause is the wait between looks at the nodes while settling.
	liveSettlePause = 3 * time.Second
	// liveAdoptWait is how long drbd-reactor gets to take the guest over.
	liveAdoptWait = 45 * time.Second
	// liveRestartWait is how long drbd-reactor gets to start a guest that
	// was found stopped.
	liveRestartWait = 90 * time.Second
)

// liveResult is how a move ended.
type liveResult struct {
	Outcome    string
	ActiveNode string
	Message    string
}

// pause waits d, or less when the context ends.
func pause(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// settledProbe probes until every replica answers about the guest, a few
// times at most: a node that is rebooting comes back, one that is down
// does not.
func (j *haLiveJob) settledProbe(ctx context.Context) []*liveNode {
	var nodes []*liveNode
	for try := 0; try < 4; try++ {
		nodes = j.probe(ctx)
		complete := true
		for _, n := range nodes {
			if !n.reachable() || (n.VM.Virsh && !n.VM.StateKnown) {
				complete = false
			}
		}
		if complete || ctx.Err() != nil {
			break
		}
		pause(ctx, liveSettlePause)
	}
	return nodes
}

// settle brings the resource to its end state around wherever the guest
// runs, and says how the move ended. moveErr is why the migration did not
// complete, nil when it did.
func (j *haLiveJob) settle(ctx context.Context, moveErr error) liveResult {
	log := j.rm.controller.traceLogger()
	rec := j.snapshot()
	res, domain := rec.Resource, rec.Domain
	var left []string
	leave := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		log.Debug("haLiveJob.settle: left undone", zap.String("resource", res), zap.String("what", msg))
		left = append(left, msg)
	}

	// 1. No migration may still be running.
	nodes := j.probe(ctx)
	if src := nodeAt(nodes, j.sourceHost); src != nil && src.reachable() && (src.MoveUnit == "active" || src.MoveUnit == "activating") {
		log.Debug("haLiveJob.settle: migration still running; giving it up", zap.String("resource", res))
		if _, err := j.script(ctx, []string{j.sourceHost}, liveAbortScript(domain)); err != nil {
			log.Debug("haLiveJob.settle: abort failed", zap.String("resource", res), zap.Error(err))
		}
		for end := time.Now().Add(liveAbortGrace); time.Now().Before(end) && ctx.Err() == nil; pause(ctx, liveSettlePause) {
			if n := nodeAt(j.probe(ctx), j.sourceHost); n == nil || !n.reachable() || (n.MoveUnit != "active" && n.MoveUnit != "activating") {
				break
			}
		}
	}

	// 2. Where the guest runs.
	nodes = j.settledProbe(ctx)
	var running []*liveNode
	for _, n := range nodes {
		switch {
		case !n.reachable():
			leave("%s did not answer, so its promoter for %s stays disabled; once it is back, "+
				"`drbd-reactorctl enable haify-ha-%s` there", n.Node, res, res)
		case n.VM.Running:
			running = append(running, n)
		}
	}
	if len(running) > 1 {
		return liveResult{Outcome: liveOutcomeAttention, Message: fmt.Sprintf(
			"%s is on %s and %s at once and the migration could not be stopped. Nothing was changed: the promoter is disabled on every "+
				"replica and %s allows two Primaries. Once the guest is on one node, enable the promoter there first "+
				"(`drbd-reactorctl enable haify-ha-%s`), then `haify resource dual-primary %s off`, then enable it on the others",
			domain, running[0].Node, running[1].Node, res, res, res)}
	}
	var k *liveNode
	if len(running) == 1 {
		k = running[0]
	}
	log.Debug("haLiveJob.settle: guest located", zap.String("resource", res), zap.Bool("running", k != nil),
		zap.String("node", liveNodeName(k)), zap.NamedError("move_error", moveErr))
	domID := ""
	if k != nil {
		domID = k.VM.DomID
	}

	// 3. Only the guest's node stays Primary.
	for _, n := range nodes {
		if n == k || !n.reachable() || n.Role != "Primary" {
			continue
		}
		start := time.Now()
		dr, err := j.rm.deployment.DRBDSecondary(ctx, n.Host, res)
		log.Debug("haLiveJob.settle: demoted", zap.String("resource", res), zap.String("node", n.Node),
			zap.Bool("success", err == nil && dr != nil && dr.Success), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil || dr == nil || !dr.Success {
			reason := fmt.Sprint(err)
			if dr != nil && strings.TrimSpace(dr.Output) != "" {
				reason = strings.TrimSpace(dr.Output)
			}
			leave("%s is still Primary on %s (%s); find what holds it open there, then `haify resource secondary %s %s`",
				res, n.Node, reason, res, n.Node)
		}
	}

	// 4. One Primary at a time again.
	if err := j.rm.SetDualPrimary(ctx, res, false); err != nil {
		log.Debug("haLiveJob.settle: closing dual-primary reported an error; the probe decides", zap.String("resource", res), zap.Error(err))
	}
	nodes = j.probe(ctx)
	windowClosed := true
	for _, n := range nodes {
		if n.reachable() && n.TwoPrimaries {
			windowClosed = false
			leave("%s still allows two Primaries on %s; `haify resource dual-primary %s off`", res, n.Node, res)
		}
	}

	// 5. The other nodes forget the guest.
	for _, n := range nodes {
		if !n.reachable() || (k != nil && n.Host == k.Host) {
			continue
		}
		kv, err := j.script(ctx, []string{n.Host}, liveRetireScript(res, j.guest), deployment.WithExecTimeout(liveStepTimeout))
		if err != nil || kv[n.Host]["HAIFY_RETIRED"] != "1" {
			log.Debug("haLiveJob.settle: node kept its units", zap.String("node", n.Node), zap.String("kept", kv[n.Host]["HAIFY_KEPT"]), zap.Error(err))
		}
	}
	if k != nil {
		if kv, err := j.script(ctx, []string{k.Host}, liveUnguardScript(j.guest.agentUnit(res))); err != nil || kv[k.Host]["HAIFY_RESTART"] == "no" {
			leave("the guest's agent unit on %s is still set to not restart; remove /run/systemd/system/%s.d/%s and run "+
				"`systemctl daemon-reload` there", k.Node, j.guest.agentUnit(res), liveGuardFile)
		}
	}

	// 6. drbd-reactor takes the guest where it runs.
	restarted := false
	if k != nil {
		switch {
		case k.Role != "Primary":
			leave("%s runs on %s but %s is not Primary there", domain, k.Node, res)
		default:
			if _, err := j.script(ctx, []string{k.Host}, liveEnableScript(res), deployment.WithExecTimeout(liveStepTimeout)); err != nil {
				leave("the promoter could not be enabled on %s (%v); `drbd-reactorctl enable haify-ha-%s` there", k.Node, err, res)
			} else if !j.adopt(ctx, k, domID) {
				restarted = true
			}
		}
	}

	// 7. The standbys stand by again.
	nodes = j.probe(ctx)
	for _, n := range nodes {
		if !n.reachable() || (k != nil && n.Host == k.Host) {
			continue
		}
		if !windowClosed || n.Role == "Primary" || n.VM.Running {
			leave("the promoter stays disabled on %s until %s is Secondary there with dual-primary off; then "+
				"`drbd-reactorctl enable haify-ha-%s` there", n.Node, res, res)
			continue
		}
		if _, err := j.script(ctx, []string{n.Host}, liveEnableScript(res), deployment.WithExecTimeout(liveStepTimeout)); err != nil {
			leave("the promoter could not be enabled on %s (%v); `drbd-reactorctl enable haify-ha-%s` there", n.Node, err, res)
		}
	}

	// 8. The hook may act again; look once more.
	var reach []string
	for _, n := range nodes {
		if n.reachable() {
			reach = append(reach, n.Host)
		}
	}
	if _, err := j.script(ctx, reach, liveUnmarkScript(res)); err != nil {
		log.Debug("haLiveJob.settle: removing the hook marker failed; it expires by itself", zap.String("resource", res), zap.Error(err))
	}
	return j.verdict(ctx, k, moveErr, restarted, left)
}

// liveNodeName names a node that may be nil.
func liveNodeName(n *liveNode) string {
	if n == nil {
		return ""
	}
	return n.Node
}

// adopt waits for drbd-reactor on k to have the guest's units active, helps
// with an explicit start when it has not by itself, and reports whether the
// guest is still the one that was running: same libvirt id, never stopped.
func (j *haLiveJob) adopt(ctx context.Context, k *liveNode, domID string) bool {
	log := j.rm.controller.traceLogger()
	res := j.rec.Resource
	begin := time.Now()
	nudged := false
	var now *liveNode
	for time.Since(begin) < liveAdoptWait && ctx.Err() == nil {
		pause(ctx, liveSettlePause)
		now = nodeAt(j.probe(ctx), k.Host)
		if now != nil && now.reachable() && now.TargetActive == "active" && now.AgentActive == "active" {
			break
		}
		if !nudged && time.Since(begin) > liveAdoptWait/4 {
			nudged = true
			log.Debug("haLiveJob.adopt: units not active yet; starting them", zap.String("resource", res), zap.String("node", k.Node))
			if _, err := j.script(ctx, []string{k.Host}, liveAdoptScript(res, j.guest), deployment.WithExecTimeout(liveStepTimeout)); err != nil {
				log.Debug("haLiveJob.adopt: start failed", zap.String("resource", res), zap.Error(err))
			}
		}
	}
	same := now != nil && now.reachable() && now.VM.Running && now.VM.DomID == domID &&
		now.TargetActive == "active" && now.AgentActive == "active"
	log.Debug("haLiveJob.adopt: done", zap.String("resource", res), zap.String("node", k.Node), zap.Bool("same_guest", same),
		zap.Bool("nudged", nudged), zap.Duration("took", time.Since(begin)))
	return same
}

// verdict looks at the nodes one last time and words how the move ended.
func (j *haLiveJob) verdict(ctx context.Context, k *liveNode, moveErr error, restarted bool, left []string) liveResult {
	log := j.rm.controller.traceLogger()
	rec := j.snapshot()
	res, domain := rec.Resource, rec.Domain
	nodes := j.probe(ctx)
	// A guest that was found stopped, or was stopped in the hand-over, is
	// started by drbd-reactor as after any failure; give it the time.
	if k == nil || restarted {
		for end := time.Now().Add(liveRestartWait); time.Now().Before(end) && ctx.Err() == nil; pause(ctx, liveSettlePause) {
			if liveRunningOn(nodes) != nil {
				break
			}
			nodes = j.probe(ctx)
		}
	}
	active := liveRunningOn(nodes)
	var primaries []string
	for _, n := range nodes {
		if n.reachable() && n.Role == "Primary" {
			primaries = append(primaries, n.Node)
		}
		if n.reachable() && n.Promoter != "enabled" && !mentions(left, n.Node) {
			left = append(left, fmt.Sprintf("the promoter is not enabled on %s; `drbd-reactorctl enable haify-ha-%s` there", n.Node, res))
		}
	}
	switch {
	case active == nil:
		left = append(left, fmt.Sprintf("%s is not running on any replica; `haify ha status %s` shows why drbd-reactor did not start it", domain, res))
	case len(primaries) != 1 || primaries[0] != active.Node:
		left = append(left, fmt.Sprintf("%s runs on %s but %s is Primary on [%s]", domain, active.Node, res, strings.Join(primaries, ", ")))
	case active.TargetActive != "active" || active.AgentActive != "active":
		left = append(left, fmt.Sprintf("%s runs on %s but drbd-reactor's units for it are not active there; `haify ha status %s`", domain, active.Node, res))
	}
	out := liveResult{ActiveNode: liveNodeName(active)}
	why := ""
	if moveErr != nil {
		why = ": " + moveErr.Error()
	}
	switch {
	case len(left) > 0:
		out.Outcome = liveOutcomeAttention
		where := "is not running"
		if active != nil {
			where = "runs on " + active.Node
		}
		out.Message = fmt.Sprintf("%s %s, but not everything could be put back%s. Left to do: %s", domain, where, why, strings.Join(left, "; "))
	case restarted || k == nil:
		out.Outcome = liveOutcomeRestarted
		out.Message = fmt.Sprintf("%s was found stopped during the move and drbd-reactor started it again on %s%s", domain, active.Node, why)
	case active.Node == rec.TargetNode:
		out.Outcome = liveOutcomeMoved
		out.Message = fmt.Sprintf("%s now runs on %s. It was not stopped, and drbd-reactor manages it there", domain, active.Node)
	default:
		out.Outcome = liveOutcomeStayed
		out.Message = fmt.Sprintf("%s stayed on %s and was not stopped%s", domain, active.Node, why)
	}
	log.Debug("haLiveJob.verdict: decided", zap.String("resource", res), zap.String("outcome", out.Outcome),
		zap.String("active_node", out.ActiveNode), zap.Strings("primaries", primaries), zap.Int("left", len(left)))
	return out
}

// liveRunningOn returns the one node the guest runs on, or nil.
func liveRunningOn(nodes []*liveNode) *liveNode {
	var on *liveNode
	for _, n := range nodes {
		if n.reachable() && n.VM.Running {
			if on != nil {
				return nil
			}
			on = n
		}
	}
	return on
}

// mentions reports whether any of the notes names node.
func mentions(notes []string, node string) bool {
	for _, n := range notes {
		if strings.Contains(n, " "+node) {
			return true
		}
	}
	return false
}
