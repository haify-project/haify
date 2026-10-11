package controller

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/haify-project/haify/pkg/wanproxy"
)

// AddReplica adds a diskful local replica to a resource that is already running.
//
// Haify could grow a resource sideways — a diskless client, a quorum tiebreaker,
// an off-site DR — but not the thing an operator most often wants: one more real
// copy, here, on a node that was not in the cluster when the resource was made.
// The only route was to destroy the resource and rebuild it, which for anything
// holding data means an outage and a restore.
//
// The new node joins the synchronous mesh as a peer of every existing replica.
// On a resource that also has an off-site DR it additionally gets its own WAN
// leg, because DRBD 9 is a full mesh: a replica the DR cannot reach is a replica
// that silently ends replication the moment it is promoted.
func (rm *ResourceManager) AddReplica(ctx context.Context, resource, node string) error {
	return rm.AddReplicaOptions(ctx, resource, node, false)
}

// AddReplicaOptions is AddReplica; ignoreFreeSpace adds the replica although
// the node's pool has less free space than the volume, for a pool known to
// hold a sparse volume or about to be grown.
func (rm *ResourceManager) AddReplicaOptions(ctx context.Context, resource, node string, ignoreFreeSpace bool) error {
	return rm.AddReplicaWith(ctx, resource, node, AddReplicaOpts{IgnoreFreeSpace: ignoreFreeSpace})
}

// AddReplicaOpts are AddReplica's options.
type AddReplicaOpts struct {
	// IgnoreFreeSpace adds the replica although the node's pool looks short.
	IgnoreFreeSpace bool
	// AllowUnreachable adds the replica while a minority of the resource's
	// members do not answer (addreplica_unreachable.go).
	AllowUnreachable bool
}

// AddReplicaWith is AddReplica with options.
func (rm *ResourceManager) AddReplicaWith(ctx context.Context, resource, node string, opts AddReplicaOpts) error {
	ignoreFreeSpace := opts.IgnoreFreeSpace
	if rm.deployment == nil {
		return fmt.Errorf("deployment client not set")
	}
	if rm.controller.db == nil {
		return fmt.Errorf("database not available")
	}

	dbRes, err := rm.controller.db.GetResource(ctx, resource)
	if err != nil || dbRes == nil {
		return fmt.Errorf("resource %q not found", resource)
	}

	node = strings.TrimSpace(node)
	if node == "" {
		return fmt.Errorf("a node is required")
	}
	if rm.controller.nodes.GetNodeAddressByName(node) == "" {
		return fmt.Errorf("node %q is not a registered node", node)
	}

	existing := splitCSV(dbRes.Nodes)
	for _, n := range existing {
		if n == node {
			return fmt.Errorf("node %q already holds a replica of %q", node, resource)
		}
	}
	// A diskless member — client or tiebreaker — gets its disk in place
	// (addreplica_convert.go).
	if nodeTakesPart(node, dbRes.DisklessNodes, dbRes.DisklessClients) {
		return rm.convertDisklessToReplica(ctx, dbRes, node, opts)
	}

	// The DR is a member of Nodes but not a primary-site replica; keep them apart.
	primaries := existing
	if dbRes.WANMode && dbRes.DRNode != "" {
		primaries = make([]string, 0, len(existing))
		for _, n := range existing {
			if n != dbRes.DRNode {
				primaries = append(primaries, n)
			}
		}
	}
	if len(primaries) == 0 {
		return fmt.Errorf("resource %q has no primary-site replica to copy from", resource)
	}

	if !ignoreFreeSpace {
		if err := rm.assertPoolRoom(ctx, resource, node); err != nil {
			return err
		}
	}
	if pool, sizeGB := rm.memberPoolAndSize(ctx, resource, ""); pool != "" {
		if err := rm.assertOvercommit(ctx, pool, []string{node}, sizeGB); err != nil {
			return err
		}
	}
	if err := rm.assertNewMemberTLS(ctx, resource, node); err != nil {
		return err
	}

	tiebreakers := splitCSV(dbRes.DisklessNodes)
	away, err := rm.unreachableMembers(ctx, dbRes, primaries, tiebreakers, opts.AllowUnreachable)
	if err != nil {
		return err
	}

	newAddr := rm.controller.ResolveHost(node)
	// A replica takes part in failover, so it gets the resource's promoters
	// once it is in; check now that it could run them, while refusing is free.
	if err := rm.checkPromoterPrereqs(ctx, resource, []string{newAddr}); err != nil {
		return err
	}
	// Commands go to the members that answer; the config still names them all.
	reachablePrimaries := withoutAll(primaries, away)
	primaryAddrs := make([]string, 0, len(reachablePrimaries))
	for _, n := range reachablePrimaries {
		primaryAddrs = append(primaryAddrs, rm.controller.ResolveHost(n))
	}

	// A replica of an encrypted resource is encrypted, or it is a hole: the new
	// node would hold a full plaintext copy of data every other node keeps
	// encrypted, and nothing in `resource status` would say so. Refuse early if
	// the newcomer cannot do it, before any storage is provisioned.
	if dbRes.Encrypted {
		if err := rm.assertEncryptionSupported(ctx, []string{newAddr}, []string{node}); err != nil {
			return err
		}
	}

	rm.controller.logger.Info("Adding replica to running resource",
		zap.String("resource", resource),
		zap.String("node", node),
		zap.Strings("existing", primaries),
		zap.Bool("wan", dbRes.WANMode))

	hosts := primaryAddrs
	if len(hosts) == 0 {
		return fmt.Errorf("resource %q has no reachable hosts", resource)
	}
	resPath := fmt.Sprintf("/etc/drbd.d/%s.res", resource)
	catRes, err := rm.deployment.Exec(ctx, []string{hosts[0]}, "cat "+resPath)
	if err != nil {
		return fmt.Errorf("read resource config: %w", err)
	}
	liveConfig, readOK := "", false
	for _, r := range catRes.Hosts {
		liveConfig, readOK = r.Output, r.Success
		break
	}
	if !readOK || strings.TrimSpace(liveConfig) == "" {
		return fmt.Errorf("read resource config for %q: %s", resource, catRes.FailureDetails())
	}

	if err := rm.assertMinorsFreeOn(ctx, newAddr, resource, resourceMinors(liveConfig)); err != nil {
		return err
	}
	// A diskful peer needs a bitmap slot on every existing replica, and slots are
	// allocated once, at create-md time. Check before provisioning anything.
	if err := rm.assertBitmapSlotFree(ctx, primaryAddrs, reachablePrimaries, resource); err != nil {
		return err
	}

	dbVols, err := rm.controller.db.ListVolumes(ctx, resource)
	if err != nil {
		return fmt.Errorf("list volumes of %q: %w", resource, err)
	}
	if len(dbVols) == 0 {
		return fmt.Errorf("resource %q has no recorded volumes to replicate", resource)
	}
	// Size from what the existing replicas actually export, not from the recorded
	// request: metadata is carved out of the same device, so a volume that was
	// extended exports more than its record implies, and DRBD refuses a peer that
	// is even one sector short.
	backingSizes, err := rm.primaryBackingSizes(ctx, primaryAddrs, resource, len(dbVols))
	if err != nil {
		return err
	}

	lanHosts := append([]string{}, primaryAddrs...)
	for _, n := range withoutAll(tiebreakers, away) {
		lanHosts = append(lanHosts, rm.controller.ResolveHost(n))
	}
	allHosts := append(append([]string{}, lanHosts...), newAddr)
	if dbRes.WANMode && dbRes.DRNode != "" {
		allHosts = append(allHosts, rm.controller.ResolveHost(dbRes.DRNode))
	}

	succeeded := false
	defer func() {
		if succeeded {
			return
		}
		rm.undoAddReplica(context.WithoutCancel(ctx), resource, resPath, liveConfig,
			lanHosts, newAddr, dbVols, dbRes.Encrypted)
	}()

	for i, v := range dbVols {
		if cerr := rm.createBackingVolumeOn(ctx, newAddr, v.Pool, v.VolumeName, backingSizes[i]); cerr != nil {
			return fmt.Errorf("create backing volume %s/%s on %q: %w", v.Pool, v.VolumeName, node, cerr)
		}
		// The newcomer generates its own key, exactly as every other replica
		// did. Sizing needs no crypt allowance here: backingSizes is copied
		// from what an existing replica's LV really is, header included.
		if dbRes.Encrypted {
			if cerr := rm.encryptBackingVolumeOn(ctx, newAddr, node, v.Pool, v.VolumeName,
				fmt.Sprintf("/dev/%s/%s", v.Pool, v.VolumeName)); cerr != nil {
				return cerr
			}
		}
	}

	// On a two-site resource the newcomer needs its own tunnel to the DR before
	// DRBD is told about the connection. Existing legs keep their index, so their
	// ports do not move; they are rewritten with identical content and restart,
	// which the async DR rides out by reconnecting.
	newBind := 0
	if dbRes.WANMode && dbRes.DRNode != "" {
		withNew := append(append([]string{}, primaryAddrs...), newAddr)
		multi := wanproxy.MultiSpec{
			Resource:          resource,
			PrimaryNodeAddrs:  withNew,
			DRNodeAddr:        rm.controller.ResolveHost(dbRes.DRNode),
			DRPublicEndpoint:  dbRes.DREndpoint,
			BaseWANPort:       dbRes.WANPort,
			BaseDRBDPort:      dbRes.Port,
			PrimaryEgressAddr: dbRes.WANEgressAddress,
			BinaryFor: rm.wanproxyBinaryResolver(ctx,
				append(append([]string{}, withNew...), rm.controller.ResolveHost(dbRes.DRNode))),
		}
		if err := wanproxy.ProvisionMulti(ctx, rm.wanproxyDeployClient(), multi); err != nil {
			return fmt.Errorf("provision WAN leg for %q: %w", node, err)
		}
		// After the push, not before: the controller may well have supplied the
		// binary itself, and checking first would reject a case that works.
		if err := rm.assertWANProxyBinary(ctx, []string{newAddr}, []string{node}); err != nil {
			return err
		}
		ports, perr := rm.pickWANBindPorts(ctx, []string{newAddr}, dbRes.Port)
		if perr != nil {
			return perr
		}
		newBind = ports[0]
	}

	// SSH goes to newAddr; DRBD talks over the node's replication network,
	// the same address create and repair write for it.
	replAddr := rm.controller.nodes.GetReplicationAddressByName(node)
	if replAddr == "" {
		replAddr = newAddr
	}
	newConfig, err := rm.addReplicaToConfig(liveConfig, resource, node, resolveToIP(replAddr), dbVols,
		dbRes.Port, len(primaries), newBind, dbRes.WANMode && dbRes.DRNode != "", dbRes.DRNode,
		dbRes.Encrypted)
	if err != nil {
		return fmt.Errorf("rewrite config for new replica: %w", err)
	}
	// Growing the primary site moves its majority; recompute rather than leaving
	// a stale number that no longer describes the site.
	if dbRes.WANMode && dbRes.DRNode != "" {
		newConfig = setLocalSiteQuorum(newConfig, len(primaries)+1+len(tiebreakers))
	}

	if _, err := rm.deployment.DistributeConfig(ctx, allHosts, newConfig, resPath); err != nil {
		return fmt.Errorf("distribute config with new replica: %w", err)
	}

	if err := rm.execAllSuccess(ctx, []string{newAddr},
		fmt.Sprintf("sudo drbdadm create-md --max-peers=%d --force %s", deployment.DefaultMaxPeers, resource),
		"create metadata on the new replica"); err != nil {
		return err
	}
	if err := rm.execAllSuccess(ctx, allHosts,
		fmt.Sprintf("sudo drbdadm adjust %s", resource),
		"adjust peers for the new replica"); err != nil {
		return err
	}
	upRes, err := rm.deployment.Exec(ctx, []string{newAddr}, fmt.Sprintf("sudo drbdadm up %s", resource))
	if err != nil {
		return fmt.Errorf("bring up the new replica: %w", err)
	}
	if !upRes.AllSuccess() {
		detail := upRes.FailureDetails()
		if !strings.Contains(detail, "exists already") && !strings.Contains(detail, "already? still?") {
			return fmt.Errorf("bring up replica on %q: %s", node, detail)
		}
	}

	succeeded = true

	// Record it last, so a failure above leaves the row describing reality. The
	// new node goes before the DR, which by convention is the final entry.
	updated := append([]string{}, primaries...)
	updated = append(updated, node)
	if dbRes.WANMode && dbRes.DRNode != "" {
		updated = append(updated, dbRes.DRNode)
	}
	dbRes.Nodes = strings.Join(updated, ",")
	// The members that were away get this config when they answer again
	// (self_heal_return.go).
	if len(away) > 0 {
		dbRes.StaleConfigNodes = strings.Join(append(splitCSV(dbRes.StaleConfigNodes), away...), ",")
	}
	if err := rm.controller.db.SaveResource(ctx, dbRes); err != nil {
		return fmt.Errorf("record the new replica: %w", err)
	}

	rm.controller.logger.Info("Replica added; initial sync runs in the background",
		zap.String("resource", resource), zap.String("node", node))

	// drbd-reactor will not promote a replica that is still syncing, so the
	// promoter can go in now and takes effect once the copy is UpToDate.
	if err := rm.SyncPromoters(ctx, resource); err != nil {
		return fmt.Errorf("replica added on %s, but the resource's promoters could not be placed there: %w; "+
			"fix that and run `haify resource repair %s`", node, err, resource)
	}
	return nil
}

// addReplicaToConfig adds a diskful node to a live resource config.
func (rm *ResourceManager) addReplicaToConfig(content, resource, node, addr string,
	vols []*database.Volume, port, legIndex, bindPort int, wan bool, drNode string,
	encrypted bool) (string, error) {

	name := rm.controller.nodes.GetDRBDNameByRef(node)
	blocks := parseOnBlocks(content)
	for _, b := range blocks {
		if b.name == name {
			return "", fmt.Errorf("node %q is already in the config of %q", node, resource)
		}
	}
	nextID := 0
	for _, b := range blocks {
		if b.nodeID >= nextID {
			nextID = b.nodeID + 1
		}
	}

	var stanza strings.Builder
	fmt.Fprintf(&stanza, "\n    on %s {\n", name)
	fmt.Fprintf(&stanza, "        address   %s:%d;\n", addr, port)
	fmt.Fprintf(&stanza, "        node-id   %d;\n", nextID)
	for _, v := range vols {
		// The crypt container is what DRBD attaches on an encrypted resource.
		// Its name comes from pool and volume, which are identical on every
		// node, so the newcomer's stanza names the same device as its peers
		// even though the key behind it is its own.
		disk := fmt.Sprintf("/dev/%s/%s", v.Pool, v.VolumeName)
		if encrypted {
			disk = luksMapperPath(v.Pool, v.VolumeName)
		}
		fmt.Fprintf(&stanza, "        volume %d {\n", v.VolumeID)
		fmt.Fprintf(&stanza, "            device    minor %d;\n", minorForVolume(content, v.VolumeID))
		fmt.Fprintf(&stanza, "            disk      %s;\n", disk)
		fmt.Fprintf(&stanza, "            meta-disk internal;\n")
		fmt.Fprintf(&stanza, "        }\n")
	}
	fmt.Fprintf(&stanza, "    }\n")

	out, err := insertBeforeResourceClose(stripConnectionMesh(content), stanza.String())
	if err != nil {
		return "", err
	}

	// Rebuild the mesh over the LAN hosts, which now include the newcomer. A
	// WAN-attached host stays out: a mesh entry would pair it with every other
	// host on that host's LAN address, which is unroutable from the other site.
	mesh := lanHostNames(parseOnBlocks(out))
	if len(mesh) > 1 {
		out, err = insertBeforeResourceClose(out, buildConnectionMesh(mesh))
		if err != nil {
			return "", err
		}
	}

	// Two-site resource: the newcomer also needs an explicit leg to the DR, on
	// the same port layout the other legs use.
	if wan && drNode != "" {
		legPort := port + legIndex
		var leg strings.Builder
		fmt.Fprintf(&leg, "\n    connection {\n")
		fmt.Fprintf(&leg, "        host %s address 127.0.0.1:%d;\n", name, bindPort)
		fmt.Fprintf(&leg, "        host %s address 127.0.0.1:%d;\n",
			rm.controller.nodes.GetDRBDNameByRef(drNode), legPort)
		fmt.Fprintf(&leg, "        net {\n")
		fmt.Fprintf(&leg, "            protocol A;\n")
		fmt.Fprintf(&leg, "            on-congestion pull-ahead;\n")
		fmt.Fprintf(&leg, "            congestion-fill 400M;\n")
		fmt.Fprintf(&leg, "            csums-alg %s;\n", wanCsumsAlg)
		fmt.Fprintf(&leg, "        }\n")
		fmt.Fprintf(&leg, "    }\n")
		out, err = insertBeforeResourceClose(out, leg.String())
		if err != nil {
			return "", err
		}
	}
	return out, nil
}

// undoAddReplica returns the cluster to its previous shape after a failed add.
//
// The surviving members go back on the old config first, so they stop trying to
// reach a peer that is about to disappear; only then is the newcomer dismantled.
// Every step is best-effort and logged — the caller is already returning the
// failure that got us here, and a rollback that aborts halfway is worse than one
// that keeps going.
func (rm *ResourceManager) undoAddReplica(ctx context.Context, resource, resPath, originalConfig string,
	lanHosts []string, newAddr string, vols []*database.Volume, encrypted bool) {

	log := rm.controller.logger.With(zap.String("resource", resource), zap.String("node", newAddr))
	log.Warn("Rolling back replica addition")

	if len(lanHosts) > 0 && strings.TrimSpace(originalConfig) != "" {
		if _, err := rm.deployment.DistributeConfig(ctx, lanHosts, originalConfig, resPath); err != nil {
			log.Error("Rollback: failed to restore the previous config", zap.Error(err))
		} else if _, err := rm.deployment.Exec(ctx, lanHosts,
			fmt.Sprintf("sudo drbdadm adjust %s", resource)); err != nil {
			log.Error("Rollback: failed to re-adjust the surviving members", zap.Error(err))
		}
	}

	if _, err := rm.deployment.Exec(ctx, []string{newAddr},
		fmt.Sprintf("sudo drbdadm down %s 2>/dev/null; sudo rm -f %s", resource, resPath)); err != nil {
		log.Error("Rollback: failed to tear down the new replica", zap.Error(err))
	}
	for _, v := range vols {
		// The container holds the LV open, and its key is worth destroying
		// whether or not the lvremove that follows succeeds.
		if encrypted {
			rm.closeBackingVolumeOn(ctx, []string{newAddr}, v.Pool, v.VolumeName)
		}
		if _, err := rm.deployment.Exec(ctx, []string{newAddr},
			fmt.Sprintf("sudo lvremove -f %s/%s", v.Pool, v.VolumeName)); err != nil {
			log.Error("Rollback: failed to remove a backing volume",
				zap.String("volume", v.Pool+"/"+v.VolumeName), zap.Error(err))
		}
	}
}

// wanproxyBinaryProbe reports whether the proxy binary is present and runnable.
const wanproxyBinaryProbe = `test -x /usr/local/bin/haify-proxy && echo present || echo missing`

// assertWANProxyBinary fails unless every node that will run a proxy leg has the
// binary.
//
// Skipping the push when the controller has no local copy is a reasonable
// default — most fleets stage haify-proxy with their image — but "assume it is
// pre-staged" is a bad thing to assume silently. A node without it gets a
// systemd unit that crash-loops with 203/EXEC, while DRBD reports only
// "Connecting". Nothing in that picture points at a missing file, and the two
// halves are on different machines: the unit fails at home, the symptom is a
// connection that never forms.
//
// A node that has never carried a WAN leg is exactly the node likely to be
// missing it — which is to say, every node this code path is about.
func (rm *ResourceManager) assertWANProxyBinary(ctx context.Context, hosts, nodes []string) error {
	for i, host := range hosts {
		res, err := rm.deployment.Exec(ctx, []string{host}, wanproxyBinaryProbe)
		if err != nil {
			return fmt.Errorf("check for the haify-proxy binary on %q: %w", nodes[i], err)
		}
		out := ""
		for _, r := range res.Hosts {
			out = strings.TrimSpace(r.Output)
			break
		}
		if strings.Contains(out, "missing") {
			return fmt.Errorf(
				"node %q has no executable %s, so its WAN leg would start and immediately fail "+
					"(systemd 203/EXEC, while DRBD reports only \"Connecting\"). Stage the binary there, "+
					"or put one on the controller at the same path so it can be pushed automatically",
				nodes[i], wanproxyLocalBinaryPath)
		}
	}
	return nil
}
