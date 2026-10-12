package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
)

// A replica added to a running resource syncs the whole volume onto the new
// node. On a thin pool the volume is created without reserving anything, so
// the add itself always succeeds — and the sync then writes until the pool has
// no space left. The kernel answers the next write with an I/O error, DRBD
// drops the disk, and the "new replica" is Diskless while the resource list
// still shows it as a replica. Seen on a test cluster: a 2 GiB volume added
// to a node whose pool had 850 MiB free.
//
// Placement already asks a pool whether it admits a volume; adding a replica
// to a named node skipped the question.

// knownPoolFree is the free bytes of pool on each node that reported them.
// A node whose pool did not report a figure — or has no such pool — is absent:
// not knowing is not a refusal, and if the pool really is missing there the
// lvcreate or lvresize that follows fails on its own, plainly.
func (rm *ResourceManager) knownPoolFree(ctx context.Context, pool string) (map[string]uint64, error) {
	log := rm.controller.traceLogger()
	log.Debug("knownPoolFree: start", zap.String("pool", pool))
	pools, err := rm.controller.storage.ListPools(ctx)
	if err != nil {
		log.Debug("knownPoolFree: listing pools failed", zap.String("pool", pool), zap.Error(err))
		return nil, err
	}
	nodes, err := rm.controller.nodes.ListNodes(ctx)
	if err != nil {
		log.Debug("knownPoolFree: listing nodes failed", zap.Error(err))
		return nil, err
	}
	nameByAddr := make(map[string]string, len(nodes)*2)
	for _, n := range nodes {
		nameByAddr[n.Address] = n.Name
		nameByAddr[rm.controller.ResolveHost(n.Address)] = n.Name
	}
	recordedThin := rm.poolRecordedThin(ctx, pool)
	out := map[string]uint64{}
	for _, p := range pools {
		if normalizeManagedName(p.Name) != pool {
			continue
		}
		c := poolPlacementCapacity(p, recordedThin)
		if !c.known {
			log.Debug("knownPoolFree: free space unknown on node", zap.String("pool", pool), zap.String("node", p.Node))
			continue
		}
		name := nameByAddr[p.Node]
		if name == "" {
			name = p.Node
		}
		log.Debug("knownPoolFree: node free space", zap.String("pool", pool), zap.String("node", name),
			zap.Uint64("free_bytes", c.freeBytes))
		out[name] = c.freeBytes
	}
	log.Debug("knownPoolFree: done", zap.String("pool", pool), zap.Bool("recorded_thin", recordedThin),
		zap.Int("count", len(out)))
	return out, nil
}

// assertPoolRoom refuses to start a sync that the node's pool cannot hold.
// Unknown is not a refusal: when the pool or the volume size cannot be read,
// the add goes on as it did before.
func (rm *ResourceManager) assertPoolRoom(ctx context.Context, resource, node string) error {
	log := rm.controller.traceLogger()
	pool, sizeGB := rm.memberPoolAndSize(ctx, resource, "")
	log.Debug("assertPoolRoom: start", zap.String("resource", resource), zap.String("node", node),
		zap.String("pool", pool), zap.Uint64("size_gb", sizeGB))
	if pool == "" || sizeGB == 0 {
		log.Debug("assertPoolRoom: pool or size unknown, not checked", zap.String("resource", resource))
		return nil
	}
	pool = normalizeManagedName(pool)
	// Placement's own test is not the one wanted here: it lets a thin pool take
	// a volume larger than the pool, because a new thin volume is empty. A sync
	// is not — it writes the volume in full — so the pool's free bytes are
	// compared with the volume's size directly.
	free, err := rm.knownPoolFree(ctx, pool)
	if err != nil {
		log.Debug("assertPoolRoom: free space unreadable, not checked", zap.String("pool", pool), zap.Error(err))
		return nil
	}
	have, known := free[node]
	if !known || have >= sizeGB<<30 {
		log.Debug("assertPoolRoom: room enough or unknown", zap.String("pool", pool), zap.String("node", node),
			zap.Bool("known", known), zap.Uint64("free_bytes", have))
		return nil
	}
	log.Debug("assertPoolRoom: pool too small for the sync", zap.String("pool", pool), zap.String("node", node),
		zap.Uint64("free_bytes", have), zap.Uint64("size_gb", sizeGB))
	return fmt.Errorf("the pool %s on %s has %s free and %s is %d GiB, which the sync writes in full: "+
		"a full pool drops the new replica's disk. Free space or grow the pool first, or add it anyway with --ignore-free-space",
		strings.TrimPrefix(pool, managedNamePrefix), node, freeText(have), resource, sizeGB)
}

// freeText writes a byte count the way an operator reads pool space: in MiB
// below a GiB, where whole GiB would round 850 MiB down to "0".
func freeText(b uint64) string {
	if b < 1<<30 {
		return fmt.Sprintf("%d MiB", b>>20)
	}
	return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
}

// assertPoolRoomForGrowth is assertPoolRoom for a resize: the added area is
// resynced onto every replica, so on a thin pool the growth counts as used
// space on each node — and one node short of it drops its disk, as a resync
// onto a full pool did to one test node while a 3 GiB volume grew to
// 4 GiB. Volumes on ZFS are not checked here.
func (rm *ResourceManager) assertPoolRoomForGrowth(ctx context.Context, resource string, volumeID uint32, newSizeGB uint64) error {
	log := rm.controller.traceLogger()
	log.Debug("assertPoolRoomForGrowth: start", zap.String("resource", resource), zap.Uint32("volume", volumeID),
		zap.Uint64("new_size_gb", newSizeGB))
	if rm.controller.db == nil {
		log.Debug("assertPoolRoomForGrowth: no database, not checked")
		return nil
	}
	dbRes, err := rm.controller.db.GetResource(ctx, resource)
	if err != nil || dbRes == nil {
		log.Debug("assertPoolRoomForGrowth: db resource not found, not checked", zap.String("resource", resource),
			zap.Error(err))
		return nil
	}
	vols, err := rm.controller.db.ListVolumes(ctx, resource)
	if err != nil {
		log.Debug("assertPoolRoomForGrowth: db list volumes failed, not checked", zap.String("resource", resource),
			zap.Error(err))
		return nil
	}
	var vol *database.Volume
	for _, v := range vols {
		if v.VolumeID == int(volumeID) {
			vol = v
		}
	}
	if vol == nil || vol.Pool == "" || strings.HasPrefix(vol.Device, "/dev/zvol/") {
		log.Debug("assertPoolRoomForGrowth: volume unknown, poolless or on ZFS, not checked",
			zap.String("resource", resource), zap.Bool("found", vol != nil))
		return nil
	}
	pool := normalizeManagedName(vol.Pool)
	free, err := rm.knownPoolFree(ctx, pool)
	if err != nil {
		log.Debug("assertPoolRoomForGrowth: free space unreadable, not checked", zap.String("pool", pool), zap.Error(err))
		return nil
	}
	newBytes := newSizeGB << 30
	var short []string
	var worst uint64
	for _, n := range splitCSV(dbRes.Nodes) {
		have, known := free[n]
		if !known {
			log.Debug("assertPoolRoomForGrowth: free space unknown on node", zap.String("node", n))
			continue
		}
		// The growth is measured against the volume as it is on that node, not
		// against the recorded size: the record can lag (a resize that failed
		// after the LVs grew), and a retry that asks for the size a volume
		// already has must not be counted as growth.
		current := uint64(vol.SizeGB) << 30
		if live, ok := rm.liveVolumeBytes(ctx, n, vol); ok {
			current = live
		}
		if newBytes <= current {
			log.Debug("assertPoolRoomForGrowth: no growth on node", zap.String("node", n),
				zap.Uint64("current_bytes", current))
			continue
		}
		growth := newBytes - current
		log.Debug("assertPoolRoomForGrowth: node checked", zap.String("node", n), zap.Uint64("free_bytes", have),
			zap.Uint64("growth_bytes", growth), zap.Bool("fits", have >= growth))
		if have < growth {
			short = append(short, fmt.Sprintf("%s (%s free)", n, freeText(have)))
			worst = max(worst, growth)
		}
	}
	if len(short) == 0 {
		log.Debug("assertPoolRoomForGrowth: every node has room", zap.String("resource", resource))
		return nil
	}
	log.Debug("assertPoolRoomForGrowth: nodes short of room", zap.String("resource", resource),
		zap.Strings("nodes", short))
	return fmt.Errorf("growing volume %d of %s to %d GiB writes the new area (up to %s) to every replica, and %s cannot hold that: "+
		"a full pool drops the disk. Free space or grow those pools first, or resize anyway with --ignore-free-space",
		volumeID, resource, newSizeGB, freeText(worst), strings.Join(short, ", "))
}

// liveVolumeBytes reads the size of a volume's backing LV on one node.
func (rm *ResourceManager) liveVolumeBytes(ctx context.Context, node string, vol *database.Volume) (uint64, bool) {
	log := rm.controller.traceLogger()
	if vol.VolumeName == "" {
		log.Debug("liveVolumeBytes: volume has no LV name", zap.String("node", node))
		return 0, false
	}
	host := rm.controller.ResolveHost(node)
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, []string{host},
		fmt.Sprintf("sudo lvs --noheadings --nosuffix --units b -o lv_size %s/%s", vol.Pool, vol.VolumeName))
	if err != nil {
		log.Debug("liveVolumeBytes: lvs failed", zap.String("host", host), zap.String("pool", vol.Pool),
			zap.String("volume", vol.VolumeName), zap.Duration("took", time.Since(start)), zap.Error(err))
		return 0, false
	}
	for _, hr := range res.Hosts {
		if !hr.Success {
			log.Debug("liveVolumeBytes: lvs failed on host", zap.String("host", hr.Host),
				zap.String("volume", vol.VolumeName), zap.Duration("took", time.Since(start)))
			return 0, false
		}
		n, err := strconv.ParseUint(strings.TrimSpace(hr.Output), 10, 64)
		log.Debug("liveVolumeBytes: read", zap.String("host", hr.Host), zap.String("volume", vol.VolumeName),
			zap.Uint64("size_bytes", n), zap.Duration("took", time.Since(start)), zap.Error(err))
		return n, err == nil && n > 0
	}
	log.Debug("liveVolumeBytes: no host result", zap.String("host", host))
	return 0, false
}
