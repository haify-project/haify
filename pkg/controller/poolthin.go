package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Reading how full a thin pool is, and deciding when that is worth saying out
// loud. Kept apart from the code that creates and extends pools so the parsing
// — where a wrong answer becomes a missed outage — can be read and tested on
// its own, the same way poolcachestatus.go is split from poolcache.go.
//
// Why this exists at all: almost all of a thin-backed group belongs to the
// thin pool LV — `pool create` gives it 95% of the free extents and
// convert-thin all of them — so vg_free stays near zero for the life of the
// pool. Capacity reported from the VG is therefore close to a constant, and
// says nothing about whether the next write will succeed.

// Thin pool utilisation thresholds, in percent.
//
// The gap between them is deliberate. A pool crossing ThinPoolNearFullPercent
// still has room to absorb ordinary writes, and the useful response is to plan
// an extension. Past ThinPoolFullPercent there may no longer be room for a
// full DRBD resync of the volumes it holds — a resync reallocates every block
// of a volume, so a pool that looks comfortable by delta can still be unable
// to survive one — and the useful response is immediate.
const (
	ThinPoolNearFullPercent = 85.0
	ThinPoolFullPercent     = 95.0
)

// PoolThinInfo is a thin pool's utilisation as an operator needs it.
//
// Data and metadata are carried separately because they are separate failure
// modes: metadata exhaustion stops writes just as completely as data
// exhaustion, and the two fill at unrelated rates.
type PoolThinInfo struct {
	// PoolLV is the thin pool logical volume inside the group, as LVM names it.
	// Its presence is what makes the percentages below meaningful.
	PoolLV string `json:"pool_lv"`
	// SizeBytes is the pool's data capacity — what DataPercent is a percentage
	// of. It is not the volume group's size.
	SizeBytes   uint64  `json:"size_bytes"`
	DataPercent float64 `json:"data_percent"`
	MetaPercent float64 `json:"metadata_percent"`
	// OutOfSpace is LVM's own verdict, read from the volume health field of
	// lv_attr rather than inferred from a percentage. A pool in this state has
	// already refused writes; the kernel drops the backing disk out from under
	// DRBD, which then reports Diskless on a node configured diskful.
	OutOfSpace bool `json:"out_of_space,omitempty"`
	// VirtualBytes is what the pool's thin volumes (snapshots left out) could
	// grow to: the sum of their sizes. Against SizeBytes it is how far the
	// pool is overcommitted ([quota] max_overcommit_ratio, quota.go).
	VirtualBytes uint64 `json:"virtual_bytes,omitempty"`
}

// thinRow is one line of `lvs -o LVMThinFields`.
type thinRow struct {
	VG          string
	Name        string
	SegType     string
	SizeBytes   uint64
	DataPercent float64
	MetaPercent float64
	Attr        string
}

// parseThinReport groups an lvs report by volume group, keeping only thin
// pools.
//
// Rows for every other LV in the group arrive on the same report and are
// dropped here, so nothing downstream has to know that the query was not
// selective. A group is absent from the result when it holds no thin pool,
// which is how a plain thick VG stays distinguishable from a thin pool that
// happens to read 0%.
func parseThinReport(output string) map[string]*PoolThinInfo {
	byVG := make(map[string]*PoolThinInfo)
	virtual := make(map[string]uint64)
	defer func() {
		for vg, v := range virtual {
			if info := byVG[vg]; info != nil {
				info.VirtualBytes = v
			}
		}
		zap.L().Debug("parseThinReport: parsed", zap.Int("count", len(byVG)), zap.Int("groups_with_thin_volumes", len(virtual)))
	}()
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, "|")
		if len(f) < 7 {
			continue // not a report line: an lvm warning, or a truncated read
		}
		row := thinRow{
			VG:          strings.TrimSpace(f[0]),
			Name:        strings.Trim(strings.TrimSpace(f[1]), "[]"),
			SegType:     strings.TrimSpace(f[2]),
			SizeBytes:   parseThinUint(f[3]),
			DataPercent: parseThinPercent(f[4]),
			MetaPercent: parseThinPercent(f[5]),
			Attr:        strings.TrimSpace(f[6]),
		}
		// A thin volume (not a snapshot: it has no origin) counts toward
		// the pool's virtual size.
		if row.SegType == "thin" && (len(f) < 8 || strings.TrimSpace(f[7]) == "") {
			virtual[row.VG] += row.SizeBytes
		}
		if row.VG == "" || row.Name == "" || row.SegType != "thin-pool" {
			continue
		}
		// A volume group holding more than one thin pool keeps the fullest, on
		// the grounds that the pool closest to failing is the one worth
		// reporting. Haify creates exactly one, so this is a tiebreak for
		// adopted groups rather than a normal path.
		if existing, ok := byVG[row.VG]; ok && existing.DataPercent >= row.DataPercent {
			continue
		}
		byVG[row.VG] = &PoolThinInfo{
			PoolLV:      row.Name,
			SizeBytes:   row.SizeBytes,
			DataPercent: row.DataPercent,
			MetaPercent: row.MetaPercent,
			OutOfSpace:  thinOutOfSpace(row.Attr),
		}
	}
	return byVG
}

// thinOutOfSpace reads the volume health field of lv_attr.
//
// It is the ninth character, and 'D' means the pool has run out of data space:
// "twi-aotzD-" is a pool that has already failed writes, against "twi-aotz--"
// for a healthy one. Reading LVM's own flag rather than comparing
// DataPercent against 100 matters because the flag is what the kernel acts on,
// and it stays set after space is freed until the pool is repaired.
func thinOutOfSpace(attr string) bool {
	const healthField = 8
	if len(attr) <= healthField {
		return false
	}
	return attr[healthField] == 'D'
}

// parseThinUint reads one numeric lvs column, tolerating a unit suffix that
// --nosuffix was supposed to remove. A blank column is absence of information
// and reads as zero.
func parseThinUint(field string) uint64 {
	v := strings.TrimSuffix(strings.TrimSpace(field), "B")
	if v == "" {
		return 0
	}
	// lvs emits sizes with a trailing ".00" under some locales even with
	// --nosuffix; take the integer part rather than failing the whole row.
	if dot := strings.IndexByte(v, '.'); dot >= 0 {
		v = v[:dot]
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// parseThinPercent reads a percentage column. LVM leaves these blank for LVs
// that have none and for a pool that is not currently active; both read as
// zero, which callers must not confuse with a genuinely empty pool. The
// presence of PoolThinInfo at all is the signal that a thin pool was found;
// use it rather than testing the percentage against zero.
func parseThinPercent(field string) float64 {
	v := strings.TrimSpace(field)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return n
}

// readThinUsage reads the thin pool utilisation of one volume group on one
// host. Returns nil without error when the group holds no thin pool.
func (sm *StorageManager) readThinUsage(ctx context.Context, address, vgName string) (*PoolThinInfo, error) {
	log := loggerOfController(sm.controller)
	start := time.Now()
	res, err := sm.controller.deployment.LVSThinReport(ctx, []string{address}, vgName)
	if err != nil {
		log.Debug("readThinUsage: lvs thin report failed", zap.String("host", address), zap.String("pool", vgName),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, fmt.Errorf("read thin pool usage of %s: %w", vgName, err)
	}
	for _, r := range res.Hosts {
		if !r.Success {
			log.Debug("readThinUsage: lvs thin report failed on host", zap.String("host", r.Host),
				zap.String("pool", vgName), zap.Duration("took", time.Since(start)))
			return nil, fmt.Errorf("read thin pool usage of %s: %s", vgName, strings.TrimSpace(r.Output))
		}
		info := parseThinReport(r.Output)[vgName]
		if info != nil {
			log.Debug("readThinUsage: read", zap.String("host", r.Host), zap.String("pool", vgName),
				zap.String("thin_pool", info.PoolLV), zap.Float64("data_percent", info.DataPercent),
				zap.Float64("meta_percent", info.MetaPercent), zap.Bool("out_of_space", info.OutOfSpace),
				zap.Duration("took", time.Since(start)))
		} else {
			log.Debug("readThinUsage: group holds no thin pool", zap.String("host", r.Host), zap.String("pool", vgName),
				zap.Duration("took", time.Since(start)))
		}
		return info, nil
	}
	log.Debug("readThinUsage: no host result", zap.String("host", address), zap.String("pool", vgName))
	return nil, fmt.Errorf("read thin pool usage of %s: no result", vgName)
}

// thinUsageByPool reports the thin pool utilisation of every volume group on
// every host, indexed by normalized node name and then by volume group.
//
// One lvs call for the whole cluster, for the same reason cacheByPool does it:
// this runs on the pool listing path, and a per-pool query would turn a cheap
// `pool list` into one SSH round per pool. A host that cannot answer is left
// out rather than failing the listing — a pool card missing its utilisation is
// a smaller problem than a pool list that errors.
func (sm *StorageManager) thinUsageByPool(ctx context.Context, hosts []string) map[string]map[string]*PoolThinInfo {
	log := loggerOfController(sm.controller)
	out := make(map[string]map[string]*PoolThinInfo)
	if len(hosts) == 0 {
		log.Debug("thinUsageByPool: no hosts")
		return out
	}
	start := time.Now()
	res, err := sm.controller.deployment.LVSThinReport(ctx, hosts, "")
	log.Debug("thinUsageByPool: lvs thin report", zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)),
		zap.Error(err))
	if err != nil {
		sm.controller.logger.Warn("Failed to read thin pool usage", zap.Error(err))
		return out
	}
	for host, r := range res.Hosts {
		if !r.Success {
			log.Debug("thinUsageByPool: host failed, left out", zap.String("host", host))
			continue
		}
		normalized := sm.controller.NormalizeHost(host)
		if normalized == "" {
			normalized = host
		}
		for vg, info := range parseThinReport(r.Output) {
			if out[normalized] == nil {
				out[normalized] = make(map[string]*PoolThinInfo)
			}
			out[normalized][vg] = info
		}
		log.Debug("thinUsageByPool: host read", zap.String("host", host), zap.String("node", normalized),
			zap.Int("count", len(out[normalized])))
	}
	return out
}
