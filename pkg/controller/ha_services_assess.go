package controller

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/haify-project/haify/pkg/deployment"
)

// Turning node probes into an HA chain's status: what each unit is, which
// standby could take the chain over and why not, and what outside the chain
// uses its mounts. Everything here is pure, over parsed probe output.

// haServiceStatus describes one unit of a chain from the node running it.
func haServiceStatus(name, status string, facts *haNodeFacts) HaServiceStatus {
	u := facts.unit(name)
	kind, label := classifyHaUnit(name, u)
	s := HaServiceStatus{Name: name, Status: status, Kind: kind, Label: label}
	if u == nil {
		return s
	}
	s.Description = unitOwnDescription(u)
	s.SubState = u.SubState
	s.Restarts = u.Restarts
	s.MainPID = u.MainPID
	switch u.ActiveState {
	case "active", "reloading", "deactivating":
		s.Since = u.ActiveEnter
	}
	return s
}

// promoterUnits is a promoter's units in start order, target last.
func promoterUnits(p *deployment.ReactorPromoterStatus) []string {
	units := make([]string, 0, len(p.Dependencies)+1)
	for _, d := range p.Dependencies {
		units = append(units, d.Name)
	}
	if p.Target.Name != "" {
		units = append(units, p.Target.Name)
	}
	return units
}

// promoterDisabled reports whether a node holds only the .disabled copy of
// the promoter, which drbd-reactor ignores.
func promoterDisabled(p *deployment.ReactorPromoterStatus) bool {
	return strings.HasSuffix(p.Path, ".disabled")
}

// haStandbyReasons says what would stop node f from taking resource's chain
// over, and gives its local disk state. No reasons means it could.
//
// Units drbd-reactor generates per node (the .mount units) are not checked;
// for ocf.rs@ units the OCF agent they run is; for every other unit, template
// instances included, systemd's LoadState says whether the unit (or its
// template) is installed there.
func haStandbyReasons(resource string, p *deployment.ReactorPromoterStatus, f *haNodeFacts) (reasons []string, disk string) {
	if f.ReactorState != "active" {
		r := "drbd-reactor not running"
		if f.ReactorState != "" && f.ReactorState != "inactive" {
			r += " (" + f.ReactorState + ")"
		}
		reasons = append(reasons, r)
	}
	for _, name := range promoterUnits(p) {
		if strings.HasSuffix(name, ".mount") {
			continue
		}
		u := f.unit(name)
		if u == nil {
			continue
		}
		if r := unitLoadReason(name, u); r != "" {
			reasons = append(reasons, r)
			continue
		}
		if agent := u.Env["AGENT"]; agent != "" {
			if installed, known := f.Agents[agent]; known && !installed {
				reasons = append(reasons, fmt.Sprintf("OCF agent %s not installed", ocfAgentName(agent)))
			}
		}
	}
	st, ok := f.Drbd[resource]
	switch {
	case !ok:
		reasons = append(reasons, "resource not up")
	case st.Disk == "":
		reasons = append(reasons, "disk state unknown")
	case st.Disk != "UpToDate":
		reasons = append(reasons, "disk "+st.Disk)
	}
	return reasons, st.Disk
}

// unitLoadReason says why a unit could not be started on a node, from its
// load state, or "" when it can be loaded.
func unitLoadReason(name string, u *haUnitFacts) string {
	switch {
	case u.LoadState == "masked" || strings.HasPrefix(u.UnitFileState, "masked"):
		return name + " masked"
	case u.LoadState == "not-found":
		prefix, instance, suffix := splitUnitName(name)
		if instance != "" {
			return fmt.Sprintf("%s@.%s not installed", prefix, suffix)
		}
		return name + " not installed"
	case u.LoadState == "error" || u.LoadState == "bad-setting":
		return fmt.Sprintf("%s cannot be loaded (%s)", name, u.LoadState)
	}
	return ""
}

// ocfAgentName is "heartbeat/nfsserver" for /usr/lib/ocf/resource.d/heartbeat/nfsserver.
func ocfAgentName(agentPath string) string {
	return path.Base(path.Dir(agentPath)) + "/" + path.Base(agentPath)
}

// haChainMounts are the mount points a chain's units mount, never "/".
func haChainMounts(services []HaServiceStatus) []string {
	var out []string
	for _, s := range services {
		if s.Kind == haKindMount && strings.HasPrefix(s.Label, "/") && s.Label != "/" {
			out = append(out, path.Clean(s.Label))
		}
	}
	return out
}

// haOutsideUsers lists the units outside chain whose processes use one of
// mounts on node f, and counts the login sessions doing so. Kernel threads
// never show up: they live in the root cgroup, which the probe drops.
func haOutsideUsers(chain map[string]bool, mounts []string, f *haNodeFacts) (outside []HaOutsideService, sessions, sessionProcs int) {
	if f == nil || len(mounts) == 0 {
		return nil, 0, 0
	}
	want := map[string]bool{}
	for _, m := range mounts {
		want[m] = true
	}
	type agg struct {
		procs  int
		mounts map[string]bool
	}
	byUnit := map[string]*agg{}
	sessionSet := map[string]bool{}
	for _, g := range f.Procs {
		var hit []string
		for _, m := range g.Mounts {
			if want[m] {
				hit = append(hit, m)
			}
		}
		if len(hit) == 0 {
			continue
		}
		unit := cgroupUnit(g.Cgroup)
		if unit == "" || chain[unit] {
			continue
		}
		if isSessionUnit(unit) {
			sessionSet[unit] = true
			sessionProcs += g.Count
			continue
		}
		a := byUnit[unit]
		if a == nil {
			a = &agg{mounts: map[string]bool{}}
			byUnit[unit] = a
		}
		a.procs += g.Count
		for _, m := range hit {
			a.mounts[m] = true
		}
	}
	for unit, a := range byUnit {
		desc := f.Outside[unit]
		if desc == "" {
			desc = unitOwnDescription(f.unit(unit))
		}
		o := HaOutsideService{Unit: unit, Description: desc, Processes: a.procs}
		for m := range a.mounts {
			o.MountPoints = append(o.MountPoints, m)
		}
		sort.Strings(o.MountPoints)
		outside = append(outside, o)
	}
	sort.Slice(outside, func(i, j int) bool { return outside[i].Unit < outside[j].Unit })
	return outside, len(sessionSet), sessionProcs
}

// haProbeResult is one node's probe, or why there is none.
type haProbeResult struct {
	facts *haNodeFacts
	err   error
}

// buildHaPromoterStatus assembles resource's status from the probes of its
// nodes. hosts is the resource's nodes in order; failover marks those that
// should be able to take over, so one that did not answer is still listed;
// label names a host for people. It returns nil when no node holds a
// promoter for resource.
func buildHaPromoterStatus(resource string, hosts []string, failover map[string]bool,
	probes map[string]haProbeResult, label func(string) string) *HaPromoterStatus {
	type holder struct {
		host  string
		entry *deployment.ReactorPromoterStatus
		facts *haNodeFacts
	}
	var holders []holder
	for _, h := range hosts {
		pr := probes[h]
		if pr.err != nil || pr.facts == nil || pr.facts.Reactor == nil {
			continue
		}
		for i := range pr.facts.Reactor.Promoter {
			if p := &pr.facts.Reactor.Promoter[i]; p.DRBDResource == resource {
				holders = append(holders, holder{host: h, entry: p, facts: pr.facts})
				break
			}
		}
	}
	if len(holders) == 0 {
		return nil
	}

	// The node running the chain: the one whose drbd-reactor says so, else
	// the one holding the resource Primary (a chain that failed to start
	// there still holds its mounts).
	running := -1
	for i, h := range holders {
		if h.entry.Status == "active" {
			running = i
			break
		}
	}
	if running < 0 {
		for i, h := range holders {
			if h.facts.Drbd[resource].Role == "Primary" {
				running = i
				break
			}
		}
	}
	view := 0
	if running >= 0 {
		view = running
	}
	v := holders[view]
	ps := &HaPromoterStatus{
		DRBDResource: resource,
		PrimaryOn:    v.entry.PrimaryOn,
		Status:       v.entry.Status,
		Target:       haServiceStatus(v.entry.Target.Name, v.entry.Target.Status, v.facts),
	}
	chain := map[string]bool{}
	for _, d := range v.entry.Dependencies {
		ps.Deps = append(ps.Deps, haServiceStatus(d.Name, d.Status, v.facts))
		chain[d.Name] = true
	}
	chain[v.entry.Target.Name] = true

	if running >= 0 {
		ps.ActiveNode = label(v.host)
		ps.Outside, ps.OutsideSessions, ps.OutsideSessionProcesses =
			haOutsideUsers(chain, haChainMounts(ps.Deps), v.facts)
	}

	answered := map[string]bool{}
	for i, h := range holders {
		answered[h.host] = true
		if i == running || promoterDisabled(h.entry) {
			continue
		}
		reasons, disk := haStandbyReasons(resource, h.entry, h.facts)
		ps.Standbys = append(ps.Standbys, HaStandby{
			Node: label(h.host), Ready: len(reasons) == 0, Reasons: reasons, DiskState: disk,
		})
	}
	for _, h := range hosts {
		if !failover[h] || answered[h] || (running >= 0 && h == holders[running].host) {
			continue
		}
		pr := probes[h]
		switch {
		case pr.err != nil || pr.facts == nil:
			ps.Standbys = append(ps.Standbys, HaStandby{Node: label(h), Reasons: []string{"node did not answer"}})
		case pr.facts.Reactor == nil:
			r := "drbd-reactor status unavailable"
			if pr.facts.ReactorState != "active" {
				r = "drbd-reactor not running"
			}
			ps.Standbys = append(ps.Standbys, HaStandby{Node: label(h), Reasons: []string{r},
				DiskState: pr.facts.Drbd[resource].Disk})
		default:
			ps.Standbys = append(ps.Standbys, HaStandby{Node: label(h), Reasons: []string{"no promoter config"},
				DiskState: pr.facts.Drbd[resource].Disk})
		}
	}
	return ps
}
