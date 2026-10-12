package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
)

// Diskless clients let a node access a DRBD resource with no local replica: it
// connects over the network, fetches data from a diskful peer, and can be
// promoted Primary to read/write the volume. This is LINSTOR's "diskless
// client" and is what allows a Kubernetes pod to run on a node that stores none
// of the data (e.g. a diskless compute node) yet still mount the volume.
//
// At the DRBD `.res` level a diskless client is identical to a quorum
// tiebreaker — an `on <node>` block whose every volume overrides `disk none`.
// The distinction is purely in Haify semantics: a tiebreaker is never promoted or
// mounted (DisklessNodes), whereas a client is (DisklessClients). Attach/detach
// therefore reuse the same config surgery the tiebreaker path relies on, but
// track the node separately and never treat it as a mere vote.

// errDisklessAlreadyPresent is returned by addDisklessClientBlock when the node
// already has an `on` block in the config; callers treat it as idempotent.
var errDisklessAlreadyPresent = fmt.Errorf("node already present in resource config")

// errDisklessNotPresent is returned by removeDisklessClientBlock when the node
// has no `on` block to remove; callers treat it as idempotent.
var errDisklessNotPresent = fmt.Errorf("node not present in resource config")

// AttachDisklessClient adds node to an existing resource as a diskless data
// client. The node stores no replica: it connects over DRBD, syncs no bulk
// data, and can then be promoted Primary (e.g. by the CSI node service) to
// serve the volume over the network. Idempotent — re-attaching an existing
// client is a no-op.
func (rm *ResourceManager) AttachDisklessClient(ctx context.Context, resource, node string) error {
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

	// A node already holding a diskful replica cannot also be a diskless client.
	for _, n := range splitCSV(dbRes.Nodes) {
		if n == node {
			return fmt.Errorf("node %q already holds a diskful replica of %q", node, resource)
		}
	}
	// A tiebreaker is already a diskless participant: in DRBD's config it is
	// exactly what a diskless client is. Refusing it left a three-node
	// cluster — two replicas and a tiebreaker on every resource — with no
	// node that could ever mount a resource remotely. Asked for explicitly,
	// the tiebreaker becomes a client; it keeps its quorum vote either way.
	for _, n := range splitCSV(dbRes.DisklessNodes) {
		if n == node {
			return rm.tiebreakerToClient(ctx, dbRes, node)
		}
	}
	if err := rm.assertNewMemberTLS(ctx, resource, node); err != nil {
		return err
	}
	clients := splitCSV(dbRes.DisklessClients)
	alreadyClient := false
	for _, n := range clients {
		if n == node {
			alreadyClient = true
			break
		}
	}

	rm.controller.logger.Info("Attaching diskless client to resource",
		zap.String("resource", resource),
		zap.String("node", node),
		zap.Bool("already_recorded", alreadyClient))

	// The client's management address, for the commands run on it.
	ip := rm.controller.nodes.GetNodeAddressByName(node)
	if ip == "" {
		rm.mu.RLock()
		ip = rm.hostMap[node]
		rm.mu.RUnlock()
	}
	if ip == "" {
		ip = node
	}
	ip = resolveToIP(ip)

	// Read the live .res from a current participant and splice in the client.
	resPath := fmt.Sprintf("/etc/drbd.d/%s.res", resource)
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		return err
	}
	catResult, err := rm.deployment.Exec(ctx, []string{hosts[0]}, "cat "+resPath)
	if err != nil {
		return fmt.Errorf("failed to read resource config: %w", err)
	}
	var originalConfig string
	var readOK bool
	for _, r := range catResult.Hosts {
		originalConfig, readOK = r.Output, r.Success
		break
	}
	if !readOK {
		return fmt.Errorf("failed to read resource config for %q", resource)
	}

	// The union of current participants is exactly the on-hosts in the live
	// config (diskful + tiebreakers + any existing clients) — resolve each so
	// the rewritten config and the adjust reach every one of them.
	existingBlocks := parseOnBlocks(originalConfig)
	participantHosts := make([]string, 0, len(existingBlocks))
	for _, b := range existingBlocks {
		participantHosts = append(participantHosts, rm.controller.ResolveHost(b.name))
	}

	// Same rule as the tiebreaker path: the resource's minors were allocated
	// over the nodes it had at create time, so a node attaching later can
	// already be using them for something else.
	if err := rm.assertMinorsFreeOn(ctx, rm.controller.ResolveHost(node), resource,
		resourceMinors(originalConfig)); err != nil {
		return err
	}

	// SSH goes to ip; DRBD talks over the node's replication network, the
	// same address create and repair write for it.
	replIP := ip
	if r := rm.controller.nodes.GetReplicationAddressByName(node); r != "" {
		replIP = resolveToIP(r)
	}
	newConfig, err := addDisklessClientBlock(originalConfig, node, replIP, dbRes.Port)
	if err == errDisklessAlreadyPresent {
		// The .res already carries the client (a prior partial attach). Make sure
		// it is up and recorded, then return success.
		if _, err := rm.deployment.DRBDUp(ctx, []string{ip}, resource); err != nil {
			return fmt.Errorf("bring up existing diskless client: %w", err)
		}
		_ = rm.ensureDRBDBootUnitEnabled(ctx, []string{ip})
		return rm.recordDisklessClient(ctx, dbRes, clients, node)
	}
	if err != nil {
		return fmt.Errorf("build diskless client config: %w", err)
	}

	// Roll back the config everywhere and tear the client down if a later step
	// fails, so a retry starts from the pre-attach state.
	allHosts := append(append([]string{}, participantHosts...), ip)
	committed := false
	defer func() {
		if committed {
			return
		}
		rm.controller.logger.Warn("Diskless client attach failed; rolling back",
			zap.String("resource", resource), zap.String("node", node))
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = rm.deployment.DRBDDown(cleanupCtx, []string{ip}, resource)
		_, _ = rm.deployment.Exec(cleanupCtx, []string{ip}, "sudo rm -f "+resPath)
		_, _ = rm.deployment.DistributeConfig(cleanupCtx, participantHosts, originalConfig, resPath)
		_, _ = rm.deployment.Exec(cleanupCtx, participantHosts, fmt.Sprintf("sudo drbdadm adjust %s", resource))
	}()

	// Push the rewritten config to every participant plus the new client.
	if _, err := rm.deployment.DistributeConfig(ctx, allHosts, newConfig, resPath); err != nil {
		return fmt.Errorf("distribute diskless client config: %w", err)
	}

	// Let existing peers learn the new connection (adjust reconciles the added
	// mesh edge without disturbing the running replica set).
	if err := rm.execAllSuccess(ctx, participantHosts,
		fmt.Sprintf("sudo drbdadm adjust %s", resource),
		"adjust peers for diskless client"); err != nil {
		return err
	}

	// Bring the resource up on the client. With `disk none` it attaches
	// Diskless, connects to the diskful peers, and is immediately promotable —
	// no create-md and no initial sync (it has no backing storage to sync to).
	if _, err := rm.deployment.DRBDUp(ctx, []string{ip}, resource); err != nil {
		return fmt.Errorf("bring up diskless client: %w", err)
	}
	// A client node is often one that holds no replica of anything, so the
	// boot unit is not there. It has no crypt container to open, so a failure
	// is logged and does not undo the attach.
	_ = rm.ensureDRBDBootUnitEnabled(ctx, []string{ip})

	committed = true
	rm.controller.logger.Info("Diskless client attached",
		zap.String("resource", resource), zap.String("node", node))
	return rm.recordDisklessClient(ctx, dbRes, clients, node)
}

// DetachDisklessClient removes a diskless client added by AttachDisklessClient:
// it downs the resource on that node, rewrites the config on the remaining
// participants, and drops the node from the resource record. Idempotent.
func (rm *ResourceManager) DetachDisklessClient(ctx context.Context, resource, node string) error {
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

	rm.controller.logger.Info("Detaching diskless client from resource",
		zap.String("resource", resource), zap.String("node", node))

	if lastVoteBesidesTwoReplicas(dbRes, node) {
		return rm.clientToTiebreaker(ctx, dbRes, node)
	}

	ip := rm.controller.nodes.GetNodeAddressByName(node)
	if ip == "" {
		ip = node
	}
	ip = resolveToIP(ip)

	resPath := fmt.Sprintf("/etc/drbd.d/%s.res", resource)
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		return err
	}
	catResult, err := rm.deployment.Exec(ctx, []string{hosts[0]}, "cat "+resPath)
	if err != nil {
		return fmt.Errorf("failed to read resource config: %w", err)
	}
	var originalConfig string
	var ok bool
	for _, r := range catResult.Hosts {
		originalConfig, ok = r.Output, r.Success
		break
	}
	if !ok {
		return fmt.Errorf("failed to read resource config for %q", resource)
	}

	// Down the client first so it stops holding a connection to peers we are
	// about to drop it from. Best-effort: the node may already be gone.
	_, _ = rm.deployment.DRBDDown(ctx, []string{ip}, resource)
	_, _ = rm.deployment.Exec(ctx, []string{ip}, "sudo rm -f "+resPath)

	adjust := fmt.Sprintf("sudo drbdadm adjust %s", resource)
	newConfig, err := removeDisklessClientBlock(originalConfig, node)
	if err == errDisklessNotPresent {
		// Not in the config: an earlier detach wrote it out and then failed.
		// The replicas may still hold a connection to the client until they
		// are adjusted to that config, so adjust them before forgetting it.
		if err := rm.execAllSuccess(ctx, hosts, adjust, "adjust the replicas after diskless client detach"); err != nil {
			return err
		}
		return rm.forgetDisklessClient(ctx, dbRes, node)
	}
	if err != nil {
		return fmt.Errorf("build config without diskless client: %w", err)
	}

	// The replicas must take the new config: they hold the data and the
	// connection to the client. The other diskless participants (clients and
	// tiebreakers) are adjusted too, but one that cannot be reached must not
	// keep a client from being detached: the usual reason to detach one is
	// that its machine is gone, and often another went with it.
	if _, err := rm.deployment.DistributeConfig(ctx, hosts, newConfig, resPath); err != nil {
		return fmt.Errorf("distribute config without diskless client: %w", err)
	}
	if err := rm.execAllSuccess(ctx, hosts, adjust, "adjust the replicas after diskless client detach"); err != nil {
		return err
	}
	rm.updateOtherParticipants(ctx, resource, resPath, newConfig, hosts)

	rm.controller.logger.Info("Diskless client detached",
		zap.String("resource", resource), zap.String("node", node))
	return rm.forgetDisklessClient(ctx, dbRes, node)
}

// updateOtherParticipants writes config to the participants of resource that
// are not among replicas, and adjusts them. Failures are logged, not
// returned; the next config change or a repair reaches a node that was down.
func (rm *ResourceManager) updateOtherParticipants(ctx context.Context, resource, resPath, config string, replicas []string) {
	isReplica := make(map[string]bool, len(replicas))
	for _, h := range replicas {
		isReplica[h] = true
	}
	var others []string
	for _, n := range onHostNames(parseOnBlocks(config)) {
		if h := rm.controller.ResolveHost(n); !isReplica[h] {
			others = append(others, h)
		}
	}
	for _, h := range others {
		_, derr := rm.deployment.DistributeConfig(ctx, []string{h}, config, resPath)
		if derr == nil {
			derr = rm.execAllSuccess(ctx, []string{h}, "sudo drbdadm adjust "+resource, "adjust "+resource)
		}
		if derr != nil {
			rm.controller.logger.Warn("Could not update a diskless participant after a client detach",
				zap.String("resource", resource), zap.String("host", h), zap.Error(derr))
		}
	}
}

// recordDisklessClient persists node into the resource's DisklessClients set.
// Best-effort at the DB layer: a save failure is logged, not fatal, because the
// resource is already live on the client at this point.
func (rm *ResourceManager) recordDisklessClient(ctx context.Context, dbRes *database.Resource, clients []string, node string) error {
	for _, n := range clients {
		if n == node {
			return nil // already recorded
		}
	}
	dbRes.DisklessClients = strings.Join(append(clients, node), ",")
	if err := rm.controller.db.SaveResource(ctx, dbRes); err != nil {
		rm.controller.logger.Warn("Failed to record diskless client in database",
			zap.String("resource", dbRes.Name), zap.String("node", node), zap.Error(err))
	}
	return nil
}

// forgetDisklessClient removes node from the resource's DisklessClients set.
func (rm *ResourceManager) forgetDisklessClient(ctx context.Context, dbRes *database.Resource, node string) error {
	var kept []string
	for _, n := range splitCSV(dbRes.DisklessClients) {
		if n != node {
			kept = append(kept, n)
		}
	}
	dbRes.DisklessClients = strings.Join(kept, ",")
	if err := rm.controller.db.SaveResource(ctx, dbRes); err != nil {
		rm.controller.logger.Warn("Failed to remove diskless client from database",
			zap.String("resource", dbRes.Name), zap.String("node", node), zap.Error(err))
	}
	return nil
}

// disklessParticipantHosts returns the resolved addresses of every node that
// takes part in a resource without a disk: quorum tiebreakers and diskless data
// clients. They hold a copy of the resource config like any other node, so any
// change to the resource's volume set has to reach them as well.
func (rm *ResourceManager) disklessParticipantHosts(ctx context.Context, resource string) []string {
	hosts := rm.disklessHosts(ctx, resource)
	if rm.controller.db == nil {
		return hosts
	}
	dbRes, err := rm.controller.db.GetResource(ctx, resource)
	if err != nil || dbRes == nil {
		return hosts
	}
	seen := map[string]bool{}
	for _, h := range hosts {
		seen[h] = true
	}
	for _, n := range splitCSV(dbRes.DisklessClients) {
		if h := rm.controller.ResolveHost(n); h != "" && !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// lastVoteBesidesTwoReplicas reports whether node, a diskless client, is the
// only participant besides a resource's two diskful replicas. Removing it
// would leave a two-node resource that loses quorum when either node does.
func lastVoteBesidesTwoReplicas(dbRes *database.Resource, node string) bool {
	if len(splitCSV(dbRes.Nodes)) != 2 || len(splitCSV(dbRes.DisklessNodes)) != 0 {
		return false
	}
	clients := splitCSV(dbRes.DisklessClients)
	return len(clients) == 1 && clients[0] == node
}

// tiebreakerToClient reclassifies a resource's tiebreaker as a diskless
// client. Nothing changes in DRBD: both are `disk none` participants. What
// changes is that Haify lets this node be promoted and mount the resource.
func (rm *ResourceManager) tiebreakerToClient(ctx context.Context, dbRes *database.Resource, node string) error {
	ip := resolveToIP(rm.controller.ResolveHost(node))
	if _, err := rm.deployment.DRBDUp(ctx, []string{ip}, dbRes.Name); err != nil {
		return fmt.Errorf("bring up %s on %s: %w", dbRes.Name, node, err)
	}
	// A tiebreaker moved here by set-tiebreaker before it installed the boot
	// unit does not have it; a client is promoted, so it matters more now.
	_ = rm.ensureDRBDBootUnitEnabled(ctx, []string{ip})
	dbRes.DisklessNodes = strings.Join(without(splitCSV(dbRes.DisklessNodes), node), ",")
	dbRes.DisklessClients = strings.Join(append(without(splitCSV(dbRes.DisklessClients), node), node), ",")
	if err := rm.controller.db.SaveResource(ctx, dbRes); err != nil {
		return fmt.Errorf("record %s as a diskless client of %s: %w", node, dbRes.Name, err)
	}
	rm.controller.logger.Info("Tiebreaker is now a diskless client; it keeps its quorum vote",
		zap.String("resource", dbRes.Name), zap.String("node", node))
	return nil
}

// clientToTiebreaker is the reverse, used when detaching the client would take
// away the resource's third vote. The node stays in the resource, demoted.
func (rm *ResourceManager) clientToTiebreaker(ctx context.Context, dbRes *database.Resource, node string) error {
	ip := resolveToIP(rm.controller.ResolveHost(node))
	if err := rm.execAllSuccess(ctx, []string{ip}, "sudo drbdadm secondary "+dbRes.Name,
		"demote "+dbRes.Name+" on "+node+" (unmount it first)"); err != nil {
		return err
	}
	dbRes.DisklessClients = strings.Join(without(splitCSV(dbRes.DisklessClients), node), ",")
	dbRes.DisklessNodes = strings.Join(append(without(splitCSV(dbRes.DisklessNodes), node), node), ",")
	if err := rm.controller.db.SaveResource(ctx, dbRes); err != nil {
		return fmt.Errorf("record %s as the tiebreaker of %s: %w", node, dbRes.Name, err)
	}
	rm.controller.logger.Info("Detached client kept as the resource's tiebreaker: it was the third vote",
		zap.String("resource", dbRes.Name), zap.String("node", node))
	return nil
}

func without(list []string, drop string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v != drop {
			out = append(out, v)
		}
	}
	return out
}
