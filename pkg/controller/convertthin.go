package controller

import (
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// Converting a thick LVM pool to a thin one, in place, one node at a time.
//
// A thick pool cannot really do snapshots: LVM makes the caller reserve a COW
// area per snapshot (Haify reserves 20% of the origin), so a 6 GiB volume costs
// 1.2 GiB per snapshot whether anything changes or not. On a 10 GiB pool that
// is two snapshots, which is not a retention policy. A thin snapshot costs only
// the blocks that diverge — measured on 2026-08-02, three snapshots of a 6 GiB
// origin moved the pool's usage by exactly nothing.
//
// The conversion destroys this node's backing volumes and rebuilds them from
// the peers, so the interesting part is not the LVM commands: it is refusing to
// start when the cluster cannot afford to lose this copy.

const gib = uint64(1) << 30

// thinConversionInput is everything the planner needs, gathered by the caller.
// Keeping it a plain struct is what makes the refusals testable without a
// cluster.
type thinConversionInput struct {
	Node string
	Pool string
	// VGFreeBytes is unallocated space in the volume group right now, before
	// the thick volumes are removed.
	VGFreeBytes uint64
	// Resources is every resource known to the controller; the planner selects
	// the ones with a volume in this pool.
	Resources []*ResourceInfo
	// BackingBytes maps a backing LV name to its exact size. DRBD records the
	// device size in its metadata, so a volume rebuilt from a rounded "6G"
	// is a different device and will not attach.
	BackingBytes map[string]uint64
	// AlreadyThin marks volumes that need no work.
	AlreadyThin map[string]bool
	// Present marks backing volumes that still exist on this node. A volume can
	// be missing because an earlier attempt removed it and then failed, which is
	// the state a rerun has to be able to pick up from. A name absent from the
	// map is treated as present, so callers that never lose a volume can ignore
	// this field.
	Present map[string]bool
	// ThinPoolExists reports whether the pool already has a thin pool — because
	// an earlier attempt built it, or because the pool is half converted.
	ThinPoolExists bool
	// ThinPoolMetadataBytes is that pool's current metadata area, so an
	// undersized one can be grown while the extents are free to do it with.
	ThinPoolMetadataBytes uint64
	// ExistingThinPool is what that pool is called. It is whatever the node
	// reports, not a constant: a pool built by `pool create` is named
	// "<pool>_thin" and one this code creates is named thinPoolName, and
	// assuming either would build a second pool beside the first.
	ExistingThinPool string
	// DRBDName maps a Haify node name to the name DRBD reports it by. The two
	// differ on most clusters — Haify knows "node-b", DRBD says "haify-b" — and
	// live resource state is keyed by the latter.
	DRBDName map[string]string
}

type thinVolumePlan struct {
	Resource  string
	LV        string
	SizeBytes uint64
	// NeedsTeardown is false when a previous attempt already removed the thick
	// volume. Detaching and removing again would both fail.
	NeedsTeardown bool
	// Encrypted marks a volume DRBD reaches through a LUKS container. The
	// container is closed before the thick LV goes and the thin one is wrapped
	// in a new one before DRBD attaches, so the rebuilt copy is not plaintext.
	Encrypted bool
}

type thinConversionPlan struct {
	Node         string
	Pool         string
	ThinPoolName string
	// PoolBytes is what the pool is expected to come out at. The command asks
	// LVM for every free extent rather than for this number; see
	// LVCreateThinPoolAllFree for why.
	PoolBytes     uint64
	MetadataBytes uint64
	CreatePool    bool
	// ExtendPool folds the freed extents into a thin pool that already exists.
	ExtendPool bool
	// MetadataGrowTo is the size to raise an existing pool's metadata area to,
	// or zero to leave it alone.
	MetadataGrowTo uint64
	Volumes        []thinVolumePlan
}

// thinPoolName is the LV the converted volumes live inside.
const thinPoolName = "haifythin"

// planThinConversion decides whether this node's pool can be rebuilt as thin
// right now, and how big to make the pool.
func planThinConversion(in *thinConversionInput) (*thinConversionPlan, error) {
	log := zap.L()
	log.Debug("planThinConversion: planning", zap.String("node", in.Node), zap.String("pool", in.Pool),
		zap.Uint64("vg_free_bytes", in.VGFreeBytes), zap.Int("resources", len(in.Resources)),
		zap.Bool("thin_pool_exists", in.ThinPoolExists), zap.String("existing_thin_pool", in.ExistingThinPool),
		zap.Uint64("thin_pool_metadata_bytes", in.ThinPoolMetadataBytes))
	if in.Node == "" || in.Pool == "" {
		log.Debug("planThinConversion: node or pool missing")
		return nil, fmt.Errorf("node and pool are both required")
	}

	var vols []thinVolumePlan
	var alreadyThin int
	var originTotal uint64
	// freeable is what removing the still-present thick volumes gives back.
	// Anything an earlier attempt already removed is counted in VGFreeBytes, and
	// counting it twice would size the pool against space that does not exist.
	var freeable uint64

	for _, res := range in.Resources {
		for _, v := range res.Volumes {
			if v.Pool != in.Pool {
				log.Debug("planThinConversion: volume in another pool, skipped", zap.String("resource", res.Name),
					zap.String("volume", v.BackingVolume), zap.String("volume_pool", v.Pool))
				continue // a different pool on the same node is not our business
			}
			if !hasNode(res, in.Node) {
				log.Debug("planThinConversion: resource not on node, skipped", zap.String("resource", res.Name))
				continue
			}
			if in.AlreadyThin[v.BackingVolume] {
				log.Debug("planThinConversion: volume already thin", zap.String("resource", res.Name),
					zap.String("volume", v.BackingVolume))
				alreadyThin++
				continue // a half-converted pool still has work in the rest of it
			}
			if err := checkSafeToRebuild(res, in.Node, in.DRBDName[in.Node]); err != nil {
				log.Debug("planThinConversion: unsafe to rebuild", zap.String("resource", res.Name), zap.Error(err))
				return nil, err
			}
			size, ok := in.BackingBytes[v.BackingVolume]
			if !ok || size == 0 {
				log.Debug("planThinConversion: exact size unknown", zap.String("resource", res.Name),
					zap.String("volume", v.BackingVolume), zap.Bool("found", ok))
				return nil, fmt.Errorf("could not read the exact size of %s/%s; refusing to guess",
					in.Pool, v.BackingVolume)
			}
			present := true
			if p, ok := in.Present[v.BackingVolume]; ok {
				present = p
			}
			log.Debug("planThinConversion: volume to convert", zap.String("resource", res.Name),
				zap.String("volume", v.BackingVolume), zap.Uint64("size_bytes", size),
				zap.Bool("present", present), zap.Bool("encrypted", v.Encrypted))
			vols = append(vols, thinVolumePlan{
				Resource: res.Name, LV: v.BackingVolume, SizeBytes: size, NeedsTeardown: present,
				Encrypted: v.Encrypted,
			})
			originTotal += size
			if present {
				freeable += size
			}
		}
	}
	if len(vols) == 0 {
		log.Debug("planThinConversion: nothing to convert", zap.Int("already_thin", alreadyThin))
		if alreadyThin > 0 {
			return nil, fmt.Errorf("every volume in %s on %s is already thin; nothing to convert",
				in.Pool, in.Node)
		}
		return nil, fmt.Errorf("no thick volumes of a known resource found in %s on %s", in.Pool, in.Node)
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].LV < vols[j].LV })

	// The pool is already there — either an earlier attempt built it, or this
	// pool is half converted. Either way it is extended, not rebuilt: creating
	// it again fails, and leaving it alone would strand the freed extents.
	if in.ThinPoolExists {
		var grow uint64
		if in.ThinPoolMetadataBytes < thinMetadataFloor {
			grow = thinMetadataFloor
		}
		name := in.ExistingThinPool
		if name == "" {
			name = thinPoolName
		}
		log.Debug("planThinConversion: extending existing thin pool", zap.String("thin_pool", name),
			zap.Uint64("metadata_grow_to", grow), zap.Int("volumes", len(vols)))
		return &thinConversionPlan{
			Node: in.Node, Pool: in.Pool, ThinPoolName: name,
			CreatePool: false, ExtendPool: true, MetadataGrowTo: grow, Volumes: vols,
		}, nil
	}

	// Removing the thick volumes returns their extents, so the pool can be as
	// large as those plus whatever was already free — minus the metadata area
	// *and* the spare copy of it that LVM allocates alongside.
	usable := freeable + in.VGFreeBytes
	metadata := thinMetadataBytes(usable)
	reserved := 2 * metadata
	log.Debug("planThinConversion: sizing new thin pool", zap.Uint64("usable_bytes", usable),
		zap.Uint64("origin_bytes", originTotal), zap.Uint64("metadata_bytes", metadata))
	if usable <= originTotal+reserved {
		log.Debug("planThinConversion: no headroom for snapshots", zap.Uint64("usable_bytes", usable),
			zap.Uint64("needed_bytes", originTotal+reserved))
		return nil, fmt.Errorf(
			"converting %s on %s would leave no headroom for snapshots (%d MiB free beyond the volumes); "+
				"grow the volume group first", in.Pool, in.Node, (usable-originTotal)/(1<<20))
	}
	poolBytes := usable - reserved
	log.Debug("planThinConversion: creating thin pool", zap.String("thin_pool", thinPoolName),
		zap.Uint64("pool_bytes", poolBytes), zap.Int("volumes", len(vols)))

	return &thinConversionPlan{
		Node:          in.Node,
		Pool:          in.Pool,
		ThinPoolName:  thinPoolName,
		PoolBytes:     poolBytes,
		MetadataBytes: metadata,
		CreatePool:    true,
		Volumes:       vols,
	}, nil
}

// checkSafeToRebuild refuses the cases where losing this node's copy — for the
// length of a full resync — is not something the resource can absorb.
func checkSafeToRebuild(res *ResourceInfo, node, drbdName string) error {
	log := zap.L()
	log.Debug("checkSafeToRebuild: checking", zap.String("resource", res.Name), zap.String("node", node),
		zap.String("drbd_name", drbdName))
	st, ok := liveState(res, node, drbdName)
	if !ok {
		log.Debug("checkSafeToRebuild: no live state", zap.String("resource", res.Name), zap.String("node", node))
		return fmt.Errorf("no live DRBD state for %s on %s; refusing to convert blind", res.Name, node)
	}
	if strings.EqualFold(st.Role, "Primary") {
		log.Debug("checkSafeToRebuild: node is Primary", zap.String("resource", res.Name), zap.String("node", node))
		return fmt.Errorf("%s is Primary on %s; move it first (haify ha evict, or resource secondary)",
			res.Name, node)
	}

	// Count what would still hold data, and make sure none of it is busy.
	diskful := 0
	for peer, ps := range res.NodeStates {
		if peer == node || (drbdName != "" && peer == drbdName) {
			continue
		}
		log.Debug("checkSafeToRebuild: peer state", zap.String("resource", res.Name), zap.String("peer", peer),
			zap.String("disk_state", ps.DiskState), zap.String("replication", ps.Replication))
		if isResyncing(ps.Replication) {
			log.Debug("checkSafeToRebuild: peer resyncing", zap.String("resource", res.Name), zap.String("peer", peer))
			return fmt.Errorf("%s is mid-resync on %s (%s, %.0f%%); wait for it to finish",
				res.Name, peer, ps.Replication, ps.SyncPercent)
		}
		if ps.DiskState == "Diskless" {
			continue // a tiebreaker votes but holds nothing
		}
		if ps.DiskState != "UpToDate" {
			log.Debug("checkSafeToRebuild: peer not UpToDate", zap.String("resource", res.Name), zap.String("peer", peer))
			return fmt.Errorf("%s is %s on %s; every other replica must be UpToDate first",
				res.Name, ps.DiskState, peer)
		}
		diskful++
	}
	if diskful < 2 {
		log.Debug("checkSafeToRebuild: too few diskful copies", zap.String("resource", res.Name), zap.Int("diskful", diskful))
		return fmt.Errorf(
			"converting %s on %s would leave only one diskful copy while it resyncs; "+
				"add a replica first", res.Name, node)
	}
	log.Debug("checkSafeToRebuild: safe", zap.String("resource", res.Name), zap.String("node", node), zap.Int("diskful", diskful))
	return nil
}

func isResyncing(replication string) bool {
	switch replication {
	case "", "Established", "Off", "WFReportParams":
		return false
	}
	return strings.Contains(replication, "Sync") || strings.Contains(replication, "Ahead") ||
		strings.Contains(replication, "Behind")
}

// liveState finds a node's DRBD state under whichever name the resource reports
// it by.
//
// NodeStates comes from `drbdadm status`, so it is keyed by the hostname DRBD
// knows — "haify-b", "iZ2vca1rjuuxbqtpm9hy7zZ" — while the caller holds the name
// Haify registered, "node-b". On a cluster where those two happen to match the
// difference is invisible, which is how looking up only the Haify name survived
// review and then refused every node on a real cluster.
func liveState(res *ResourceInfo, node, drbdName string) (*ResourceNodeState, bool) {
	if st, ok := res.NodeStates[node]; ok {
		zap.L().Debug("liveState: found under node name", zap.String("resource", res.Name), zap.String("node", node))
		return st, true
	}
	if drbdName != "" {
		if st, ok := res.NodeStates[drbdName]; ok {
			zap.L().Debug("liveState: found under DRBD name", zap.String("resource", res.Name),
				zap.String("node", node), zap.String("drbd_name", drbdName))
			return st, true
		}
	}
	zap.L().Debug("liveState: not found", zap.String("resource", res.Name), zap.String("node", node),
		zap.String("drbd_name", drbdName))
	return nil, false
}

func hasNode(res *ResourceInfo, node string) bool {
	for _, n := range res.Nodes {
		if strings.TrimSpace(n) == node {
			return true
		}
	}
	return false
}

// thinMetadataFloor is the smallest metadata area worth creating. LVM's own
// default is around 8 MiB, which a single converted volume already fills to
// 30%; exhausting metadata takes the whole pool read-only.
const thinMetadataFloor = 128 << 20

// thinMetadataBytes sizes the pool's metadata area.
//
// LVM's default is small enough that a pool carrying many snapshots exhausts
// metadata long before it exhausts data — and a full metadata area takes the
// pool read-only, which is a far worse failure than simply running out of
// space. Measured on a freshly converted 8 GiB pool with a single volume:
// 30% of the default area was already gone. Reserve ~1% of the pool, floored
// at 128 MiB, which LVM accepts and which leaves room for hundreds of
// snapshots.
//
// The result is a whole number of MiB: lvextend refuses a byte size that is
// not a multiple of 512, and 1% of a real pool's size rarely is one.
func thinMetadataBytes(poolBytes uint64) uint64 {
	const mib = 1 << 20
	m := (poolBytes/100 + mib - 1) / mib * mib
	if m < thinMetadataFloor {
		zap.L().Debug("thinMetadataBytes: using floor", zap.Uint64("pool_bytes", poolBytes), zap.Uint64("metadata_bytes", thinMetadataFloor))
		return thinMetadataFloor
	}
	// LVM caps thin metadata at 16 GiB.
	if m > 16*gib {
		zap.L().Debug("thinMetadataBytes: capped at 16 GiB", zap.Uint64("pool_bytes", poolBytes))
		return 16 * gib
	}
	zap.L().Debug("thinMetadataBytes: one percent of pool", zap.Uint64("pool_bytes", poolBytes), zap.Uint64("metadata_bytes", m))
	return m
}
