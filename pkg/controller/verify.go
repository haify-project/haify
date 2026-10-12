package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	pb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// Two replicas can both say UpToDate and still hold different data: a disk
// that returned the wrong block, a write that reached one copy's cache and not
// its platter, the leftovers of a split brain resolved by hand. DRBD cannot
// know until something reads both copies and compares them, and which copy a
// read lands on decides what the application sees. Online verify is that
// comparison; the controller always wrote a verify-alg for it and never ran it.
//
// VerifyResource starts it, follows it, and repairs what it found. Like
// dr-failback it can be run again at any point and carries on from where the
// resource stands.

const (
	verifyRunning  = "running"
	verifyDone     = "done"
	verifyResynced = "resynced"
)

// defaultVerifyAlg is written into a resource that predates the default, so
// verify can run on it.
const defaultVerifyAlg = "crc32c"

var verifyPollInterval = 5 * time.Second

func (s *Server) VerifyResource(ctx context.Context, req *pb.VerifyResourceRequest) (*pb.VerifyResourceResponse, error) {
	resp := s.resources.Verify(ctx, req)
	s.serverLog().Debug("VerifyResource: done", zap.String("resource", req.GetName()), zap.Bool("success", resp.Success),
		zap.String("phase", resp.Phase))
	return resp, nil
}

// Verify is VerifyResource for callers inside the controller: the verify
// schedule, and the RPC. A failure is reported in the response.
func (rm *ResourceManager) Verify(ctx context.Context, req *pb.VerifyResourceRequest) *pb.VerifyResourceResponse {
	log := rm.controller.traceLogger()
	log.Debug("Verify: starting", zap.String("resource", req.GetName()), zap.String("node", req.GetNode()),
		zap.Bool("resync", req.GetResync()), zap.Uint32("wait_seconds", req.GetWaitSeconds()))
	resp := &pb.VerifyResourceResponse{}
	fail := func(format string, args ...any) *pb.VerifyResourceResponse {
		resp.Success, resp.Message = false, fmt.Sprintf(format, args...)
		log.Debug("Verify: failed", zap.String("resource", req.GetName()), zap.String("message", resp.Message))
		return resp
	}
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return fail("resource name is required")
	}
	source, err := rm.verifySource(ctx, name, strings.TrimSpace(req.GetNode()))
	if err != nil {
		return fail("%v", err)
	}
	resp.Source = source
	step := func(format string, args ...any) { resp.Steps = append(resp.Steps, fmt.Sprintf(format, args...)) }
	addr := rm.controller.ResolveHost(source)
	log.Debug("Verify: source chosen", zap.String("resource", name), zap.String("node", source), zap.String("host", addr))

	st, err := rm.readDrbdStatus(ctx, addr, name)
	if err != nil {
		return fail("read %s on %s: %v", name, source, err)
	}

	switch {
	case verifying(st):
		// Already under way: follow it.
		log.Debug("Verify: already verifying, following it", zap.String("resource", name))
	case req.GetResync():
		log.Debug("Verify: resyncing the marked blocks", zap.String("resource", name))
		if err := rm.resyncOutOfSync(ctx, name, source, st, step); err != nil {
			return fail("%v", err)
		}
		if st, err = rm.readDrbdStatus(ctx, addr, name); err != nil {
			return fail("read %s on %s: %v", name, source, err)
		}
		resp.Success, resp.Phase = true, verifyResynced
		resp.Peers = verifyPeers(st, nil)
		resp.Message = fmt.Sprintf("%s: the blocks marked out of sync were copied from %s; the copies are identical and the marks are cleared", name, source)
		log.Debug("Verify: resynced", zap.String("resource", name), zap.Int("peers", len(resp.Peers)))
		return resp
	default:
		added, err := rm.ensureVerifyAlg(ctx, name, addr)
		if err != nil {
			return fail("%v", err)
		}
		if added {
			step("verify-alg %s added to %s", defaultVerifyAlg, name)
		}
		rm.rememberMarks(name, st)
		started, err := rm.startVerify(ctx, name, addr, st, step)
		if err != nil {
			return fail("%v", err)
		}
		if started == 0 {
			return fail("%s on %s has no connected, UpToDate peer to verify against (%s)", name, source, strings.Join(resp.Steps, "; "))
		}
		log.Debug("Verify: started", zap.String("resource", name), zap.Int("peers", started), zap.Bool("verify_alg_added", added))
	}

	deadline := time.Now().Add(time.Duration(req.GetWaitSeconds()) * time.Second)
	for {
		if st, err = rm.readDrbdStatus(ctx, addr, name); err != nil {
			return fail("read %s on %s: %v", name, source, err)
		}
		running := verifying(st)
		log.Debug("Verify: poll", zap.String("resource", name), zap.Bool("verifying", running),
			zap.Duration("left", time.Until(deadline)))
		if !running || !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return fail("%v", ctx.Err())
		case <-time.After(verifyPollInterval):
		}
	}
	resp.Peers = verifyPeers(st, func(peer string) (uint64, bool) { return rm.marksBefore(name, peer) })
	resp.Success = true
	if verifying(st) {
		resp.Phase = verifyRunning
		resp.Message = fmt.Sprintf("verifying %s from %s; run it again to follow it", name, source)
		log.Debug("Verify: still running, returning", zap.String("resource", name))
		return resp
	}
	resp.Phase = verifyDone
	var differ []string
	resp.Message, differ = verifyMessage(name, source, resp.Peers)
	rm.controller.logger.Info("Online verify", zap.String("resource", name), zap.String("source", source),
		zap.String("phase", resp.Phase), zap.Strings("marked", differ))
	return resp
}

// verifySource picks the node to verify from: the one asked for, else the
// Primary — whose data is by definition what the application sees — else the
// first diskful node.
func (rm *ResourceManager) verifySource(ctx context.Context, name, node string) (string, error) {
	log := rm.controller.traceLogger()
	if rm.controller.db == nil {
		log.Debug("verifySource: no database")
		return "", fmt.Errorf("database not available")
	}
	dbRes, err := rm.controller.db.GetResource(ctx, name)
	if err != nil || dbRes == nil {
		log.Debug("verifySource: resource not found", zap.String("resource", name), zap.Error(err))
		return "", fmt.Errorf("resource %q not found", name)
	}
	diskful := splitCSV(dbRes.Nodes)
	if len(diskful) == 0 {
		log.Debug("verifySource: no diskful replica", zap.String("resource", name))
		return "", fmt.Errorf("%q has no diskful replica", name)
	}
	if node != "" {
		if !containsString(diskful, node) {
			log.Debug("verifySource: asked-for node holds no replica", zap.String("node", node), zap.Strings("diskful", diskful))
			return "", fmt.Errorf("%s holds no replica of %s (those are %s)", node, name, strings.Join(diskful, ", "))
		}
		log.Debug("verifySource: the node asked for", zap.String("node", node))
		return node, nil
	}
	for _, n := range diskful {
		if st, err := rm.readDrbdStatus(ctx, rm.controller.ResolveHost(n), name); err == nil && st.Role == "Primary" {
			log.Debug("verifySource: the Primary", zap.String("node", n))
			return n, nil
		}
	}
	log.Debug("verifySource: no Primary, the first diskful node", zap.String("node", diskful[0]))
	return diskful[0], nil
}

// ensureVerifyAlg gives a resource created before verify-alg was a default
// one, in its config and on every node, so verify can start.
func (rm *ResourceManager) ensureVerifyAlg(ctx context.Context, name, addr string) (bool, error) {
	log := rm.controller.traceLogger()
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, []string{addr}, "sudo drbdsetup show "+name)
	if err != nil {
		log.Debug("ensureVerifyAlg: drbdsetup show errored", zap.String("host", addr), zap.Duration("took", time.Since(start)), zap.Error(err))
		return false, fmt.Errorf("read %s's settings: %w", name, err)
	}
	for _, hr := range res.Hosts {
		if !hr.Success {
			log.Debug("ensureVerifyAlg: drbdsetup show failed", zap.String("host", addr), zap.Duration("took", time.Since(start)))
			return false, fmt.Errorf("read %s's settings: %s", name, hostFailure(hr))
		}
		if strings.Contains(hr.Output, "verify-alg") {
			log.Debug("ensureVerifyAlg: already set", zap.String("resource", name), zap.Duration("took", time.Since(start)))
			return false, nil
		}
	}
	log.Debug("ensureVerifyAlg: adding", zap.String("resource", name), zap.String("alg", defaultVerifyAlg))
	if err := rm.SetOptions(ctx, name, map[string]string{"net/verify-alg": defaultVerifyAlg}); err != nil {
		log.Debug("ensureVerifyAlg: option not set", zap.Error(err))
		return false, fmt.Errorf("add verify-alg to %s: %w", name, err)
	}
	return true, nil
}

// startVerify starts a verify against every peer it can: connected, fully
// replicating and holding a disk. Verifying the whole resource at once would
// fail outright on one disconnected peer, and a diskless tiebreaker has
// nothing to compare. It returns how many peers it started on.
func (rm *ResourceManager) startVerify(ctx context.Context, name, addr string, st *drbdsetupStatus, step func(string, ...any)) (int, error) {
	log := rm.controller.traceLogger()
	started := 0
	for _, c := range st.Connections {
		if reason := verifiable(c.ConnectionState, c.PeerDevices); reason != "" {
			log.Debug("startVerify: peer skipped", zap.String("peer", c.Name), zap.String("reason", reason))
			step("%s: skipped, %s", c.Name, reason)
			continue
		}
		cmd := fmt.Sprintf("sudo drbdadm verify %s:%s", name, c.Name)
		if err := rm.execAllSuccess(ctx, []string{addr}, cmd, "start verify against "+c.Name); err != nil {
			log.Debug("startVerify: verify not started", zap.String("peer", c.Name), zap.Error(err))
			return started, err
		}
		started++
		log.Debug("startVerify: verify started", zap.String("peer", c.Name))
		step("%s: verify started", c.Name)
	}
	return started, nil
}

func verifiable(connection string, pds []drbdPeerDevice) string {
	if connection != "Connected" {
		return "not connected (" + connection + ")"
	}
	if len(pds) == 0 {
		return "no volumes"
	}
	for _, pd := range pds {
		if pd.PeerDiskState == "Diskless" {
			return "diskless"
		}
		if pd.ReplicationState != "Established" || pd.PeerDiskState != "UpToDate" {
			return fmt.Sprintf("%s/%s", pd.ReplicationState, pd.PeerDiskState)
		}
	}
	return ""
}

// resyncOutOfSync copies the source's data over the blocks a verify found
// different, one peer at a time, with invalidate-remote --reset-bitmap=no:
// the peer becomes SyncTarget for exactly the blocks marked out of sync. The
// usual advice — disconnect and connect again — does nothing on DRBD 9.3,
// which reconnects peers at equal current UUIDs without a resync and leaves
// the marked blocks as they are.
//
// Which copy is right is not something verify can tell, so the source has to
// be one that can be trusted: the Primary, or a node that agrees with at
// least one other diskful replica. A peer that is Primary is never
// overwritten — that is the data the running service reads.
func (rm *ResourceManager) resyncOutOfSync(ctx context.Context, name, source string, st *drbdsetupStatus, step func(string, ...any)) error {
	log := rm.controller.traceLogger()
	log.Debug("resyncOutOfSync: checking peers", zap.String("resource", name), zap.String("source", source), zap.String("role", st.Role))
	var differ []string
	agrees := 0
	for _, c := range st.Connections {
		if reason := verifiable(c.ConnectionState, c.PeerDevices); reason != "" {
			log.Debug("resyncOutOfSync: peer skipped", zap.String("peer", c.Name), zap.String("reason", reason))
			continue
		}
		var oos uint64
		for _, pd := range c.PeerDevices {
			oos += pd.OutOfSyncKiB
		}
		if oos == 0 {
			log.Debug("resyncOutOfSync: peer agrees", zap.String("peer", c.Name))
			agrees++
			continue
		}
		if c.PeerRole == "Primary" {
			log.Debug("resyncOutOfSync: differing peer is Primary, refusing", zap.String("peer", c.Name), zap.Uint64("out_of_sync_kib", oos))
			return fmt.Errorf("%s differs from %s but is Primary; run resync from %s, whose data the service is using", c.Name, source, c.Name)
		}
		log.Debug("resyncOutOfSync: peer differs", zap.String("peer", c.Name), zap.Uint64("out_of_sync_kib", oos))
		differ = append(differ, c.Name)
	}
	if len(differ) == 0 {
		log.Debug("resyncOutOfSync: nothing to resync", zap.String("resource", name))
		return fmt.Errorf("nothing to resync: no connected replica of %s differs from %s (run verify first)", name, source)
	}
	if st.Role != "Primary" && agrees == 0 {
		log.Debug("resyncOutOfSync: source cannot be trusted, refusing", zap.String("source", source))
		return fmt.Errorf("%s is not Primary and no other replica agrees with it, so there is no telling which copy is right; "+
			"run the resync from the Primary, or promote the copy you trust first", source)
	}
	addr := rm.controller.ResolveHost(source)
	for _, peer := range differ {
		cmd := fmt.Sprintf("sudo drbdadm invalidate-remote --reset-bitmap=no %s:%s", name, peer)
		if err := rm.execAllSuccess(ctx, []string{addr}, cmd, "resync "+peer); err != nil {
			log.Debug("resyncOutOfSync: resync not started", zap.String("peer", peer), zap.Error(err))
			return err
		}
		log.Debug("resyncOutOfSync: resyncing", zap.String("peer", peer), zap.String("source", source))
		step("%s: resyncing the blocks that differed from %s", peer, source)
	}
	return rm.waitOutOfSyncCleared(ctx, name, addr, differ)
}

// waitOutOfSyncCleared waits for the peers' resync to finish, so the caller
// reports the result rather than the start of it.
func (rm *ResourceManager) waitOutOfSyncCleared(ctx context.Context, name, addr string, peers []string) error {
	log := rm.controller.traceLogger()
	deadline := time.Now().Add(2 * time.Minute)
	log.Debug("waitOutOfSyncCleared: waiting", zap.String("resource", name), zap.Strings("peers", peers))
	for {
		st, err := rm.readDrbdStatus(ctx, addr, name)
		if err != nil {
			log.Debug("waitOutOfSyncCleared: status not read", zap.Error(err))
			return err
		}
		var pending []string
		for _, c := range st.Connections {
			if !containsString(peers, c.Name) {
				continue
			}
			if verifiable(c.ConnectionState, c.PeerDevices) != "" {
				pending = append(pending, c.Name)
				continue
			}
			for _, pd := range c.PeerDevices {
				if pd.OutOfSyncKiB > 0 {
					pending = append(pending, c.Name)
					break
				}
			}
		}
		log.Debug("waitOutOfSyncCleared: poll", zap.String("resource", name), zap.Strings("pending", pending))
		if len(pending) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			log.Debug("waitOutOfSyncCleared: deadline passed", zap.Strings("pending", pending))
			return fmt.Errorf("resync to %s still running; check resource status", strings.Join(pending, ", "))
		}
		select {
		case <-ctx.Done():
			log.Debug("waitOutOfSyncCleared: cancelled", zap.Error(ctx.Err()))
			return ctx.Err()
		case <-time.After(verifyPollInterval):
		}
	}
}

func verifying(st *drbdsetupStatus) bool {
	for _, c := range st.Connections {
		for _, pd := range c.PeerDevices {
			if pd.ReplicationState == "VerifyS" || pd.ReplicationState == "VerifyT" {
				return true
			}
		}
	}
	return false
}

// verifyPeers reports each diskful peer's out-of-sync amount and, while a
// verify runs against it, its progress.
func verifyPeers(st *drbdsetupStatus, before func(peer string) (uint64, bool)) []*pb.VerifyPeer {
	log := zap.L()
	var out []*pb.VerifyPeer
	for _, c := range st.Connections {
		p := &pb.VerifyPeer{Node: c.Name, State: verifyDone}
		diskless := false
		for _, pd := range c.PeerDevices {
			p.OutOfSyncKib += pd.OutOfSyncKiB
			if pd.PeerDiskState == "Diskless" {
				diskless = true
			}
			if pd.ReplicationState == "VerifyS" || pd.ReplicationState == "VerifyT" {
				p.State = "verifying"
				if pd.PercentResyncDone != nil {
					p.PercentDone = *pd.PercentResyncDone
				}
			}
		}
		if diskless {
			log.Debug("verifyPeers: diskless peer left out", zap.String("peer", c.Name))
			continue
		}
		if before != nil {
			if was, ok := before(c.Name); ok {
				p.BaselineKnown = true
				if p.OutOfSyncKib > was {
					p.FoundKib = p.OutOfSyncKib - was
				}
			}
		}
		if reason := verifiable(c.ConnectionState, c.PeerDevices); reason != "" && p.State != "verifying" {
			p.State = reason
		}
		log.Debug("verifyPeers: peer", zap.String("peer", c.Name), zap.String("state", p.State),
			zap.Uint64("out_of_sync_kib", p.OutOfSyncKib), zap.Uint64("found_kib", p.FoundKib), zap.Bool("baseline_known", p.BaselineKnown))
		out = append(out, p)
	}
	return out
}

// verifySweepWait bounds how long the sweep waits on one resource before it
// moves on; the verify itself keeps running.
const verifySweepWait = 6 * time.Hour

// runVerifySweep verifies every resource in turn. Differences are not
// repaired here: the alert monitor raises resource.out_of_sync for them, and
// which copy is right is for an operator to decide.
func (sm *ScheduleManager) runVerifySweep() {
	if !sm.verifying.CompareAndSwap(false, true) {
		sm.controller.logger.Info("Verify sweep still running from last time; skipping this one")
		return
	}
	defer sm.verifying.Store(false)
	ctx := context.Background()
	log := sm.controller.traceLogger()
	log.Debug("runVerifySweep: tick, sweep starting")
	start := time.Now()
	resources, err := sm.controller.db.ListResources(ctx)
	if err != nil {
		log.Warn("Verify sweep: list resources", zap.Error(err))
		return
	}
	for _, r := range resources {
		log.Debug("runVerifySweep: verifying", zap.String("resource", r.Name))
		rctx, cancel := context.WithTimeout(ctx, verifySweepWait+time.Minute)
		resp := sm.controller.resources.Verify(rctx, &pb.VerifyResourceRequest{
			Name: r.Name, WaitSeconds: uint32(verifySweepWait.Seconds()),
		})
		cancel()
		log.Info("Verify sweep", zap.String("resource", r.Name), zap.Bool("ok", resp.Success),
			zap.String("phase", resp.Phase), zap.String("message", resp.Message))
	}
	log.Debug("runVerifySweep: sweep done", zap.Int("resources", len(resources)), zap.Duration("took", time.Since(start)))
}

// DRBD's verify only ever adds to the out-of-sync bitmap: a block found equal
// is left as it was, and the total it prints when it finishes — "found N
// blocks out of sync" — is the whole bitmap, not what this run added. Marks
// also outlive the event that made them: a resync-free reconnect at equal
// current UUIDs, an interrupted resync or an earlier verify all leave them,
// and they stay until a resync copies the blocks. On the haify-meta resource of one
// cluster 97% of the volume was marked while a block-by-block comparison of
// the two replicas found them identical. Reading the total as "differences"
// made every such mark a false alarm, so a verify records what was marked
// before it started and reports what it added separately.

func marksKey(resource, peer string) string { return resource + "\x00" + peer }

// rememberMarks records each peer's marks as a verify of resource begins.
func (rm *ResourceManager) rememberMarks(resource string, st *drbdsetupStatus) {
	for _, c := range st.Connections {
		var kib uint64
		for _, pd := range c.PeerDevices {
			kib += pd.OutOfSyncKiB
		}
		rm.controller.traceLogger().Debug("rememberMarks: baseline", zap.String("resource", resource), zap.String("peer", c.Name),
			zap.Uint64("out_of_sync_kib", kib))
		rm.verifyMarks.Store(marksKey(resource, c.Name), kib)
	}
}

// marksBefore is what was marked out of sync with peer when the last verify of
// resource started, if this controller saw it start.
func (rm *ResourceManager) marksBefore(resource, peer string) (uint64, bool) {
	v, ok := rm.verifyMarks.Load(marksKey(resource, peer))
	if !ok {
		rm.controller.traceLogger().Debug("marksBefore: no baseline", zap.String("resource", resource), zap.String("peer", peer))
		return 0, false
	}
	return v.(uint64), true
}

// verifyMessage says what a finished verify found, and returns the peers with
// marks for the log.
func verifyMessage(name, source string, peers []*pb.VerifyPeer) (string, []string) {
	var marked, notes []string
	for _, p := range peers {
		if p.OutOfSyncKib == 0 {
			continue
		}
		marked = append(marked, fmt.Sprintf("%s (%d KiB)", p.Node, p.OutOfSyncKib))
		switch {
		case !p.BaselineKnown:
			notes = append(notes, fmt.Sprintf("%s: %d KiB marked out of sync; this call cannot say how much of that this verify found", p.Node, p.OutOfSyncKib))
		case p.FoundKib > 0:
			notes = append(notes, fmt.Sprintf("%s: this verify found %d KiB that differ from %s (%d KiB were marked before it)", p.Node, p.FoundKib, source, p.OutOfSyncKib-p.FoundKib))
		default:
			notes = append(notes, fmt.Sprintf("%s: this verify found no new difference; %d KiB were already marked out of sync", p.Node, p.OutOfSyncKib))
		}
	}
	zap.L().Debug("verifyMessage: summarised", zap.String("resource", name), zap.String("source", source),
		zap.Int("peers", len(peers)), zap.Int("marked", len(marked)))
	if len(marked) == 0 {
		return fmt.Sprintf("%s: every verified replica holds the same data as %s", name, source), nil
	}
	return fmt.Sprintf("%s: %s. Marks are left by verifies, interrupted resyncs and reconnects, and only a resync clears them — "+
		"`resource verify %s --node %s --resync` copies %s's data over the marked blocks, which is harmless when the copies are in fact identical",
		name, strings.Join(notes, "; "), name, source, source), marked
}
