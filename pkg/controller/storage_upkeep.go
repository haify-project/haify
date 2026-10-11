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
	if c.db == nil || c.resources == nil {
		return
	}
	go func() {
		// Let the controller settle (health checks, node addresses) first.
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
		c.resources.backfillThinResyncDefaults(ctx)
	}()
	_ = c.db.PruneStorageJobs(ctx, storageJobKeep)
	c.storage.resumeDiskJobs(ctx)
	c.resources.resumeVolumeMoves(ctx)
	c.backups.failInterruptedShips(ctx)
}

// backfillThinResyncDefaults gives every resource on thin storage the
// rs-discard-granularity new ones get (thin_resync.go). Without it the next
// full resync of an old resource — a replacement replica, a rebuilt node —
// writes every block of every copy and can fill the pool, which is how a
// test cluster's pool reached 99.5%. The .res file is the record of a
// resource's options, so a resource that already has the option is skipped
// and this is cheap to repeat on every takeover.
func (rm *ResourceManager) backfillThinResyncDefaults(ctx context.Context) {
	log := rm.controller.logger
	resources, err := rm.controller.db.ListResources(ctx)
	if err != nil {
		return
	}
	updated := 0
	for _, r := range resources {
		if ctx.Err() != nil {
			return
		}
		nodes := splitCSV(r.Nodes)
		if len(nodes) == 0 || !rm.resourceOnThinStorage(ctx, r.Name, rm.controller.ResolveHost(nodes[0])) {
			continue
		}
		res, err := rm.deployment.Exec(ctx, []string{rm.controller.ResolveHost(nodes[0])}, "sudo cat /etc/drbd.d/"+r.Name+".res")
		if err != nil || !res.AllSuccess() {
			continue
		}
		var conf string
		for _, h := range res.Hosts {
			conf = h.Output
		}
		if strings.Contains(conf, "rs-discard-granularity") {
			continue
		}
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
}

// resourceOnThinStorage reports whether any LVM volume of resource is a thin
// volume on host. ZFS datasets are left alone: their discards are a matter of
// the pool's own settings.
func (rm *ResourceManager) resourceOnThinStorage(ctx context.Context, resource, host string) bool {
	vols, err := rm.controller.db.ListVolumes(ctx, resource)
	if err != nil {
		return false
	}
	for _, v := range vols {
		if v.Pool == "" || isZFSDevice(v.Device) {
			continue
		}
		if rm.poolRecordedThin(ctx, v.Pool) {
			return true
		}
		if thin, err := rm.deployment.LVIsThin(ctx, host, v.Pool, v.VolumeName); err == nil && thin {
			return true
		}
	}
	return false
}
