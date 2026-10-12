package wanproxy

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// StaleLeg is a proxy instance found on a node that the current spec does not
// account for — typically a leg left behind after the node was renumbered, or
// after a replica was removed from the resource.
type StaleLeg struct {
	Host  string
	LegID string
}

// FindStaleLegs reports proxy instances for this resource that the spec does
// not expect: on a primary-site node, anything that is not that node's own leg;
// on the DR node, anything that is not one of the legs.
//
// Matching is deliberately narrow. A resource's instances are exactly
// "<resource>" and "<resource>_<key>", so a resource named "foo" must not sweep
// up "foobar"'s instances. It is still possible to construct a collision by
// naming one resource "<other>_<something>"; such a name is rejected at
// creation (see ValidateResourceNameForWAN) precisely so this stays safe.
func FindStaleLegs(ctx context.Context, deploy DeploymentClient, m MultiSpec) ([]StaleLeg, error) {
	log := zap.L()
	log.Debug("FindStaleLegs: start", zap.String("resource", m.Resource), zap.Strings("primaries", m.PrimaryNodeAddrs),
		zap.String("dr", m.DRNodeAddr))
	if deploy == nil {
		log.Debug("FindStaleLegs: no deployment client", zap.String("resource", m.Resource))
		return nil, fmt.Errorf("wanproxy: deployment client is nil")
	}
	if err := m.Validate(); err != nil {
		log.Debug("FindStaleLegs: spec invalid", zap.String("resource", m.Resource), zap.Error(err))
		return nil, err
	}

	legs := m.Legs()
	hosts := append(append([]string{}, m.PrimaryNodeAddrs...), m.DRNodeAddr)

	start := time.Now()
	res, err := deploy.Exec(ctx, hosts, fmt.Sprintf(
		"systemctl list-units --all --plain --no-legend --no-pager '%s*' 2>/dev/null || true",
		UnitInstance(m.Resource)))
	log.Debug("FindStaleLegs: listed proxy units", zap.String("resource", m.Resource), zap.Strings("hosts", hosts),
		zap.Error(err), zap.Duration("took", time.Since(start)))
	if err != nil {
		return nil, fmt.Errorf("wanproxy: list proxy instances: %w", err)
	}

	// expected[host] is the set of leg ids that host is supposed to run.
	expected := make(map[string]map[string]bool, len(hosts))
	for _, h := range hosts {
		expected[h] = map[string]bool{}
	}
	for _, leg := range legs {
		expected[leg.PrimaryNodeAddr][leg.Resource] = true
		expected[m.DRNodeAddr][leg.Resource] = true
	}

	var stale []StaleLeg
	if res != nil {
		for _, host := range hosts {
			h := res.Hosts[host]
			if h == nil || !h.Success {
				// A node that cannot be reached is not evidence of a stale leg.
				log.Debug("FindStaleLegs: host unreachable, not judged", zap.String("host", host))
				continue
			}
			for unit := range parseActiveUnits(h.Output) {
				legID := strings.TrimSuffix(strings.TrimPrefix(unit, "haify-proxy@"), ".service")
				if !belongsToResource(legID, m.Resource) || expected[host][legID] {
					log.Debug("FindStaleLegs: unit expected or not this resource's", zap.String("host", host),
						zap.String("unit", unit), zap.Bool("expected", expected[host][legID]))
					continue
				}
				log.Debug("FindStaleLegs: stale leg found", zap.String("host", host), zap.String("leg", legID))
				stale = append(stale, StaleLeg{Host: host, LegID: legID})
			}
		}
	} else {
		log.Debug("FindStaleLegs: no result from the listing", zap.String("resource", m.Resource))
	}
	sort.Slice(stale, func(i, j int) bool {
		if stale[i].Host != stale[j].Host {
			return stale[i].Host < stale[j].Host
		}
		return stale[i].LegID < stale[j].LegID
	})
	log.Debug("FindStaleLegs: done", zap.String("resource", m.Resource), zap.Int("count", len(stale)))
	return stale, nil
}

// belongsToResource reports whether a leg id is one of this resource's, i.e.
// exactly the resource (single-replica naming) or "<resource>_<key>".
func belongsToResource(legID, resource string) bool {
	return legID == resource || strings.HasPrefix(legID, resource+"_")
}

// RemoveStaleLegs stops, disables and removes the config of each stale leg.
//
// It is best-effort per leg so one unreachable node cannot strand the rest, and
// it only ever touches instances FindStaleLegs identified — a leg the spec does
// expect is never removed, so calling this on a healthy resource is a no-op.
func RemoveStaleLegs(ctx context.Context, deploy DeploymentClient, stale []StaleLeg) error {
	log := zap.L()
	log.Debug("RemoveStaleLegs: start", zap.Int("count", len(stale)))
	if deploy == nil {
		log.Debug("RemoveStaleLegs: no deployment client")
		return fmt.Errorf("wanproxy: deployment client is nil")
	}
	var firstErr error
	for _, s := range stale {
		log.Debug("RemoveStaleLegs: removing leg", zap.String("host", s.Host), zap.String("leg", s.LegID))
		if err := run(ctx, deploy, []string{s.Host},
			fmt.Sprintf("sudo systemctl disable --now %s 2>/dev/null || true", UnitInstance(s.LegID)),
			"disable stale proxy leg"); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := run(ctx, deploy, []string{s.Host},
			fmt.Sprintf("sudo rm -f %s %s", NodeConfigPath(s.LegID), NodeMetricsPath(s.LegID)),
			"remove stale proxy config"); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	log.Debug("RemoveStaleLegs: done", zap.Int("count", len(stale)), zap.Error(firstErr))
	return firstErr
}
