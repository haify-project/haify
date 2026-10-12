package inspect

import (
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// maxEventEvidence bounds how many undelivered events a check lists.
const maxEventEvidence = 10

// checkAlerts asks whether an alert would reach anyone: that delivery is on,
// that there is somewhere to deliver to, that each channel's recent
// deliveries succeeded, and that every warning or critical raised since the
// last inspection was accepted by at least one channel.
//
// It sends nothing. A test message per run would page somebody daily to
// report that paging works; the delivery results of real alerts say the same
// without the noise.
func checkAlerts(in *Input) []Check {
	log := zap.L()
	a := in.Alerts
	log.Debug("checkAlerts: start", zap.Bool("enabled", a.Enabled), zap.Int("targets", len(a.Targets)),
		zap.Int("events", len(a.Events)), zap.Time("since", a.Since))
	if !a.Enabled {
		log.Debug("checkAlerts: alerting disabled")
		return []Check{{ID: "alerts.disabled", Area: AreaAlerts, Status: StatusWarn,
			Message: "[alert] enabled = false: nothing watches cluster health between inspections, and nothing is delivered",
			Fix:     "set [alert] enabled = true in /etc/haify/controller.toml and restart haify-controller"}}
	}
	var out []Check
	active := 0
	for _, t := range a.Targets {
		if t.Enabled {
			active++
		}
	}
	log.Debug("checkAlerts: enabled channels", zap.Int("count", active))
	if active == 0 {
		out = append(out, Check{ID: "alerts.no_channel", Area: AreaAlerts, Status: StatusFail,
			Message: "alerts are raised but no enabled notification channel exists; they reach nobody",
			Fix:     "haify channel add --name <name> --kind <generic|feishu|slack|wecom|dingtalk> --url <webhook-url>"})
	}
	for _, t := range a.Targets {
		// LastError is not traced: a failed delivery's error can carry the
		// webhook URL, and with it a bot token.
		log.Debug("checkAlerts: channel", zap.String("channel", t.Name), zap.Bool("enabled", t.Enabled), zap.Bool("known", t.Known),
			zap.Int("consecutive_failures", t.ConsecutiveFailures), zap.Time("last_success", t.LastSuccess),
			zap.Time("last_failure", t.LastFailure))
		switch {
		case !t.Enabled:
			out = append(out, Check{ID: "alerts.channel_muted", Area: AreaAlerts, Subject: t.Name, Status: StatusWarn,
				Message: "channel is muted; it delivers nothing until re-enabled"})
		case t.Known && t.ConsecutiveFailures > 0 && t.LastFailure.After(t.LastSuccess):
			out = append(out, Check{ID: "alerts.channel_failing", Area: AreaAlerts, Subject: t.Name, Status: StatusFail,
				Message: fmt.Sprintf("the last %s to this channel failed: %s",
					plural(t.ConsecutiveFailures, "delivery", "deliveries"), t.LastError),
				Evidence: []string{"last failure " + stamp(t.LastFailure), "last success " + stamp(t.LastSuccess)},
				Fix:      notifyTestFix(t.Name)})
		}
	}
	if c, ok := undelivered(a); ok {
		out = append(out, c)
	}
	if len(out) == 0 {
		out = append(out, pass("alerts.delivery", AreaAlerts, "%s, last deliveries succeeded, %s since %s all accepted",
			plural(active, "channel enabled", "channels enabled"), plural(len(a.Events), "alert", "alerts"), stamp(a.Since)))
	}
	return out
}

// undelivered finds firing warning/critical events that no channel took: none
// accepts their type and severity, or every channel that does gave up on them.
func undelivered(a AlertInput) (Check, bool) {
	var lost []string
	critical := false
	for _, e := range a.Events {
		accepted, failed, later := 0, 0, 0
		for _, t := range a.Targets {
			if !t.Enabled || (t.Accepts != nil && !t.Accepts(e)) {
				continue
			}
			if !t.Since.IsZero() && e.At.Before(t.Since) {
				later++
				continue
			}
			accepted++
			if t.Failed[e.ID] {
				failed++
			}
		}
		zap.L().Debug("undelivered: event", zap.Uint64("event", e.ID), zap.String("type", e.Type), zap.String("severity", e.Severity),
			zap.Int("accepted", accepted), zap.Int("failed", failed), zap.Int("later", later))
		if accepted > 0 && failed < accepted {
			continue
		}
		why := "no channel accepts it"
		if later > 0 && accepted == 0 {
			why = "raised before a channel that accepts it existed"
		}
		if accepted > 0 {
			why = "every channel that accepts it failed to deliver it"
		}
		if e.Severity == "critical" {
			critical = true
		}
		lost = append(lost, fmt.Sprintf("#%d %s %s %s %s: %s (%s)", e.ID, e.At.UTC().Format(time.RFC3339), e.Severity, e.Type,
			strings.TrimSpace(e.Resource+" "+e.Node), e.Message, why))
	}
	if len(lost) == 0 {
		zap.L().Debug("undelivered: every event reached a channel", zap.Int("events", len(a.Events)))
		return Check{}, false
	}
	n := len(lost)
	zap.L().Debug("undelivered: events reached no one", zap.Int("count", n), zap.Bool("critical", critical))
	if n > maxEventEvidence {
		lost = append(lost[:maxEventEvidence], fmt.Sprintf("... and %d more", n-maxEventEvidence))
	}
	st := StatusWarn
	if critical {
		st = StatusFail
	}
	return Check{ID: "alerts.undelivered", Area: AreaAlerts, Status: st,
		Message:  fmt.Sprintf("%s raised since %s reached no one", plural(n, "alert", "alerts"), stamp(a.Since)),
		Evidence: lost,
		Fix:      "haify event list --min-severity warning"}, true
}

func notifyTestFix(name string) string {
	if strings.HasPrefix(name, "config:") {
		return "fix the [alert] webhook in /etc/haify/controller.toml and restart haify-controller"
	}
	return "haify channel test " + name
}
