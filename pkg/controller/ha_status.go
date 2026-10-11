package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/deployment"
)

// HaPromoterStatus is one resource's drbd-reactor promoter: where its chain
// runs, what each unit of the chain is and how it is doing, which other
// nodes could take the chain over, and what outside the chain uses its
// mounts.
type HaPromoterStatus struct {
	DRBDResource string
	PrimaryOn    string
	Status       string
	Target       HaServiceStatus
	Deps         []HaServiceStatus
	// ActiveNode is the Haify node running the chain, "" when none is.
	ActiveNode string
	Standbys   []HaStandby
	Outside    []HaOutsideService
	// Login sessions, and their processes, using the chain's mounts.
	OutsideSessions         int
	OutsideSessionProcesses int
}

// HaServiceStatus is one unit of an HA chain.
type HaServiceStatus struct {
	Name   string
	Status string
	// Kind and Label say what the unit is for; see classifyHaUnit.
	Kind        string
	Label       string
	Description string
	Since       string // RFC 3339; "" while the unit is not active
	SubState    string
	Restarts    int
	MainPID     int
}

// HaStandby is a node holding an enabled promoter that does not run the
// chain, and whether it could take it over.
type HaStandby struct {
	Node      string
	Ready     bool
	Reasons   []string
	DiskState string
}

// HaOutsideService is a unit outside the chain using the chain's mounts.
type HaOutsideService struct {
	Unit        string
	Description string
	Processes   int
	MountPoints []string
}

// haProbeTTL is how long a node's probe answers GetHaStatus. The web UI
// polls the status; drbd-reactorctl status alone takes a second or two on a
// small node, so every poll probing every node would keep them busy.
const haProbeTTL = 5 * time.Second

// haProbeTimeout bounds one node's probe.
const haProbeTimeout = 20 * time.Second

// haProbeCache keeps recent node probes. Its zero value is ready to use.
type haProbeCache struct {
	mu      sync.Mutex
	entries map[string]haProbeEntry
}

type haProbeEntry struct {
	haProbeResult
	at time.Time
}

func (c *haProbeCache) get(host string, now time.Time) (haProbeResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[host]
	if !ok || now.Sub(e.at) > haProbeTTL {
		return haProbeResult{}, false
	}
	return e.haProbeResult, true
}

func (c *haProbeCache) put(host string, r haProbeResult, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]haProbeEntry{}
	}
	c.entries[host] = haProbeEntry{haProbeResult: r, at: at}
}

// probeHaNodes probes hosts, one SSH exec each and all at once, answering
// from the cache for hosts probed within haProbeTTL.
func (rm *ResourceManager) probeHaNodes(ctx context.Context, hosts []string) map[string]haProbeResult {
	out := make(map[string]haProbeResult, len(hosts))
	var stale []string
	now := time.Now()
	for _, h := range hosts {
		if r, ok := rm.haProbes.get(h, now); ok {
			out[h] = r
		} else {
			stale = append(stale, h)
		}
	}
	if len(stale) == 0 {
		return out
	}
	cmd := haProbeCommand()
	results := make([]haProbeResult, len(stale))
	var wg sync.WaitGroup
	for i, h := range stale {
		wg.Add(1)
		go func(i int, h string) {
			defer wg.Done()
			results[i] = rm.probeHaNode(ctx, h, cmd)
		}(i, h)
	}
	wg.Wait()
	done := time.Now()
	for i, h := range stale {
		out[h] = results[i]
		rm.haProbes.put(h, results[i], done)
	}
	return out
}

func (rm *ResourceManager) probeHaNode(ctx context.Context, host, cmd string) haProbeResult {
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, []string{host}, cmd, deployment.WithExecTimeout(haProbeTimeout))
	if err != nil {
		rm.controller.logger.Warn("HA status probe failed",
			zap.String("node", rm.nodeLabel(host)), zap.Error(err))
		return haProbeResult{err: fmt.Errorf("probe %s: %w", rm.nodeLabel(host), err)}
	}
	hr := res.Hosts[host]
	if hr == nil || !hr.Success {
		rm.controller.logger.Warn("HA status probe failed",
			zap.String("node", rm.nodeLabel(host)), zap.String("reason", hostFailure(hr)))
		return haProbeResult{err: fmt.Errorf("probe %s: %s", rm.nodeLabel(host), hostFailure(hr))}
	}
	facts, err := parseHaNodeProbe(hr.Output)
	if err != nil {
		rm.controller.logger.Warn("Unreadable HA status probe",
			zap.String("node", rm.nodeLabel(host)), zap.Error(err))
		return haProbeResult{err: fmt.Errorf("probe %s: %w", rm.nodeLabel(host), err)}
	}
	rm.controller.logger.Debug("HA status probe",
		zap.String("node", rm.nodeLabel(host)), zap.Duration("took", time.Since(start)))
	return haProbeResult{facts: facts}
}

// GetHaStatus reports the promoter of one HA resource, or of every one when
// resource is empty. Each node that may run a resource's promoter is probed
// once (haProbeScript), concurrently, and a node shared by several resources
// once for all of them. A named resource without an `ha create` config is
// still reported when it carries a gateway or an app, whose promoters are the
// same kind of chain.
func (rm *ResourceManager) GetHaStatus(ctx context.Context, resource string) ([]*HaPromoterStatus, error) {
	if rm.controller.db == nil {
		return nil, fmt.Errorf("database not available")
	}
	names, err := rm.haStatusResources(ctx, resource)
	if err != nil {
		return nil, err
	}

	type target struct {
		name     string
		hosts    []string
		failover map[string]bool
	}
	var targets []target
	var all []string
	seen := map[string]bool{}
	for _, name := range names {
		dbRes, err := rm.controller.db.GetResource(ctx, name)
		if err != nil || dbRes == nil {
			continue
		}
		// Only the primary site's diskful replicas hold a promoter
		// (promoter_placement.go); tiebreakers, diskless clients and a WAN
		// resource's DR node are not probed.
		hosts, ferr := rm.failoverHosts(ctx, name)
		if ferr != nil || len(hosts) == 0 {
			continue
		}
		t := target{name: name, hosts: hosts, failover: map[string]bool{}}
		for _, h := range hosts {
			t.failover[h] = true
			if !seen[h] {
				seen[h] = true
				all = append(all, h)
			}
		}
		targets = append(targets, t)
	}
	if len(targets) == 0 {
		return nil, nil
	}

	probes := rm.probeHaNodes(ctx, all)
	var result []*HaPromoterStatus
	for _, t := range targets {
		if ps := buildHaPromoterStatus(t.name, t.hosts, t.failover, probes, rm.nodeLabel); ps != nil {
			result = append(result, ps)
		}
	}
	return result, nil
}

// haStatusResources names the resources GetHaStatus reports on.
func (rm *ResourceManager) haStatusResources(ctx context.Context, resource string) ([]string, error) {
	db := rm.controller.db
	if resource == "" {
		configs, err := db.ListHaConfigs(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list HA configs: %w", err)
		}
		names := make([]string, 0, len(configs))
		for _, c := range configs {
			names = append(names, c.Resource)
		}
		return names, nil
	}
	cfg, err := db.GetHaConfig(ctx, resource)
	if err == nil && cfg != nil {
		return []string{resource}, nil
	}
	if gw, gerr := db.GetGatewayByResource(ctx, resource); gerr == nil && gw != nil {
		return []string{resource}, nil
	}
	if rm.hasApp(ctx, resource) {
		return []string{resource}, nil
	}
	if err == nil {
		err = fmt.Errorf("ha config not found")
	}
	return nil, fmt.Errorf("failed to get HA config: %w", err)
}

// hasApp reports whether an app runs on resource.
func (rm *ResourceManager) hasApp(ctx context.Context, resource string) bool {
	apps, err := rm.controller.db.ListApps(ctx)
	if err != nil {
		return false
	}
	for _, a := range apps {
		if strings.EqualFold(a.Resource, resource) {
			return true
		}
	}
	return false
}
