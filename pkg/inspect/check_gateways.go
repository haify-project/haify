package inspect

import (
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// checkGateways requires every gateway that is not stopped to have exactly
// one Primary, and its promoter config to sit on exactly the nodes that can
// take it over.
func checkGateways(in *Input) []Check {
	var out []Check
	gws := append([]Gateway(nil), in.Gateways...)
	sort.Slice(gws, func(i, j int) bool { return gws[i].Resource < gws[j].Resource })
	log := zap.L()
	log.Debug("checkGateways: start", zap.Int("gateways", len(gws)))
	serving := 0
	for _, g := range gws {
		log.Debug("checkGateways: gateway", zap.String("resource", g.Resource), zap.String("type", g.Type),
			zap.String("status", g.Status), zap.Bool("serving", g.Serving()))
		if !g.Serving() {
			continue
		}
		serving++
		r, ok := resourceNamed(in, g.Resource)
		if !ok {
			log.Debug("checkGateways: gateway resource missing", zap.String("resource", g.Resource))
			out = append(out, Check{ID: "gateway.orphan", Area: AreaGateways, Subject: g.Resource, Status: StatusFail,
				Message: fmt.Sprintf("%s gateway is recorded as %s but its resource %s does not exist", g.Type, g.Status, g.Resource),
				Fix:     fmt.Sprintf("haify gateway delete --resource %s", g.Resource)})
			continue
		}
		why := fmt.Sprintf("%s gateway, %s", g.Type, orDash(g.Status))
		out = append(out, primaryCheck(in, r, AreaGateways, "gateway", why,
			fmt.Sprintf("haify gateway start --resource %s", g.Resource))...)
		// Start re-enables a .disabled config and retires one left on a node
		// without a replica.
		out = append(out, promoterChecks(in, r, AreaGateways, "gateway", fmt.Sprintf("haify-%s-%s.toml", g.Type, g.Resource),
			fmt.Sprintf("haify gateway start --resource %s", g.Resource))...)
	}
	log.Debug("checkGateways: done", zap.Int("serving", serving), zap.Int("findings", len(out)))
	if len(out) == 0 && serving == 0 {
		msg := "no gateways"
		if len(gws) > 0 {
			msg = fmt.Sprintf("no gateways serving (%s stopped)", plural(len(gws), "gateway", "gateways"))
		}
		out = append(out, pass("gateway.serving", AreaGateways, "%s", msg))
	}
	if len(out) == 0 {
		out = append(out, pass("gateway.serving", AreaGateways,
			"%s serving: each has one Primary and its promoter on every diskful node", plural(serving, "gateway", "gateways")))
	}
	return out
}

// promoterChecks requires a promoter config on every diskful node of r, and
// notes one on a tiebreaker or diskless client. A WAN resource's DR node is the
// exception both ways: failover to its asynchronous copy is manual, so it must
// hold no promoter, and one there is a warning.
//
// Missing on a diskful node means failover cannot land there. Present on a
// diskless node is legal — a diskless Primary reads and writes over the
// network — but it may be an accident, so it is a warning, not a failure.
//
// fix repairs both; empty means copy the config from a node that has it.
func promoterChecks(in *Input, r Resource, area Area, prefix, file, fix string) []Check {
	log := zap.L()
	log.Debug("promoterChecks: start", zap.String("resource", r.Name), zap.String("file", file), zap.Strings("diskful", r.Diskful),
		zap.String("dr", r.DR))
	var out []Check
	var missing, disabled []string
	source := ""
	for _, n := range r.Diskful {
		p, ok := in.probe(n)
		if !ok {
			log.Debug("promoterChecks: node not probed", zap.String("node", n))
			continue
		}
		if n == r.DR {
			log.Debug("promoterChecks: DR node, must hold no promoter", zap.String("node", n), zap.Bool("has_conf", p.HasReactorConf(file)))
			if p.HasReactorConf(file) {
				name := in.nodeName(n)
				out = append(out, Check{ID: prefix + ".promoter_on_dr", Area: area, Subject: r.Name + "@" + name, Status: StatusWarn,
					Message: fmt.Sprintf("%s is the off-site DR node of %s and has its promoter config: drbd-reactor can promote "+
						"its asynchronous copy, which may be behind, without anyone deciding to fail over", name, r.Name),
					Evidence: []string{"/etc/drbd-reactor.d/" + file + " present on " + name},
					Fix:      "haify resource repair " + r.Name})
			}
			continue
		}
		switch {
		case p.HasReactorConf(file):
			log.Debug("promoterChecks: promoter active", zap.String("node", n))
			if source == "" {
				source = in.nodeName(n)
			}
		case p.HasReactorConf(file + ".disabled"):
			log.Debug("promoterChecks: promoter disabled", zap.String("node", n))
			disabled = append(disabled, in.nodeName(n))
		default:
			log.Debug("promoterChecks: promoter missing", zap.String("node", n))
			missing = append(missing, in.nodeName(n))
		}
	}
	if len(missing)+len(disabled) > 0 {
		var ev []string
		if len(missing) > 0 {
			ev = append(ev, "no /etc/drbd-reactor.d/"+file+" on: "+strings.Join(missing, ", "))
		}
		if len(disabled) > 0 {
			ev = append(ev, "only the .disabled copy on: "+strings.Join(disabled, ", "))
		}
		out = append(out, Check{ID: prefix + ".promoter_missing", Area: area, Subject: r.Name, Status: StatusWarn,
			Message:  "the promoter config is not active on every diskful node, so a failover cannot land on " + strings.Join(append(missing, disabled...), ", "),
			Evidence: ev,
			Fix:      promoterFix(in, fix, file, source, append(missing, disabled...))})
	}
	for _, n := range append(append([]string{}, r.Tiebreakers...), r.Clients...) {
		p, ok := in.probe(n)
		if !ok || !p.HasReactorConf(file) {
			continue
		}
		name := in.nodeName(n)
		log.Debug("promoterChecks: promoter on a diskless node", zap.String("resource", r.Name), zap.String("node", name))
		out = append(out, Check{ID: prefix + ".promoter_on_diskless", Area: area, Subject: r.Name + "@" + name, Status: StatusWarn,
			Message: fmt.Sprintf("%s holds no copy of %s but has its promoter config: the service can run there as a diskless Primary, "+
				"every I/O then crosses the network, and it stops if %s loses its diskful peers. Remove the promoter there if that is not intended",
				name, r.Name, name),
			Evidence: []string{"/etc/drbd-reactor.d/" + file + " present on " + name},
			Fix: firstNonEmpty(fix, fmt.Sprintf("ssh %s sudo rm /etc/drbd-reactor.d/%s && ssh %s sudo systemctl reload drbd-reactor",
				sshTarget(in, name), file, sshTarget(in, name)))})
	}
	return out
}

func resourceNamed(in *Input, name string) (Resource, bool) {
	for _, r := range in.Resources {
		if r.Name == name {
			return r, true
		}
	}
	return Resource{}, false
}

// promoterFix copies the promoter config from source to the first node that
// lacks it, when no command was given that repairs it.
func promoterFix(in *Input, fix, file, source string, lacking []string) string {
	if fix != "" || source == "" || len(lacking) == 0 {
		zap.L().Debug("promoterFix: using the given fix or none", zap.Bool("has_fix", fix != ""), zap.String("source", source),
			zap.Int("lacking", len(lacking)))
		return fix
	}
	zap.L().Debug("promoterFix: copying config from a node that has it", zap.String("source", source), zap.String("node", lacking[0]))
	path := "/etc/drbd-reactor.d/" + file
	dst := sshTarget(in, lacking[0])
	return fmt.Sprintf("ssh %s sudo cat %s | ssh %s sudo tee %s >/dev/null && ssh %s sudo systemctl reload drbd-reactor",
		sshTarget(in, source), path, dst, path, dst)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
