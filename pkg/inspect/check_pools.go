package inspect

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// Growth thresholds: how soon a pool that keeps filling at its recent rate
// runs out.
const (
	growthWarnDays = 14.0
	growthFailDays = 3.0
	// minTrendHours keeps two reports minutes apart from producing a rate.
	minTrendHours = 1.0
)

// checkPools judges thin pool data and metadata use against the alert
// thresholds, and extrapolates the growth since the previous report.
func checkPools(in *Input) []Check {
	log := zap.L()
	near, full := in.PoolNearFull, in.PoolFull
	if near <= 0 || near >= 100 {
		log.Debug("checkPools: near-full threshold unusable, using 85", zap.Float64("near", near))
		near = 85
	}
	if full <= 0 || full > 100 || full <= near {
		log.Debug("checkPools: full threshold unusable, using 95", zap.Float64("full", full))
		full = 95
	}
	log.Debug("checkPools: start", zap.Float64("near", near), zap.Float64("full", full), zap.Bool("has_previous", in.Previous != nil))
	prev := map[string]PoolSample{}
	if in.Previous != nil {
		for _, s := range in.Previous.Pools {
			prev[s.Node+"/"+s.Pool] = s
		}
	}
	var out []Check
	var unreadable []string
	pools := 0
	for _, node := range sortedKeys(in.Probes) {
		p := in.Probes[node]
		if !p.LVsOK {
			log.Debug("checkPools: lvs unreadable on node", zap.String("node", node))
			unreadable = append(unreadable, node)
			continue
		}
		for _, lv := range p.LVs {
			if lv.Segtype != "thin-pool" || !managedVG(lv.VG) || !lv.HasUsage {
				continue
			}
			pools++
			key := lv.VG + "/" + lv.Name
			subject := node + ":" + key
			ev := []string{fmt.Sprintf("data %.1f%%, metadata %.1f%% (warn %.0f%%, fail %.0f%%)", lv.DataPercent, lv.MetaPercent, near, full)}
			trend, days, growing := poolTrend(in, prev[node+"/"+key], lv.DataPercent)
			log.Debug("checkPools: thin pool", zap.String("node", node), zap.String("pool", key),
				zap.Float64("data_percent", lv.DataPercent), zap.Float64("meta_percent", lv.MetaPercent),
				zap.Bool("growing", growing), zap.Float64("days_to_full", days))
			if trend != "" {
				ev = append(ev, trend)
			}
			fix := fmt.Sprintf("haify pool add --pool %s --nodes %s --devices <new-device>", strings.TrimPrefix(lv.VG, "haify_"), node)
			for _, dim := range []struct {
				id, what string
				pct      float64
			}{{"pool.data", "data", lv.DataPercent}, {"pool.metadata", "metadata", lv.MetaPercent}} {
				st := StatusPass
				switch {
				case dim.pct >= full:
					st = StatusFail
				case dim.pct >= near:
					st = StatusWarn
				}
				if st == StatusPass {
					continue
				}
				msg := fmt.Sprintf("thin pool %s is %.1f%% full", dim.what, dim.pct)
				if dim.what == "data" {
					msg += "; when it reaches 100% writes fail and every replica on it goes Diskless"
				} else {
					msg += "; metadata does not grow with the pool and fails writes just as hard"
				}
				out = append(out, Check{ID: dim.id, Area: AreaPools, Subject: subject, Status: st, Message: msg, Evidence: ev, Fix: fix})
			}
			if growing && days < growthWarnDays && lv.DataPercent < near {
				st := StatusWarn
				if days < growthFailDays {
					st = StatusFail
				}
				out = append(out, Check{ID: "pool.growth", Area: AreaPools, Subject: subject, Status: st,
					Message: fmt.Sprintf("thin pool data is growing fast enough to be full in about %.1f days", days), Evidence: ev, Fix: fix})
			}
		}
	}
	if len(unreadable) > 0 {
		out = append(out, Check{ID: "pool.unreadable", Area: AreaPools, Subject: strings.Join(unreadable, ","), Status: StatusError,
			Message: "lvs gave no readable answer, so thin pool use there is unknown"})
	}
	room, thick := snapshotRoom(in)
	out = append(out, room...)
	log.Debug("checkPools: done", zap.Int("thin_pools", pools), zap.Int("thick_groups", thick),
		zap.Strings("unreadable", unreadable), zap.Int("findings", len(out)))
	if len(out) == 0 {
		var parts []string
		if pools > 0 {
			parts = append(parts, fmt.Sprintf("%s below %.0f%% data and metadata, none filling within %.0f days",
				plural(pools, "thin pool", "thin pools"), near, growthWarnDays))
		}
		if thick > 0 {
			parts = append(parts, plural(thick, "thick pool has", "thick pools have")+" room for a snapshot of every volume")
		}
		if len(parts) == 0 {
			parts = append(parts, "no pools")
		}
		out = append(out, pass("pool.usage", AreaPools, "%s", strings.Join(parts, "; ")))
	}
	return out
}

// poolTrend describes data growth since the previous sample and, when the
// pool is filling, the days until it is full at that rate.
func poolTrend(in *Input, prev PoolSample, now float64) (string, float64, bool) {
	if in.Previous == nil || prev.Pool == "" {
		zap.L().Debug("poolTrend: no previous sample", zap.Bool("has_previous", in.Previous != nil))
		return "", 0, false
	}
	hours := in.Now.Sub(in.Previous.FinishedAt).Hours()
	if hours < minTrendHours {
		zap.L().Debug("poolTrend: previous report too recent for a rate", zap.String("pool", prev.Pool), zap.Float64("hours", hours))
		return "", 0, false
	}
	perDay := (now - prev.DataPercent) / hours * 24
	desc := fmt.Sprintf("since report %s (%.0fh ago): %.1f%% -> %.1f%%, %+.2f%%/day", in.Previous.ID, hours, prev.DataPercent, now, perDay)
	if perDay <= 0 {
		zap.L().Debug("poolTrend: not growing", zap.String("pool", prev.Pool), zap.Float64("per_day", perDay))
		return desc, 0, false
	}
	days := (100 - now) / perDay
	zap.L().Debug("poolTrend: growing", zap.String("pool", prev.Pool), zap.Float64("per_day", perDay), zap.Float64("days_to_full", days))
	return desc + fmt.Sprintf(", full in ~%.1f days", days), days, true
}
