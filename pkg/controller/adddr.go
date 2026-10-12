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

// AddDR attaches an asynchronous off-site replica to a resource that is already
// running, turning a LAN cluster into the two-site shape without recreating it.
//
// WAN mode could only be chosen when a resource was created. That is the wrong
// time to have to decide: off-site DR is exactly the thing an operator adds
// *after* a service has proven it matters, and the only way to get there was to
// destroy a serving resource and rebuild it — which for anything holding real
// data means an outage and a restore.
//
// Everything here is additive. The existing replicas keep their synchronous
// mesh and their addresses; the DR joins through one haify-proxy leg per replica
// (DRBD 9 is a full mesh, so a failover inside the primary site must not land
// on a node with no path to the DR). The mounted filesystem is never touched,
// so a promoted resource keeps serving throughout.
func (rm *ResourceManager) AddDR(ctx context.Context, resource, drNode, drEndpoint string, wanPort uint32, egressAddr string) error {
	log := rm.controller.traceLogger()
	log.Debug("AddDR: start", zap.String("resource", resource), zap.String("dr_node", drNode),
		zap.String("dr_endpoint", drEndpoint), zap.Uint32("wan_port", wanPort), zap.String("egress_addr", egressAddr))
	if rm.deployment == nil {
		log.Debug("AddDR: no deployment client", zap.String("resource", resource))
		return fmt.Errorf("deployment client not set")
	}
	if rm.controller.db == nil {
		log.Debug("AddDR: no database", zap.String("resource", resource))
		return fmt.Errorf("database not available")
	}

	start := time.Now()
	dbRes, err := rm.controller.db.GetResource(ctx, resource)
	log.Debug("AddDR: db get resource", zap.String("resource", resource), zap.Bool("found", dbRes != nil),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || dbRes == nil {
		log.Debug("AddDR: resource not found", zap.String("resource", resource), zap.Error(err))
		return fmt.Errorf("resource %q not found", resource)
	}
	if dbRes.WANMode {
		log.Debug("AddDR: resource already has a DR site", zap.String("resource", resource),
			zap.String("dr_node", dbRes.DRNode))
		return fmt.Errorf("resource %q already replicates to DR node %q; remove it first to change the DR site",
			resource, dbRes.DRNode)
	}

	drNode = strings.TrimSpace(drNode)
	drEndpoint = strings.TrimSpace(drEndpoint)
	if drNode == "" {
		log.Debug("AddDR: no DR node given", zap.String("resource", resource))
		return fmt.Errorf("a DR node is required")
	}
	if drEndpoint == "" {
		log.Debug("AddDR: no DR endpoint given", zap.String("resource", resource))
		return fmt.Errorf("a DR endpoint is required (the address the primary site dials)")
	}
	if err := validDREndpoint(drEndpoint); err != nil {
		log.Debug("AddDR: invalid DR endpoint", zap.String("dr_endpoint", drEndpoint), zap.Error(err))
		return err
	}
	if rm.controller.nodes.GetNodeAddressByName(drNode) == "" {
		log.Debug("AddDR: DR node not registered", zap.String("dr_node", drNode))
		return fmt.Errorf("DR node %q is not a registered node", drNode)
	}

	primaries := splitCSV(dbRes.Nodes)
	for _, n := range primaries {
		if n == drNode {
			log.Debug("AddDR: DR node already holds a replica", zap.String("dr_node", drNode))
			return fmt.Errorf("node %q already holds a replica of %q; the DR site must be a different node", drNode, resource)
		}
	}
	for _, n := range splitCSV(dbRes.DisklessNodes) {
		if n == drNode {
			log.Debug("AddDR: DR node is the tiebreaker", zap.String("dr_node", drNode))
			return fmt.Errorf("node %q is the quorum tiebreaker of %q; it cannot also be the DR site", drNode, resource)
		}
	}
	if len(primaries) == 0 {
		log.Debug("AddDR: resource has no replicas", zap.String("resource", resource))
		return fmt.Errorf("resource %q has no replicas to replicate from", resource)
	}

	if wanPort == 0 {
		wanPort = randomWANPort()
		rm.controller.logger.Info("auto-allocated WAN proxy port",
			zap.String("resource", resource), zap.Uint32("wan_port", wanPort))
	}

	drAddr := rm.controller.ResolveHost(drNode)
	primaryAddrs := make([]string, 0, len(primaries))
	for _, n := range primaries {
		primaryAddrs = append(primaryAddrs, rm.controller.ResolveHost(n))
	}
	log.Debug("AddDR: resolved addresses", zap.String("resource", resource), zap.String("dr_addr", drAddr),
		zap.Strings("primaries", primaries), zap.Strings("primary_addrs", primaryAddrs))

	rm.controller.logger.Info("Adding DR site to running resource",
		zap.String("resource", resource),
		zap.Strings("primaries", primaries),
		zap.String("dr_node", drNode),
		zap.String("dr_endpoint", drEndpoint),
		zap.Uint32("wan_port", wanPort))

	// The DR inherits the resource's minors; make sure they are free there
	// before anything is provisioned.
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		log.Debug("AddDR: resource hosts lookup failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	if len(hosts) == 0 {
		log.Debug("AddDR: no reachable hosts", zap.String("resource", resource))
		return fmt.Errorf("resource %q has no reachable hosts", resource)
	}
	resPath := fmt.Sprintf("/etc/drbd.d/%s.res", resource)
	start = time.Now()
	catRes, err := rm.deployment.Exec(ctx, []string{hosts[0]}, "cat "+resPath)
	log.Debug("AddDR: read live config", zap.String("host", hosts[0]), zap.String("path", resPath),
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
		log.Debug("AddDR: live config unreadable or empty", zap.String("resource", resource), zap.Bool("read_ok", readOK))
		return fmt.Errorf("read resource config for %q: %s", resource, catRes.FailureDetails())
	}
	if err := rm.assertMinorsFreeOn(ctx, drAddr, resource, resourceMinors(liveConfig)); err != nil {
		log.Debug("AddDR: minors not free on DR node", zap.String("dr_addr", drAddr), zap.Error(err))
		return err
	}

	// A diskful peer needs a bitmap slot, and slots are allocated once, when
	// metadata is created. Check before provisioning anything: without this the
	// failure surfaces from `drbdadm adjust` as "(162) Invalid configuration
	// request", by which point there are LVs, proxy units and a rewritten config
	// to unwind, and nothing in the message says what to do about it.
	if err := rm.assertBitmapSlotFree(ctx, primaryAddrs, primaries, resource); err != nil {
		log.Debug("AddDR: bitmap slot check failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	// The DR copy of an encrypted resource is encrypted too, the way add-replica
	// builds a local one. A plain LV there would put the only plaintext copy of
	// the data at the site furthest from the operator, and nothing in `resource
	// status` would say so. Refuse now if the DR node cannot do it, before any
	// storage is provisioned there.
	log.Debug("AddDR: encryption", zap.String("resource", resource), zap.Bool("encrypted", dbRes.Encrypted))
	if dbRes.Encrypted {
		if err := rm.assertEncryptionSupported(ctx, []string{drAddr}, []string{drNode}); err != nil {
			log.Debug("AddDR: DR node cannot encrypt", zap.String("dr_node", drNode), zap.Error(err))
			return err
		}
	}

	// The DR node needs backing volumes of its own: it is a full replica, not a
	// diskless voter. Sizes and pools come from the resource record, which is
	// where create wrote them.
	start = time.Now()
	dbVols, err := rm.controller.db.ListVolumes(ctx, resource)
	log.Debug("AddDR: db list volumes", zap.String("resource", resource), zap.Int("count", len(dbVols)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("list volumes of %q: %w", resource, err)
	}
	if len(dbVols) == 0 {
		log.Debug("AddDR: no recorded volumes", zap.String("resource", resource))
		return fmt.Errorf("resource %q has no recorded volumes to replicate", resource)
	}
	// Size the DR's volumes from what the primary site is actually exporting,
	// not from the recorded request — see primaryBackingSizes.
	backingSizes, err := rm.primaryBackingSizes(ctx, primaryAddrs, resource, len(dbVols))
	if err != nil {
		log.Debug("AddDR: backing sizes unreadable", zap.String("resource", resource), zap.Error(err))
		return err
	}
	backingSizes = replicaBackingSizes(backingSizes, dbRes.Encrypted)
	log.Debug("AddDR: DR backing sizes", zap.String("resource", resource), zap.Uint64s("size_bytes", backingSizes))

	bindPorts, err := rm.pickWANBindPorts(ctx, primaryAddrs, dbRes.Port)
	if err != nil {
		log.Debug("AddDR: bind port selection failed", zap.String("resource", resource), zap.Error(err))
		return err
	}

	// Everyone that must end up holding the new config.
	lanHosts := append([]string{}, primaryAddrs...)
	for _, n := range splitCSV(dbRes.DisklessNodes) {
		lanHosts = append(lanHosts, rm.controller.ResolveHost(n))
	}
	allHosts := append(append([]string{}, lanHosts...), drAddr)
	log.Debug("AddDR: hosts that get the new config", zap.String("resource", resource),
		zap.Strings("lan_hosts", lanHosts), zap.Strings("hosts", allHosts))

	multi := wanproxy.MultiSpec{
		Resource:          resource,
		PrimaryNodeAddrs:  primaryAddrs,
		DRNodeAddr:        drAddr,
		DRPublicEndpoint:  drEndpoint,
		BaseWANPort:       int(wanPort),
		BaseDRBDPort:      dbRes.Port,
		PrimaryEgressAddr: egressAddr,
		// Resolved per node: the two ends of a WAN leg are frequently different
		// architectures.
		BinaryFor: rm.wanproxyBinaryResolver(ctx, append(append([]string{}, primaryAddrs...), drAddr)),
	}

	// From here on the cluster is being changed, so a failure has to unwind. A
	// half-applied DR leaves the running replicas carrying a peer they can never
	// reach, plus orphaned volumes and proxy units on the DR — none of which is
	// visible until something goes looking for quorum.
	//
	// The rollback context is detached from ctx: the most likely reason to be
	// here is that ctx was cancelled or timed out, and unwinding with a dead
	// context would do nothing at all.
	succeeded := false
	defer func() {
		if succeeded {
			return
		}
		log.Debug("AddDR: failed part way, rolling back", zap.String("resource", resource))
		rm.undoAddDR(context.WithoutCancel(ctx), resource, resPath, liveConfig, lanHosts, drAddr, multi, dbVols,
			dbRes.Encrypted)
	}()

	for i, v := range dbVols {
		log.Debug("AddDR: creating DR backing volume", zap.String("host", drAddr), zap.String("pool", v.Pool),
			zap.String("volume", v.VolumeName), zap.Uint64("size_bytes", backingSizes[i]))
		if err := rm.createBackingVolumeOn(ctx, drAddr, v.Pool, v.VolumeName, backingSizes[i]); err != nil {
			log.Debug("AddDR: backing volume creation failed", zap.String("volume", v.VolumeName), zap.Error(err))
			return fmt.Errorf("create backing volume %s/%s on DR node %q: %w", v.Pool, v.VolumeName, drNode, err)
		}
		// The DR generates its own key on the node, as every other replica
		// did; nothing about the key crosses the WAN.
		if dbRes.Encrypted {
			if err := rm.encryptBackingVolumeOn(ctx, drAddr, drNode, v.Pool, v.VolumeName,
				fmt.Sprintf("/dev/%s/%s", v.Pool, v.VolumeName)); err != nil {
				log.Debug("AddDR: encrypting DR volume failed", zap.String("volume", v.VolumeName), zap.Error(err))
				return err
			}
		}
	}

	// Proxy first: in WAN mode DRBD connects to a loopback port the proxy owns,
	// so bringing DRBD up first would find nothing listening.
	start = time.Now()
	err = wanproxy.ProvisionMulti(ctx, rm.wanproxyDeployClient(), multi)
	log.Debug("AddDR: provision WAN proxy legs", zap.String("resource", resource),
		zap.Int("base_wan_port", multi.BaseWANPort), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("provision WAN proxy for %s: %w", resource, err)
	}
	// After the push, not before: the controller may well have supplied the
	// binary itself, and checking first would reject a case that works.
	if err := rm.assertWANProxyBinary(ctx, append(append([]string{}, primaryAddrs...), drAddr),
		append(append([]string{}, primaries...), drNode)); err != nil {
		log.Debug("AddDR: haify-proxy binary missing", zap.String("resource", resource), zap.Error(err))
		return err
	}

	// Rewrite the LIVE config rather than regenerating from the record: the
	// resource row does not carry the storage type or DRBD option overrides, so
	// a regenerated file would silently drop them (and any manual tuning).
	newConfig, err := rm.addDRToConfig(liveConfig, resource, drNode, primaries,
		splitCSV(dbRes.DisklessNodes), drAddr, dbVols, dbRes.Port, bindPorts, dbRes.Encrypted)
	if err != nil {
		log.Debug("AddDR: config rewrite failed", zap.String("resource", resource), zap.Error(err))
		return fmt.Errorf("rewrite config for DR site: %w", err)
	}
	// The DR replicates but does not get a vote in whether the primary site may
	// write; see setLocalSiteQuorum.
	newConfig = setLocalSiteQuorum(newConfig, len(primaries)+len(splitCSV(dbRes.DisklessNodes)))

	start = time.Now()
	_, err = rm.deployment.DistributeConfig(ctx, allHosts, newConfig, resPath)
	log.Debug("AddDR: distribute config", zap.Strings("hosts", allHosts), zap.String("path", resPath),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("distribute DR config: %w", err)
	}

	// Metadata on the DR only — the existing replicas keep theirs.
	if err := rm.execAllSuccess(ctx, []string{drAddr},
		fmt.Sprintf("sudo drbdadm create-md --max-peers=%d --force %s", deployment.DefaultMaxPeers, resource),
		"create metadata on DR node"); err != nil {
		log.Debug("AddDR: create-md on DR failed", zap.String("host", drAddr), zap.Error(err))
		return err
	}

	// Let the running peers learn the new connection, then bring the DR up.
	if err := rm.execAllSuccess(ctx, allHosts,
		fmt.Sprintf("sudo drbdadm adjust %s", resource),
		"adjust peers for DR site"); err != nil {
		log.Debug("AddDR: adjust failed", zap.Strings("hosts", allHosts), zap.Error(err))
		return err
	}
	start = time.Now()
	upRes, err := rm.deployment.Exec(ctx, []string{drAddr}, fmt.Sprintf("sudo drbdadm up %s", resource))
	log.Debug("AddDR: drbdadm up on DR", zap.String("host", drAddr),
		zap.Strings("failed_hosts", tracedFailedHosts(upRes)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("bring up DR replica: %w", err)
	}
	if !upRes.AllSuccess() {
		detail := upRes.FailureDetails()
		if !strings.Contains(detail, "exists already") && !strings.Contains(detail, "already? still?") {
			log.Debug("AddDR: DR up failed", zap.String("dr_node", drNode))
			return fmt.Errorf("bring up DR replica on %q: %s", drNode, detail)
		}
		log.Debug("AddDR: DR replica was already up", zap.String("dr_node", drNode))
	}
	// The DR is usually a node that holds nothing else of this cluster, so
	// nothing has put the boot unit there yet.
	if err := rm.ensureBootUnitOnMembers(ctx, resource, []string{drAddr}, dbRes.Encrypted); err != nil {
		log.Debug("AddDR: boot unit install failed", zap.String("host", drAddr), zap.Error(err))
		return err
	}

	// The cluster now carries the DR; past this point a failure to write the
	// record is worth reporting but not worth tearing the replica back down.
	succeeded = true

	// Record it last, so a failure above leaves the DB describing reality.
	dbRes.WANMode = true
	dbRes.DRNode = drNode
	dbRes.DREndpoint = drEndpoint
	dbRes.WANPort = int(wanPort)
	dbRes.WANEgressAddress = egressAddr
	if !strings.Contains(","+dbRes.Nodes+",", ","+drNode+",") {
		dbRes.Nodes = dbRes.Nodes + "," + drNode
	}
	start = time.Now()
	err = rm.controller.db.SaveResource(ctx, dbRes)
	log.Debug("AddDR: db save resource", zap.String("resource", resource), zap.String("nodes", dbRes.Nodes),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("record DR site: %w", err)
	}

	log.Debug("AddDR: done", zap.String("resource", resource), zap.String("dr_node", drNode),
		zap.Ints("bind_ports", bindPorts))
	rm.controller.logger.Info("DR site added; initial sync runs in the background",
		zap.String("resource", resource), zap.String("dr_node", drNode))
	return nil
}

// undoAddDR returns the cluster to the LAN shape after a failed AddDR.
//
// Order matters and is the reverse of the build: the running replicas are put
// back on the original config first, so they stop trying to reach a peer that
// is about to disappear, and only then is the DR dismantled. Every step is
// best-effort and logged rather than returned — the caller is already returning
// the failure that got us here, and a rollback that aborts halfway is worse than
// one that keeps going.
func (rm *ResourceManager) undoAddDR(ctx context.Context, resource, resPath, originalConfig string,
	lanHosts []string, drAddr string, multi wanproxy.MultiSpec, vols []*database.Volume, encrypted bool) {

	log := rm.controller.logger.With(zap.String("resource", resource), zap.String("dr_node", drAddr))
	log.Warn("Rolling back DR site")
	log.Debug("undoAddDR: start", zap.Strings("lan_hosts", lanHosts), zap.Int("volumes", len(vols)),
		zap.Bool("encrypted", encrypted), zap.Bool("has_original_config", strings.TrimSpace(originalConfig) != ""))

	if len(lanHosts) > 0 && strings.TrimSpace(originalConfig) != "" {
		start := time.Now()
		if _, err := rm.deployment.DistributeConfig(ctx, lanHosts, originalConfig, resPath); err != nil {
			log.Error("Rollback: failed to restore config on the primary site", zap.Error(err))
		} else if _, err := rm.deployment.Exec(ctx, lanHosts,
			fmt.Sprintf("sudo drbdadm adjust %s", resource)); err != nil {
			log.Error("Rollback: failed to re-adjust the primary site", zap.Error(err))
		}
		log.Debug("undoAddDR: primary site config restored and adjusted", zap.Strings("hosts", lanHosts),
			zap.Duration("took", time.Since(start)))
	} else {
		log.Debug("undoAddDR: no original config to restore")
	}

	start := time.Now()
	_, err := rm.deployment.Exec(ctx, []string{drAddr},
		fmt.Sprintf("sudo drbdadm down %s 2>/dev/null; sudo rm -f %s", resource, resPath))
	log.Debug("undoAddDR: DR replica down and config removed", zap.String("host", drAddr),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		log.Error("Rollback: failed to tear down the DR replica", zap.Error(err))
	}

	start = time.Now()
	err = wanproxy.DeprovisionMulti(ctx, rm.wanproxyDeployClient(), multi)
	log.Debug("undoAddDR: WAN proxy legs deprovisioned", zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		log.Error("Rollback: failed to remove the WAN proxy legs", zap.Error(err))
	}

	// The volumes go last: while the config still names them, removing them
	// would only produce a replica that is up and broken.
	for _, v := range vols {
		log.Debug("undoAddDR: removing DR backing volume", zap.String("host", drAddr), zap.String("pool", v.Pool),
			zap.String("volume", v.VolumeName))
		// The open container holds the LV, and its key is worth destroying
		// whether or not the lvremove that follows succeeds.
		if encrypted {
			rm.closeBackingVolumeOn(ctx, []string{drAddr}, v.Pool, v.VolumeName)
		}
		start := time.Now()
		_, err := rm.deployment.Exec(ctx, []string{drAddr},
			fmt.Sprintf("sudo lvremove -f %s/%s", v.Pool, v.VolumeName))
		log.Debug("undoAddDR: lvremove done", zap.String("volume", v.VolumeName),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			log.Error("Rollback: failed to remove a backing volume on the DR node",
				zap.String("volume", v.Pool+"/"+v.VolumeName), zap.Error(err))
		}
	}
	log.Debug("undoAddDR: done")
}
