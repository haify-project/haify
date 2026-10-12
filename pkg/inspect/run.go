package inspect

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// checker is one family of checks over the whole input.
type checker struct {
	area Area
	run  func(*Input) []Check
}

var checkers = []checker{
	{AreaResources, checkResources},
	{AreaGateways, checkGateways},
	{AreaNodes, checkNodes},
	{AreaPools, checkPools}, {AreaPools, checkDisks},
	{AreaBackups, checkBackups},
	{AreaAlerts, checkAlerts},
	{AreaSelfHA, checkSelfHA},
	{AreaTLS, checkTLS},
	{AreaHygiene, checkHygiene},
}

// Run evaluates every check in the requested areas (all when empty) and
// returns them sorted.
func Run(in *Input, areas []Area) []Check {
	log := zap.L()
	log.Debug("Run: start", zap.Int("areas", len(areas)), zap.Int("nodes", len(in.Nodes)), zap.Int("probes", len(in.Probes)),
		zap.Int("probe_errors", len(in.ProbeErrors)), zap.Int("resources", len(in.Resources)), zap.Int("gateways", len(in.Gateways)))
	want := map[Area]bool{}
	for _, a := range areas {
		want[a] = true
	}
	var out []Check
	for _, c := range checkers {
		if len(want) > 0 && !want[c.area] {
			log.Debug("Run: area not requested, skipping", zap.String("area", string(c.area)))
			continue
		}
		start := time.Now()
		checks := c.run(in)
		log.Debug("Run: area checked", zap.String("area", string(c.area)), zap.Int("count", len(checks)),
			zap.String("worst", string(Worst(checks))), zap.Duration("took", time.Since(start)))
		// The message is left out: some quote a delivery or backup error,
		// which can carry a webhook URL or target credentials.
		for _, ch := range checks {
			log.Debug("Run: check verdict", zap.String("id", ch.ID), zap.String("area", string(ch.Area)),
				zap.String("subject", ch.Subject), zap.String("status", string(ch.Status)))
		}
		out = append(out, checks...)
	}
	Sort(out)
	s := Summarize(out)
	log.Debug("Run: done", zap.Int("count", len(out)), zap.Int("pass", s.Pass), zap.Int("warn", s.Warn),
		zap.Int("fail", s.Fail), zap.Int("error", s.Error))
	return out
}

// PoolSamples extracts the thin pool fill levels to store with a report.
func PoolSamples(in *Input) []PoolSample {
	log := zap.L()
	var out []PoolSample
	for _, name := range sortedKeys(in.Probes) {
		p := in.Probes[name]
		if p == nil {
			log.Debug("PoolSamples: node has no probe", zap.String("node", name))
			continue
		}
		for _, lv := range p.LVs {
			if lv.Segtype == "thin-pool" && lv.HasUsage && managedVG(lv.VG) {
				log.Debug("PoolSamples: sampled thin pool", zap.String("node", name), zap.String("pool", lv.VG+"/"+lv.Name),
					zap.Float64("data_percent", lv.DataPercent), zap.Float64("meta_percent", lv.MetaPercent))
				out = append(out, PoolSample{Node: name, Pool: lv.VG + "/" + lv.Name,
					DataPercent: lv.DataPercent, MetaPercent: lv.MetaPercent})
			}
		}
	}
	log.Debug("PoolSamples: done", zap.Int("count", len(out)))
	return out
}

// Headline is a one-paragraph summary of a report for a notification: the
// counts and the most serious items, each with its subject.
func Headline(r *Report, max int) string {
	zap.L().Debug("Headline: building", zap.String("report", r.ID), zap.Int("max", max), zap.Int("checks", len(r.Checks)))
	s := r.Summary
	head := fmt.Sprintf("Inspection %s: %d fail, %d warn, %d error, %d pass", r.ID, s.Fail, s.Warn, s.Error, s.Pass)
	var items []string
	for _, st := range []Status{StatusFail, StatusError, StatusWarn} {
		for _, c := range r.Checks {
			if c.Status != st || len(items) >= max {
				continue
			}
			item := c.ID
			if c.Subject != "" {
				item += " " + c.Subject
			}
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		return head + "."
	}
	more := s.Fail + s.Warn + s.Error - len(items)
	line := head + ". " + strings.Join(items, "; ")
	if more > 0 {
		line += fmt.Sprintf("; and %d more", more)
	}
	return line + ". Details: haify inspect show " + r.ID
}

// managedVG reports whether a volume group is a Haify pool.
func managedVG(vg string) bool { return strings.HasPrefix(vg, "haify_") }

func pass(id string, area Area, msg string, args ...any) Check {
	return Check{ID: id, Area: area, Status: StatusPass, Message: fmt.Sprintf(msg, args...)}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
