package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// VDO-backed thin pools ("lvm-thin-vdo").
//
// lvm2 2.03.24 can put a thin pool's data area on a VDO volume
// (`lvcreate --type thin-pool --pooldatavdo y`), so every thin volume in the
// pool gets deduplication and compression underneath while everything above
// it — thin volumes, thin snapshots, DRBD — works exactly as on a plain thin
// pool. To the rest of the controller the pool is a thin pool; this file holds
// what differs:
//
//   - creation probes the node first: dm-vdo in the kernel (Ubuntu 24.04's GA
//     kernel lacks it), vdoformat, and an lvcreate that knows --pooldatavdo.
//     Without the probe the failure surfaces as an lvcreate error naming a
//     segment type, after the PVs and the group already exist;
//   - the pool has two fullness figures. The thin pool's data_percent is the
//     *logical* space handed out; the VDO pool's data_percent is the
//     *physical* space dedup and compression did not save. Physical
//     exhaustion is the one that hurts — VDO turns writes into I/O errors and
//     the thin pool above it does not know why — so it gets alerts of its own;
//   - encryption is refused: dm-crypt above VDO hands it ciphertext, which
//     neither deduplicates nor compresses, so the pool would cost VDO's memory
//     and CPU for nothing.
//
// VDO sits under DRBD, so it deduplicates within each node's copy. DRBD
// replicates every logical block in full; VDO saves disk, not network.

// vdoPoolType is the pool type recorded for a VDO-backed thin pool.
const vdoPoolType = "thin_vdo"

// isThinPoolType reports whether a recorded pool type is a thin pool, VDO-backed
// or not.
func isThinPoolType(poolType string) bool {
	return poolType == "thin_pool" || poolType == vdoPoolType
}

// PoolVDOInfo is the physical side of a VDO-backed thin pool.
type PoolVDOInfo struct {
	// PhysicalPercent is how much of the VDO pool's physical space is used —
	// the figure that, at 100%, turns writes into I/O errors.
	PhysicalPercent float64 `json:"physical_percent"`
	// SavingPercent is what deduplication and compression save, as VDO
	// reports it.
	SavingPercent float64 `json:"saving_percent"`
}

// vdoProbeScript checks that a node can build a VDO-backed thin pool. Run
// through base64 like every script that uses $vars.
const vdoProbeScript = `missing=
modprobe dm_vdo 2>/dev/null || modprobe kvdo 2>/dev/null || missing="$missing dm-vdo-kernel-module"
command -v vdoformat >/dev/null 2>&1 || test -x /usr/sbin/vdoformat || test -x /sbin/vdoformat || missing="$missing vdoformat(vdo)"
lvcreate --help 2>&1 | grep -q pooldatavdo || missing="$missing lvm2>=2.03.24(--pooldatavdo)"
if [ -n "$missing" ]; then echo "missing:$missing"; exit 1; fi`

// assertVDOSupported probes the node a VDO-backed pool is about to be built on.
func (sm *StorageManager) assertVDOSupported(ctx context.Context, address, node string) error {
	log := sm.controller.traceLogger()
	log.Debug("assertVDOSupported: probing", zap.String("node", node), zap.String("host", address))
	cmd := "echo " + base64Std(vdoProbeScript) + " | base64 -d | sudo /bin/bash"
	start := time.Now()
	res, err := sm.controller.deployment.Exec(ctx, []string{address}, cmd)
	if err != nil {
		log.Debug("assertVDOSupported: probe errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("probe VDO support on %s: %w", node, err)
	}
	if !res.AllSuccess() {
		log.Debug("assertVDOSupported: node lacks VDO support", zap.String("host", address), zap.Duration("took", time.Since(start)))
		return fmt.Errorf("node %s cannot build a VDO-backed thin pool (%s); "+
			"it needs the dm-vdo kernel module (kernel 6.9+ or kmod-kvdo), the vdo package "+
			"for vdoformat, and lvm2 2.03.24 or later", node, strings.TrimSpace(res.FailureDetails()))
	}
	log.Debug("assertVDOSupported: supported", zap.String("host", address), zap.Duration("took", time.Since(start)))
	return nil
}

// createVDOThinPool creates the thin pool, with its data area on VDO, in the
// group just built. size is an lvcreate size ("95%FREE" or "<n>G").
func (sm *StorageManager) createVDOThinPool(ctx context.Context, address, vg, size string) error {
	log := sm.controller.traceLogger()
	flag := "-L"
	if strings.Contains(size, "%") {
		flag = "-l"
	}
	log.Debug("createVDOThinPool: creating", zap.String("host", address), zap.String("pool", vg), zap.String("size", size),
		zap.String("size_flag", flag))
	cmd := fmt.Sprintf("sudo lvcreate -y --type thin-pool --pooldatavdo y %s %s -n %s_thin %s", flag, size, vg, vg)
	start := time.Now()
	res, err := sm.controller.deployment.Exec(ctx, []string{address}, cmd)
	if err != nil {
		log.Debug("createVDOThinPool: lvcreate errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to create VDO thin pool: %w", err)
	}
	if !res.AllSuccess() {
		log.Debug("createVDOThinPool: lvcreate failed", zap.String("host", address), zap.Duration("took", time.Since(start)))
		return fmt.Errorf("failed to create VDO thin pool: %s", res.FailureDetails())
	}
	log.Debug("createVDOThinPool: created", zap.String("pool", vg), zap.Duration("took", time.Since(start)))
	return nil
}

// vdoPoolNames returns the names of the pools recorded as VDO-backed.
func (sm *StorageManager) vdoPoolNames(ctx context.Context) map[string]bool {
	log := sm.controller.traceLogger()
	out := map[string]bool{}
	if sm.controller.db == nil {
		log.Debug("vdoPoolNames: no database")
		return out
	}
	pools, err := sm.controller.db.ListPools(ctx)
	if err != nil {
		log.Debug("vdoPoolNames: pool list failed", zap.Error(err))
		return out
	}
	for _, p := range pools {
		if p.Type == vdoPoolType {
			out[p.Name] = true
		}
	}
	log.Debug("vdoPoolNames: VDO pools", zap.Int("count", len(out)))
	return out
}

// vdoUsageQuery lists every VDO pool LV with its physical usage and savings.
// It is a query of its own, not more fields on the thin report: an lvm2
// without VDO support rejects vdo_saving_percent, and the thin report has to
// work everywhere.
const vdoUsageQuery = "sudo lvs -a --noheadings --separator '|' -S segtype=vdo-pool -o vg_name,data_percent,vdo_saving_percent 2>/dev/null"

// parseVDOReport folds vdoUsageQuery's rows by volume group. A group with more
// than one VDO pool reports the fullest.
func parseVDOReport(output string) map[string]*PoolVDOInfo {
	out := map[string]*PoolVDOInfo{}
	for _, line := range strings.Split(output, "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) < 3 || strings.TrimSpace(f[0]) == "" {
			continue
		}
		vg := strings.TrimSpace(f[0])
		info := &PoolVDOInfo{
			PhysicalPercent: parseThinPercent(f[1]),
			SavingPercent:   parseThinPercent(f[2]),
		}
		if prev, ok := out[vg]; ok && prev.PhysicalPercent >= info.PhysicalPercent {
			continue
		}
		out[vg] = info
	}
	zap.L().Debug("parseVDOReport: parsed", zap.Int("groups", len(out)))
	return out
}

// attachVDOUsage fills PoolInfo.VDO for the VDO-backed pools in the listing,
// with one lvs call on the hosts holding them. It costs nothing when no pool
// is VDO-backed. A host that does not answer leaves its pools without the
// figure rather than failing the listing.
func (sm *StorageManager) attachVDOUsage(ctx context.Context, pools map[string]*PoolInfo) {
	log := sm.controller.traceLogger()
	vdo := sm.vdoPoolNames(ctx)
	if len(vdo) == 0 {
		log.Debug("attachVDOUsage: no VDO pools")
		return
	}
	var hosts []string
	seen := map[string]bool{}
	for _, p := range pools {
		if vdo[p.Name] && !seen[p.Node] {
			seen[p.Node] = true
			hosts = append(hosts, p.Node)
		}
	}
	if len(hosts) == 0 {
		log.Debug("attachVDOUsage: no VDO pool in this listing")
		return
	}
	start := time.Now()
	res, err := sm.controller.deployment.Exec(ctx, hosts, vdoUsageQuery)
	if err != nil {
		sm.controller.logger.Warn("Failed to read VDO pool usage", zap.Error(err))
		return
	}
	log.Debug("attachVDOUsage: lvs answered", zap.Strings("hosts", hosts), zap.Strings("failed_hosts", res.FailedHosts()),
		zap.Duration("took", time.Since(start)))
	byHost := map[string]map[string]*PoolVDOInfo{}
	for host, r := range res.Hosts {
		if !r.Success {
			continue
		}
		normalized := sm.controller.NormalizeHost(host)
		if normalized == "" {
			normalized = host
		}
		byHost[normalized] = parseVDOReport(r.Output)
	}
	for _, p := range pools {
		if !vdo[p.Name] {
			continue
		}
		if byVG, ok := byHost[p.Node]; ok {
			p.VDO = byVG[p.Name]
			if p.VDO != nil {
				log.Debug("attachVDOUsage: pool", zap.String("node", p.Node), zap.String("pool", p.Name),
					zap.Float64("physical_percent", p.VDO.PhysicalPercent), zap.Float64("saving_percent", p.VDO.SavingPercent))
			}
		}
	}
}

// errVDONotGrown is AddDiskToPool's answer for a VDO-backed pool. Growing one
// means growing two layers — the VDO pool's physical space and the thin pool's
// logical size above it — in a ratio only the operator can pick (it is a bet
// on the dedup rate), so the disk joins the group and the growing is left to
// the operator, as the user guide describes.
func errVDONotGrown(pool, disk, node string) error {
	zap.L().Debug("errVDONotGrown: VDO pool not grown automatically", zap.String("pool", pool), zap.String("disk", disk),
		zap.String("node", node))
	return fmt.Errorf("disk %s added to %s on %s, but %s is VDO-backed and is not grown automatically: "+
		"extend its VDO pool (physical) and then its thin pool (logical) with lvextend; see the user guide",
		disk, pool, node, pool)
}

// poolIsVDO reports whether pool is recorded as VDO-backed.
func (rm *ResourceManager) poolIsVDO(ctx context.Context, pool string) bool {
	if rm.controller.db == nil {
		rm.controller.traceLogger().Debug("poolIsVDO: no database", zap.String("pool", pool))
		return false
	}
	p, err := rm.controller.db.GetPool(ctx, normalizeManagedName(pool))
	vdo := err == nil && p != nil && p.Type == vdoPoolType
	rm.controller.traceLogger().Debug("poolIsVDO: checked", zap.String("pool", pool), zap.Bool("vdo", vdo), zap.Error(err))
	return vdo
}

// assertEncryptableVDO refuses encryption on a VDO-backed pool: dm-crypt
// above VDO hands it ciphertext, which neither deduplicates nor compresses.
func (rm *ResourceManager) assertEncryptableVDO(ctx context.Context, pools ...string) error {
	for _, pool := range pools {
		if rm.poolIsVDO(ctx, pool) {
			rm.controller.traceLogger().Debug("assertEncryptableVDO: refusing encryption on VDO pool", zap.String("pool", pool))
			return fmt.Errorf("pool %s is VDO-backed: encryption above VDO defeats its "+
				"deduplication and compression; use a plain lvm-thin pool for encrypted resources", pool)
		}
	}
	return nil
}
