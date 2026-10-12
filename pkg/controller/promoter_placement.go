package controller

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/gateway"
)

// Where a resource's drbd-reactor promoters live.
//
// A promoter may run on a node that holds an UpToDate copy of the data and is
// meant to take over automatically: the resource's diskful replicas at the
// primary site. Not a tiebreaker or a diskless client, which hold no copy, and
// not the DR node of a WAN resource, which holds an asynchronous copy that may
// be behind — failing over to it is a manual, lossy decision
// (docs/design/wan-replication.md), and a promoter there turned it into an
// automatic one the first time the primary site demoted.
//
// Promoters were written to those nodes when an HA config or a gateway was
// created or edited, and at no other time. A replica added later had no
// promoter and could never take over; a replica removed kept one. SyncPromoters
// puts them right, and runs after every change to the replica set.

// failoverHosts returns the resolved addresses of the nodes that may run
// resource's promoters: its diskful replicas, without the DR node.
func (rm *ResourceManager) failoverHosts(ctx context.Context, resource string) ([]string, error) {
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.failoverHosts: start", zap.String("resource", resource))
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		log.Debug("ResourceManager.failoverHosts: resource hosts unknown", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	if dr := rm.drHost(ctx, resource); dr != "" {
		log.Debug("ResourceManager.failoverHosts: leaving out the DR node", zap.String("resource", resource), zap.String("host", dr))
		hosts = without(hosts, dr)
	}
	if len(hosts) == 0 {
		log.Debug("ResourceManager.failoverHosts: no primary-site replica", zap.String("resource", resource))
		return nil, fmt.Errorf("resource %q has no primary-site replica to run a promoter on", resource)
	}
	log.Debug("ResourceManager.failoverHosts: done", zap.String("resource", resource), zap.Strings("hosts", hosts))
	return hosts, nil
}

// drHost is the resolved address of resource's DR node, or "" for a LAN
// resource.
func (rm *ResourceManager) drHost(ctx context.Context, resource string) string {
	log := rm.controller.traceLogger()
	if rm.controller.db == nil {
		log.Debug("ResourceManager.drHost: no database, no DR node", zap.String("resource", resource))
		return ""
	}
	start := time.Now()
	dbRes, err := rm.controller.db.GetResource(ctx, resource)
	log.Debug("ResourceManager.drHost: database read resource", zap.String("resource", resource),
		zap.Bool("found", err == nil && dbRes != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || dbRes == nil || !dbRes.WANMode || dbRes.DRNode == "" {
		return ""
	}
	host := rm.controller.ResolveHost(dbRes.DRNode)
	log.Debug("ResourceManager.drHost: WAN resource has a DR node", zap.String("resource", resource),
		zap.String("node", dbRes.DRNode), zap.String("host", host))
	return host
}

// SyncPromoters makes every Haify promoter of resource — its `ha create` config,
// its gateway or its app — present on the nodes failoverHosts names and absent from
// every other node. former names nodes that held a replica until just now,
// which are retired even when they are not registered nodes.
//
// The controller's own resource is left alone: Self-HA places its promoter
// together with the controller binary and unit it starts (selfha.go).
func (rm *ResourceManager) SyncPromoters(ctx context.Context, resource string, former ...string) error {
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.SyncPromoters: start", zap.String("resource", resource), zap.Strings("former", former))
	if resource == SelfHaResource {
		log.Debug("ResourceManager.SyncPromoters: the controller's own resource is left to Self-HA", zap.String("resource", resource))
		return nil
	}
	run, err := rm.failoverHosts(ctx, resource)
	if err != nil {
		log.Debug("ResourceManager.SyncPromoters: no failover hosts", zap.String("resource", resource), zap.Error(err))
		return err
	}
	var errs []error
	if err := rm.syncHaPlacement(ctx, resource, run, former); err != nil {
		log.Debug("ResourceManager.SyncPromoters: HA placement failed", zap.String("resource", resource), zap.Error(err))
		errs = append(errs, err)
	}
	if rm.controller.gateway != nil {
		start := time.Now()
		err := rm.controller.gateway.SyncPlacement(ctx, resource)
		log.Debug("ResourceManager.SyncPromoters: gateway placement", zap.String("resource", resource),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			errs = append(errs, err)
		}
	} else {
		log.Debug("ResourceManager.SyncPromoters: no gateway manager, gateway placement skipped", zap.String("resource", resource))
	}
	if err := rm.syncAppPlacement(ctx, resource, run, former); err != nil {
		log.Debug("ResourceManager.SyncPromoters: app placement failed", zap.String("resource", resource), zap.Error(err))
		errs = append(errs, err)
	}
	log.Debug("ResourceManager.SyncPromoters: done", zap.String("resource", resource), zap.Strings("hosts", run),
		zap.Int("errors", len(errs)))
	return errors.Join(errs...)
}

// syncHaPlacement installs resource's HA config on the run hosts that lack it
// and retires it from every other node.
//
// Hosts that already hold it are not written to: on the node running the
// service chain a rewrite plus reload is a restart of that chain.
func (rm *ResourceManager) syncHaPlacement(ctx context.Context, resource string, run, former []string) error {
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.syncHaPlacement: start", zap.String("resource", resource), zap.Strings("hosts", run),
		zap.Strings("former", former))
	cfg := haTomlPath(resource)
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, run, gateway.ConfigDumpCommand(cfg, cfg+".disabled"))
	log.Debug("ResourceManager.syncHaPlacement: dumped HA configs", zap.String("resource", resource),
		zap.Strings("hosts", run), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("read the HA config of %s: %w", resource, err)
	}

	type copyOf struct {
		host, content string
		disabled      bool
	}
	var copies []copyOf
	var missing []string
	for _, h := range run {
		hr := res.Hosts[h]
		if hr == nil || !hr.Success {
			rm.controller.logger.Warn("Replica did not answer; its HA promoter is left as it is",
				zap.String("resource", resource), zap.String("host", h))
			continue
		}
		files, perr := gateway.ParseConfigDump(hr.Output)
		if perr != nil {
			log.Debug("ResourceManager.syncHaPlacement: config dump did not parse", zap.String("host", h), zap.Error(perr))
			return fmt.Errorf("read the HA config of %s on %s: %w", resource, rm.nodeLabel(h), perr)
		}
		if c, ok := files[cfg]; ok {
			log.Debug("ResourceManager.syncHaPlacement: host holds the live config", zap.String("host", h))
			copies = append(copies, copyOf{host: h, content: c})
		} else if c, ok := files[cfg+".disabled"]; ok {
			log.Debug("ResourceManager.syncHaPlacement: host holds the disabled config", zap.String("host", h))
			copies = append(copies, copyOf{host: h, content: c, disabled: true})
		} else {
			log.Debug("ResourceManager.syncHaPlacement: host lacks the config", zap.String("host", h))
			missing = append(missing, h)
		}
	}
	log.Debug("ResourceManager.syncHaPlacement: copies found", zap.String("resource", resource),
		zap.Int("copies", len(copies)), zap.Strings("missing", missing))

	if len(copies) > 0 && len(missing) > 0 {
		// The live copy most replicas hold; the first host in order on a tie.
		sort.SliceStable(copies, func(i, j int) bool { return !copies[i].disabled && copies[j].disabled })
		count := map[string]int{}
		for _, c := range copies {
			count[c.content]++
		}
		chosen := copies[0]
		for _, c := range copies[1:] {
			if c.disabled == chosen.disabled && count[c.content] > count[chosen.content] {
				chosen = c
			}
		}
		log.Debug("ResourceManager.syncHaPlacement: copying the config to hosts that lack it", zap.String("resource", resource),
			zap.String("source", chosen.host), zap.Bool("disabled", chosen.disabled), zap.Int("same_copies", count[chosen.content]),
			zap.Strings("hosts", missing))
		if err := rm.installHaPromoter(ctx, resource, chosen.host, chosen.content, chosen.disabled, missing); err != nil {
			log.Debug("ResourceManager.syncHaPlacement: install failed", zap.String("resource", resource), zap.Error(err))
			return err
		}
	} else {
		log.Debug("ResourceManager.syncHaPlacement: nothing to install", zap.String("resource", resource),
			zap.Int("copies", len(copies)), zap.Int("missing", len(missing)))
	}

	runSet := map[string]bool{}
	for _, h := range run {
		runSet[h] = true
	}
	var rest []string
	seen := map[string]bool{}
	for _, h := range append(rm.allNodeAddresses(), rm.resolveAll(former)...) {
		if h != "" && !runSet[h] && !seen[h] {
			seen[h] = true
			rest = append(rest, h)
		}
	}
	log.Debug("ResourceManager.syncHaPlacement: retiring from nodes that must not run it", zap.String("resource", resource),
		zap.Strings("hosts", rest))
	rm.retireHaPromoter(ctx, resource, rest)
	return nil
}

// installHaPromoter copies an HA config, and the mount units its chain
// starts, from src to hosts — after checking the hosts can run the chain.
func (rm *ResourceManager) installHaPromoter(ctx context.Context, resource, src, content string, disabled bool, hosts []string) error {
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.installHaPromoter: start", zap.String("resource", resource), zap.String("source", src),
		zap.Bool("disabled", disabled), zap.Strings("hosts", hosts), zap.Int("bytes", len(content)))
	chain, err := gateway.PromoterChain(content)
	if err != nil {
		log.Debug("ResourceManager.installHaPromoter: chain did not parse", zap.String("resource", resource), zap.Error(err))
		return fmt.Errorf("HA config of %s: %w", resource, err)
	}
	if err := rm.checkHaChain(ctx, chain, hosts); err != nil {
		log.Debug("ResourceManager.installHaPromoter: chain cannot run on the hosts", zap.String("resource", resource), zap.Error(err))
		return fmt.Errorf("the HA config of %s cannot run on %s: %w", resource, rm.nodeLabels(hosts), err)
	}

	var mounts []string
	vip := false
	for _, e := range chain {
		switch {
		case strings.HasSuffix(e.Unit, ".mount"):
			mounts = append(mounts, path.Join("/etc/systemd/system", e.Unit))
		case strings.HasPrefix(e.Unit, "service-ip@"):
			vip = true
		}
	}
	log.Debug("ResourceManager.installHaPromoter: chain needs", zap.String("resource", resource),
		zap.Int("chain", len(chain)), zap.Strings("mount_units", mounts), zap.Bool("service_ip", vip))
	if vip {
		if err := rm.ensureServiceIP(ctx, hosts); err != nil {
			log.Debug("ResourceManager.installHaPromoter: service-ip not available", zap.Strings("hosts", hosts), zap.Error(err))
			return err
		}
	}
	if len(mounts) > 0 {
		start := time.Now()
		res, err := rm.deployment.Exec(ctx, []string{src}, gateway.ConfigDumpCommand(mounts...))
		log.Debug("ResourceManager.installHaPromoter: read mount units from the source", zap.String("host", src),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil || res == nil || res.Hosts[src] == nil || !res.Hosts[src].Success {
			return fmt.Errorf("read the mount units of %s from %s: %v", resource, rm.nodeLabel(src), err)
		}
		units, err := gateway.ParseConfigDump(res.Hosts[src].Output)
		if err != nil {
			log.Debug("ResourceManager.installHaPromoter: mount unit dump did not parse", zap.String("host", src), zap.Error(err))
			return err
		}
		for _, p := range mounts {
			unit, ok := units[p]
			if !ok {
				log.Debug("ResourceManager.installHaPromoter: source lacks a mount unit", zap.String("host", src), zap.String("path", p))
				return fmt.Errorf("the HA config of %s starts %s, which %s does not have", resource, path.Base(p), rm.nodeLabel(src))
			}
			start := time.Now()
			if _, err := rm.deployment.DistributeConfig(ctx, hosts, unit, p); err != nil {
				log.Debug("ResourceManager.installHaPromoter: installing mount unit failed", zap.String("path", p),
					zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
				return fmt.Errorf("install %s: %w", path.Base(p), err)
			}
			log.Debug("ResourceManager.installHaPromoter: mount unit installed", zap.String("path", p),
				zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)))
		}
		start = time.Now()
		_, err = rm.deployment.Exec(ctx, hosts, "sudo systemctl daemon-reload")
		log.Debug("ResourceManager.installHaPromoter: daemon-reload", zap.Strings("hosts", hosts),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			rm.controller.logger.Warn("daemon-reload failed after installing mount units", zap.Error(err))
		}
	}

	dest := haTomlPath(resource)
	if disabled {
		dest += ".disabled"
	}
	start := time.Now()
	if _, err := rm.deployment.DistributeConfig(ctx, hosts, content, dest); err != nil {
		log.Debug("ResourceManager.installHaPromoter: installing the config failed", zap.String("path", dest),
			zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("install the HA config of %s: %w", resource, err)
	}
	log.Debug("ResourceManager.installHaPromoter: config installed", zap.String("path", dest),
		zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)))
	if !disabled {
		start := time.Now()
		_, err := rm.deployment.ReactorReload(ctx, hosts)
		log.Debug("ResourceManager.installHaPromoter: drbd-reactor reload", zap.Strings("hosts", hosts),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			rm.controller.logger.Warn("drbd-reactor reload failed after placing an HA config",
				zap.String("resource", resource), zap.Error(err))
		}
	} else {
		log.Debug("ResourceManager.installHaPromoter: config is disabled, no reload", zap.String("resource", resource))
	}
	rm.controller.logger.Info("HA promoter placed on new replicas",
		zap.String("resource", resource), zap.Strings("hosts", hosts), zap.String("copied_from", src))
	return nil
}

// checkHaChain verifies that every entry of an HA chain can start on hosts:
// the OCF agents exist and every plain unit is installed. Mount units and
// service-ip are not checked, because installHaPromoter provides them.
func (rm *ResourceManager) checkHaChain(ctx context.Context, chain []gateway.ChainEntry, hosts []string) error {
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.checkHaChain: start", zap.Int("chain", len(chain)), zap.Strings("hosts", hosts))
	var checks []string
	for _, e := range chain {
		log.Debug("ResourceManager.checkHaChain: chain entry", zap.String("unit", e.Unit), zap.String("agent", e.AgentPath))
		switch {
		case e.AgentPath != "":
			checks = append(checks, fmt.Sprintf(`[ -x %s ] || echo "missing OCF agent %s"`,
				shellSingleQuote(e.AgentPath), e.AgentPath))
		case strings.HasSuffix(e.Unit, ".mount"), strings.HasPrefix(e.Unit, "service-ip@"):
		default:
			checks = append(checks, fmt.Sprintf(
				`[ "$(systemctl show -p LoadState --value %[1]s 2>/dev/null)" = loaded ] || echo "missing unit %[2]s"`,
				shellSingleQuote(e.Unit), e.Unit))
		}
	}
	if len(checks) == 0 {
		log.Debug("ResourceManager.checkHaChain: nothing to check")
		return nil
	}
	cmd := "echo " + base64Std(strings.Join(checks, "\n")+"\ntrue\n") + " | base64 -d | /bin/sh"
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, hosts, cmd)
	log.Debug("ResourceManager.checkHaChain: ran the checks", zap.Strings("hosts", hosts), zap.Int("checks", len(checks)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return err
	}
	var problems []string
	for _, h := range hosts {
		hr := res.Hosts[h]
		if hr == nil || !hr.Success {
			log.Debug("ResourceManager.checkHaChain: host could not be checked", zap.String("host", h), zap.String("reason", hostFailure(hr)))
			problems = append(problems, fmt.Sprintf("%s: could not be checked (%s)", rm.nodeLabel(h), hostFailure(hr)))
			continue
		}
		before := len(problems)
		for _, line := range strings.Split(hr.Output, "\n") {
			if line = strings.TrimSpace(line); strings.HasPrefix(line, "missing ") {
				problems = append(problems, rm.nodeLabel(h)+": "+line)
			}
		}
		log.Debug("ResourceManager.checkHaChain: host checked", zap.String("host", h), zap.Int("missing", len(problems)-before))
	}
	if len(problems) > 0 {
		log.Debug("ResourceManager.checkHaChain: chain cannot start", zap.Strings("problems", problems))
		return fmt.Errorf("%s; install them there first", strings.Join(problems, "; "))
	}
	return nil
}

// retireHaPromoter removes resource's HA config from hosts and stops its
// service chain there — except on a node holding the resource Primary with
// UpToDate data, which is a DR node after a manual failover serving what is
// left: it loses only the promoter, never the service.
func (rm *ResourceManager) retireHaPromoter(ctx context.Context, resource string, hosts []string) {
	log := rm.controller.traceLogger()
	if len(hosts) == 0 {
		log.Debug("ResourceManager.retireHaPromoter: no hosts to retire from", zap.String("resource", resource))
		return
	}
	log.Debug("ResourceManager.retireHaPromoter: start", zap.String("resource", resource), zap.Strings("hosts", hosts))
	script := fmt.Sprintf(`found=
for f in %[1]s %[1]s.disabled; do
  [ -e "$f" ] && rm -f "$f" && found=1
done
[ -n "$found" ] || exit 0
systemctl reload drbd-reactor || systemctl restart drbd-reactor
st=$(drbdsetup status %[2]s 2>/dev/null)
if printf '%%s\n' "$st" | head -n1 | grep -q 'role:Primary' && printf '%%s\n' "$st" | grep -Eq '^ +(volume:[0-9]+ +)?disk:UpToDate'; then
  echo "kept the services of %[2]s running: this node is Primary with UpToDate data"
  exit 0
fi
systemctl stop %[3]s 2>/dev/null
true`, haTomlPath(resource), resource, shellSingleQuote(fmt.Sprintf("drbd-services@%s.target", strings.ReplaceAll(resource, "-", `\x2d`))))
	cmd := "echo " + base64Std(script) + " | base64 -d | sudo /bin/sh"
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, hosts, cmd)
	log.Debug("ResourceManager.retireHaPromoter: ran the retire script", zap.String("resource", resource),
		zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		rm.controller.logger.Warn("Failed to retire the HA promoter from nodes that must not run it",
			zap.String("resource", resource), zap.Strings("hosts", hosts), zap.Error(err))
		return
	}
	for h, hr := range res.Hosts {
		if hr != nil && hr.Success {
			log.Debug("ResourceManager.retireHaPromoter: host done", zap.String("resource", resource), zap.String("host", h),
				zap.String("output", strings.TrimSpace(hr.Output)))
		}
		if hr != nil && !hr.Success {
			rm.controller.logger.Warn("Could not retire the HA promoter; remove it there when the node is back",
				zap.String("resource", resource), zap.String("node", rm.nodeLabel(h)),
				zap.String("config", haTomlPath(resource)))
		}
	}
}

// checkPromoterPrereqs refuses, before anything is provisioned, a new replica
// on a node that could not run the resource's HA chain or gateway.
func (rm *ResourceManager) checkPromoterPrereqs(ctx context.Context, resource string, hosts []string) error {
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.checkPromoterPrereqs: start", zap.String("resource", resource), zap.Strings("hosts", hosts))
	if resource == SelfHaResource {
		log.Debug("ResourceManager.checkPromoterPrereqs: the controller's own resource, not checked", zap.String("resource", resource))
		return nil
	}
	if rm.controller.db != nil && rm.controller.gateway != nil {
		start := time.Now()
		gw, err := rm.controller.db.GetGatewayByResource(ctx, resource)
		log.Debug("ResourceManager.checkPromoterPrereqs: database read gateway", zap.String("resource", resource),
			zap.Bool("found", err == nil && gw != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err == nil && gw != nil {
			start := time.Now()
			if err := rm.controller.gateway.CheckPlacementPrereqs(ctx, string(gw.Type), hosts); err != nil {
				log.Debug("ResourceManager.checkPromoterPrereqs: gateway cannot run on the hosts", zap.String("resource", resource),
					zap.String("type", string(gw.Type)), zap.Duration("took", time.Since(start)), zap.Error(err))
				return fmt.Errorf("%s is exported by gateway %q, which %s could not run: %w",
					resource, gw.Name, rm.nodeLabels(hosts), err)
			}
			log.Debug("ResourceManager.checkPromoterPrereqs: gateway can run on the hosts", zap.String("resource", resource),
				zap.String("type", string(gw.Type)), zap.Duration("took", time.Since(start)))
		}
	}
	_, content, err := rm.GetHaToml(ctx, resource)
	log.Debug("ResourceManager.checkPromoterPrereqs: HA config", zap.String("resource", resource),
		zap.Bool("found", err == nil && strings.TrimSpace(content) != ""), zap.Error(err))
	if err == nil && strings.TrimSpace(content) != "" {
		chain, perr := gateway.PromoterChain(content)
		if perr != nil {
			log.Debug("ResourceManager.checkPromoterPrereqs: chain did not parse", zap.String("resource", resource), zap.Error(perr))
			return fmt.Errorf("HA config of %s: %w", resource, perr)
		}
		if err := rm.checkHaChain(ctx, chain, hosts); err != nil {
			log.Debug("ResourceManager.checkPromoterPrereqs: chain cannot start on the hosts", zap.String("resource", resource), zap.Error(err))
			return fmt.Errorf("%s has an HA config whose chain cannot start on %s: %w", resource, rm.nodeLabels(hosts), err)
		}
	}
	log.Debug("ResourceManager.checkPromoterPrereqs: hosts can run the promoters", zap.String("resource", resource))
	return nil
}

// resolveAll resolves node names to addresses.
func (rm *ResourceManager) resolveAll(nodes []string) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, rm.controller.ResolveHost(n))
	}
	rm.controller.traceLogger().Debug("ResourceManager.resolveAll: resolved", zap.Strings("nodes", nodes),
		zap.Strings("hosts", out))
	return out
}
