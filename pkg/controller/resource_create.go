package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/wanproxy"
	"go.uber.org/zap"
)

// CreateResourceWithVolumesMetadata creates a resource and persists its
// organizational metadata with the resolved resource configuration.
func (rm *ResourceManager) CreateResourceWithVolumesMetadata(ctx context.Context, name string, port uint32, nodes []string, protocol string, storageType string, drbdOptions map[string]string, volumes []VolumeSpec, wan *WANSpec, metadata ResourceMetadata) error {
	log := rm.resourcesLog()
	log.Debug("CreateResourceWithVolumesMetadata: start", zap.String("resource", name), zap.Uint32("port", port),
		zap.Strings("nodes", nodes), zap.String("protocol", protocol), zap.String("storage_type", storageType),
		zap.Int("drbd_options", len(drbdOptions)), zap.Int("volumes", len(volumes)), zap.Bool("wan", wan != nil),
		zap.Int("labels", len(metadata.Labels)), zap.String("profile", metadata.Profile), zap.Bool("encrypt", metadata.Encrypt))
	if rm.deployment == nil {
		log.Debug("CreateResourceWithVolumesMetadata: no deployment client", zap.String("resource", name))
		return fmt.Errorf("deployment client not set")
	}
	if len(volumes) == 0 {
		log.Debug("CreateResourceWithVolumesMetadata: no volumes", zap.String("resource", name))
		return fmt.Errorf("at least one volume is required")
	}

	if storageType == "" {
		storageType = "lvm"
	}
	if protocol == "" {
		protocol = "C"
	}

	// WAN mode is opt-in and strictly gated: when wan == nil nothing changes
	// and the LAN path stays byte-for-byte unchanged. When set, the resource
	// becomes a two-endpoint (primary + DR) async replica routed through a
	// per-resource haify-proxy pair; see planCreateWAN.
	nodes, protocol, wanCfg, err := rm.planCreateWAN(ctx, name, port, nodes, protocol, wan)
	if err != nil {
		return err
	}

	// Resolve every volume: auto-select+normalize its pool and derive a backing
	// volume name. Volume 0 keeps the historical "<name>_data" name (so existing
	// resources and callers are unaffected); extra volumes use "<name>_vol<K>".
	resolved := make([]resolvedVolume, len(volumes))
	for i, v := range volumes {
		sizeGB, exact, err := volumeSize(v)
		if err != nil {
			log.Debug("CreateResourceWithVolumesMetadata: invalid volume size", zap.String("resource", name), zap.Int("volume_id", i), zap.Error(err))
			return fmt.Errorf("volume %d: %w", i, err)
		}
		v.SizeGB = sizeGB
		pool := v.Pool
		if pool == "" {
			// Auto-select the pool when none was given: with exactly one
			// registered pool name the choice is unambiguous; otherwise the
			// caller must decide.
			selected, err := rm.autoSelectPool(ctx)
			if err != nil {
				log.Debug("CreateResourceWithVolumesMetadata: pool auto-selection failed", zap.String("resource", name), zap.Error(err))
				return err
			}
			pool = selected
		}
		volumeName := fmt.Sprintf("%s_data", name)
		if i > 0 {
			volumeName = fmt.Sprintf("%s_vol%d", name, i)
		}
		resolved[i] = resolvedVolume{
			id:         i,
			volumeName: volumeName,
			pool:       normalizeManagedName(pool),
			sizeGB:     v.SizeGB,
			exactBytes: exact,
			encrypted:  metadata.Encrypt,
		}
		log.Debug("CreateResourceWithVolumesMetadata: resolved volume", zap.String("resource", name), zap.Int("volume_id", i),
			zap.String("volume", volumeName), zap.String("pool", resolved[i].pool), zap.Uint32("size_gb", sizeGB),
			zap.Uint64("size_bytes", exact), zap.Bool("pool_auto_selected", v.Pool == ""))
		if metadata.Encrypt {
			if err := validateLUKSNames(resolved[i].pool, volumeName); err != nil {
				log.Debug("CreateResourceWithVolumesMetadata: names unusable for LUKS", zap.String("resource", name), zap.Error(err))
				return err
			}
		}
	}

	// Encryption is decided here and nowhere else, so the refusal to change it
	// belongs before anything is built. A name that already carries a resource
	// with the other setting is a request to convert in place, which this does
	// not do.
	if err := rm.assertEncryptionNotRetrofitted(ctx, name, metadata.Encrypt); err != nil {
		log.Debug("CreateResourceWithVolumesMetadata: encryption would change", zap.String("resource", name), zap.Error(err))
		return err
	}
	if metadata.Encrypt {
		if err := assertEncryptableStorage(storageType); err != nil {
			log.Debug("CreateResourceWithVolumesMetadata: storage type not encryptable", zap.String("storage_type", storageType), zap.Error(err))
			return err
		}
		for _, v := range resolved {
			if err := rm.assertEncryptableVDO(ctx, v.pool); err != nil {
				log.Debug("CreateResourceWithVolumesMetadata: pool not encryptable", zap.String("pool", v.pool), zap.Error(err))
				return err
			}
		}
	}

	// A replica on a node the controller cannot reach fails part-way — after
	// volumes exist on the others — with an error about whatever step first
	// touched that node. Say so before anything is built.
	if err := rm.assertNodesOnline(nodes); err != nil {
		log.Debug("CreateResourceWithVolumesMetadata: a node is offline", zap.String("resource", name), zap.Error(err))
		return err
	}
	if err := rm.assertCreateQuota(ctx, resolved, nodes, metadata.Labels); err != nil {
		log.Debug("CreateResourceWithVolumesMetadata: quota refuses", zap.String("resource", name), zap.Error(err))
		return err
	}

	rm.controller.logger.Info("Creating DRBD resource",
		zap.String("name", name),
		zap.Uint32("port", port),
		zap.Strings("nodes", nodes),
		zap.String("protocol", protocol),
		zap.Int("volumes", len(resolved)),
		zap.String("storage_type", storageType),
		zap.Bool("encrypted", metadata.Encrypt),
		zap.Strings("options", optionKeys(drbdOptions)))

	// Quorum tiebreaker: a 2-node resource under quorum=majority loses its
	// majority the moment either node fails (the survivor is only 1/2), so
	// DRBD suspends I/O. Add a third diskless node — it votes in quorum but
	// stores no data — when one is available, so the survivor keeps a 2/3
	// majority through any single-node failure. Mirrors LINSTOR's
	// auto-add-quorum-tiebreaker. If no spare node exists we proceed with a
	// bare 2-node resource but flag the quorum risk loudly.
	// WAN resources are strictly two-endpoint (primary + DR); a diskless
	// tiebreaker would need a third mesh connection the haify-proxy pair does not
	// carry, so the auto-tiebreaker is skipped entirely for WAN.
	var disklessNodes []string
	if len(nodes) == 2 && wan == nil {
		auto := rm.controller.config != nil && rm.controller.config.Resource.AutoTiebreaker
		log.Debug("CreateResourceWithVolumesMetadata: two-node resource; considering a tiebreaker",
			zap.String("resource", name), zap.Bool("auto_tiebreaker", auto))
		if auto {
			if tb := rm.selectTiebreaker(ctx, nodes); tb != "" {
				disklessNodes = []string{tb}
				rm.controller.logger.Info("Adding diskless quorum tiebreaker to 2-node resource",
					zap.String("resource", name),
					zap.String("tiebreaker", tb))
			}
		}
		if len(disklessNodes) == 0 {
			rm.controller.logger.Warn("2-node resource has no quorum tiebreaker: a single node failure will suspend I/O (no quorum majority). Register a third node, or it stays a degraded 2-node resource.",
				zap.String("resource", name))
		}
	}

	// Convert diskful node names to IP addresses for deployment.
	nodeIPs := make([]string, len(nodes))
	for i, node := range nodes {
		ip := rm.controller.nodes.GetNodeAddressByName(node)
		if ip == "" {
			ip = node // fallback to node name
		}
		nodeIPs[i] = ip
	}

	// Diskless tiebreaker IPs, and the union of all participating node IPs.
	// Config, `drbdadm up` and teardown reach every node; LV creation and
	// create-md touch diskful nodes only.
	disklessIPs := make([]string, len(disklessNodes))
	for i, node := range disklessNodes {
		ip := rm.controller.nodes.GetNodeAddressByName(node)
		if ip == "" {
			ip = node
		}
		disklessIPs[i] = ip
	}
	allIPs := append(append([]string{}, nodeIPs...), disklessIPs...)
	log.Debug("CreateResourceWithVolumesMetadata: resolved hosts", zap.String("resource", name),
		zap.Strings("hosts", nodeIPs), zap.Strings("diskless_hosts", disklessIPs))

	// Ports and minors reserved below are held until this create returns.
	defer rm.releaseAlloc(name)
	if port == 0 {
		p, err := rm.reservePort(ctx, nodeIPs[0], name)
		if err != nil {
			log.Debug("CreateResourceWithVolumesMetadata: port allocation failed", zap.String("resource", name), zap.Error(err))
			return fmt.Errorf("allocate port: %w", err)
		}
		port = p
		rm.controller.logger.Info("auto-allocated DRBD port", zap.Uint32("port", port), zap.String("resource", name))
	}

	// Pre-flight: reject a port already bound by another DRBD resource on the
	// nodes (including ones Haify does not manage) with a clear message, rather
	// than letting `drbdadm create-md` fail later with an opaque error.
	if conflict, err := rm.findPortConflict(ctx, nodeIPs[0], port, name); err != nil {
		rm.controller.logger.Warn("port conflict pre-check failed; continuing",
			zap.Uint32("port", port), zap.Error(err))
	} else if conflict != "" {
		log.Debug("CreateResourceWithVolumesMetadata: port in use", zap.Uint32("port", port), zap.String("owner", conflict))
		return fmt.Errorf("DRBD port %d is already in use by resource %q; choose a different port", port, conflict)
	}

	// Pre-flight the crypt layer on every diskful node before the first volume
	// is made. Discovering a node without cryptsetup halfway through would
	// leave encrypted volumes on the nodes already visited and a rollback to
	// unwind, for a request that could never have succeeded.
	if metadata.Encrypt {
		if err := rm.assertEncryptionSupported(ctx, nodeIPs, nodes); err != nil {
			log.Debug("CreateResourceWithVolumesMetadata: encryption unsupported on a node", zap.String("resource", name), zap.Error(err))
			return err
		}
	}

	// Roll back partial state if a later step fails: a half-created resource
	// (e.g. LVs made but create-md failed) otherwise leaves orphaned backing
	// volumes and a stray .res that block a clean retry.
	committed := false
	defer func() {
		if committed {
			return
		}
		rm.controller.logger.Warn("Resource create failed; rolling back partial state",
			zap.String("name", name))
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, downErr := rm.deployment.DRBDDown(cleanupCtx, allIPs, name)
		_, rmErr := rm.deployment.Exec(cleanupCtx, allIPs, fmt.Sprintf("sudo rm -f /etc/drbd.d/%s.res", name))
		log.Debug("CreateResourceWithVolumesMetadata: rollback took resource down and removed config", zap.String("resource", name),
			zap.Strings("hosts", allIPs), zap.NamedError("down_error", downErr), zap.NamedError("rm_error", rmErr))
		// A WAN create may have provisioned the haify-proxy pair before failing;
		// tear it down too so a retry starts clean. Best-effort (idempotent at
		// the shell level). nodeIPs is [primaryIP, drIP] for a WAN resource.
		if wan != nil && len(nodeIPs) >= 2 {
			derr := wanproxy.DeprovisionMulti(cleanupCtx, rm.wanproxyDeployClient(), wanproxy.MultiSpec{
				Resource:         name,
				PrimaryNodeAddrs: nodeIPs[:len(nodeIPs)-1],
				DRNodeAddr:       nodeIPs[len(nodeIPs)-1],
				BaseWANPort:      int(wan.WANPort),
				BaseDRBDPort:     int(port),
			})
			log.Debug("CreateResourceWithVolumesMetadata: rollback deprovisioned WAN proxy", zap.String("resource", name), zap.Error(derr))
		}
		// Backing volumes exist on diskful nodes only. An encrypted volume's
		// container has to be closed and its key destroyed first: while the
		// container is open the LV is held and lvremove refuses, and a key left
		// behind after a failed create is a secret nothing will ever collect.
		for _, v := range resolved {
			if v.encrypted {
				rm.closeBackingVolumeOn(cleanupCtx, nodeIPs, v.pool, v.volumeName)
			}
			var verr error
			if storageType == "zfs" || storageType == "zfs-thin" {
				_, verr = rm.deployment.ZFSDestroyDataset(cleanupCtx, nodeIPs, fmt.Sprintf("%s/%s", v.pool, v.volumeName))
			} else {
				_, verr = rm.deployment.LVRemove(cleanupCtx, nodeIPs, fmt.Sprintf("/dev/%s/%s", v.pool, v.volumeName))
			}
			log.Debug("CreateResourceWithVolumesMetadata: rollback removed backing volume", zap.String("resource", name),
				zap.String("pool", v.pool), zap.String("volume", v.volumeName), zap.Error(verr))
		}
	}()

	// 1. Create the backing storage for every volume on all diskful nodes.
	for _, v := range resolved {
		log.Debug("CreateResourceWithVolumesMetadata: creating backing volume", zap.String("resource", name),
			zap.String("pool", v.pool), zap.String("volume", v.volumeName))
		if err := rm.createBackingVolume(ctx, nodeIPs, nodes, storageType, v.pool, v.volumeName, v.sizeGB, v.encrypted); err != nil {
			log.Debug("CreateResourceWithVolumesMetadata: backing volume failed", zap.String("volume", v.volumeName), zap.Error(err))
			return err
		}
	}

	// 2. Generate DRBD config.
	// Allocate node-global device minors: minors are shared across every DRBD
	// resource on a node. nextGlobalMinor returns (max existing minor)+1, so a
	// run of len(resolved) consecutive minors from that base is collision-free.
	baseMinor, err := rm.reserveMinors(ctx, allIPs, len(resolved), name)
	if err != nil {
		log.Debug("CreateResourceWithVolumesMetadata: minor allocation failed", zap.String("resource", name), zap.Error(err))
		return fmt.Errorf("failed to allocate device minor: %w", err)
	}
	for i := range resolved {
		resolved[i].minor = baseMinor + i
	}
	log.Debug("CreateResourceWithVolumesMetadata: allocated minors", zap.String("resource", name),
		zap.Int("base_minor", baseMinor), zap.Int("count", len(resolved)))
	// wanCfg is nil for a LAN resource (output unchanged); non-nil renders the
	// WAN variant (protocol A + pull-ahead + loopback addresses).
	drbdOptions = rm.withThinResyncDefaults(ctx, drbdOptions, storageType, resolved)
	drbdConfig := rm.generateDrbdConfig(name, port, resolved, nodes, disklessNodes, protocol, storageType, drbdOptions, wanCfg)

	// 3. Distribute config to all nodes (diskful + diskless tiebreaker)
	start := time.Now()
	configResult, err := rm.deployment.DistributeConfig(ctx, allIPs, drbdConfig, fmt.Sprintf("/etc/drbd.d/%s.res", name))
	log.Debug("CreateResourceWithVolumesMetadata: distributed config", zap.String("resource", name), zap.Strings("hosts", allIPs),
		zap.Bool("success", configResult != nil && configResult.Success), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("failed to distribute config: %w", err)
	}
	if !configResult.Success {
		return fmt.Errorf("config distribution failed: %s", configResult.FailureDetails())
	}

	// 4. Create metadata on diskful nodes only. A diskless tiebreaker has no
	// backing disk, so `drbdadm create-md` does not apply to it.
	start = time.Now()
	mdResult, err := rm.deployment.DRBDCreateMD(ctx, nodeIPs, name, minMetadataPeers)
	log.Debug("CreateResourceWithVolumesMetadata: created metadata", zap.String("resource", name), zap.Strings("hosts", nodeIPs),
		zap.Array("failed_hosts", resourcesFailedHosts{mdResult}), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("failed to create metadata: %w", err)
	}
	if !mdResult.AllSuccess() {
		return fmt.Errorf("metadata creation failed: %s", mdResult.FailureDetails())
	}

	// 4a. WAN only: bring up the per-resource haify-proxy pair BEFORE `drbdadm up`.
	// In WAN mode DRBD connects to 127.0.0.1:<port> (the local proxy), so the
	// loopback proxy must be listening first — otherwise the resource comes up
	// with nothing to connect to. The rollback defer deprovisions on any later
	// failure. This step is entirely gated behind wan != nil.
	if wan != nil {
		// nodes is [primaries..., DR]; the DR is always last (set when the WAN
		// spec was validated), so the primary addresses are everything before it.
		primaryAddrs := nodeIPs[:len(nodeIPs)-1]
		drAddr := nodeIPs[len(nodeIPs)-1]

		multi := wanproxy.MultiSpec{
			Resource:         name,
			PrimaryNodeAddrs: primaryAddrs,
			// Names, not addresses, decide what each leg is called: a node that
			// is later renumbered keeps its name, and so keeps its leg.
			PrimaryNodeKeys:   nodes[:len(nodes)-1],
			DRNodeAddr:        drAddr,
			DRPublicEndpoint:  wan.DREndpoint,
			BaseWANPort:       int(wan.WANPort),
			BaseDRBDPort:      int(port),
			PrimaryEgressAddr: wan.EgressAddress,
			BinaryFor:         rm.wanproxyBinaryResolver(ctx, append(append([]string{}, primaryAddrs...), drAddr)),
		}
		rm.controller.logger.Info("Provisioning WAN replication proxy before DRBD up",
			zap.String("resource", name),
			zap.Strings("primaries", primaryAddrs),
			zap.String("dr", drAddr),
			zap.String("dr_endpoint", wan.DREndpoint),
			zap.Int("base_wan_port", int(wan.WANPort)))
		start = time.Now()
		err := wanproxy.ProvisionMulti(ctx, rm.wanproxyDeployClient(), multi)
		log.Debug("CreateResourceWithVolumesMetadata: provisioned WAN proxy", zap.String("resource", name),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return fmt.Errorf("provision WAN proxy for %s: %w", name, err)
		}
	}

	// 5. Bring up resource on all nodes. The diskless node comes up Diskless
	// and connects; it only participates in quorum.
	start = time.Now()
	upResult, err := rm.deployment.DRBDUp(ctx, allIPs, name)
	log.Debug("CreateResourceWithVolumesMetadata: brought resource up", zap.String("resource", name), zap.Strings("hosts", allIPs),
		zap.Array("failed_hosts", resourcesFailedHosts{upResult}), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("failed to bring up resource: %w", err)
	}
	if !upResult.AllSuccess() {
		return fmt.Errorf("resource up failed: %s", upResult.FailureDetails())
	}

	// 5a. Establish the initial UpToDate generation. A freshly created resource
	// comes up Inconsistent on EVERY volume of EVERY node with no UpToDate copy
	// anywhere, so it cannot be promoted (a normal `drbdsetup primary` fails with
	// "Need access to UpToDate data", exit 17) and it never resyncs — there is no
	// sync source. Force-promote the first diskful node once, then demote it back
	// to Secondary: that marks its volumes UpToDate and gives peers a source to
	// sync from, leaving the resource in a neutral Secondary+UpToDate state.
	//
	// Doing it HERE (before any gateway state volume is added) is what makes the
	// later gateway promote a plain non-forced promote: otherwise the auto-added
	// state volume becomes UpToDate on its own while the data volume stays
	// Inconsistent, and the promote fails on the data volume. This mirrors the
	// initial force the CSI/filesystem path already performs. It is safe because
	// create-md (step 4) just wiped all metadata: every replica is Inconsistent,
	// so there is no data anywhere to lose. This path only ever runs for a
	// brand-new resource — adopting an existing resource goes through
	// AdoptResource, which never reaches here.
	//
	// On thin storage there is nothing to sync; see initial_sync.go.
	skipped := false
	if rm.backedByZeroReadingStorage(ctx, storageType, nodeIPs, resolved) {
		log.Debug("CreateResourceWithVolumesMetadata: zero-reading storage; skipping initial sync", zap.String("resource", name))
		if err := rm.skipInitialSync(ctx, name, nodeIPs[0]); err != nil {
			rm.controller.logger.Warn("Could not skip the initial sync; running a full one",
				zap.String("resource", name), zap.Error(err))
		} else {
			skipped = true
		}
	}
	if !skipped {
		log.Debug("CreateResourceWithVolumesMetadata: establishing initial sync", zap.String("resource", name), zap.String("host", nodeIPs[0]))
		if err := rm.establishInitialSync(ctx, name, nodeIPs[0]); err != nil {
			log.Debug("CreateResourceWithVolumesMetadata: initial sync failed", zap.String("resource", name), zap.Error(err))
			return fmt.Errorf("failed to establish initial sync for %s: %w", name, err)
		}
	}

	// 5a. WAN only: now that the resource exists and its peers are configured,
	// narrow quorum to the primary site so the DR does not get a vote on whether
	// home may write. This could not be done in the generated config — a numeric
	// quorum would have blocked the force-promote above. Best effort: a resource
	// that is otherwise created should not be failed for a tuning step, and the
	// setting can be applied later with `resource set-options`.
	if wan != nil {
		// allIPs is every participant; nodeIPs is the diskful ones, the DR last.
		localVoters := localSiteVoters(len(allIPs))
		log.Debug("CreateResourceWithVolumesMetadata: narrowing quorum to the primary site", zap.String("resource", name),
			zap.Int("local_voters", localVoters))
		if err := rm.applyLocalSiteQuorum(ctx, name, allIPs, localVoters); err != nil {
			rm.controller.logger.Warn("Could not narrow quorum to the primary site; the DR still votes",
				zap.String("resource", name), zap.Error(err))
		}
	}

	// 5b. Ensure a DRBD boot unit is installed and enabled on every
	// participating node so a rebooted node re-runs `drbdadm adjust all` on boot
	// and auto-rejoins replication without a manual `drbdadm adjust`. The
	// packaged drbd.service is an LSB/SysV unit whose Default-Start header is
	// empty, so `systemctl enable drbd.service` fails ("Default-Start contains
	// no runlevels") and it can never be enabled. Instead we install our own
	// native systemd oneshot (haify-drbd-up.service) that runs `drbdadm adjust all`
	// before drbd-reactor, letting the reactor promote once resources are up.
	// Installing/enabling is idempotent and harmless on any node with
	// drbd-utils (diskful or diskless tiebreaker). Best-effort: never fail
	// resource creation just because it did not stick — the resource is
	// already up at this point.
	//
	// Except when the resource is encrypted. That same unit is what opens the
	// crypt containers at boot, so without it a rebooted node comes back with
	// no backing device and the replica attaches Diskless — silently, and only
	// discovered the next time the node restarts, which is the worst possible
	// moment to find out. A resource that cannot survive a reboot is not one to
	// hand back as created.
	if bootErr := rm.ensureDRBDBootUnitEnabled(ctx, allIPs); bootErr != nil && metadata.Encrypt {
		log.Debug("CreateResourceWithVolumesMetadata: boot unit missing on an encrypted resource", zap.String("resource", name), zap.Error(bootErr))
		return fmt.Errorf("install the boot unit that opens the LUKS containers: %w", bootErr)
	}

	// 6. Save to database
	if rm.controller.db != nil {
		dbRes := &database.Resource{
			Name:          name,
			Port:          int(port),
			Nodes:         strings.Join(nodes, ","),
			Protocol:      protocol,
			Replicas:      len(nodes),
			DisklessNodes: strings.Join(disklessNodes, ","),
			Labels:        cloneStringMap(metadata.Labels),
			Profile:       metadata.Profile,
			Encrypted:     metadata.Encrypt,
		}
		// Persist WAN metadata so DeleteResource can deprovision the proxy pair
		// and the UI/CLI can show the resource is WAN-replicated.
		if wan != nil {
			dbRes.WANMode = true
			dbRes.DRNode = wan.DRNode
			dbRes.DREndpoint = wan.DREndpoint
			dbRes.WANPort = int(wan.WANPort)
			dbRes.WANEgressAddress = wan.EgressAddress
		}
		err := rm.controller.db.SaveResource(ctx, dbRes)
		log.Debug("CreateResourceWithVolumesMetadata: saved resource record", zap.String("resource", name), zap.Error(err))
		if err != nil {
			rm.controller.logger.Warn("Failed to save resource to database", zap.Error(err))
		}

		for _, v := range resolved {
			volumeRecord := &database.Volume{
				ResourceName: name,
				VolumeName:   v.volumeName,
				VolumeID:     v.id,
				Pool:         v.pool,
				SizeGB:       int(v.sizeGB),
				SizeBytes:    int64(v.exactBytes),
				// The device DRBD actually consumes, crypt container included.
				// Teardown and resize read this back to decide what they are
				// dealing with, exactly as they already do for a zvol.
				Device: v.backingDevice(storageType),
			}
			err := rm.controller.db.SaveVolume(ctx, volumeRecord)
			log.Debug("CreateResourceWithVolumesMetadata: saved volume record", zap.String("resource", name),
				zap.Int("volume_id", v.id), zap.String("device", volumeRecord.Device), zap.Error(err))
			if err != nil {
				rm.controller.logger.Warn("Failed to save volume to database",
					zap.String("resource", name),
					zap.Int("volume", v.id),
					zap.Error(err))
			}
		}
	} else {
		log.Debug("CreateResourceWithVolumesMetadata: no database; resource not recorded", zap.String("resource", name))
	}

	rm.controller.logger.Info("DRBD resource created successfully",
		zap.String("name", name))

	committed = true
	return nil
}
