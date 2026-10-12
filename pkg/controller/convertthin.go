package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/deployment"
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
	if in.Node == "" || in.Pool == "" {
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
				continue // a different pool on the same node is not our business
			}
			if !hasNode(res, in.Node) {
				continue
			}
			if in.AlreadyThin[v.BackingVolume] {
				alreadyThin++
				continue // a half-converted pool still has work in the rest of it
			}
			if err := checkSafeToRebuild(res, in.Node, in.DRBDName[in.Node]); err != nil {
				return nil, err
			}
			size, ok := in.BackingBytes[v.BackingVolume]
			if !ok || size == 0 {
				return nil, fmt.Errorf("could not read the exact size of %s/%s; refusing to guess",
					in.Pool, v.BackingVolume)
			}
			present := true
			if p, ok := in.Present[v.BackingVolume]; ok {
				present = p
			}
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
	if usable <= originTotal+reserved {
		return nil, fmt.Errorf(
			"converting %s on %s would leave no headroom for snapshots (%d MiB free beyond the volumes); "+
				"grow the volume group first", in.Pool, in.Node, (usable-originTotal)/(1<<20))
	}
	poolBytes := usable - reserved

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
	st, ok := liveState(res, node, drbdName)
	if !ok {
		return fmt.Errorf("no live DRBD state for %s on %s; refusing to convert blind", res.Name, node)
	}
	if strings.EqualFold(st.Role, "Primary") {
		return fmt.Errorf("%s is Primary on %s; move it first (haify ha evict, or resource secondary)",
			res.Name, node)
	}

	// Count what would still hold data, and make sure none of it is busy.
	diskful := 0
	for peer, ps := range res.NodeStates {
		if peer == node || (drbdName != "" && peer == drbdName) {
			continue
		}
		if isResyncing(ps.Replication) {
			return fmt.Errorf("%s is mid-resync on %s (%s, %.0f%%); wait for it to finish",
				res.Name, peer, ps.Replication, ps.SyncPercent)
		}
		if ps.DiskState == "Diskless" {
			continue // a tiebreaker votes but holds nothing
		}
		if ps.DiskState != "UpToDate" {
			return fmt.Errorf("%s is %s on %s; every other replica must be UpToDate first",
				res.Name, ps.DiskState, peer)
		}
		diskful++
	}
	if diskful < 2 {
		return fmt.Errorf(
			"converting %s on %s would leave only one diskful copy while it resyncs; "+
				"add a replica first", res.Name, node)
	}
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
		return st, true
	}
	if drbdName != "" {
		if st, ok := res.NodeStates[drbdName]; ok {
			return st, true
		}
	}
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

// ConvertPoolToThin rebuilds one node's pool as an LVM thin pool.
//
// Only one node at a time, on purpose: the node's copy is destroyed and
// resynced in full from its peers, and a full 6 GiB resync is minutes on a LAN
// and around half an hour to an off-site replica. Doing two at once would
// stack those windows.
func (rm *ResourceManager) ConvertPoolToThin(ctx context.Context, nodeName, poolName string) error {
	if rm.deployment == nil {
		return fmt.Errorf("deployment client not set")
	}
	host := rm.controller.nodes.GetNodeAddressByName(nodeName)
	if host == "" {
		return fmt.Errorf("node %q is not registered", nodeName)
	}

	resources, err := rm.liveResourcesOn(ctx, nodeName)
	if err != nil {
		return err
	}

	in := &thinConversionInput{
		Node:         nodeName,
		Pool:         poolName,
		Resources:    resources,
		BackingBytes: map[string]uint64{},
		AlreadyThin:  map[string]bool{},
		Present:      map[string]bool{},
		DRBDName: map[string]string{
			nodeName: rm.controller.nodes.GetDRBDNameByRef(nodeName),
		},
	}
	if in.VGFreeBytes, err = rm.deployment.VGFreeBytes(ctx, host, poolName); err != nil {
		return fmt.Errorf("inspect %s on %s: %w", poolName, nodeName, err)
	}
	if in.ExistingThinPool, err = rm.deployment.LVThinPoolIn(ctx, host, poolName); err != nil {
		return fmt.Errorf("look for a thin pool in %s on %s: %w", poolName, nodeName, err)
	}
	in.ThinPoolExists = in.ExistingThinPool != ""
	if in.ThinPoolExists {
		if in.ThinPoolMetadataBytes, err = rm.deployment.LVSizeBytes(ctx, host, poolName,
			in.ExistingThinPool+"_tmeta"); err != nil {
			return fmt.Errorf("read the metadata size of %s/%s on %s: %w",
				poolName, in.ExistingThinPool, nodeName, err)
		}
	}
	for _, res := range resources {
		for _, v := range res.Volumes {
			if v.Pool != poolName || !hasNode(res, nodeName) {
				continue
			}
			thin, terr := rm.deployment.LVIsThin(ctx, host, poolName, v.BackingVolume)
			if terr != nil {
				return fmt.Errorf("check whether %s/%s is thin: %w", poolName, v.BackingVolume, terr)
			}
			in.AlreadyThin[v.BackingVolume] = thin

			present, perr := rm.deployment.LVExists(ctx, host, poolName, v.BackingVolume)
			if perr != nil {
				return fmt.Errorf("look for %s/%s on %s: %w", poolName, v.BackingVolume, nodeName, perr)
			}
			in.Present[v.BackingVolume] = present

			// Every replica of a DRBD volume is the same number of bytes, so a
			// peer is an equally good source — and the only one left when an
			// earlier attempt already removed this node's copy.
			sizeHost := host
			if !present {
				if sizeHost, err = rm.peerHostWith(res, nodeName); err != nil {
					return err
				}
			}
			size, serr := rm.deployment.LVSizeBytes(ctx, sizeHost, poolName, v.BackingVolume)
			if serr != nil {
				return fmt.Errorf("read size of %s/%s: %w", poolName, v.BackingVolume, serr)
			}
			in.BackingBytes[v.BackingVolume] = size
		}
	}

	plan, err := planThinConversion(in)
	if err != nil {
		return err
	}

	rm.controller.logger.Info("Converting pool to thin",
		zap.String("node", nodeName), zap.String("pool", poolName),
		zap.Uint64("expected_pool_bytes", plan.PoolBytes), zap.Int("volumes", len(plan.Volumes)))

	return rm.applyThinConversion(ctx, host, plan)
}

// applyThinConversion carries out an approved plan. Every step past the first
// removal is destructive-in-progress: the node holds no copy until the last
// attach, so a step that fails has to say so. It is kept apart from planning
// and from gathering so that failure handling is testable without a cluster.
func (rm *ResourceManager) applyThinConversion(ctx context.Context, host string, plan *thinConversionPlan) error {
	log := rm.controller.logger

	// Detach and remove every volume first, then build the pool from all the
	// freed extents. Interleaving would size the pool against space the other
	// volumes still hold.
	for _, v := range plan.Volumes {
		if !v.NeedsTeardown {
			continue // an earlier attempt already removed it
		}
		if err := execFailure(rm.deployment.DRBDDetach(ctx, host, v.Resource)); err != nil {
			return fmt.Errorf("detach %s on %s: %w", v.Resource, plan.Node, err)
		}
		// The open container holds the LV, so lvremove would refuse. Its key
		// goes too: the volume it protected is about to be released.
		if v.Encrypted {
			rm.closeBackingVolumeOn(ctx, []string{host}, plan.Pool, v.LV)
		}
		if err := execFailure(rm.deployment.LVRemove(ctx, []string{host},
			fmt.Sprintf("%s/%s", plan.Pool, v.LV))); err != nil {
			return fmt.Errorf("remove %s/%s (the node is now diskless; rerun to finish): %w",
				plan.Pool, v.LV, err)
		}
	}

	switch {
	case plan.CreatePool:
		if err := execFailure(rm.deployment.LVCreateThinPoolAllFree(ctx, []string{host},
			plan.Pool, plan.ThinPoolName, plan.MetadataBytes)); err != nil {
			return fmt.Errorf("create thin pool %s/%s (the node is diskless; rerun to finish): %w",
				plan.Pool, plan.ThinPoolName, err)
		}
	case plan.ExtendPool:
		// Metadata first — see LVExtendThinPoolMetadata for why the order is
		// not interchangeable.
		if plan.MetadataGrowTo > 0 {
			if err := execFailure(rm.deployment.LVExtendThinPoolMetadata(ctx, []string{host},
				plan.Pool, plan.ThinPoolName, plan.MetadataGrowTo)); err != nil {
				return fmt.Errorf("grow the metadata area of %s/%s: %w", plan.Pool, plan.ThinPoolName, err)
			}
		}
		if err := execFailure(rm.deployment.LVExtendThinPoolAllFree(ctx, []string{host},
			plan.Pool, plan.ThinPoolName)); err != nil {
			return fmt.Errorf("grow %s/%s into the freed extents (the node is diskless; rerun to finish): %w",
				plan.Pool, plan.ThinPoolName, err)
		}
	}

	for _, v := range plan.Volumes {
		if err := execFailure(rm.deployment.LVCreateThinVolume(ctx, []string{host},
			plan.Pool, plan.ThinPoolName, v.LV, fmt.Sprintf("%dB", v.SizeBytes))); err != nil {
			return fmt.Errorf("create thin volume %s/%s: %w", plan.Pool, v.LV, err)
		}
		// The config still names the container, so DRBD attaches nothing
		// until it exists again, under a key made fresh on this node. The size
		// was read from the old LV, header included.
		if v.Encrypted {
			if err := rm.encryptBackingVolumeOn(ctx, host, plan.Node, plan.Pool, v.LV,
				fmt.Sprintf("/dev/%s/%s", plan.Pool, v.LV)); err != nil {
				return err
			}
		}
		if err := execFailure(rm.deployment.DRBDCreateMD(ctx, []string{host}, v.Resource,
			deployment.DefaultMaxPeers)); err != nil {
			return fmt.Errorf("create metadata for %s: %w", v.Resource, err)
		}
		if err := execFailure(rm.deployment.DRBDAttach(ctx, host, v.Resource)); err != nil {
			return fmt.Errorf("attach %s: %w", v.Resource, err)
		}
		log.Info("Volume rebuilt as thin; full resync started",
			zap.String("node", plan.Node), zap.String("volume", v.LV),
			zap.String("resource", v.Resource))
	}
	return nil
}

// peerHostWith returns the address of another node holding this resource, for
// the questions this node can no longer answer about itself.
func (rm *ResourceManager) peerHostWith(res *ResourceInfo, node string) (string, error) {
	for _, peer := range res.Nodes {
		if peer == node {
			continue
		}
		if addr := rm.controller.nodes.GetNodeAddressByName(peer); addr != "" {
			return addr, nil
		}
	}
	return "", fmt.Errorf("no registered peer of %s left to read the size of its volumes from", res.Name)
}

// execFailure collapses a deployment call's two failure channels into one.
//
// Exec returns a nil error whenever SSH itself worked, so a command that ran
// and exited non-zero — lvcreate exits 5 on "Insufficient free space" — is
// reported only through the result. Checking the error alone reports those
// runs as success, which is how a conversion that built nothing at all still
// printed "rebuilt as thin" and left an empty volume group behind.
func execFailure(res *deployment.ExecResult, err error) error {
	if err != nil {
		return err
	}
	if res == nil || res.AllSuccess() {
		return nil
	}
	var msgs []string
	for _, h := range res.Hosts {
		if !h.Success {
			msgs = append(msgs, fmt.Sprintf("%s: %s", h.Host, strings.TrimSpace(h.Output)))
		}
	}
	sort.Strings(msgs) // map iteration order must not change the message
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}

// liveResourcesOn returns live state for every resource with a replica on the
// node. Planning from the database alone would miss a resync in flight, which
// is exactly the state that must block a conversion.
func (rm *ResourceManager) liveResourcesOn(ctx context.Context, nodeName string) ([]*ResourceInfo, error) {
	if rm.controller.db == nil {
		return nil, fmt.Errorf("database not available")
	}
	dbResources, err := rm.controller.db.ListResources(ctx)
	if err != nil {
		return nil, fmt.Errorf("list resources: %w", err)
	}
	var out []*ResourceInfo
	for _, dbRes := range dbResources {
		info, gerr := rm.GetResource(ctx, dbRes.Name)
		if gerr != nil {
			// A resource whose state cannot be read might be the one that makes
			// this unsafe, so refuse rather than plan around it.
			return nil, fmt.Errorf("read live state of %s: %w", dbRes.Name, gerr)
		}
		if hasNode(info, nodeName) {
			out = append(out, info)
		}
	}
	return out, nil
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
		return thinMetadataFloor
	}
	// LVM caps thin metadata at 16 GiB.
	if m > 16*gib {
		return 16 * gib
	}
	return m
}
