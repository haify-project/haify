package controller

import (
	"context"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Storage upkeep the active controller starts on taking over: resyncs that
// keep thin volumes thin on resources created before that was the default,
// and storage jobs (disk moves, volume moves) a previous controller left
// running.

// storageJobKeep is how long a finished storage job stays listed.
const storageJobKeep = 14 * 24 * time.Hour

func (c *Controller) startStorageUpkeep(ctx context.Context) {
	log := c.traceLogger()
	if c.db == nil || c.resources == nil {
		log.Debug("startStorageUpkeep: no database or resource manager, skipped")
		return
	}
	log.Debug("startStorageUpkeep: starting")
	go func() {
		// Let the controller settle (health checks, node addresses) first.
		select {
		case <-ctx.Done():
			log.Debug("startStorageUpkeep: stopped before the thin backfill ran")
			return
		case <-time.After(30 * time.Second):
		}
		log.Debug("startStorageUpkeep: running the thin resync backfill")
		c.resources.backfillThinResyncDefaults(ctx)
	}()
	perr := c.db.PruneStorageJobs(ctx, storageJobKeep)
	log.Debug("startStorageUpkeep: pruned old storage jobs", zap.Duration("keep", storageJobKeep), zap.Error(perr))
	c.storage.resumeDiskJobs(ctx)
	c.resources.resumeVolumeMoves(ctx)
	c.backups.failInterruptedShips(ctx)
	log.Debug("startStorageUpkeep: interrupted jobs resumed")
}

// backfillThinResyncDefaults gives every resource on thin storage the
// rs-discard-granularity new ones get (thin_resync.go). Without it the next
// full resync of an old resource — a replacement replica, a rebuilt node —
// writes every block of every copy and can fill the pool, which is how a
// test cluster's pool reached 99.5%. The .res file is the record of a
// resource's options, so a resource that already has the option is skipped
// and this is cheap to repeat on every takeover.
func (rm *ResourceManager) backfillThinResyncDefaults(ctx context.Context) {
	log := rm.controller.traceLogger()
	resources, err := rm.controller.db.ListResources(ctx)
	if err != nil {
		log.Debug("backfillThinResyncDefaults: resource list failed", zap.Error(err))
		return
	}
	log.Debug("backfillThinResyncDefaults: checking resources", zap.Int("count", len(resources)))
	updated := 0
	for _, r := range resources {
		if ctx.Err() != nil {
			log.Debug("backfillThinResyncDefaults: cancelled", zap.Int("updated", updated), zap.Error(ctx.Err()))
			return
		}
		nodes := splitCSV(r.Nodes)
		if len(nodes) == 0 || !rm.resourceOnThinStorage(ctx, r.Name, rm.controller.ResolveHost(nodes[0])) {
			log.Debug("backfillThinResyncDefaults: not on thin storage, skipped", zap.String("resource", r.Name))
			continue
		}
		host := rm.controller.ResolveHost(nodes[0])
		start := time.Now()
		res, err := rm.deployment.Exec(ctx, []string{host}, "sudo cat /etc/drbd.d/"+r.Name+".res")
		if err != nil || !res.AllSuccess() {
			log.Debug("backfillThinResyncDefaults: .res not readable, skipped", zap.String("resource", r.Name),
				zap.String("host", host), zap.Duration("took", time.Since(start)), zap.Error(err))
			continue
		}
		var conf string
		for _, h := range res.Hosts {
			conf = h.Output
		}
		// The .res body is not traced: it can hold a shared-secret.
		if strings.Contains(conf, "rs-discard-granularity") {
			log.Debug("backfillThinResyncDefaults: already set, skipped", zap.String("resource", r.Name))
			continue
		}
		log.Debug("backfillThinResyncDefaults: setting discard granularity", zap.String("resource", r.Name))
		if err := rm.SetOptions(ctx, r.Name, map[string]string{"disk/rs-discard-granularity": rsDiscardGranularity}); err != nil {
			log.Warn("Could not give a thin resource its resync discard granularity; it will be retried on the next takeover",
				zap.String("resource", r.Name), zap.Error(err))
			continue
		}
		updated++
		log.Info("Resource on thin storage now resyncs zeros as discards", zap.String("resource", r.Name))
	}
	if updated > 0 {
		log.Info("Backfilled rs-discard-granularity on thin resources", zap.Int("resources", updated))
	}
	log.Debug("backfillThinResyncDefaults: done", zap.Int("updated", updated))
}

// resourceOnThinStorage reports whether any LVM volume of resource is a thin
// volume on host. ZFS datasets are left alone: their discards are a matter of
// the pool's own settings.
func (rm *ResourceManager) resourceOnThinStorage(ctx context.Context, resource, host string) bool {
	log := rm.controller.traceLogger()
	vols, err := rm.controller.db.ListVolumes(ctx, resource)
	if err != nil {
		log.Debug("resourceOnThinStorage: volume list failed", zap.String("resource", resource), zap.Error(err))
		return false
	}
	for _, v := range vols {
		if v.Pool == "" || isZFSDevice(v.Device) {
			log.Debug("resourceOnThinStorage: ZFS or no pool, skipped", zap.String("resource", resource), zap.String("device", v.Device))
			continue
		}
		if rm.poolRecordedThin(ctx, v.Pool) {
			log.Debug("resourceOnThinStorage: pool recorded thin", zap.String("resource", resource), zap.String("pool", v.Pool))
			return true
		}
		if thin, err := rm.deployment.LVIsThin(ctx, host, v.Pool, v.VolumeName); err == nil && thin {
			log.Debug("resourceOnThinStorage: volume is thin on the node", zap.String("resource", resource),
				zap.String("host", host), zap.String("volume", v.VolumeName))
			return true
		}
	}
	log.Debug("resourceOnThinStorage: not thin", zap.String("resource", resource), zap.Int("volumes", len(vols)))
	return false
}
