package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
)

// Quotas ([quota]).
//
// A thin pool admits volumes as long as it has room for the data already
// written, which is the point of thin provisioning and also how a pool ends up
// promising ten times what it holds and filling on a Tuesday. max_overcommit_ratio
// caps what may be promised: a replica, a new volume or a resize that would
// take a node's thin pool past that many times its real size is refused there,
// and auto-placement passes such a node over. Snapshots do not count; they
// share their origin's blocks until it is rewritten.
//
// Projects are resources sharing a label (project_label, "project" by
// default). A project's quota caps the total size of its resources — counted
// once, as the size a user asked for, not once per replica — and their
// number.

func (rm *ResourceManager) maxOvercommit() float64 {
	if rm.controller == nil || rm.controller.config == nil {
		return 0
	}
	return rm.controller.config.Quota.MaxOvercommitRatio
}

// overcommitted reports whether adding sizeGB to a thin pool takes it past
// its ratio.
func (c poolCapacity) overcommitted(sizeGB uint64) bool {
	if c.maxOvercommit <= 0 || c.sizeBytes == 0 {
		return false
	}
	return float64(c.virtualBytes+sizeGB<<30) > c.maxOvercommit*float64(c.sizeBytes)
}

// assertOvercommit refuses adding addGB of thin volume to pool on nodes when
// a node's pool would pass the ratio.
func (rm *ResourceManager) assertOvercommit(ctx context.Context, pool string, nodes []string, addGB uint64) error {
	log := rm.controller.traceLogger()
	ratio := rm.maxOvercommit()
	log.Debug("assertOvercommit: checking", zap.String("pool", pool), zap.Strings("nodes", nodes),
		zap.Uint64("add_gb", addGB), zap.Float64("ratio", ratio))
	if ratio <= 0 || addGB == 0 || rm.controller.storage == nil {
		log.Debug("assertOvercommit: nothing to check", zap.Bool("has_storage", rm.controller.storage != nil))
		return nil
	}
	start := time.Now()
	pools, err := rm.controller.storage.ListPools(ctx)
	log.Debug("assertOvercommit: listed pools", zap.Int("count", len(pools)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil // capacity unknown is not capacity exhausted; placement says the same
	}
	pool = normalizeManagedName(pool)
	for _, n := range nodes {
		addr := rm.controller.ResolveHost(n)
		for _, p := range pools {
			if normalizeManagedName(p.Name) != pool || (p.Node != n && p.Node != addr && rm.controller.ResolveHost(p.Node) != addr) {
				continue
			}
			c := poolPlacementCapacity(p, false)
			c.maxOvercommit = ratio
			over := c.thin && c.overcommitted(addGB)
			log.Debug("assertOvercommit: pool on node", zap.String("node", n), zap.String("pool", p.Name),
				zap.Bool("thin", c.thin), zap.Uint64("virtual_bytes", c.virtualBytes), zap.Uint64("size_bytes", c.sizeBytes),
				zap.Bool("overcommitted", over))
			if over {
				return fmt.Errorf("%d GB more would promise %.1f times the size of thin pool %s on %s, past "+
					"[quota] max_overcommit_ratio %.1f; grow the pool or place it elsewhere",
					addGB, float64(c.virtualBytes+addGB<<30)/float64(c.sizeBytes), p.Name, n, ratio)
			}
		}
	}
	return nil
}

// assertProjectQuota refuses growing the project labels name by addGB (and,
// for a new resource, by one resource) past its quota.
func (rm *ResourceManager) assertProjectQuota(ctx context.Context, labels map[string]string, addGB uint64, newResource bool) error {
	log := rm.controller.traceLogger()
	if rm.controller.config == nil || rm.controller.db == nil {
		log.Debug("assertProjectQuota: no config or database, not checking")
		return nil
	}
	q := rm.controller.config.Quota
	project := labels[q.ProjectLabel]
	quota := q.Project(project)
	log.Debug("assertProjectQuota: checking", zap.String("project", project), zap.Uint64("add_gb", addGB),
		zap.Bool("new_resource", newResource), zap.Bool("has_quota", quota != nil))
	if project == "" || quota == nil {
		return nil
	}
	start := time.Now()
	resources, err := rm.controller.db.ListResources(ctx)
	log.Debug("assertProjectQuota: listed resources", zap.Int("count", len(resources)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return err
	}
	var used uint64
	count := 0
	for _, r := range resources {
		if r.Labels[q.ProjectLabel] != project {
			continue
		}
		count++
		gb := rm.resourceSizeGB(ctx, r)
		log.Debug("assertProjectQuota: project resource", zap.String("project", project),
			zap.String("resource", r.Name), zap.Uint64("size_gb", gb))
		used += gb
	}
	log.Debug("assertProjectQuota: project usage", zap.String("project", project), zap.Int("count", count),
		zap.Uint64("used_gb", used), zap.Int("max_resources", quota.MaxResources), zap.Uint64("max_gb", quota.MaxGB))
	if newResource && quota.MaxResources > 0 && count+1 > quota.MaxResources {
		log.Debug("assertProjectQuota: resource count quota reached", zap.String("project", project))
		return fmt.Errorf("project %s has %d of its %d resources; its quota allows no more", project, count, quota.MaxResources)
	}
	if quota.MaxGB > 0 && used+addGB > quota.MaxGB {
		log.Debug("assertProjectQuota: size quota exceeded", zap.String("project", project))
		return fmt.Errorf("project %s uses %d GB of its %d GB quota; %d GB more does not fit", project, used, quota.MaxGB, addGB)
	}
	return nil
}

func (rm *ResourceManager) resourceSizeGB(ctx context.Context, r *database.Resource) uint64 {
	start := time.Now()
	vols, err := rm.controller.db.ListVolumes(ctx, r.Name)
	rm.controller.traceLogger().Debug("resourceSizeGB: listed volumes", zap.String("resource", r.Name),
		zap.Int("count", len(vols)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return 0
	}
	var gb uint64
	for _, v := range vols {
		gb += uint64(max(v.SizeGB, 0))
	}
	return gb
}

// assertQuotaForResource checks both for growing resource by addGB on nodes.
func (rm *ResourceManager) assertQuotaForResource(ctx context.Context, resource, pool string, nodes []string, addGB uint64) error {
	log := rm.controller.traceLogger()
	log.Debug("assertQuotaForResource: checking", zap.String("resource", resource), zap.String("pool", pool),
		zap.Strings("nodes", nodes), zap.Uint64("add_gb", addGB))
	if rm.controller.db != nil {
		start := time.Now()
		r, err := rm.controller.db.GetResource(ctx, resource)
		log.Debug("assertQuotaForResource: read resource", zap.String("resource", resource),
			zap.Bool("found", err == nil && r != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err == nil && r != nil {
			if err := rm.assertProjectQuota(ctx, r.Labels, addGB, false); err != nil {
				log.Debug("assertQuotaForResource: project quota refused", zap.String("resource", resource), zap.Error(err))
				return err
			}
		}
	}
	return rm.assertOvercommit(ctx, pool, nodes, addGB)
}

// assertCreateQuota checks a new resource against its project's quota and
// every target node's thin pools.
func (rm *ResourceManager) assertCreateQuota(ctx context.Context, volumes []resolvedVolume, nodes []string, labels map[string]string) error {
	log := rm.controller.traceLogger()
	var total uint64
	perPool := map[string]uint64{}
	for _, v := range volumes {
		total += uint64(v.sizeGB)
		perPool[v.pool] += uint64(v.sizeGB)
	}
	log.Debug("assertCreateQuota: checking", zap.Int("volumes", len(volumes)), zap.Strings("nodes", nodes),
		zap.Uint64("total_gb", total), zap.Int("pools", len(perPool)))
	if err := rm.assertProjectQuota(ctx, labels, total, true); err != nil {
		log.Debug("assertCreateQuota: project quota refused", zap.Error(err))
		return err
	}
	for pool, gb := range perPool {
		if err := rm.assertOvercommit(ctx, pool, nodes, gb); err != nil {
			log.Debug("assertCreateQuota: overcommit refused", zap.String("pool", pool), zap.Uint64("add_gb", gb), zap.Error(err))
			return err
		}
	}
	return nil
}

// assertResizeQuota checks the growth of a resize.
func (rm *ResourceManager) assertResizeQuota(ctx context.Context, resource string, volumeID uint32, newSizeGB uint64, hosts []string) error {
	log := rm.controller.traceLogger()
	log.Debug("assertResizeQuota: checking", zap.String("resource", resource), zap.Uint32("volume", volumeID),
		zap.Uint64("new_size_gb", newSizeGB), zap.Strings("hosts", hosts))
	if rm.controller.db == nil {
		log.Debug("assertResizeQuota: no database, not checking")
		return nil
	}
	start := time.Now()
	vols, err := rm.controller.db.ListVolumes(ctx, resource)
	log.Debug("assertResizeQuota: listed volumes", zap.String("resource", resource), zap.Int("count", len(vols)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil
	}
	v := findVolumeRecord(vols, volumeID)
	if v == nil || newSizeGB <= uint64(max(v.SizeGB, 0)) {
		log.Debug("assertResizeQuota: no growth to check", zap.Bool("volume_found", v != nil))
		return nil
	}
	return rm.assertQuotaForResource(ctx, resource, v.Pool, hosts, newSizeGB-uint64(v.SizeGB))
}
