package inspect

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// selfHAPromoter is the promoter config the Self-HA handoff installs.
const selfHAPromoter = "haify-ha-haify-meta.toml"

// checkSelfHA judges the controller's own HA: enough current copies of its
// database, a promoter on every node that may take over, and exactly one
// controller running, on the node that holds the database.
func checkSelfHA(in *Input) []Check {
	log := zap.L()
	s := in.SelfHA
	if s == nil {
		log.Debug("checkSelfHA: Self-HA not enabled")
		return []Check{pass("selfha.disabled", AreaSelfHA, "Self-HA is not enabled; nothing to check")}
	}
	var out []Check
	log.Debug("checkSelfHA: start", zap.String("resource", s.Resource), zap.Strings("nodes", s.Nodes))
	r, ok := resourceNamed(in, s.Resource)
	if !ok {
		log.Debug("checkSelfHA: meta resource not in the database, using the Self-HA nodes", zap.String("resource", s.Resource))
		r = Resource{Name: s.Resource, Diskful: s.Nodes}
	}

	upToDate, known := 0, 0
	var ev []string
	for _, n := range r.Diskful {
		p, ok := in.probe(n)
		if !ok || !p.DRBDOK {
			log.Debug("checkSelfHA: no DRBD view from node", zap.String("node", n), zap.Bool("probed", ok))
			continue
		}
		known++
		v := p.Resource(r.Name)
		if v == nil {
			log.Debug("checkSelfHA: meta resource not up on node", zap.String("node", n))
			ev = append(ev, in.nodeName(n)+": not up")
			continue
		}
		ev = append(ev, fmt.Sprintf("%s: %s, %s", in.nodeName(n), v.Role, strings.Join(v.diskStates(), "/")))
		worst := v.worstDisk(false)
		log.Debug("checkSelfHA: meta copy", zap.String("node", n), zap.String("role", v.Role), zap.String("worst_disk", worst))
		if worst == "" {
			upToDate++
		}
	}
	log.Debug("checkSelfHA: meta copies counted", zap.Int("up_to_date", upToDate), zap.Int("known", known),
		zap.Int("diskful", len(r.Diskful)))
	if known > 0 && upToDate < 2 {
		out = append(out, Check{ID: "selfha.meta_replicas", Area: AreaSelfHA, Subject: r.Name, Status: StatusFail,
			Message: fmt.Sprintf("only %d of %d copies of the controller database are UpToDate; one more failure loses the controller",
				upToDate, len(r.Diskful)),
			Evidence: ev, Fix: "haify resource status " + r.Name})
	}

	out = append(out, promoterChecks(in, r, AreaSelfHA, "selfha", selfHAPromoter, "")...)

	var running, missingBin []string
	for _, n := range r.Diskful {
		p, ok := in.probe(n)
		if !ok {
			continue
		}
		log.Debug("checkSelfHA: controller on node", zap.String("node", n), zap.String("ctl_active", p.CtlActive),
			zap.String("ctl_bin", p.CtlBin))
		if p.CtlActive == "active" {
			running = append(running, in.nodeName(n))
		}
		if p.CtlBin == "" {
			missingBin = append(missingBin, in.nodeName(n))
		}
	}
	prim, _ := primaries(in, r)
	log.Debug("checkSelfHA: controller placement", zap.Strings("running", running), zap.Strings("primary", prim),
		zap.Strings("missing_bin", missingBin))
	switch {
	case len(in.unanswered(r.Diskful)) > 0 && len(running) < 2:
		// A node that did not answer may be the one running it.
	case len(running) == 0:
		out = append(out, Check{ID: "selfha.controller_active", Area: AreaSelfHA, Status: StatusFail,
			Message: "no Self-HA node reports haify-controller active, yet this inspection ran: the controller is running outside its promoter",
			Fix:     "haify ha self status"})
	case len(running) > 1:
		out = append(out, Check{ID: "selfha.controller_active", Area: AreaSelfHA, Subject: strings.Join(running, ","), Status: StatusFail,
			Message:  "haify-controller is active on more than one node; two controllers act on the cluster at once",
			Evidence: []string{"haify-meta Primary: " + strings.Join(prim, ", ")},
			Fix:      fmt.Sprintf("ssh %s sudo systemctl stop haify-controller", sshTarget(in, otherThan(running, prim)))})
	case len(prim) == 1 && running[0] != prim[0]:
		out = append(out, Check{ID: "selfha.controller_active", Area: AreaSelfHA, Subject: running[0], Status: StatusFail,
			Message: fmt.Sprintf("haify-controller runs on %s but haify-meta is Primary on %s; it is not using the replicated database", running[0], prim[0]),
			Fix:     fmt.Sprintf("ssh %s sudo systemctl stop haify-controller", sshTarget(in, running[0]))})
	}
	if len(missingBin) > 0 {
		out = append(out, Check{ID: "selfha.controller_binary", Area: AreaSelfHA, Subject: strings.Join(missingBin, ","), Status: StatusFail,
			Message: "no haify-controller binary is installed where the unit points on " + strings.Join(missingBin, ", ") + "; a failover there starts nothing",
			Fix:     "./scripts/deploy-all.sh " + strings.Join(r.Diskful, ",")})
	}
	if len(out) == 0 {
		out = append(out, pass("selfha.ready", AreaSelfHA, "%d of %d database copies UpToDate, promoter on every candidate, one controller active",
			upToDate, len(r.Diskful)))
	}
	return out
}

// otherThan returns the first of nodes not in keep, or the first node.
func otherThan(nodes, keep []string) string {
	for _, n := range nodes {
		if !contains(keep, n) {
			zap.L().Debug("otherThan: chose node", zap.String("node", n))
			return n
		}
	}
	zap.L().Debug("otherThan: every node kept, using the first", zap.String("node", nodes[0]))
	return nodes[0]
}
