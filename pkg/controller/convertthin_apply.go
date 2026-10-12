package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/deployment"
)

// Carrying out a thick-to-thin conversion on one node: gathering the live
// state convertthin.go plans from, then tearing down and rebuilding the
// volumes.

// ConvertPoolToThin rebuilds one node's pool as an LVM thin pool.
//
// Only one node at a time, on purpose: the node's copy is destroyed and
// resynced in full from its peers, and a full 6 GiB resync is minutes on a LAN
// and around half an hour to an off-site replica. Doing two at once would
// stack those windows.
func (rm *ResourceManager) ConvertPoolToThin(ctx context.Context, nodeName, poolName string) error {
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.ConvertPoolToThin: start", zap.String("node", nodeName), zap.String("pool", poolName))
	if rm.deployment == nil {
		log.Debug("ResourceManager.ConvertPoolToThin: no deployment client")
		return fmt.Errorf("deployment client not set")
	}
	host := rm.controller.nodes.GetNodeAddressByName(nodeName)
	if host == "" {
		log.Debug("ResourceManager.ConvertPoolToThin: node not registered", zap.String("node", nodeName))
		return fmt.Errorf("node %q is not registered", nodeName)
	}

	resources, err := rm.liveResourcesOn(ctx, nodeName)
	if err != nil {
		log.Debug("ResourceManager.ConvertPoolToThin: reading live resources failed", zap.Error(err))
		return err
	}
	log.Debug("ResourceManager.ConvertPoolToThin: live resources on node", zap.String("host", host),
		zap.Int("count", len(resources)))

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
	start := time.Now()
	if in.VGFreeBytes, err = rm.deployment.VGFreeBytes(ctx, host, poolName); err != nil {
		log.Debug("ResourceManager.ConvertPoolToThin: reading VG free space failed", zap.String("host", host),
			zap.String("pool", poolName), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("inspect %s on %s: %w", poolName, nodeName, err)
	}
	log.Debug("ResourceManager.ConvertPoolToThin: VG free space", zap.String("host", host),
		zap.Uint64("vg_free_bytes", in.VGFreeBytes), zap.Duration("took", time.Since(start)))
	start = time.Now()
	if in.ExistingThinPool, err = rm.deployment.LVThinPoolIn(ctx, host, poolName); err != nil {
		log.Debug("ResourceManager.ConvertPoolToThin: thin pool lookup failed", zap.String("host", host),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("look for a thin pool in %s on %s: %w", poolName, nodeName, err)
	}
	in.ThinPoolExists = in.ExistingThinPool != ""
	log.Debug("ResourceManager.ConvertPoolToThin: thin pool lookup", zap.String("host", host),
		zap.String("thin_pool", in.ExistingThinPool), zap.Bool("exists", in.ThinPoolExists),
		zap.Duration("took", time.Since(start)))
	if in.ThinPoolExists {
		start = time.Now()
		if in.ThinPoolMetadataBytes, err = rm.deployment.LVSizeBytes(ctx, host, poolName,
			in.ExistingThinPool+"_tmeta"); err != nil {
			log.Debug("ResourceManager.ConvertPoolToThin: reading thin metadata size failed",
				zap.String("host", host), zap.Duration("took", time.Since(start)), zap.Error(err))
			return fmt.Errorf("read the metadata size of %s/%s on %s: %w",
				poolName, in.ExistingThinPool, nodeName, err)
		}
		log.Debug("ResourceManager.ConvertPoolToThin: thin metadata size", zap.String("host", host),
			zap.Uint64("metadata_bytes", in.ThinPoolMetadataBytes), zap.Duration("took", time.Since(start)))
	}
	for _, res := range resources {
		for _, v := range res.Volumes {
			if v.Pool != poolName || !hasNode(res, nodeName) {
				log.Debug("ResourceManager.ConvertPoolToThin: volume not in pool on node, skipped",
					zap.String("resource", res.Name), zap.String("volume", v.BackingVolume), zap.String("volume_pool", v.Pool))
				continue
			}
			start = time.Now()
			thin, terr := rm.deployment.LVIsThin(ctx, host, poolName, v.BackingVolume)
			if terr != nil {
				log.Debug("ResourceManager.ConvertPoolToThin: thin check failed", zap.String("host", host),
					zap.String("volume", v.BackingVolume), zap.Duration("took", time.Since(start)), zap.Error(terr))
				return fmt.Errorf("check whether %s/%s is thin: %w", poolName, v.BackingVolume, terr)
			}
			in.AlreadyThin[v.BackingVolume] = thin

			present, perr := rm.deployment.LVExists(ctx, host, poolName, v.BackingVolume)
			if perr != nil {
				log.Debug("ResourceManager.ConvertPoolToThin: existence check failed", zap.String("host", host),
					zap.String("volume", v.BackingVolume), zap.Duration("took", time.Since(start)), zap.Error(perr))
				return fmt.Errorf("look for %s/%s on %s: %w", poolName, v.BackingVolume, nodeName, perr)
			}
			in.Present[v.BackingVolume] = present
			log.Debug("ResourceManager.ConvertPoolToThin: volume inspected", zap.String("resource", res.Name),
				zap.String("volume", v.BackingVolume), zap.Bool("thin", thin), zap.Bool("present", present),
				zap.Duration("took", time.Since(start)))

			// Every replica of a DRBD volume is the same number of bytes, so a
			// peer is an equally good source — and the only one left when an
			// earlier attempt already removed this node's copy.
			sizeHost := host
			if !present {
				if sizeHost, err = rm.peerHostWith(res, nodeName); err != nil {
					log.Debug("ResourceManager.ConvertPoolToThin: no peer to read size from",
						zap.String("resource", res.Name), zap.Error(err))
					return err
				}
				log.Debug("ResourceManager.ConvertPoolToThin: volume gone here, reading size from peer",
					zap.String("volume", v.BackingVolume), zap.String("peer_host", sizeHost))
			}
			start = time.Now()
			size, serr := rm.deployment.LVSizeBytes(ctx, sizeHost, poolName, v.BackingVolume)
			if serr != nil {
				log.Debug("ResourceManager.ConvertPoolToThin: reading volume size failed", zap.String("host", sizeHost),
					zap.String("volume", v.BackingVolume), zap.Duration("took", time.Since(start)), zap.Error(serr))
				return fmt.Errorf("read size of %s/%s: %w", poolName, v.BackingVolume, serr)
			}
			in.BackingBytes[v.BackingVolume] = size
			log.Debug("ResourceManager.ConvertPoolToThin: volume size", zap.String("host", sizeHost),
				zap.String("volume", v.BackingVolume), zap.Uint64("size_bytes", size), zap.Duration("took", time.Since(start)))
		}
	}

	plan, err := planThinConversion(in)
	if err != nil {
		log.Debug("ResourceManager.ConvertPoolToThin: planning refused", zap.Error(err))
		return err
	}
	log.Debug("ResourceManager.ConvertPoolToThin: plan ready", zap.String("thin_pool", plan.ThinPoolName),
		zap.Bool("create_pool", plan.CreatePool), zap.Bool("extend_pool", plan.ExtendPool),
		zap.Uint64("metadata_grow_to", plan.MetadataGrowTo), zap.Int("volumes", len(plan.Volumes)))

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
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.applyThinConversion: start", zap.String("node", plan.Node), zap.String("host", host),
		zap.String("pool", plan.Pool), zap.String("thin_pool", plan.ThinPoolName), zap.Int("volumes", len(plan.Volumes)))

	// Detach and remove every volume first, then build the pool from all the
	// freed extents. Interleaving would size the pool against space the other
	// volumes still hold.
	for _, v := range plan.Volumes {
		if !v.NeedsTeardown {
			log.Debug("ResourceManager.applyThinConversion: volume already removed, no teardown",
				zap.String("resource", v.Resource), zap.String("volume", v.LV))
			continue // an earlier attempt already removed it
		}
		log.Debug("ResourceManager.applyThinConversion: tearing down volume", zap.String("resource", v.Resource),
			zap.String("volume", v.LV), zap.Bool("encrypted", v.Encrypted))
		start := time.Now()
		if err := execFailure(rm.deployment.DRBDDetach(ctx, host, v.Resource)); err != nil {
			log.Debug("ResourceManager.applyThinConversion: detach failed", zap.String("host", host),
				zap.String("resource", v.Resource), zap.Duration("took", time.Since(start)), zap.Error(err))
			return fmt.Errorf("detach %s on %s: %w", v.Resource, plan.Node, err)
		}
		log.Debug("ResourceManager.applyThinConversion: detached", zap.String("host", host),
			zap.String("resource", v.Resource), zap.Duration("took", time.Since(start)))
		// The open container holds the LV, so lvremove would refuse. Its key
		// goes too: the volume it protected is about to be released.
		if v.Encrypted {
			rm.closeBackingVolumeOn(ctx, []string{host}, plan.Pool, v.LV)
		}
		start = time.Now()
		if err := execFailure(rm.deployment.LVRemove(ctx, []string{host},
			fmt.Sprintf("%s/%s", plan.Pool, v.LV))); err != nil {
			log.Debug("ResourceManager.applyThinConversion: lvremove failed", zap.String("host", host),
				zap.String("volume", v.LV), zap.Duration("took", time.Since(start)), zap.Error(err))
			return fmt.Errorf("remove %s/%s (the node is now diskless; rerun to finish): %w",
				plan.Pool, v.LV, err)
		}
		log.Debug("ResourceManager.applyThinConversion: thick volume removed", zap.String("host", host),
			zap.String("volume", v.LV), zap.Duration("took", time.Since(start)))
	}

	switch {
	case plan.CreatePool:
		log.Debug("ResourceManager.applyThinConversion: creating thin pool from all free extents",
			zap.String("host", host), zap.String("thin_pool", plan.ThinPoolName), zap.Uint64("metadata_bytes", plan.MetadataBytes))
		start := time.Now()
		if err := execFailure(rm.deployment.LVCreateThinPoolAllFree(ctx, []string{host},
			plan.Pool, plan.ThinPoolName, plan.MetadataBytes)); err != nil {
			log.Debug("ResourceManager.applyThinConversion: thin pool creation failed", zap.String("host", host),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			return fmt.Errorf("create thin pool %s/%s (the node is diskless; rerun to finish): %w",
				plan.Pool, plan.ThinPoolName, err)
		}
		log.Debug("ResourceManager.applyThinConversion: thin pool created", zap.String("host", host),
			zap.Duration("took", time.Since(start)))
	case plan.ExtendPool:
		log.Debug("ResourceManager.applyThinConversion: extending existing thin pool", zap.String("host", host),
			zap.String("thin_pool", plan.ThinPoolName), zap.Uint64("metadata_grow_to", plan.MetadataGrowTo))
		// Metadata first — see LVExtendThinPoolMetadata for why the order is
		// not interchangeable.
		if plan.MetadataGrowTo > 0 {
			start := time.Now()
			if err := execFailure(rm.deployment.LVExtendThinPoolMetadata(ctx, []string{host},
				plan.Pool, plan.ThinPoolName, plan.MetadataGrowTo)); err != nil {
				log.Debug("ResourceManager.applyThinConversion: metadata grow failed", zap.String("host", host),
					zap.Duration("took", time.Since(start)), zap.Error(err))
				return fmt.Errorf("grow the metadata area of %s/%s: %w", plan.Pool, plan.ThinPoolName, err)
			}
			log.Debug("ResourceManager.applyThinConversion: metadata grown", zap.String("host", host),
				zap.Duration("took", time.Since(start)))
		}
		start := time.Now()
		if err := execFailure(rm.deployment.LVExtendThinPoolAllFree(ctx, []string{host},
			plan.Pool, plan.ThinPoolName)); err != nil {
			log.Debug("ResourceManager.applyThinConversion: pool extend failed", zap.String("host", host),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			return fmt.Errorf("grow %s/%s into the freed extents (the node is diskless; rerun to finish): %w",
				plan.Pool, plan.ThinPoolName, err)
		}
		log.Debug("ResourceManager.applyThinConversion: pool extended", zap.String("host", host),
			zap.Duration("took", time.Since(start)))
	default:
		log.Debug("ResourceManager.applyThinConversion: neither creating nor extending the pool")
	}

	for _, v := range plan.Volumes {
		log.Debug("ResourceManager.applyThinConversion: rebuilding volume as thin", zap.String("resource", v.Resource),
			zap.String("volume", v.LV), zap.Uint64("size_bytes", v.SizeBytes), zap.Bool("encrypted", v.Encrypted))
		start := time.Now()
		if err := execFailure(rm.deployment.LVCreateThinVolume(ctx, []string{host},
			plan.Pool, plan.ThinPoolName, v.LV, fmt.Sprintf("%dB", v.SizeBytes))); err != nil {
			log.Debug("ResourceManager.applyThinConversion: thin volume creation failed", zap.String("host", host),
				zap.String("volume", v.LV), zap.Duration("took", time.Since(start)), zap.Error(err))
			return fmt.Errorf("create thin volume %s/%s: %w", plan.Pool, v.LV, err)
		}
		log.Debug("ResourceManager.applyThinConversion: thin volume created", zap.String("host", host),
			zap.String("volume", v.LV), zap.Duration("took", time.Since(start)))
		// The config still names the container, so DRBD attaches nothing
		// until it exists again, under a key made fresh on this node. The size
		// was read from the old LV, header included.
		if v.Encrypted {
			if err := rm.encryptBackingVolumeOn(ctx, host, plan.Node, plan.Pool, v.LV,
				fmt.Sprintf("/dev/%s/%s", plan.Pool, v.LV)); err != nil {
				log.Debug("ResourceManager.applyThinConversion: re-encrypting volume failed",
					zap.String("volume", v.LV), zap.Error(err))
				return err
			}
		}
		start = time.Now()
		if err := execFailure(rm.deployment.DRBDCreateMD(ctx, []string{host}, v.Resource,
			deployment.DefaultMaxPeers)); err != nil {
			log.Debug("ResourceManager.applyThinConversion: create-md failed", zap.String("host", host),
				zap.String("resource", v.Resource), zap.Duration("took", time.Since(start)), zap.Error(err))
			return fmt.Errorf("create metadata for %s: %w", v.Resource, err)
		}
		log.Debug("ResourceManager.applyThinConversion: metadata created", zap.String("host", host),
			zap.String("resource", v.Resource), zap.Duration("took", time.Since(start)))
		start = time.Now()
		if err := execFailure(rm.deployment.DRBDAttach(ctx, host, v.Resource)); err != nil {
			log.Debug("ResourceManager.applyThinConversion: attach failed", zap.String("host", host),
				zap.String("resource", v.Resource), zap.Duration("took", time.Since(start)), zap.Error(err))
			return fmt.Errorf("attach %s: %w", v.Resource, err)
		}
		log.Debug("ResourceManager.applyThinConversion: attached", zap.String("host", host),
			zap.String("resource", v.Resource), zap.Duration("took", time.Since(start)))
		log.Info("Volume rebuilt as thin; full resync started",
			zap.String("node", plan.Node), zap.String("volume", v.LV),
			zap.String("resource", v.Resource))
	}
	log.Debug("ResourceManager.applyThinConversion: done", zap.String("node", plan.Node), zap.Int("volumes", len(plan.Volumes)))
	return nil
}

// peerHostWith returns the address of another node holding this resource, for
// the questions this node can no longer answer about itself.
func (rm *ResourceManager) peerHostWith(res *ResourceInfo, node string) (string, error) {
	log := rm.controller.traceLogger()
	for _, peer := range res.Nodes {
		if peer == node {
			continue
		}
		if addr := rm.controller.nodes.GetNodeAddressByName(peer); addr != "" {
			log.Debug("ResourceManager.peerHostWith: chose peer", zap.String("resource", res.Name),
				zap.String("peer", peer), zap.String("host", addr))
			return addr, nil
		}
		log.Debug("ResourceManager.peerHostWith: peer not registered", zap.String("resource", res.Name), zap.String("peer", peer))
	}
	log.Debug("ResourceManager.peerHostWith: no registered peer", zap.String("resource", res.Name), zap.String("node", node))
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
		zap.L().Debug("execFailure: transport error", zap.Error(err))
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
	zap.L().Debug("execFailure: command failed on hosts", zap.Strings("failed_hosts", res.FailedHosts()))
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}

// liveResourcesOn returns live state for every resource with a replica on the
// node. Planning from the database alone would miss a resync in flight, which
// is exactly the state that must block a conversion.
func (rm *ResourceManager) liveResourcesOn(ctx context.Context, nodeName string) ([]*ResourceInfo, error) {
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.liveResourcesOn: start", zap.String("node", nodeName))
	if rm.controller.db == nil {
		log.Debug("ResourceManager.liveResourcesOn: no database")
		return nil, fmt.Errorf("database not available")
	}
	start := time.Now()
	dbResources, err := rm.controller.db.ListResources(ctx)
	log.Debug("ResourceManager.liveResourcesOn: listed resources", zap.Int("count", len(dbResources)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("list resources: %w", err)
	}
	var out []*ResourceInfo
	for _, dbRes := range dbResources {
		info, gerr := rm.GetResource(ctx, dbRes.Name)
		if gerr != nil {
			log.Debug("ResourceManager.liveResourcesOn: reading live state failed", zap.String("resource", dbRes.Name), zap.Error(gerr))
			// A resource whose state cannot be read might be the one that makes
			// this unsafe, so refuse rather than plan around it.
			return nil, fmt.Errorf("read live state of %s: %w", dbRes.Name, gerr)
		}
		on := hasNode(info, nodeName)
		log.Debug("ResourceManager.liveResourcesOn: resource", zap.String("resource", dbRes.Name), zap.Bool("on_node", on))
		if on {
			out = append(out, info)
		}
	}
	log.Debug("ResourceManager.liveResourcesOn: done", zap.String("node", nodeName), zap.Int("count", len(out)))
	return out, nil
}
