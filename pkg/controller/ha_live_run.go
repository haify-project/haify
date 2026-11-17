package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"go.uber.org/zap"
)

// The forward steps of a live move, in the only order that is safe:
//
//  1. Tell the libvirt hook on every replica to stand back. With the
//     promoter config disabled the hook would take the resource for one of
//     its own and promote, demote and toggle dual-primary alongside this.
//  2. Set Restart=no on the guest's agent unit on the source (see
//     liveGuardScript). Done first among the changes that matter, and
//     verified, because it is what keeps the source from starting the guest
//     a second time once it has left.
//  3. Disable the promoter config on every replica and reload drbd-reactor.
//     From here to the end the roles are the controller's to change. An
//     enabled promoter acts on DRBD's events by itself: it starts the chain
//     on a replica the moment DRBD says it may promote, and it starts it on
//     its own node when it is loaded while that node is Primary — either of
//     which, while two Primaries are allowed, is a second copy of the guest.
//     The services already running are left running.
//  4. Check that the guest is still the same running guest on the source.
//  5. Allow two Primaries between source and target, then promote the
//     target (never forced: it must be UpToDate and connected).
//  6. Run `virsh migrate --live` on the source in a unit of its own and
//     watch it.
//
// Nothing here undoes anything. Whatever happens — a step fails, the
// migration fails, it succeeds — settle (ha_live_settle.go) looks at where
// the guest actually is and puts everything back around it.

var (
	// livePollInterval is how often a running migration is looked at.
	livePollInterval = 3 * time.Second
	// liveAbortGrace is how long an aborted migration gets to wind down.
	liveAbortGrace = 90 * time.Second
)

// liveStepTimeout bounds a node-side step that waits on systemd.
const liveStepTimeout = 2 * time.Minute

// script runs a root script on hosts and returns each host's KEY=value
// lines. A host that did not run it is an error.
func (j *haLiveJob) script(ctx context.Context, hosts []string, script string, opts ...deployment.ExecOption) (map[string]map[string]string, error) {
	log := j.rm.controller.traceLogger()
	start := time.Now()
	res, err := j.rm.runNodeScript(ctx, hosts, script, opts...)
	log.Debug("haLiveJob.script: ran", zap.String("resource", j.rec.Resource), zap.Strings("hosts", hosts),
		zap.Int("size_bytes", len(script)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]string, len(hosts))
	var failed []string
	for _, h := range hosts {
		hr := res.Hosts[h]
		if hr == nil {
			failed = append(failed, j.rm.nodeLabel(h)+": no result")
			continue
		}
		kv := keyValues(hr.Output)
		out[h] = kv
		if !hr.Success {
			reason := kv["HAIFY_ERR"]
			if reason == "" {
				reason = hostFailure(hr)
			}
			failed = append(failed, j.rm.nodeLabel(h)+": "+reason)
		}
	}
	if len(failed) > 0 {
		log.Debug("haLiveJob.script: hosts failed", zap.Strings("failed", failed))
		return out, fmt.Errorf("%s", strings.Join(failed, "; "))
	}
	return out, nil
}

// probe reads every replica's state.
func (j *haLiveJob) probe(ctx context.Context) []*liveNode {
	return j.rm.probeLiveNodes(ctx, j.rec.Resource, j.guest, j.agentPath, j.hosts)
}

// nodeAt returns the probed node at host.
func nodeAt(nodes []*liveNode, host string) *liveNode {
	for _, n := range nodes {
		if n.Host == host {
			return n
		}
	}
	return nil
}

// run is one live move from the first change to the settled end.
func (j *haLiveJob) run(ctx context.Context) {
	err := j.prepare(ctx)
	if err == nil {
		err = j.waitForMove(ctx)
	}
	j.finish(ctx, err)
}

// prepare takes steps 1 to 6 up to the migration being started.
func (j *haLiveJob) prepare(ctx context.Context) error {
	log := j.rm.controller.traceLogger()
	rec := j.snapshot()
	res, unit := rec.Resource, j.guest.agentUnit(rec.Resource)
	log.Debug("haLiveJob.prepare: start", zap.String("resource", res), zap.String("source", rec.SourceNode),
		zap.String("target", rec.TargetNode), zap.String("agent_unit", unit))

	before := nodeAt(j.probe(ctx), j.sourceHost)
	if before == nil || !before.reachable() || !before.VM.Running {
		return fmt.Errorf("%s no longer runs on %s", rec.Domain, rec.SourceNode)
	}

	// 1. The hook stands back.
	if _, err := j.script(ctx, j.hosts, liveMarkScript(res, rec.Deadline.Add(liveMarkerGrace))); err != nil {
		return fmt.Errorf("could not announce the move to the nodes: %w", err)
	}

	// 2. The source's agent unit must not restart.
	j.step(ctx, livePhaseReleasing, "keeping %s from restarting the guest once it has left", rec.SourceNode)
	kv, err := j.script(ctx, []string{j.sourceHost}, liveGuardScript(unit))
	if err != nil || kv[j.sourceHost]["HAIFY_RESTART"] != "no" {
		return fmt.Errorf("the guest's agent unit on %s could not be set to not restart (Restart=%s): %v",
			rec.SourceNode, orDash(kv[j.sourceHost]["HAIFY_RESTART"]), err)
	}

	// 3. drbd-reactor lets go, on every replica.
	if err := ctx.Err(); err != nil {
		return err
	}
	j.step(ctx, livePhaseReleasing, "pausing drbd-reactor's promoter for %s on %s", res, strings.Join(j.rm.nodeNames(j.hosts), ", "))
	if _, err := j.script(ctx, j.hosts, liveDisableScript(res), deployment.WithExecTimeout(liveStepTimeout)); err != nil {
		return fmt.Errorf("the promoter could not be paused on every replica: %w", err)
	}

	// 4. The guest must have come through that untouched.
	after := nodeAt(j.probe(ctx), j.sourceHost)
	switch {
	case after == nil || !after.reachable():
		return fmt.Errorf("%s stopped answering", rec.SourceNode)
	case !after.VM.Running || after.VM.DomID != before.VM.DomID:
		return fmt.Errorf("%s did not keep running on %s while the promoter was paused", rec.Domain, rec.SourceNode)
	case after.AgentRestart != "no":
		return fmt.Errorf("the guest's agent unit on %s would restart (Restart=%s)", rec.SourceNode, orDash(after.AgentRestart))
	}

	// 5. Two Primaries, between these two nodes only.
	if err := ctx.Err(); err != nil {
		return err
	}
	j.step(ctx, livePhaseWindow, "allowing two Primaries on %s between %s and %s", res, rec.SourceNode, rec.TargetNode)
	if err := j.rm.SetDualPrimaryOn(ctx, res, true, []string{rec.SourceNode, rec.TargetNode}); err != nil {
		return err
	}
	start := time.Now()
	pr, err := j.rm.deployment.DRBDPrimary(ctx, j.targetHost, res, false)
	log.Debug("haLiveJob.prepare: promoted the target", zap.String("resource", res), zap.String("host", j.targetHost),
		zap.Bool("success", err == nil && pr != nil && pr.Success), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || pr == nil || !pr.Success {
		reason := fmt.Sprint(err)
		if pr != nil && strings.TrimSpace(pr.Output) != "" {
			reason = strings.TrimSpace(pr.Output)
		}
		return fmt.Errorf("%s could not be made Primary on %s: %s", res, rec.TargetNode, reason)
	}

	// 6. The migration itself.
	if err := ctx.Err(); err != nil {
		return err
	}
	j.step(ctx, livePhaseMigrating, "migrating %s to %s", rec.Domain, rec.TargetNode)
	if _, err := j.script(ctx, []string{j.sourceHost}, liveStartMoveScript(res, rec.Domain, j.targetHost)); err != nil {
		return fmt.Errorf("the migration could not be started on %s: %w", rec.SourceNode, err)
	}
	return nil
}

// waitForMove watches the migration until it ends, and aborts it at the
// deadline. nil means `virsh migrate` exited 0.
func (j *haLiveJob) waitForMove(ctx context.Context) error {
	log := j.rm.controller.traceLogger()
	rec := j.snapshot()
	res := rec.Resource
	var aborted time.Time
	silent, noResult := 0, 0
	for {
		select {
		case <-ctx.Done():
			log.Debug("haLiveJob.waitForMove: context done", zap.String("resource", res), zap.Error(ctx.Err()))
			return ctx.Err()
		case <-time.After(livePollInterval):
		}
		kv, err := j.script(ctx, []string{j.sourceHost}, livePollScript(res, rec.Domain))
		st := kv[j.sourceHost]
		if err != nil || st == nil {
			silent++
			log.Debug("haLiveJob.waitForMove: source did not answer", zap.String("resource", res), zap.Int("silent", silent), zap.Error(err))
			// The migration runs on without the controller; give the node
			// a while before deciding from what the nodes say.
			if silent >= 20 {
				return fmt.Errorf("%s stopped answering during the migration", rec.SourceNode)
			}
			continue
		}
		silent = 0
		if rc := st["RC"]; rc != "" {
			log.Debug("haLiveJob.waitForMove: migration ended", zap.String("resource", res), zap.String("rc", rc))
			if rc == "0" {
				return nil
			}
			out, _ := base64.StdEncoding.DecodeString(st["LOG"])
			reason := strings.Join(strings.Fields(string(out)), " ")
			if reason == "" {
				reason = "virsh migrate exited " + rc
			}
			if !aborted.IsZero() {
				reason = fmt.Sprintf("the memory copy did not finish by %s and was given up (%s)", rec.Deadline.Format("15:04:05"), reason)
			}
			return fmt.Errorf("%s", reason)
		}
		if st["UNIT"] != "active" && st["UNIT"] != "activating" {
			// Neither running nor finished: it never started, or the node
			// rebooted under it.
			if noResult++; noResult >= 3 {
				return fmt.Errorf("the migration on %s ended without a result", rec.SourceNode)
			}
			continue
		}
		noResult = 0
		if p := liveProgress(st["JOB_PROCESSED"], st["JOB_TOTAL"]); p != j.snapshot().Progress {
			j.update(ctx, func(r *database.HaMigration) { r.Progress = p })
		}
		switch {
		case aborted.IsZero() && time.Now().After(rec.Deadline):
			j.step(ctx, livePhaseMigrating, "the memory copy did not finish in time; giving the migration up")
			if _, err := j.script(ctx, []string{j.sourceHost}, liveAbortScript(rec.Domain)); err != nil {
				log.Debug("haLiveJob.waitForMove: abort failed", zap.String("resource", res), zap.Error(err))
			}
			aborted = time.Now()
		case !aborted.IsZero() && time.Since(aborted) > liveAbortGrace:
			return fmt.Errorf("the migration did not stop after it was given up")
		}
	}
}

// liveProgress turns domjobinfo's "processed" and "total" into a percentage.
// A guest that dirties memory is copied more than once, so it stops at 99
// until the migration says it is done.
func liveProgress(processed, total string) int {
	p, t := liveBytes(processed), liveBytes(total)
	if p <= 0 || t <= 0 {
		return 0
	}
	if pct := int(p * 100 / t); pct < 99 {
		return pct
	}
	return 99
}

// liveBytes reads a size as virsh prints it: "1.234 GiB".
func liveBytes(s string) float64 {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil || len(f) < 2 {
		return v
	}
	switch f[1] {
	case "KiB":
		v *= 1 << 10
	case "MiB":
		v *= 1 << 20
	case "GiB":
		v *= 1 << 30
	case "TiB":
		v *= 1 << 40
	}
	return v
}

// finish settles the move and records how it ended. A controller that is
// shutting down does neither: the record stays unfinished, and the
// controller that starts next settles it (resumeLiveMigrations).
func (j *haLiveJob) finish(ctx context.Context, moveErr error) {
	log := j.rm.controller.traceLogger()
	rec := j.snapshot()
	if ctx.Err() != nil {
		log.Debug("haLiveJob.finish: controller stopping; the move is left for the next controller",
			zap.String("resource", rec.Resource), zap.String("phase", rec.Phase))
		return
	}
	if moveErr != nil {
		j.step(ctx, livePhaseSettling, "the move did not complete (%v); putting things back around the guest", moveErr)
	} else {
		j.step(ctx, livePhaseSettling, "%s has migrated; handing it to drbd-reactor on %s", rec.Domain, rec.TargetNode)
	}
	res := j.settle(ctx, moveErr)
	if ctx.Err() != nil {
		log.Debug("haLiveJob.finish: controller stopped while settling", zap.String("resource", rec.Resource))
		return
	}
	j.update(ctx, func(r *database.HaMigration) {
		r.Phase, r.Done, r.Outcome, r.ActiveNode, r.Message = livePhaseDone, true, res.Outcome, res.ActiveNode, res.Message
		r.Steps = append(r.Steps, res.Message)
		if res.Outcome == liveOutcomeMoved {
			r.Progress = 100
		}
	})
	done := j.snapshot()
	j.rm.controller.logger.Info("Live move ended", zap.String("resource", done.Resource), zap.String("outcome", done.Outcome),
		zap.String("active_node", done.ActiveNode), zap.String("message", done.Message))
	j.publish(done)
}
