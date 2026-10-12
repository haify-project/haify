package controller

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// poolCapacity is what one node's copy of a pool can say about its room for a
// new replica, plus how far that answer can be trusted.
//
// Three states, deliberately not two. "No room" and "could not say" are the
// same number — zero — and reading the second as the first is what made
// auto-placement impossible on thin storage: a thin pool is built from nearly
// all of its volume group (see PoolInfo.ThinUsage), so the group reports
// vg_free near zero for the pool's whole life however empty it is. Every
// thin node was therefore filtered out as full, and the failure was reported as
// "insufficient capacity" — sending the operator to hunt for space that was
// never missing. An unknown capacity now costs a node its rank, never its
// candidacy.
type poolCapacity struct {
	// freeGB is what the pool has left, for ranking. Meaningless unless known.
	freeGB uint64
	// freeBytes is the same, unrounded.
	freeBytes uint64
	// known is false when nothing in the pool report describes usable room.
	known bool
	// thin marks an over-provisioned pool, where freeGB is a health signal
	// rather than a ceiling on the size of the next volume.
	thin bool
	// full is LVM's or the thresholds' verdict that a thin pool is out of room.
	full bool
	// sizeBytes and virtualBytes are a thin pool's real and promised size;
	// maxOvercommit, when set, caps their ratio (quota.go).
	sizeBytes, virtualBytes uint64
	maxOvercommit           float64
}

// admits reports whether a pool may host a replica of a volume asking sizeGB.
//
// The rule differs by pool kind, because "free space" means different things:
//
//   - Unknown capacity is admitted. A pool that did not tell us how full it is
//     has not told us it is full, and a filter that treats silence as rejection
//     empties itself of candidates on a cluster that is perfectly healthy. Such
//     a node carries freeGB 0, so it is picked only when nothing better exists.
//
//   - A thick volume group is a hard ceiling: lvcreate fails the instant the
//     group cannot cover the volume, so free >= requested is the real
//     precondition and stays exactly as it always was.
//
//   - A thin pool is not a ceiling, and imposing one here would disable the
//     feature. Thin volumes allocate as they are written, so a 100GB volume on
//     a pool with 10GB left is not an accident — over-provisioning is the
//     reason to run thin at all, and demanding the nominal size up front would
//     refuse every placement that thin exists to allow. What is still worth
//     refusing is a pool with no room for the data it already holds: at
//     ThinPoolFullPercent, or with LVM's own out-of-space flag set, seeding a
//     replica there is how a pool gets pushed over the edge — a new replica
//     syncs, and a resync reallocates every block of a volume (see poolthin.go)
//     — and a pool that refuses writes takes its DRBD disk down with it. Below
//     that line the pool is admitted and ranked by what it has left, so
//     placement still prefers the emptiest node.
func (c poolCapacity) admits(sizeGB uint64) bool {
	log := zap.L()
	switch {
	case !c.known:
		log.Debug("poolCapacity.admits: capacity unknown, admitted", zap.Uint64("size_gb", sizeGB), zap.Bool("thin", c.thin))
		return true
	case c.thin:
		over := c.overcommitted(sizeGB)
		log.Debug("poolCapacity.admits: thin pool judged by fullness and overcommit",
			zap.Uint64("size_gb", sizeGB), zap.Bool("full", c.full), zap.Bool("overcommitted", over),
			zap.Uint64("free_gb", c.freeGB), zap.Bool("admitted", !c.full && !over))
		return !c.full && !over
	default:
		log.Debug("poolCapacity.admits: thick pool judged by free space",
			zap.Uint64("size_gb", sizeGB), zap.Uint64("free_gb", c.freeGB), zap.Bool("admitted", c.freeGB >= sizeGB))
		return c.freeGB >= sizeGB
	}
}

// poolPlacementCapacity reads a pool report the way placement needs it.
//
// recordedThin is the pool type the controller has on file. It is consulted
// only when the pool reported no thin utilisation, and only to tell a thin pool
// whose `lvs` read failed (capacity unknown) from a thick group that is
// genuinely full (capacity zero) — two states the numbers alone cannot
// separate, since both read zero free. The live report wins whenever it exists,
// because the record goes stale: a group converted to thin after creation still
// has "vg" on file.
func poolPlacementCapacity(p *PoolInfo, recordedThin bool) poolCapacity {
	log := zap.L()
	if p == nil {
		log.Debug("poolPlacementCapacity: no pool report, capacity unknown")
		return poolCapacity{}
	}
	if u := p.ThinUsage; u != nil {
		// SizeBytes zero means the report reached us without the one figure the
		// percentages are a percentage of, which leaves the pool's room unknown
		// rather than exhausted.
		if u.SizeBytes == 0 {
			log.Debug("poolPlacementCapacity: thin report without a size, capacity unknown",
				zap.String("pool", p.Name), zap.String("node", p.Node))
			return poolCapacity{thin: true}
		}
		used := u.DataPercent
		if used < 0 {
			used = 0
		}
		if used > 100 {
			used = 100
		}
		free := float64(u.SizeBytes) * (100 - used) / 100
		full := thinPoolExhausted(u)
		log.Debug("poolPlacementCapacity: thin pool from live usage",
			zap.String("pool", p.Name), zap.String("node", p.Node), zap.Float64("data_percent", u.DataPercent),
			zap.Uint64("free_bytes", uint64(free)), zap.Bool("full", full))
		return poolCapacity{
			freeGB:       bytesToGB(uint64(free)),
			freeBytes:    uint64(free),
			known:        true,
			thin:         true,
			full:         full,
			sizeBytes:    u.SizeBytes,
			virtualBytes: u.VirtualBytes,
		}
	}
	if p.Thin || recordedThin {
		log.Debug("poolPlacementCapacity: thin pool without live usage, capacity unknown",
			zap.String("pool", p.Name), zap.String("node", p.Node), zap.Bool("reported_thin", p.Thin),
			zap.Bool("recorded_thin", recordedThin))
		return poolCapacity{thin: true}
	}
	log.Debug("poolPlacementCapacity: thick pool from free space",
		zap.String("pool", p.Name), zap.String("node", p.Node), zap.Uint64("free_gb", p.FreeGB))
	return poolCapacity{freeGB: p.FreeGB, freeBytes: p.FreeBytes, known: true}
}

// thinPoolExhausted reports whether a thin pool is too full to be seeded with
// another replica. Metadata counts as well as data because exhausting either
// one stops writes just as completely, and they fill at unrelated rates.
func thinPoolExhausted(u *PoolThinInfo) bool {
	return u.OutOfSpace || u.DataPercent >= ThinPoolFullPercent || u.MetaPercent >= ThinPoolFullPercent
}

// poolRecordedThin reports whether the controller created this pool as a thin
// pool. A pool the cluster cannot currently be asked about — an unreachable
// node, a failed `lvs` — still has its type on file, and that is the only thing
// left to distinguish a thin pool of unknown fullness from a full thick group.
// A missing or unreadable record answers "not thin", which is the pre-existing
// behaviour and never widens what placement accepts.
func (rm *ResourceManager) poolRecordedThin(ctx context.Context, pool string) bool {
	log := loggerOfController(rm.controller)
	if rm.controller.db == nil {
		log.Debug("ResourceManager.poolRecordedThin: no database, not thin", zap.String("pool", pool))
		return false
	}
	start := time.Now()
	p, err := rm.controller.db.GetPool(ctx, pool)
	if err != nil || p == nil {
		log.Debug("ResourceManager.poolRecordedThin: no pool record, not thin", zap.String("pool", pool),
			zap.Bool("found", p != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
		return false
	}
	thin := isThinPoolType(p.Type)
	log.Debug("ResourceManager.poolRecordedThin: pool record read", zap.String("pool", pool),
		zap.String("type", p.Type), zap.Bool("thin", thin), zap.Duration("took", time.Since(start)))
	return thin
}
