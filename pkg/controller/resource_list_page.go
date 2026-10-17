package controller

import (
	"context"
	"sort"
	"strings"

	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// The resource list as a page: each resource's health from the last poll and
// what uses it, the counts the filter chips show, then filter, sort and page.
// A cluster with thousands of resources is served a page at a time, and
// nothing here asks a node anything.

// attentionRank orders states for the default sort: the worst first.
var attentionRank = map[string]int{stateNoQuorum: 0, stateDegraded: 1, stateResyncing: 2, stateUnknown: 3, stateHealthy: 4}

// resourceUsers says, per resource, which system uses it, from the records
// of gateways, apps and HA services.
func (c *Controller) resourceUsers(ctx context.Context) map[string]string {
	users := map[string]string{}
	if c.db == nil {
		return users
	}
	if has, err := c.db.ListHaConfigs(ctx); err == nil {
		for _, h := range has {
			users[h.Resource] = "ha"
		}
	}
	if apps, err := c.db.ListApps(ctx); err == nil {
		for _, a := range apps {
			users[a.Resource] = "app"
		}
	}
	if gws, err := c.db.ListGateways(ctx); err == nil {
		for _, g := range gws {
			users[g.Resource] = "gateway"
		}
	}
	return users
}

// usedBy names the system a resource belongs to: the one that created it,
// when its labels say, otherwise what runs on it.
func usedBy(r *haifypb.ResourceInfo, users map[string]string) string {
	l := r.Labels
	switch {
	case l[csiManagedByLabel] == "csi":
		return "pvc"
	case l[pveManagedByLabel] == "pve", l[pveManagedByLabel] == "" && pveDefaultNameRe.MatchString(r.Name):
		return "proxmox"
	case l[cinderManagedByLabel] == "cinder":
		return "openstack"
	case l[xcpManagedByLabel] == "xcp-ng":
		return "xcp-ng"
	}
	if u := users[r.Name]; u != "" {
		return u
	}
	return "none"
}

// decorateResources fills in health, owner and used_by.
func (c *Controller) decorateResources(ctx context.Context, list []*haifypb.ResourceInfo) {
	users := c.resourceUsers(ctx)
	var snap viewSnapshot
	if c.view != nil {
		snap = c.view.snapshot()
	}
	for _, r := range list {
		r.Owner = resourceOwner(r.Labels)
		r.UsedBy = usedBy(r, users)
		rv, ok := snap.Resources[r.Name]
		if !ok {
			continue
		}
		h := &haifypb.ResourceHealth{State: rv.State, Summary: rv.Summary, OutOfSyncBytes: rv.OutOfSyncBytes,
			ObservedUnix: snap.At.Unix()}
		if rv.WriteBps >= 0 {
			w := rv.WriteBps
			h.WriteBytesPerSec = &w
		}
		for _, rep := range rv.Replicas {
			h.Replicas = append(h.Replicas, &haifypb.ReplicaHealth{Node: rep.Node, State: rep.State, Role: rep.Role})
		}
		r.Health = h
	}
}

func healthState(r *haifypb.ResourceInfo) string {
	if r.Health == nil {
		return stateUnknown
	}
	return r.Health.State
}

// stateCounts is how many resources are in each state, keyed like the
// request's state filter.
func stateCounts(list []*haifypb.ResourceInfo) map[string]int32 {
	counts := map[string]int32{"all": int32(len(list))}
	for _, r := range list {
		st := healthState(r)
		counts[st]++
		if st != stateHealthy {
			counts["attention"]++
		}
		if r.WanMode {
			counts["offsite"]++
		}
	}
	return counts
}

// matchResource applies every filter of the request to one resource.
func matchResource(r *haifypb.ResourceInfo, req *haifypb.ListResourcesRequest, pools map[string]bool) bool {
	switch st := req.GetState(); st {
	case "":
	case "attention":
		if healthState(r) == stateHealthy {
			return false
		}
	case "offsite":
		if !r.WanMode {
			return false
		}
	default:
		if healthState(r) != st {
			return false
		}
	}
	if u := req.GetUsedBy(); u != "" && r.UsedBy != u {
		return false
	}
	if n := req.GetNode(); n != "" && !containsString(r.Nodes, n) && !containsString(r.DisklessNodes, n) &&
		!containsString(r.DisklessClients, n) {
		return false
	}
	if p := req.GetPool(); p != "" && !pools[r.Name] {
		return false
	}
	if q := strings.ToLower(strings.TrimSpace(req.GetQuery())); q != "" {
		hay := strings.ToLower(r.Name + " " + r.Owner)
		for _, v := range r.Labels {
			hay += " " + strings.ToLower(v)
		}
		if !strings.Contains(hay, q) {
			return false
		}
	}
	return true
}

func resourceSize(r *haifypb.ResourceInfo) uint64 {
	var total uint64
	for _, v := range r.Volumes {
		total += v.SizeBytes
	}
	return total
}

func resourceWrites(r *haifypb.ResourceInfo) float64 {
	if r.Health == nil || r.Health.WriteBytesPerSec == nil {
		return -1
	}
	return *r.Health.WriteBytesPerSec
}

// sortResources orders the list as the request asks; ties go by name.
func sortResources(list []*haifypb.ResourceInfo, by string) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		switch by {
		case "name":
		case "size":
			if sa, sb := resourceSize(a), resourceSize(b); sa != sb {
				return sa > sb
			}
		case "writes":
			if wa, wb := resourceWrites(a), resourceWrites(b); wa != wb {
				return wa > wb
			}
		default:
			if ra, rb := attentionRank[healthState(a)], attentionRank[healthState(b)]; ra != rb {
				return ra < rb
			}
		}
		return a.Name < b.Name
	})
}

// pageResources decorates, counts, filters, sorts and pages the list.
func (c *Controller) pageResources(ctx context.Context, req *haifypb.ListResourcesRequest, list []*haifypb.ResourceInfo) *haifypb.ListResourcesResponse {
	c.decorateResources(ctx, list)
	counts := stateCounts(list)
	pools := map[string]bool{}
	if p := req.GetPool(); p != "" {
		for _, r := range list {
			for _, v := range r.Volumes {
				if v.Pool == p {
					pools[r.Name] = true
				}
			}
		}
	}
	matched := list[:0:0]
	for _, r := range list {
		if matchResource(r, req, pools) {
			matched = append(matched, r)
		}
	}
	sortResources(matched, req.GetSort())
	total := len(matched)
	if off := int(req.GetOffset()); off > 0 {
		matched = matched[min(off, len(matched)):]
	}
	if lim := int(req.GetLimit()); lim > 0 && lim < len(matched) {
		matched = matched[:lim]
	}
	c.traceLogger().Debug("Controller.pageResources: paged", zap.Int("all", len(list)), zap.Int("matched", total),
		zap.Int("returned", len(matched)), zap.String("state", req.GetState()), zap.String("sort", req.GetSort()))
	return &haifypb.ListResourcesResponse{Success: true, Message: "Resources listed successfully",
		Resources: matched, Total: int32(total), Counts: counts}
}
