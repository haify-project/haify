package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	log := rm.controller.traceLogger()
	log.Debug("AddReplicaWith: start", zap.String("resource", resource), zap.String("node", node),
		zap.Bool("ignore_free_space", ignoreFreeSpace), zap.Bool("allow_unreachable", opts.AllowUnreachable))
	if rm.deployment == nil {
		log.Debug("AddReplicaWith: no deployment client", zap.String("resource", resource))
		return fmt.Errorf("deployment client not set")
	}
	if rm.controller.db == nil {
		log.Debug("AddReplicaWith: no database", zap.String("resource", resource))
		return fmt.Errorf("database not available")
	}

	start := time.Now()
	dbRes, err := rm.controller.db.GetResource(ctx, resource)
	log.Debug("AddReplicaWith: db get resource", zap.String("resource", resource), zap.Bool("found", dbRes != nil),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || dbRes == nil {
		return fmt.Errorf("resource %q not found", resource)
	}

	node = strings.TrimSpace(node)
	if node == "" {
		log.Debug("AddReplicaWith: no node given", zap.String("resource", resource))
		return fmt.Errorf("a node is required")
	}
	if rm.controller.nodes.GetNodeAddressByName(node) == "" {
		log.Debug("AddReplicaWith: node not registered", zap.String("node", node))
		return fmt.Errorf("node %q is not a registered node", node)
	}

	existing := splitCSV(dbRes.Nodes)
	for _, n := range existing {
		if n == node {
			log.Debug("AddReplicaWith: node already holds a replica", zap.String("node", node), zap.Strings("nodes", existing))
			return fmt.Errorf("node %q already holds a replica of %q", node, resource)
		}
	}
	// A diskless member — client or tiebreaker — gets its disk in place
	// (addreplica_convert.go).
	if nodeTakesPart(node, dbRes.DisklessNodes, dbRes.DisklessClients) {
		log.Debug("AddReplicaWith: node is a diskless member, converting in place", zap.String("node", node))
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
	log.Debug("AddReplicaWith: primary-site replicas", zap.String("resource", resource), zap.Strings("primaries", primaries),
		zap.Bool("wan_mode", dbRes.WANMode), zap.String("dr_node", dbRes.DRNode))
	if len(primaries) == 0 {
		log.Debug("AddReplicaWith: no primary-site replica", zap.String("resource", resource))
		return fmt.Errorf("resource %q has no primary-site replica to copy from", resource)
	}

	if !ignoreFreeSpace {
		if err := rm.assertPoolRoom(ctx, resource, node); err != nil {
			log.Debug("AddReplicaWith: pool has no room", zap.String("node", node), zap.Error(err))
			return err
		}
	} else {
		log.Debug("AddReplicaWith: free-space check skipped", zap.String("node", node))
	}
	if pool, sizeGB := rm.memberPoolAndSize(ctx, resource, ""); pool != "" {
		log.Debug("AddReplicaWith: checking overcommit", zap.String("pool", pool), zap.Uint64("size_gb", sizeGB))
		if err := rm.assertOvercommit(ctx, pool, []string{node}, sizeGB); err != nil {
			log.Debug("AddReplicaWith: overcommit refused", zap.String("pool", pool), zap.Error(err))
			return err
		}
	} else {
		log.Debug("AddReplicaWith: no member pool known, overcommit not checked")
	}
	if err := rm.assertNewMemberTLS(ctx, resource, node); err != nil {
		log.Debug("AddReplicaWith: replication TLS check failed", zap.String("node", node), zap.Error(err))
		return err
	}

	tiebreakers := splitCSV(dbRes.DisklessNodes)
	away, err := rm.unreachableMembers(ctx, dbRes, primaries, tiebreakers, opts.AllowUnreachable)
	if err != nil {
		log.Debug("AddReplicaWith: unreachable members block the add", zap.Error(err))
		return err
	}

	newAddr := rm.controller.ResolveHost(node)
	// A replica takes part in failover, so it gets the resource's promoters
	// once it is in; check now that it could run them, while refusing is free.
	if err := rm.checkPromoterPrereqs(ctx, resource, []string{newAddr}); err != nil {
		log.Debug("AddReplicaWith: promoter prerequisites missing", zap.String("host", newAddr), zap.Error(err))
		return err
	}
	// Commands go to the members that answer; the config still names them all.
	reachablePrimaries := withoutAll(primaries, away)
	primaryAddrs := make([]string, 0, len(reachablePrimaries))
	for _, n := range reachablePrimaries {
		primaryAddrs = append(primaryAddrs, rm.controller.ResolveHost(n))
	}
	log.Debug("AddReplicaWith: reachable replicas", zap.String("host", newAddr), zap.Strings("away", away),
		zap.Strings("primary_addrs", primaryAddrs))

	// A replica of an encrypted resource is encrypted, or it is a hole: the new
	// node would hold a full plaintext copy of data every other node keeps
	// encrypted, and nothing in `resource status` would say so. Refuse early if
	// the newcomer cannot do it, before any storage is provisioned.
	if dbRes.Encrypted {
		if err := rm.assertEncryptionSupported(ctx, []string{newAddr}, []string{node}); err != nil {
			log.Debug("AddReplicaWith: node cannot encrypt", zap.String("node", node), zap.Error(err))
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
		log.Debug("AddReplicaWith: no reachable hosts", zap.String("resource", resource))
		return fmt.Errorf("resource %q has no reachable hosts", resource)
	}
	resPath := fmt.Sprintf("/etc/drbd.d/%s.res", resource)
	start = time.Now()
	catRes, err := rm.deployment.Exec(ctx, []string{hosts[0]}, "cat "+resPath)
	log.Debug("AddReplicaWith: read live config", zap.String("host", hosts[0]), zap.String("path", resPath),
		zap.Strings("failed_hosts", tracedFailedHosts(catRes)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("read resource config: %w", err)
	}
	liveConfig, readOK := "", false
	for _, r := range catRes.Hosts {
		liveConfig, readOK = r.Output, r.Success
		break
	}
	if !readOK || strings.TrimSpace(liveConfig) == "" {
		log.Debug("AddReplicaWith: live config unreadable or empty", zap.Bool("read_ok", readOK))
		return fmt.Errorf("read resource config for %q: %s", resource, catRes.FailureDetails())
	}

	if err := rm.assertMinorsFreeOn(ctx, newAddr, resource, resourceMinors(liveConfig)); err != nil {
		log.Debug("AddReplicaWith: minors not free on the new node", zap.String("host", newAddr), zap.Error(err))
		return err
	}
	// A diskful peer needs a bitmap slot on every existing replica, and slots are
	// allocated once, at create-md time. Check before provisioning anything.
	if err := rm.assertBitmapSlotFree(ctx, primaryAddrs, reachablePrimaries, resource); err != nil {
		log.Debug("AddReplicaWith: no bitmap slot", zap.Error(err))
		return err
	}

	start = time.Now()
	dbVols, err := rm.controller.db.ListVolumes(ctx, resource)
	log.Debug("AddReplicaWith: db list volumes", zap.String("resource", resource), zap.Int("count", len(dbVols)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("list volumes of %q: %w", resource, err)
	}
	if len(dbVols) == 0 {
		log.Debug("AddReplicaWith: no recorded volumes", zap.String("resource", resource))
		return fmt.Errorf("resource %q has no recorded volumes to replicate", resource)
	}
	// Size from what the existing replicas actually export, not from the recorded
	// request: metadata is carved out of the same device, so a volume that was
	// extended exports more than its record implies, and DRBD refuses a peer that
	// is even one sector short.
	backingSizes, err := rm.primaryBackingSizes(ctx, primaryAddrs, resource, len(dbVols))
	if err != nil {
		log.Debug("AddReplicaWith: backing sizes unreadable", zap.Error(err))
		return err
	}
	backingSizes = replicaBackingSizes(backingSizes, dbRes.Encrypted)
	log.Debug("AddReplicaWith: new replica sizes", zap.Uint64s("size_bytes", backingSizes), zap.Bool("encrypted", dbRes.Encrypted))

	lanHosts := append([]string{}, primaryAddrs...)
	for _, n := range withoutAll(tiebreakers, away) {
		lanHosts = append(lanHosts, rm.controller.ResolveHost(n))
	}
	allHosts := append(append([]string{}, lanHosts...), newAddr)
	if dbRes.WANMode && dbRes.DRNode != "" {
		allHosts = append(allHosts, rm.controller.ResolveHost(dbRes.DRNode))
	}
	log.Debug("AddReplicaWith: hosts that get the new config", zap.Strings("lan_hosts", lanHosts), zap.Strings("hosts", allHosts))

	succeeded := false
	defer func() {
		if succeeded {
			return
		}
		log.Debug("AddReplicaWith: failed part way, rolling back", zap.String("resource", resource), zap.String("node", node))
		rm.undoAddReplica(context.WithoutCancel(ctx), resource, resPath, liveConfig,
			lanHosts, newAddr, dbVols, dbRes.Encrypted)
	}()

	for i, v := range dbVols {
		log.Debug("AddReplicaWith: creating backing volume", zap.String("host", newAddr), zap.String("pool", v.Pool),
			zap.String("volume", v.VolumeName), zap.Uint64("size_bytes", backingSizes[i]))
		if cerr := rm.createBackingVolumeOn(ctx, newAddr, v.Pool, v.VolumeName, backingSizes[i]); cerr != nil {
			log.Debug("AddReplicaWith: backing volume creation failed", zap.String("volume", v.VolumeName), zap.Error(cerr))
			return fmt.Errorf("create backing volume %s/%s on %q: %w", v.Pool, v.VolumeName, node, cerr)
		}
		// The newcomer generates its own key, exactly as every other replica
		// did. backingSizes already carries the header (replicaBackingSizes).
		if dbRes.Encrypted {
			if cerr := rm.encryptBackingVolumeOn(ctx, newAddr, node, v.Pool, v.VolumeName,
				fmt.Sprintf("/dev/%s/%s", v.Pool, v.VolumeName)); cerr != nil {
				log.Debug("AddReplicaWith: encrypting volume failed", zap.String("volume", v.VolumeName), zap.Error(cerr))
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
		log.Debug("AddReplicaWith: two-site resource, provisioning a WAN leg for the newcomer",
			zap.String("dr_node", dbRes.DRNode), zap.Strings("hosts", withNew))
		start := time.Now()
		if err := wanproxy.ProvisionMulti(ctx, rm.wanproxyDeployClient(), multi); err != nil {
			log.Debug("AddReplicaWith: WAN leg provisioning failed", zap.Duration("took", time.Since(start)), zap.Error(err))
			return fmt.Errorf("provision WAN leg for %q: %w", node, err)
		}
		log.Debug("AddReplicaWith: WAN legs provisioned", zap.Duration("took", time.Since(start)))
		// After the push, not before: the controller may well have supplied the
		// binary itself, and checking first would reject a case that works.
		if err := rm.assertWANProxyBinary(ctx, []string{newAddr}, []string{node}); err != nil {
			log.Debug("AddReplicaWith: haify-proxy binary missing", zap.String("host", newAddr), zap.Error(err))
			return err
		}
		ports, perr := rm.pickWANBindPorts(ctx, []string{newAddr}, dbRes.Port)
		if perr != nil {
			log.Debug("AddReplicaWith: bind port selection failed", zap.Error(perr))
			return perr
		}
		newBind = ports[0]
	} else {
		log.Debug("AddReplicaWith: LAN-only resource, no WAN leg", zap.String("resource", resource))
	}

	// SSH goes to newAddr; DRBD talks over the node's replication network,
	// the same address create and repair write for it.
	replAddr := rm.controller.nodes.GetReplicationAddressByName(node)
	if replAddr == "" {
		replAddr = newAddr
	}
	log.Debug("AddReplicaWith: replication address", zap.String("node", node), zap.String("address", replAddr),
		zap.Int("bind_port", newBind))
	newConfig, err := rm.addReplicaToConfig(liveConfig, resource, node, resolveToIP(replAddr), dbVols,
		dbRes.Port, len(primaries), newBind, dbRes.WANMode && dbRes.DRNode != "", dbRes.DRNode,
		dbRes.Encrypted)
	if err != nil {
		log.Debug("AddReplicaWith: config rewrite failed", zap.Error(err))
		return fmt.Errorf("rewrite config for new replica: %w", err)
	}
	// Growing the primary site moves its majority; recompute rather than leaving
	// a stale number that no longer describes the site.
	if dbRes.WANMode && dbRes.DRNode != "" {
		newConfig = setLocalSiteQuorum(newConfig, len(primaries)+1+len(tiebreakers))
	}

	start = time.Now()
	_, err = rm.deployment.DistributeConfig(ctx, allHosts, newConfig, resPath)
	log.Debug("AddReplicaWith: distribute config", zap.Strings("hosts", allHosts), zap.String("path", resPath),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("distribute config with new replica: %w", err)
	}

	if err := rm.execAllSuccess(ctx, []string{newAddr},
		fmt.Sprintf("sudo drbdadm create-md --max-peers=%d --force %s", deployment.DefaultMaxPeers, resource),
		"create metadata on the new replica"); err != nil {
		log.Debug("AddReplicaWith: create-md failed", zap.String("host", newAddr), zap.Error(err))
		return err
	}
	if err := rm.execAllSuccess(ctx, allHosts,
		fmt.Sprintf("sudo drbdadm adjust %s", resource),
		"adjust peers for the new replica"); err != nil {
		log.Debug("AddReplicaWith: adjust failed", zap.Strings("hosts", allHosts), zap.Error(err))
		return err
	}
	start = time.Now()
	upRes, err := rm.deployment.Exec(ctx, []string{newAddr}, fmt.Sprintf("sudo drbdadm up %s", resource))
	log.Debug("AddReplicaWith: drbdadm up on the new replica", zap.String("host", newAddr),
		zap.Strings("failed_hosts", tracedFailedHosts(upRes)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("bring up the new replica: %w", err)
	}
	if !upRes.AllSuccess() {
		detail := upRes.FailureDetails()
		if !strings.Contains(detail, "exists already") && !strings.Contains(detail, "already? still?") {
			log.Debug("AddReplicaWith: drbdadm up failed", zap.String("node", node))
			return fmt.Errorf("bring up replica on %q: %s", node, detail)
		}
		log.Debug("AddReplicaWith: replica was already up", zap.String("node", node))
	}
	// The newcomer may never have held a resource before; without the boot
	// unit its replica stays down after a reboot.
	if err := rm.ensureBootUnitOnMembers(ctx, resource, []string{newAddr}, dbRes.Encrypted); err != nil {
		log.Debug("AddReplicaWith: boot unit install failed", zap.String("host", newAddr), zap.Error(err))
		return err
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
		log.Debug("AddReplicaWith: away members marked for a config repair", zap.String("stale_config_nodes", dbRes.StaleConfigNodes))
	}
	start = time.Now()
	err = rm.controller.db.SaveResource(ctx, dbRes)
	log.Debug("AddReplicaWith: db save resource", zap.String("resource", resource), zap.String("nodes", dbRes.Nodes),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("record the new replica: %w", err)
	}

	rm.controller.logger.Info("Replica added; initial sync runs in the background",
		zap.String("resource", resource), zap.String("node", node))

	// drbd-reactor will not promote a replica that is still syncing, so the
	// promoter can go in now and takes effect once the copy is UpToDate.
	if err := rm.SyncPromoters(ctx, resource); err != nil {
		log.Debug("AddReplicaWith: promoter sync failed", zap.String("resource", resource), zap.Error(err))
		return fmt.Errorf("replica added on %s, but the resource's promoters could not be placed there: %w; "+
			"fix that and run `haify resource repair %s`", node, err, resource)
	}
	log.Debug("AddReplicaWith: done", zap.String("resource", resource), zap.String("node", node))
	return nil
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
	log.Debug("undoAddReplica: start", zap.Strings("lan_hosts", lanHosts), zap.Int("volumes", len(vols)),
		zap.Bool("encrypted", encrypted), zap.Bool("has_original_config", strings.TrimSpace(originalConfig) != ""))

	if len(lanHosts) > 0 && strings.TrimSpace(originalConfig) != "" {
		start := time.Now()
		if _, err := rm.deployment.DistributeConfig(ctx, lanHosts, originalConfig, resPath); err != nil {
			log.Error("Rollback: failed to restore the previous config", zap.Error(err))
		} else if _, err := rm.deployment.Exec(ctx, lanHosts,
			fmt.Sprintf("sudo drbdadm adjust %s", resource)); err != nil {
			log.Error("Rollback: failed to re-adjust the surviving members", zap.Error(err))
		}
		log.Debug("undoAddReplica: previous config restored and adjusted", zap.Strings("hosts", lanHosts),
			zap.Duration("took", time.Since(start)))
	} else {
		log.Debug("undoAddReplica: no previous config to restore")
	}

	start := time.Now()
	_, err := rm.deployment.Exec(ctx, []string{newAddr},
		fmt.Sprintf("sudo drbdadm down %s 2>/dev/null; sudo rm -f %s", resource, resPath))
	log.Debug("undoAddReplica: new replica down and config removed", zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		log.Error("Rollback: failed to tear down the new replica", zap.Error(err))
	}
	for _, v := range vols {
		log.Debug("undoAddReplica: removing backing volume", zap.String("pool", v.Pool), zap.String("volume", v.VolumeName))
		// The container holds the LV open, and its key is worth destroying
		// whether or not the lvremove that follows succeeds.
		if encrypted {
			rm.closeBackingVolumeOn(ctx, []string{newAddr}, v.Pool, v.VolumeName)
		}
		start := time.Now()
		_, err := rm.deployment.Exec(ctx, []string{newAddr},
			fmt.Sprintf("sudo lvremove -f %s/%s", v.Pool, v.VolumeName))
		log.Debug("undoAddReplica: lvremove done", zap.String("volume", v.VolumeName),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			log.Error("Rollback: failed to remove a backing volume",
				zap.String("volume", v.Pool+"/"+v.VolumeName), zap.Error(err))
		}
	}
	log.Debug("undoAddReplica: done")
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
	log := rm.controller.traceLogger()
	log.Debug("assertWANProxyBinary: checking", zap.Strings("hosts", hosts), zap.Strings("nodes", nodes))
	for i, host := range hosts {
		start := time.Now()
		res, err := rm.deployment.Exec(ctx, []string{host}, wanproxyBinaryProbe)
		log.Debug("assertWANProxyBinary: probe done", zap.String("host", host),
			zap.Strings("failed_hosts", tracedFailedHosts(res)), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return fmt.Errorf("check for the haify-proxy binary on %q: %w", nodes[i], err)
		}
		out := ""
		for _, r := range res.Hosts {
			out = strings.TrimSpace(r.Output)
			break
		}
		log.Debug("assertWANProxyBinary: probe result", zap.String("node", nodes[i]), zap.String("output", out))
		if strings.Contains(out, "missing") {
			return fmt.Errorf(
				"node %q has no executable %s, so its WAN leg would start and immediately fail "+
					"(systemd 203/EXEC, while DRBD reports only \"Connecting\"). Stage the binary there, "+
					"or put one on the controller at the same path so it can be pushed automatically",
				nodes[i], wanproxyLocalBinaryPath)
		}
	}
	log.Debug("assertWANProxyBinary: binary present everywhere", zap.Strings("hosts", hosts))
	return nil
}
