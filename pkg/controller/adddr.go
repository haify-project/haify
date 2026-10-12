package controller

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

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
	if dbRes.WANMode {
		return fmt.Errorf("resource %q already replicates to DR node %q; remove it first to change the DR site",
			resource, dbRes.DRNode)
	}

	drNode = strings.TrimSpace(drNode)
	drEndpoint = strings.TrimSpace(drEndpoint)
	if drNode == "" {
		return fmt.Errorf("a DR node is required")
	}
	if drEndpoint == "" {
		return fmt.Errorf("a DR endpoint is required (the address the primary site dials)")
	}
	if err := validDREndpoint(drEndpoint); err != nil {
		return err
	}
	if rm.controller.nodes.GetNodeAddressByName(drNode) == "" {
		return fmt.Errorf("DR node %q is not a registered node", drNode)
	}

	primaries := splitCSV(dbRes.Nodes)
	for _, n := range primaries {
		if n == drNode {
			return fmt.Errorf("node %q already holds a replica of %q; the DR site must be a different node", drNode, resource)
		}
	}
	for _, n := range splitCSV(dbRes.DisklessNodes) {
		if n == drNode {
			return fmt.Errorf("node %q is the quorum tiebreaker of %q; it cannot also be the DR site", drNode, resource)
		}
	}
	if len(primaries) == 0 {
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
		return err
	}
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
	if err := rm.assertMinorsFreeOn(ctx, drAddr, resource, resourceMinors(liveConfig)); err != nil {
		return err
	}

	// A diskful peer needs a bitmap slot, and slots are allocated once, when
	// metadata is created. Check before provisioning anything: without this the
	// failure surfaces from `drbdadm adjust` as "(162) Invalid configuration
	// request", by which point there are LVs, proxy units and a rewritten config
	// to unwind, and nothing in the message says what to do about it.
	if err := rm.assertBitmapSlotFree(ctx, primaryAddrs, primaries, resource); err != nil {
		return err
	}

	// The DR node needs backing volumes of its own: it is a full replica, not a
	// diskless voter. Sizes and pools come from the resource record, which is
	// where create wrote them.
	dbVols, err := rm.controller.db.ListVolumes(ctx, resource)
	if err != nil {
		return fmt.Errorf("list volumes of %q: %w", resource, err)
	}
	if len(dbVols) == 0 {
		return fmt.Errorf("resource %q has no recorded volumes to replicate", resource)
	}
	// Size the DR's volumes from what the primary site is actually exporting,
	// not from the recorded request — see primaryBackingSizes.
	backingSizes, err := rm.primaryBackingSizes(ctx, primaryAddrs, resource, len(dbVols))
	if err != nil {
		return err
	}

	bindPorts, err := rm.pickWANBindPorts(ctx, primaryAddrs, dbRes.Port)
	if err != nil {
		return err
	}

	// Everyone that must end up holding the new config.
	lanHosts := append([]string{}, primaryAddrs...)
	for _, n := range splitCSV(dbRes.DisklessNodes) {
		lanHosts = append(lanHosts, rm.controller.ResolveHost(n))
	}
	allHosts := append(append([]string{}, lanHosts...), drAddr)

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
		rm.undoAddDR(context.WithoutCancel(ctx), resource, resPath, liveConfig, lanHosts, drAddr, multi, dbVols)
	}()

	for i, v := range dbVols {
		if err := rm.createBackingVolumeOn(ctx, drAddr, v.Pool, v.VolumeName, backingSizes[i]); err != nil {
			return fmt.Errorf("create backing volume %s/%s on DR node %q: %w", v.Pool, v.VolumeName, drNode, err)
		}
	}

	// Proxy first: in WAN mode DRBD connects to a loopback port the proxy owns,
	// so bringing DRBD up first would find nothing listening.
	if err := wanproxy.ProvisionMulti(ctx, rm.wanproxyDeployClient(), multi); err != nil {
		return fmt.Errorf("provision WAN proxy for %s: %w", resource, err)
	}
	// After the push, not before: the controller may well have supplied the
	// binary itself, and checking first would reject a case that works.
	if err := rm.assertWANProxyBinary(ctx, append(append([]string{}, primaryAddrs...), drAddr),
		append(append([]string{}, primaries...), drNode)); err != nil {
		return err
	}

	// Rewrite the LIVE config rather than regenerating from the record: the
	// resource row does not carry the storage type or DRBD option overrides, so
	// a regenerated file would silently drop them (and any manual tuning).
	newConfig, err := rm.addDRToConfig(liveConfig, resource, drNode, primaries,
		splitCSV(dbRes.DisklessNodes), drAddr, dbVols, dbRes.Port, bindPorts)
	if err != nil {
		return fmt.Errorf("rewrite config for DR site: %w", err)
	}
	// The DR replicates but does not get a vote in whether the primary site may
	// write; see setLocalSiteQuorum.
	newConfig = setLocalSiteQuorum(newConfig, len(primaries)+len(splitCSV(dbRes.DisklessNodes)))

	if _, err := rm.deployment.DistributeConfig(ctx, allHosts, newConfig, resPath); err != nil {
		return fmt.Errorf("distribute DR config: %w", err)
	}

	// Metadata on the DR only — the existing replicas keep theirs.
	if err := rm.execAllSuccess(ctx, []string{drAddr},
		fmt.Sprintf("sudo drbdadm create-md --max-peers=%d --force %s", deployment.DefaultMaxPeers, resource),
		"create metadata on DR node"); err != nil {
		return err
	}

	// Let the running peers learn the new connection, then bring the DR up.
	if err := rm.execAllSuccess(ctx, allHosts,
		fmt.Sprintf("sudo drbdadm adjust %s", resource),
		"adjust peers for DR site"); err != nil {
		return err
	}
	upRes, err := rm.deployment.Exec(ctx, []string{drAddr}, fmt.Sprintf("sudo drbdadm up %s", resource))
	if err != nil {
		return fmt.Errorf("bring up DR replica: %w", err)
	}
	if !upRes.AllSuccess() {
		detail := upRes.FailureDetails()
		if !strings.Contains(detail, "exists already") && !strings.Contains(detail, "already? still?") {
			return fmt.Errorf("bring up DR replica on %q: %s", drNode, detail)
		}
	}
	// The DR is usually a node that holds nothing else of this cluster, so
	// nothing has put the boot unit there yet.
	if err := rm.ensureBootUnitOnMembers(ctx, resource, []string{drAddr}, dbRes.Encrypted); err != nil {
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
	if err := rm.controller.db.SaveResource(ctx, dbRes); err != nil {
		return fmt.Errorf("record DR site: %w", err)
	}

	rm.controller.logger.Info("DR site added; initial sync runs in the background",
		zap.String("resource", resource), zap.String("dr_node", drNode))
	return nil
}

// addDRToConfig rewrites a LAN resource config into the two-site shape.
//
// Three edits, all additive to the primary site:
//   - a host stanza for the DR node, with its own volumes and a fresh node-id;
//   - connection-mesh narrowed to the primary site, because a mesh entry for the
//     DR would pair it with the replicas on their LAN addresses, which cannot
//     reach it;
//   - one explicit `connection` per replica carrying the loopback endpoints of
//     that replica's WAN leg, plus protocol A and pull-ahead so the WAN cannot
//     stall writes in the primary site.
func (rm *ResourceManager) addDRToConfig(content, resource, drNode string, primaries, tiebreakers []string,
	drAddr string, vols []*database.Volume, port int, bindPorts []int) (string, error) {

	drName := rm.controller.nodes.GetDRBDNameByRef(drNode)
	blocks := parseOnBlocks(content)
	for _, b := range blocks {
		if b.name == drName {
			return "", fmt.Errorf("node %q is already in the config of %q", drNode, resource)
		}
	}
	nextID := 0
	for _, b := range blocks {
		if b.nodeID >= nextID {
			nextID = b.nodeID + 1
		}
	}

	// 1. The DR host stanza. Its address is loopback: every connection it has
	//    is a WAN leg terminated by its local acceptor.
	var dr strings.Builder
	fmt.Fprintf(&dr, "\n    on %s {\n", drName)
	fmt.Fprintf(&dr, "        address   127.0.0.1:%d;\n", port)
	fmt.Fprintf(&dr, "        node-id   %d;\n", nextID)
	for _, v := range vols {
		fmt.Fprintf(&dr, "        volume %d {\n", v.VolumeID)
		fmt.Fprintf(&dr, "            device    minor %d;\n", minorForVolume(content, v.VolumeID))
		fmt.Fprintf(&dr, "            disk      /dev/%s/%s;\n", v.Pool, v.VolumeName)
		fmt.Fprintf(&dr, "            meta-disk internal;\n")
		fmt.Fprintf(&dr, "        }\n")
	}
	fmt.Fprintf(&dr, "    }\n")

	// 2 + 3. Insert before the resource's closing brace, dropping any existing
	//        connection-mesh so it can be re-emitted for the primary site only.
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines)+40)
	inMesh := false
	closeIdx := -1
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "connection-mesh") {
			inMesh = true
			continue
		}
		if inMesh {
			if strings.Contains(line, "}") {
				inMesh = false
			}
			continue
		}
		if trimmed == "}" {
			closeIdx = len(out) // resource-level close; keep updating, last wins
		}
		out = append(out, line)
	}
	if closeIdx < 0 {
		return "", fmt.Errorf("malformed config for %q: no closing brace", resource)
	}

	var tail strings.Builder
	tail.WriteString(dr.String())

	lanHosts := make([]string, 0, len(primaries)+len(tiebreakers))
	for _, n := range append(append([]string{}, primaries...), tiebreakers...) {
		lanHosts = append(lanHosts, rm.controller.nodes.GetDRBDNameByRef(n))
	}
	if len(lanHosts) > 1 {
		tail.WriteString("\n    connection-mesh {\n        hosts")
		for _, h := range lanHosts {
			fmt.Fprintf(&tail, " %s", h)
		}
		tail.WriteString(";\n    }\n")
	}

	for i, p := range primaries {
		legPort := port + i
		fmt.Fprintf(&tail, "\n    connection {\n")
		bind := legPort + wanDRBDBindOffset
		if i < len(bindPorts) && bindPorts[i] > 0 {
			bind = bindPorts[i]
		}
		fmt.Fprintf(&tail, "        host %s address 127.0.0.1:%d;\n",
			rm.controller.nodes.GetDRBDNameByRef(p), bind)
		fmt.Fprintf(&tail, "        host %s address 127.0.0.1:%d;\n", drName, legPort)
		fmt.Fprintf(&tail, "        net {\n")
		fmt.Fprintf(&tail, "            protocol A;\n")
		fmt.Fprintf(&tail, "            on-congestion pull-ahead;\n")
		fmt.Fprintf(&tail, "            congestion-fill 400M;\n")
		fmt.Fprintf(&tail, "            csums-alg %s;\n", wanCsumsAlg)
		fmt.Fprintf(&tail, "        }\n")
		fmt.Fprintf(&tail, "    }\n")
	}

	final := append([]string{}, out[:closeIdx]...)
	final = append(final, strings.Split(strings.TrimRight(tail.String(), "\n"), "\n")...)
	final = append(final, out[closeIdx:]...)
	return strings.Join(final, "\n"), nil
}

// minorForVolume finds the device minor an existing config assigns to a volume,
// so the DR's stanza uses the same one (a resource's minor is cluster-wide).
func minorForVolume(content string, volumeID int) int {
	inVol := false
	depth := 0
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inVol && strings.HasPrefix(trimmed, fmt.Sprintf("volume %d", volumeID)) {
			inVol = true
			depth = strings.Count(line, "{") - strings.Count(line, "}")
			continue
		}
		if inVol {
			if m, ok := parseAnyDeviceMinor(line); ok {
				return m
			}
			depth += strings.Count(line, "{") - strings.Count(line, "}")
			if depth <= 0 {
				inVol = false
			}
		}
	}
	// Volume 0 may be written without an explicit `volume 0 {` wrapper.
	if volumeID == 0 {
		for _, line := range strings.Split(content, "\n") {
			if m, ok := parseAnyDeviceMinor(line); ok {
				return m
			}
		}
	}
	return volumeID
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
	lanHosts []string, drAddr string, multi wanproxy.MultiSpec, vols []*database.Volume) {

	log := rm.controller.logger.With(zap.String("resource", resource), zap.String("dr_node", drAddr))
	log.Warn("Rolling back DR site")

	if len(lanHosts) > 0 && strings.TrimSpace(originalConfig) != "" {
		if _, err := rm.deployment.DistributeConfig(ctx, lanHosts, originalConfig, resPath); err != nil {
			log.Error("Rollback: failed to restore config on the primary site", zap.Error(err))
		} else if _, err := rm.deployment.Exec(ctx, lanHosts,
			fmt.Sprintf("sudo drbdadm adjust %s", resource)); err != nil {
			log.Error("Rollback: failed to re-adjust the primary site", zap.Error(err))
		}
	}

	if _, err := rm.deployment.Exec(ctx, []string{drAddr},
		fmt.Sprintf("sudo drbdadm down %s 2>/dev/null; sudo rm -f %s", resource, resPath)); err != nil {
		log.Error("Rollback: failed to tear down the DR replica", zap.Error(err))
	}

	if err := wanproxy.DeprovisionMulti(ctx, rm.wanproxyDeployClient(), multi); err != nil {
		log.Error("Rollback: failed to remove the WAN proxy legs", zap.Error(err))
	}

	// The volumes go last: while the config still names them, removing them
	// would only produce a replica that is up and broken.
	for _, v := range vols {
		if _, err := rm.deployment.Exec(ctx, []string{drAddr},
			fmt.Sprintf("sudo lvremove -f %s/%s", v.Pool, v.VolumeName)); err != nil {
			log.Error("Rollback: failed to remove a backing volume on the DR node",
				zap.String("volume", v.Pool+"/"+v.VolumeName), zap.Error(err))
		}
	}
}

// quorumLineRe matches the `quorum <value>;` line inside a resource's options.
var quorumLineRe = regexp.MustCompile(`(?m)^(\s*)quorum\s+\S+;`)

// primarySiteQuorum renders the `quorum` value for a resource with an off-site
// DR: a majority of the nodes that could actually take over.
//
// DRBD counts every configured node toward a majority, but the DR is an
// asynchronous copy that is never promoted automatically — it cannot take over,
// so letting it vote on whether the primary site may accept writes is backwards.
// Worse, it actively costs availability: adding a DR to a 3-node resource lifts
// the bar from 2 votes to 3, so the site that used to survive one local failure
// no longer does. That is how a quorum tiebreaker can be added, a DR attached,
// and the tiebreaker's vote silently cancelled out.
//
// The narrower guarantee is deliberate and bounded: the excluded node is
// unreachable from the primary site's network by construction — it is reached
// only through a proxy tunnel — so it cannot form a rival quorate partition with
// any local node. This is not the same as picking a small number arbitrarily.
//
// This is the only place the number is computed. It used to be derived here and
// again, independently, at each of the two call sites that narrow a resource's
// quorum, while the tests asserted on a third copy that nothing called — so the
// arithmetic the cluster actually ran was unguarded.
func primarySiteQuorum(localVoters int) string {
	if localVoters < 1 {
		localVoters = 1
	}
	return strconv.Itoa(localVoters/2 + 1)
}

// setLocalSiteQuorum rewrites a config's quorum to a majority of the primary
// site, so attaching a DR does not raise the bar the local nodes must clear.
//
// Without this, add-dr makes a resource LESS available: DRBD counts the new
// member toward `majority`, so a 3-node resource that survived one local failure
// suddenly needs 3 of 4 votes and no longer does. The operator asked for an
// off-site copy and silently got a downgrade to local resilience.
func setLocalSiteQuorum(content string, localVoters int) string {
	want := primarySiteQuorum(localVoters)
	if quorumLineRe.MatchString(content) {
		return quorumLineRe.ReplaceAllString(content, "${1}quorum "+want+";")
	}
	// No explicit quorum: the resource is relying on DRBD's default, so state it
	// rather than leaving the new member to shift a majority nobody wrote down.
	return strings.Replace(content, "    options {",
		"    options {\n        quorum "+want+";", 1)
}
