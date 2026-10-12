package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// Growing a thin pool into space that `pool add-disk` just added to its volume
// group. vgextend alone leaves the new extents unallocated in the group, where
// thin volumes cannot use them: they are carved from the thin pool LV, not
// from the group. Without this step adding a disk to a thin pool changes
// nothing a client can write to.

// thinPoolGrowPercentFree is the share of the group's free extents the thin
// pool's data area is extended by.
//
// It mirrors the 95%FREE `pool create` builds the pool with, for the same
// reason: the remainder stays unallocated so the metadata area (and the spare
// copy LVM keeps of it) can still be grown, and so an operator has room to
// act when the pool fills. Convert-thin takes 100%FREE instead because it
// sizes the metadata area explicitly up front; here the metadata grows after
// the fact and needs extents to grow into.
const thinPoolGrowPercentFree = 95

// thinPoolPercentFree is the share of a group's free extents a thin pool takes
// when created or grown: the 95% above, less [storage.thin] reserve_percent,
// which stays in the group for the pool to be grown into before it fills
// (pool_autoextend.go).
func (c *Controller) thinPoolPercentFree() uint64 {
	pct := thinPoolGrowPercentFree
	if c.config != nil {
		pct -= c.config.Storage.Thin.ReservePercent
	}
	loggerOfController(c).Debug("Controller.thinPoolPercentFree: computed", zap.Int("percent", pct),
		zap.Int("used_percent", max(pct, 45)))
	return uint64(max(pct, 45))
}

// thinGrowPlan is what to do to an existing thin pool after its group grew.
type thinGrowPlan struct {
	// MetadataGrowTo is the size to raise the metadata area to, or zero to
	// leave it alone.
	MetadataGrowTo uint64
	// ExtendData is false when the group has nothing free to extend into.
	ExtendData bool
}

// planThinGrow keeps the metadata area proportional to the data area, using
// the same sizing rule convert-thin applies (thinMetadataBytes: 1%, floored
// and capped). Metadata is only ever grown, and only when the growth — paid
// twice, once for the spare copy — fits comfortably in what is free, so a
// short group never has its data extension starved by the metadata one.
func planThinGrow(dataBytes, metaBytes, vgFreeBytes, pct uint64) thinGrowPlan {
	log := zap.L()
	if vgFreeBytes == 0 {
		log.Debug("planThinGrow: group has nothing free", zap.Uint64("data_bytes", dataBytes),
			zap.Uint64("meta_bytes", metaBytes))
		return thinGrowPlan{}
	}
	plan := thinGrowPlan{ExtendData: true}
	projected := dataBytes + vgFreeBytes*pct/100
	if target := thinMetadataBytes(projected); target > metaBytes {
		if 2*(target-metaBytes) < vgFreeBytes*(100-pct)/100 {
			plan.MetadataGrowTo = target
		} else {
			log.Debug("planThinGrow: metadata growth does not fit, left alone", zap.Uint64("target_bytes", target),
				zap.Uint64("meta_bytes", metaBytes), zap.Uint64("free_bytes", vgFreeBytes))
		}
	}
	log.Debug("planThinGrow: planned", zap.Uint64("data_bytes", dataBytes), zap.Uint64("meta_bytes", metaBytes),
		zap.Uint64("free_bytes", vgFreeBytes), zap.Uint64("percent", pct),
		zap.Uint64("metadata_grow_to_bytes", plan.MetadataGrowTo), zap.Bool("extend_data", plan.ExtendData))
	return plan
}

// growThinPoolAfterExtend extends the thin pool in vgName, if it has one, into
// the group's free extents. A group without a thin pool is left alone: thick
// volumes allocate from the group directly and already see the new space.
func (sm *StorageManager) growThinPoolAfterExtend(ctx context.Context, address, vgName string) error {
	log := loggerOfController(sm.controller)
	log.Debug("growThinPoolAfterExtend: start", zap.String("host", address), zap.String("pool", vgName))
	dep := sm.controller.deployment
	start := time.Now()
	thinLV, err := dep.LVThinPoolIn(ctx, address, vgName)
	log.Debug("growThinPoolAfterExtend: looked for a thin pool", zap.String("host", address),
		zap.String("pool", vgName), zap.String("thin_pool", thinLV), zap.Duration("took", time.Since(start)),
		zap.Error(err))
	if err != nil {
		return fmt.Errorf("look for a thin pool in %s: %w", vgName, err)
	}
	if thinLV == "" {
		log.Debug("growThinPoolAfterExtend: no thin pool, nothing to grow", zap.String("pool", vgName))
		return nil
	}
	start = time.Now()
	dataBytes, err := dep.LVSizeBytes(ctx, address, vgName, thinLV)
	if err != nil {
		log.Debug("growThinPoolAfterExtend: reading data size failed", zap.String("pool", vgName),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("read the size of %s/%s: %w", vgName, thinLV, err)
	}
	metaBytes, err := dep.LVSizeBytes(ctx, address, vgName, thinLV+"_tmeta")
	if err != nil {
		log.Debug("growThinPoolAfterExtend: reading metadata size failed", zap.String("pool", vgName),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("read the metadata size of %s/%s: %w", vgName, thinLV, err)
	}
	freeBytes, err := dep.VGFreeBytes(ctx, address, vgName)
	if err != nil {
		log.Debug("growThinPoolAfterExtend: reading free space failed", zap.String("pool", vgName),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("read the free space of %s: %w", vgName, err)
	}
	log.Debug("growThinPoolAfterExtend: sized", zap.String("pool", vgName), zap.Uint64("data_bytes", dataBytes),
		zap.Uint64("meta_bytes", metaBytes), zap.Uint64("free_bytes", freeBytes), zap.Duration("took", time.Since(start)))

	pct := sm.controller.thinPoolPercentFree()
	plan := planThinGrow(dataBytes, metaBytes, freeBytes, pct)
	// Metadata first — see LVExtendThinPoolMetadata for why the order matters.
	if plan.MetadataGrowTo > 0 {
		start := time.Now()
		err := execFailure(dep.LVExtendThinPoolMetadata(ctx, []string{address},
			vgName, thinLV, plan.MetadataGrowTo))
		log.Debug("growThinPoolAfterExtend: grow metadata", zap.String("host", address), zap.String("pool", vgName),
			zap.Uint64("size_bytes", plan.MetadataGrowTo), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return fmt.Errorf("grow the metadata area of %s/%s: %w", vgName, thinLV, err)
		}
	}
	if plan.ExtendData {
		start := time.Now()
		err := execFailure(dep.LVExtendThinPoolPercentFree(ctx, []string{address},
			vgName, thinLV, int(pct)))
		log.Debug("growThinPoolAfterExtend: extend data", zap.String("host", address), zap.String("pool", vgName),
			zap.Uint64("percent", pct), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return fmt.Errorf("extend %s/%s: %w", vgName, thinLV, err)
		}
	}
	log.Debug("growThinPoolAfterExtend: done", zap.String("pool", vgName))
	return nil
}
