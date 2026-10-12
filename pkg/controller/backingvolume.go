package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// createBackingVolumeOn makes one replica's backing volume on one node, in
// whichever shape that node's pool actually has.
//
// Three call sites used to decide this three different ways: resource creation
// switched on the pool type recorded in the database and guessed the thin
// pool's name from a "<pool>_thin" convention, while add-replica and add-dr
// ran a plain thick `lvcreate` regardless. On a node whose volume group is
// entirely consumed by a thin pool the thick path cannot succeed at all —
// there is no free space left to take, by construction.
//
// The name is not guessed either. A pool built by `pool create` is called
// "<pool>_thin" and one produced by converting a thick pool in place is called
// something else, so the node is asked what it has rather than told.
func (rm *ResourceManager) createBackingVolumeOn(ctx context.Context, host, pool, volume string, sizeBytes uint64) error {
	log := rm.controller.traceLogger()
	log.Debug("createBackingVolumeOn: creating backing volume", zap.String("host", host), zap.String("pool", pool),
		zap.String("volume", volume), zap.Uint64("size_bytes", sizeBytes))
	start := time.Now()
	thinPool, err := rm.deployment.LVThinPoolIn(ctx, host, pool)
	log.Debug("createBackingVolumeOn: looked for a thin pool", zap.String("host", host), zap.String("pool", pool),
		zap.String("thin_pool", thinPool), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("look for a thin pool in %s on %s: %w", pool, host, err)
	}

	// Bytes, never a rounded size: DRBD stores the device size in its metadata,
	// and a replica built at a different number of bytes will not attach.
	size := fmt.Sprintf("%dB", sizeBytes)

	if thinPool != "" {
		log.Debug("createBackingVolumeOn: thin pool found, creating thin volume", zap.String("host", host),
			zap.String("thin_pool", thinPool), zap.String("volume", volume))
		start = time.Now()
		err = execFailure(rm.deployment.LVCreateThinVolume(ctx, []string{host}, pool, thinPool, volume, size))
		log.Debug("createBackingVolumeOn: lvcreate thin finished", zap.String("host", host), zap.String("volume", volume),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return err
	}
	log.Debug("createBackingVolumeOn: no thin pool, creating thick volume", zap.String("host", host),
		zap.String("pool", pool), zap.String("volume", volume))
	start = time.Now()
	err = execFailure(rm.deployment.LVCreate(ctx, []string{host}, pool, volume, size))
	log.Debug("createBackingVolumeOn: lvcreate finished", zap.String("host", host), zap.String("volume", volume),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}
