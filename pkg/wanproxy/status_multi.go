package wanproxy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// LegStatus is one primary-site node's WAN leg: the dialer on that node and the
// matching acceptor on the DR.
type LegStatus struct {
	// LegID is the systemd instance name, e.g. "data_192-0-2-10".
	LegID string
	// PrimaryHost is the primary-site node this leg belongs to.
	PrimaryHost   string
	PrimaryActive bool
	DRActive      bool
}

// Healthy reports whether both ends of the leg are running.
func (l LegStatus) Healthy() bool { return l.PrimaryActive && l.DRActive }

// MultiStatus is the health of every WAN leg of one resource.
type MultiStatus struct {
	Resource string
	DRHost   string
	Legs     []LegStatus
	// WANReachable is whether the DR's public endpoint answered a probe. It is
	// reported per resource rather than per leg: the legs share one endpoint and
	// differ only by port, and probing every one of them on a 30s status path
	// costs a cross-WAN round trip per replica.
	WANReachable bool
}

// Healthy reports whether every leg is up and the DR endpoint answered.
func (s *MultiStatus) Healthy() bool {
	if s == nil || len(s.Legs) == 0 {
		return false
	}
	for _, l := range s.Legs {
		if !l.Healthy() {
			return false
		}
	}
	return s.WANReachable
}

// Unhealthy returns the legs that are not fully up, in spec order.
func (s *MultiStatus) Unhealthy() []LegStatus {
	if s == nil {
		return nil
	}
	var out []LegStatus
	for _, l := range s.Legs {
		if !l.Healthy() {
			out = append(out, l)
		}
	}
	zap.L().Debug("MultiStatus.Unhealthy: counted", zap.String("resource", s.Resource), zap.Int("legs", len(s.Legs)),
		zap.Int("count", len(out)))
	return out
}

// StatusMulti reports the live health of every leg of a multi-replica WAN
// resource.
//
// A resource with N primary-site replicas has N independent tunnels, each a
// separately-named systemd instance (see LegID). Checking a single
// "haify-proxy@<resource>" unit — as a per-resource status query would — names a
// unit that exists on no node once N > 1, so a perfectly healthy WAN reads as
// completely down.
//
// Every host is queried in one Exec with one command: the per-leg unit names
// differ by host, so instead of asking each host about a specific unit, each
// host lists the instances it has for this resource and the result is matched
// up here.
func StatusMulti(ctx context.Context, deploy DeploymentClient, m MultiSpec) (*MultiStatus, error) {
	log := zap.L()
	log.Debug("StatusMulti: start", zap.String("resource", m.Resource), zap.Strings("primaries", m.PrimaryNodeAddrs),
		zap.String("dr", m.DRNodeAddr))
	if deploy == nil {
		log.Debug("StatusMulti: no deployment client", zap.String("resource", m.Resource))
		return nil, fmt.Errorf("wanproxy: deployment client is nil")
	}
	if err := m.Validate(); err != nil {
		log.Debug("StatusMulti: spec invalid", zap.String("resource", m.Resource), zap.Error(err))
		return nil, err
	}

	legs := m.Legs()
	hosts := append(append([]string{}, m.PrimaryNodeAddrs...), m.DRNodeAddr)

	// `systemctl is-active` takes one unit; listing is what lets a single
	// command serve hosts that each run different instances. --plain drops the
	// tree glyphs and --no-legend the trailing prose, leaving parseable rows.
	cmd := fmt.Sprintf(
		"systemctl list-units --all --plain --no-legend --no-pager '%s*' 2>/dev/null || true",
		UnitInstance(m.Resource))
	start := time.Now()
	res, err := deploy.Exec(ctx, hosts, cmd)
	log.Debug("StatusMulti: listed proxy units", zap.String("resource", m.Resource), zap.Strings("hosts", hosts),
		zap.Error(err), zap.Duration("took", time.Since(start)))
	if err != nil {
		return nil, fmt.Errorf("wanproxy: query proxy status: %w", err)
	}

	// active[host][unit] — absent means the host does not have that instance,
	// which is indistinguishable from it being stopped and means the same thing.
	active := make(map[string]map[string]bool, len(hosts))
	if res != nil {
		for host, h := range res.Hosts {
			if h == nil || !h.Success {
				log.Debug("StatusMulti: host did not answer, its units count as down", zap.String("host", host))
				continue
			}
			active[host] = parseActiveUnits(h.Output)
			log.Debug("StatusMulti: host units", zap.String("host", host), zap.Int("count", len(active[host])))
		}
	}

	st := &MultiStatus{Resource: m.Resource, DRHost: m.DRNodeAddr}
	for _, leg := range legs {
		unit := UnitInstance(leg.Resource) + ".service"
		legID := leg.Resource

		// A leg's name encodes the address the node had when its tunnel was
		// provisioned. Renumber the node — a DHCP lease that moves, a VM that
		// comes back on a different address — and the computed name stops
		// matching the instance that is actually running and replicating fine.
		//
		// A node runs at most one leg per resource, so when the computed name is
		// absent but the host has exactly one instance for this resource, that
		// instance IS this node's leg. Trusting it reports the tunnel's real
		// state instead of a phantom outage, and keeps the DR side keyed to the
		// same name. A host with no instance still reports down, under the name
		// that was expected, which is what an operator needs to go looking for.
		if !active[leg.PrimaryNodeAddr][unit] {
			if found := soleUnitFor(active[leg.PrimaryNodeAddr]); found != "" {
				log.Debug("StatusMulti: expected unit absent, adopting the host's only instance", zap.String("host", leg.PrimaryNodeAddr),
					zap.String("expected", unit), zap.String("unit", found))
				unit = found
				legID = strings.TrimSuffix(strings.TrimPrefix(found, "haify-proxy@"), ".service")
			} else {
				log.Debug("StatusMulti: expected unit absent and no sole instance", zap.String("host", leg.PrimaryNodeAddr),
					zap.String("expected", unit))
			}
		}
		log.Debug("StatusMulti: leg state", zap.String("leg", legID), zap.String("primary", leg.PrimaryNodeAddr),
			zap.Bool("primary_active", active[leg.PrimaryNodeAddr][unit]), zap.Bool("dr_active", active[m.DRNodeAddr][unit]))

		st.Legs = append(st.Legs, LegStatus{
			LegID:         legID,
			PrimaryHost:   leg.PrimaryNodeAddr,
			PrimaryActive: active[leg.PrimaryNodeAddr][unit],
			DRActive:      active[m.DRNodeAddr][unit],
		})
	}

	// One probe, from the first primary-site node that has a live dialer — that
	// node is known to be up, so a failure here is about the WAN path rather
	// than about the node being down.
	if probe := reachProbeSpec(m, legs, st); probe != nil {
		st.WANReachable = Reachable(ctx, deploy, *probe)
	} else {
		log.Debug("StatusMulti: no leg to probe from", zap.String("resource", m.Resource))
	}
	log.Debug("StatusMulti: done", zap.String("resource", m.Resource), zap.Int("legs", len(st.Legs)),
		zap.Bool("wan_reachable", st.WANReachable))
	return st, nil
}

// reachProbeSpec picks the leg to probe the DR endpoint from: the first whose
// primary dialer is running, falling back to the first leg. Probing from a node
// that is itself down reports "WAN unreachable" for what is really a dead node,
// which sends people looking at the wrong end of the link.
func reachProbeSpec(m MultiSpec, legs []ProxySpec, st *MultiStatus) *ProxySpec {
	if len(legs) == 0 {
		zap.L().Debug("reachProbeSpec: no legs", zap.String("resource", m.Resource))
		return nil
	}
	for i, l := range st.Legs {
		if l.PrimaryActive {
			zap.L().Debug("reachProbeSpec: probing from the first live dialer", zap.String("leg", legs[i].Resource),
				zap.String("host", legs[i].PrimaryNodeAddr))
			return &legs[i]
		}
	}
	zap.L().Debug("reachProbeSpec: no live dialer, probing from the first leg", zap.String("leg", legs[0].Resource),
		zap.String("host", legs[0].PrimaryNodeAddr))
	return &legs[0]
}

// soleUnitFor returns the host's only proxy instance, or "" when it has none or
// more than one. Ambiguity is left to the caller's computed name rather than
// guessed at: picking arbitrarily among several would report some other
// resource's leg as this one's.
func soleUnitFor(units map[string]bool) string {
	if len(units) != 1 {
		return ""
	}
	for u := range units {
		return u
	}
	return ""
}

// parseActiveUnits reads `systemctl list-units --plain --no-legend` output into
// unit -> active. Rows are "UNIT LOAD ACTIVE SUB DESCRIPTION...".
func parseActiveUnits(out string) map[string]bool {
	units := map[string]bool{}
	skipped := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 {
			skipped++
			continue
		}
		units[fields[0]] = fields[2] == "active"
	}
	zap.L().Debug("parseActiveUnits: parsed", zap.Int("count", len(units)), zap.Int("skipped", skipped))
	return units
}
