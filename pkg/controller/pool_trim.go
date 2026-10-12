package controller

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/deployment"
	"github.com/haify-project/haify/pkg/event"
)

// Trimming the filesystems on DRBD devices ([storage.thin] trim_schedule,
// `haify pool trim`).
//
// A filesystem that frees a block tells nobody: the thin pool under every
// replica keeps it allocated. Worse, a full resync writes the zeros of every
// block the filesystem does not use, so after one a 1G volume holding 50M of
// files holds 1G of every replica's pool. fstrim on the node serving the
// filesystem discards the free blocks; DRBD passes the discards to every
// peer, and each peer's thin pool lets the blocks go.
//
// Only a mounted filesystem can be trimmed, and only the Primary mounts one,
// so the sweep simply trims every DRBD-backed mount on every node. A raw block
// device (an iSCSI LUN, a CSI block volume) is the initiator's to trim.

const trimTimeout = 15 * time.Minute

// trimScript prints one "mount|exit|fstrim output" line per DRBD-backed
// ext4/xfs mount. A suspended device would block fstrim forever, hence the
// timeout.
const trimScript = `for t in $(findmnt -rn -t ext4,xfs -o TARGET,SOURCE | awk '$2 ~ "^/dev/drbd" {print $1}'); do
  out=$(timeout 900 fstrim -v "$t" 2>&1); rc=$?
  echo "$t|$rc|$(echo "$out" | tr '\n' ' ')"
done`

// TrimResult is one filesystem's trim.
type TrimResult struct {
	Node  string
	Mount string
	Bytes uint64
	Err   string
}

var trimBytesRE = regexp.MustCompile(`\((\d+) bytes\) trimmed`)

// parseTrimOutput reads trimScript's output from one node.
func parseTrimOutput(node, out string) []TrimResult {
	var res []TrimResult
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "|", 3)
		if len(parts) != 3 || parts[0] == "" {
			continue
		}
		r := TrimResult{Node: node, Mount: strings.ReplaceAll(parts[0], `\x20`, " ")}
		if m := trimBytesRE.FindStringSubmatch(parts[2]); m != nil {
			r.Bytes, _ = strconv.ParseUint(m[1], 10, 64)
		}
		if parts[1] != "0" {
			r.Err = strings.TrimSpace(parts[2])
			switch {
			case parts[1] == "124":
				r.Err = "timed out (is the device suspended?)"
			case r.Err == "":
				r.Err = "fstrim exited " + parts[1]
			}
		}
		res = append(res, r)
	}
	zap.L().Debug("parseTrimOutput: parsed", zap.String("node", node), zap.Int("count", len(res)))
	return res
}

// Trim trims every DRBD-backed filesystem on node, or on every online node
// when node is empty.
func (sm *StorageManager) Trim(ctx context.Context, node string) ([]TrimResult, error) {
	c := sm.controller
	log := c.traceLogger()
	log.Debug("Trim: start", zap.String("node", node))
	nodes, err := c.nodes.ListNodes(ctx)
	if err != nil {
		log.Debug("Trim: listing nodes failed", zap.Error(err))
		return nil, err
	}
	var hosts []string
	nameOf := map[string]string{}
	for _, n := range nodes {
		if node != "" && n.Name != node && n.Address != node {
			log.Debug("Trim: skipping node, not the one asked for", zap.String("node", n.Name))
			continue
		}
		if node == "" && n.State != NodeStateOnline {
			log.Debug("Trim: skipping node that is not online", zap.String("node", n.Name),
				zap.String("state", string(n.State)))
			continue
		}
		log.Debug("Trim: node selected", zap.String("node", n.Name), zap.String("host", n.Address))
		hosts = append(hosts, n.Address)
		nameOf[n.Address] = n.Name
	}
	if len(hosts) == 0 {
		if node != "" {
			log.Debug("Trim: node not registered", zap.String("node", node))
			return nil, fmt.Errorf("node %q is not registered", node)
		}
		log.Debug("Trim: no online nodes")
		return nil, nil
	}
	cmd := "echo " + base64Std(trimScript) + " | base64 -d | sudo /bin/bash"
	start := time.Now()
	res, err := c.deployment.Exec(ctx, hosts, cmd, deployment.WithExecTimeout(trimTimeout))
	if err != nil {
		log.Debug("Trim: fstrim sweep failed", zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)),
			zap.Error(err))
		return nil, err
	}
	log.Debug("Trim: fstrim sweep ran", zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)))
	var out []TrimResult
	for _, h := range hosts {
		hr := res.Hosts[h]
		if hr == nil {
			log.Debug("Trim: no answer from host", zap.String("host", h))
			out = append(out, TrimResult{Node: nameOf[h], Err: "no answer"})
			continue
		}
		if !hr.Success && strings.TrimSpace(hr.Output) == "" {
			log.Debug("Trim: trim failed on host without output", zap.String("host", h))
			out = append(out, TrimResult{Node: nameOf[h], Err: "trim failed"})
			continue
		}
		log.Debug("Trim: host answered", zap.String("host", h), zap.Bool("success", hr.Success))
		out = append(out, parseTrimOutput(nameOf[h], hr.Output)...)
	}
	log.Debug("Trim: done", zap.String("node", node), zap.Int("count", len(out)))
	return out, nil
}

// trimSummary is one line about a sweep, for the log and the event.
func trimSummary(results []TrimResult) (string, bool) {
	var total uint64
	var failed []string
	mounts := 0
	for _, r := range results {
		if r.Err != "" {
			failed = append(failed, fmt.Sprintf("%s:%s (%s)", r.Node, r.Mount, r.Err))
			continue
		}
		if r.Mount != "" {
			mounts++
			total += r.Bytes
		}
	}
	msg := fmt.Sprintf("trimmed %d filesystem(s), %s of free space discarded", mounts, formatBytes(total))
	if len(failed) > 0 {
		msg += "; failed: " + strings.Join(failed, ", ")
	}
	zap.L().Debug("trimSummary: summarised", zap.Int("count", mounts), zap.Uint64("trimmed_bytes", total),
		zap.Int("failed", len(failed)))
	return msg, len(failed) > 0
}

var trimming atomic.Bool

// runTrimSweep is the scheduled trim.
func (sm *ScheduleManager) runTrimSweep() {
	log := sm.controller.traceLogger()
	if !trimming.CompareAndSwap(false, true) {
		log.Debug("ScheduleManager.runTrimSweep: a sweep is already running, skipped")
		return
	}
	defer trimming.Store(false)
	c := sm.controller
	log.Debug("ScheduleManager.runTrimSweep: tick")
	start := time.Now()
	results, err := c.storage.Trim(context.Background(), "")
	log.Debug("ScheduleManager.runTrimSweep: trim done", zap.Int("count", len(results)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		c.logger.Warn("Trim sweep failed", zap.Error(err))
		return
	}
	msg, failed := trimSummary(results)
	c.logger.Info("Trim sweep", zap.String("result", msg))
	if c.events != nil {
		sev := event.SeverityInfo
		if failed {
			sev = event.SeverityWarning
		}
		log.Debug("ScheduleManager.runTrimSweep: publishing event", zap.Bool("failed", failed))
		c.events.Publish(event.Event{Type: event.TypePoolTrimmed, Severity: sev, Status: event.StatusInfo, Message: msg})
	}
}

func (sm *ScheduleManager) trimSchedule() string {
	if sm.controller.config == nil {
		sm.controller.traceLogger().Debug("ScheduleManager.trimSchedule: no config, no schedule")
		return ""
	}
	sched := strings.TrimSpace(sm.controller.config.Storage.Thin.TrimSchedule)
	sm.controller.traceLogger().Debug("ScheduleManager.trimSchedule: schedule", zap.String("schedule", sched))
	return sched
}
