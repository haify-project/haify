package controller

import (
	"context"
	"maps"
	"strings"

	"go.uber.org/zap"
)

// rsDiscardGranularity is what a resource on thin storage resyncs with. A
// resync writes every block it covers, zeros included, so a full one — a new
// replica, an invalidate, a failback whose history no longer matches — turns a
// thin volume into a fully allocated one: 0.03% to 100% on a 1G volume on
// a test cluster, and the same thing is how a production thin pool filled
// up. With this set the sync source sends a run of zeros as a discard, and the
// target keeps it unallocated. DRBD rounds the value up to the backing
// device's discard granularity, and 64 KiB is the thin-pool chunk size LVM
// picks for all but very large pools.
const rsDiscardGranularity = "65536"

// withThinResyncDefaults returns options with disk/rs-discard-granularity
// added when any volume is backed by thin storage and the caller did not set
// it. The caller's map is not modified. Thick volumes are left alone: on them
// a discard can reach a device that does not read back zeros afterwards.
func (rm *ResourceManager) withThinResyncDefaults(ctx context.Context, options map[string]string, storageType string, volumes []resolvedVolume) map[string]string {
	log := ctrlLogOrGlobal(rm.controller)
	log.Debug("withThinResyncDefaults: checking", zap.String("storage_type", storageType), zap.Int("volumes", len(volumes)),
		zap.Int("options", len(options)))
	for k := range options {
		if strings.TrimPrefix(strings.ToLower(k), "disk/") == "rs-discard-granularity" {
			log.Debug("withThinResyncDefaults: caller set it, left alone", zap.String("option", k))
			return options
		}
	}
	thin := storageType == "lvm-thin" || storageType == "zfs-thin"
	for _, v := range volumes {
		if thin {
			break
		}
		thin = rm.poolRecordedThin(ctx, v.pool) || rm.poolRecordedThin(ctx, normalizeManagedName(v.pool))
		log.Debug("withThinResyncDefaults: volume pool", zap.String("pool", v.pool), zap.Bool("thin", thin))
	}
	if !thin {
		log.Debug("withThinResyncDefaults: thick storage, no discard granularity")
		return options
	}
	out := make(map[string]string, len(options)+1)
	maps.Copy(out, options)
	out["disk/rs-discard-granularity"] = rsDiscardGranularity
	log.Debug("withThinResyncDefaults: thin storage, discard granularity added", zap.String("value", rsDiscardGranularity))
	return out
}

// wanCsumsAlg is the checksum a resync across the WAN compares blocks by
// before sending them. A resync over the WAN is bounded by the link, and most
// of what one covers — a failback whose history no longer matches, a DR copy
// invalidated by hand — is already the same on both sides: a full resync of a
// 1G volume on a test cluster found every block equal and sent nothing.
// sha256 rather than crc32c: a block that differs but hashes the same is
// skipped, and the copies disagree without anything saying so.
const wanCsumsAlg = "sha256"
