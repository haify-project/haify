package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Filling a volume from a snapshot or a backup image writes every byte of the
// image, zeros included. On thin storage each zero block written is a block
// allocated, so restoring a 100 GiB image holding 1 GiB of data took 100 GiB
// of pool on every replica — the same thin-pool exhaustion that has taken
// replicas offline in production.
//
// The zeros need not be written when the target already reads as zeros where
// the image does. That holds when every replica of the target is thin (an LV
// or a zvol: unwritten blocks read as zeros), none is encrypted (under LUKS an
// unwritten block decrypts to noise), and the device has just been discarded —
// DRBD passes the discard to every replica, and a thin volume unmaps the
// blocks. Then dd conv=sparse can skip the zero runs. When any of that cannot
// be established, the full write is kept.

// zeroReadingReplicas reports whether every diskful replica of resource keeps
// its data on storage that reads unwritten blocks as zeros.
func (rm *ResourceManager) zeroReadingReplicas(ctx context.Context, resource string) bool {
	log := rm.controller.traceLogger()
	log.Debug("zeroReadingReplicas: checking", zap.String("resource", resource))
	if rm.controller.db == nil {
		log.Debug("zeroReadingReplicas: no database, full write")
		return false
	}
	dbRes, err := rm.controller.db.GetResource(ctx, resource)
	if err != nil || dbRes == nil || dbRes.Encrypted {
		log.Debug("zeroReadingReplicas: resource unreadable or encrypted, full write", zap.String("resource", resource),
			zap.Bool("encrypted", dbRes != nil && dbRes.Encrypted), zap.Error(err))
		return false
	}
	vols, err := rm.controller.db.ListVolumes(ctx, resource)
	if err != nil || len(vols) == 0 {
		log.Debug("zeroReadingReplicas: no volumes, full write", zap.String("resource", resource), zap.Error(err))
		return false
	}
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil || len(hosts) == 0 {
		log.Debug("zeroReadingReplicas: no hosts, full write", zap.String("resource", resource), zap.Error(err))
		return false
	}
	for _, v := range vols {
		if strings.HasPrefix(v.Device, "/dev/zvol/") {
			log.Debug("zeroReadingReplicas: zvol reads zeros", zap.String("device", v.Device))
			continue
		}
		for _, h := range hosts {
			start := time.Now()
			thin, err := rm.deployment.LVIsThin(ctx, h, v.Pool, v.VolumeName)
			if err != nil || !thin {
				log.Debug("zeroReadingReplicas: volume not thin here, full write", zap.String("host", h),
					zap.String("pool", v.Pool), zap.String("volume", v.VolumeName), zap.Bool("thin", thin),
					zap.Duration("took", time.Since(start)), zap.Error(err))
				return false
			}
			log.Debug("zeroReadingReplicas: volume thin", zap.String("host", h), zap.String("volume", v.VolumeName),
				zap.Duration("took", time.Since(start)))
		}
	}
	log.Debug("zeroReadingReplicas: every replica reads zeros, sparse write possible", zap.String("resource", resource))
	return true
}

// sparseWriteSetup returns shell that, when the target can take a sparse
// write, discards it and sets CONV to "sparse,fsync"; otherwise CONV is
// "fsync". A failed discard falls back to the full write.
func sparseWriteSetup(sparse bool, target string) string {
	zap.L().Debug("sparseWriteSetup: choosing dd conv", zap.String("target", target), zap.Bool("sparse", sparse))
	if !sparse {
		return "CONV=fsync; "
	}
	return fmt.Sprintf("if sudo blkdiscard -f %s 2>/dev/null; then CONV=sparse,fsync; else CONV=fsync; fi; ", target)
}
