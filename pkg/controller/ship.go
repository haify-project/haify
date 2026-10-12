package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/client"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/ship"
	"github.com/haify-project/haify/pkg/util"
	"go.uber.org/zap"
)

// `haify resource ship`: copy a resource to another Haify cluster with
// nothing in between. Every volume is snapshotted at once on one replica,
// the receiving controller creates the resource and opens a receiver per
// volume (ship_receive.go), and the snapshot streams from this cluster's node
// to that one's over TLS (pkg/ship). Both ends checksum what passed through;
// they must agree, or the copy on the other side is deleted.
//
// The target's API token and the per-volume ship tokens are never traced,
// nor the start scripts that carry them.

// ShipTarget is what a ship needs from the receiving controller.
type ShipTarget interface {
	ListResources(ctx context.Context) ([]*haifypb.ResourceInfo, error)
	CreateResourceRequest(ctx context.Context, req *haifypb.CreateResourceRequest) error
	DeleteResource(ctx context.Context, name string) error
	PrepareShipReceive(ctx context.Context, resource string, vol uint32, token string) (*haifypb.PrepareShipReceiveResponse, error)
	FinishShipReceive(ctx context.Context, resource string, vol uint32, last bool) (*haifypb.FinishShipReceiveResponse, error)
	Close() error
}

// dialShipTarget is how the target is reached; tests replace it.
var dialShipTarget = func(address, token string) (ShipTarget, error) {
	zap.L().Debug("dialShipTarget: dialing", zap.String("target", address), zap.Bool("has_token", token != ""))
	return client.NewHaifyClient(address, client.WithToken(token))
}

// ShipRequest is a ship as asked for.
type ShipRequest struct {
	Resource, Target, TargetToken, Name, Pool string
	Replicas                                  uint32
}

// checkShipTarget proves the address and the token before anything is
// touched here, and that the name is free there.
func checkShipTarget(ctx context.Context, target ShipTarget, address, name string) error {
	log := zap.L()
	start := time.Now()
	existing, err := target.ListResources(ctx)
	if err != nil {
		log.Debug("checkShipTarget: target unreachable", zap.String("target", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("ask %s for its resources: %w", address, err)
	}
	for _, r := range existing {
		if r.GetName() == name {
			log.Debug("checkShipTarget: name taken on the target", zap.String("target", address), zap.String("name", name))
			return fmt.Errorf("%s already has a resource named %s; give another with --as", address, name)
		}
	}
	log.Debug("checkShipTarget: target ready", zap.String("target", address), zap.String("name", name),
		zap.Int("resources", len(existing)), zap.Duration("took", time.Since(start)))
	return nil
}

// shipPlan is what the checks before a ship found.
type shipPlan struct {
	req   ShipRequest
	info  *ResourceInfo
	node  string
	host  string
	sizes map[uint32]uint64
	// haify is the CLI on host that runs the sender.
	haify string
}

// StartShip checks everything a ship needs, then runs it as a storage job.
func (bm *BackupManager) StartShip(ctx context.Context, req ShipRequest) (string, error) {
	c := bm.controller
	log := ctrlLogOrGlobal(c)
	log.Debug("StartShip: checking", zap.String("resource", req.Resource), zap.String("target", req.Target),
		zap.String("name", req.Name), zap.String("pool", req.Pool), zap.Uint32("replicas", req.Replicas),
		zap.Bool("has_target_token", req.TargetToken != ""))
	if c.db == nil {
		log.Debug("StartShip: no database")
		return "", fmt.Errorf("database not available")
	}
	if req.Target == "" {
		log.Debug("StartShip: no target address")
		return "", fmt.Errorf("give the receiving controller's address")
	}
	if req.Name == "" {
		log.Debug("StartShip: no name, keeping the resource's", zap.String("name", req.Resource))
		req.Name = req.Resource
	}
	info, err := c.resources.GetResource(ctx, req.Resource)
	if err != nil {
		log.Debug("StartShip: resource not read", zap.String("resource", req.Resource), zap.Error(err))
		return "", err
	}
	if len(info.Volumes) == 0 {
		log.Debug("StartShip: resource has no volumes", zap.String("resource", req.Resource))
		return "", fmt.Errorf("resource %q has no volumes", req.Resource)
	}
	backing, err := bm.backingDevices(ctx, req.Resource)
	if err != nil {
		log.Debug("StartShip: backing devices not resolved", zap.Error(err))
		return "", err
	}
	if enc := bm.encryptedVolumes(ctx, req.Resource, backing); len(enc) > 0 {
		log.Debug("StartShip: resource is encrypted, refusing", zap.String("resource", req.Resource), zap.Int("encrypted_volumes", len(enc)))
		return "", fmt.Errorf("%s is encrypted; shipping copies plaintext only, so back it up and restore it there instead", req.Resource)
	}
	for _, v := range info.Volumes {
		if isZFSDevice(backing[v.VolumeID]) || v.Pool == "" || v.BackingVolume == "" {
			log.Debug("StartShip: volume not on LVM, refusing", zap.Uint32("volume", v.VolumeID), zap.String("device", backing[v.VolumeID]))
			return "", fmt.Errorf("%s volume %d is not an LVM volume; only LVM-backed resources can be shipped", req.Resource, v.VolumeID)
		}
	}
	if req.Pool == "" {
		req.Pool = strings.TrimPrefix(info.Volumes[0].Pool, managedNamePrefix)
		log.Debug("StartShip: no pool, using the source's", zap.String("pool", req.Pool))
	}
	if req.Replicas == 0 {
		req.Replicas = uint32(len(info.Nodes))
		log.Debug("StartShip: no replica count, using the source's", zap.Uint32("replicas", req.Replicas))
	}

	target, err := dialShipTarget(req.Target, req.TargetToken)
	if err != nil {
		log.Debug("StartShip: target not reached", zap.String("target", req.Target), zap.Error(err))
		return "", fmt.Errorf("reach %s: %w", req.Target, err)
	}
	defer func() { _ = target.Close() }()
	if err := checkShipTarget(ctx, target, req.Target, req.Name); err != nil {
		return "", err
	}

	node, err := bm.pickBackupNode(info, "")
	if err != nil {
		log.Debug("StartShip: no node to send from", zap.Error(err))
		return "", err
	}
	host := c.ResolveHost(node)
	haify, err := c.resources.ensureHaifyCLI(ctx, host)
	if err != nil {
		log.Debug("StartShip: no haify CLI on the sender", zap.String("node", node), zap.Error(err))
		return "", err
	}
	sizes := make(map[uint32]uint64, len(info.Volumes))
	for _, v := range info.Volumes {
		n, err := bm.drbdDeviceBytes(ctx, host, req.Resource, v.VolumeID)
		if err != nil {
			log.Debug("StartShip: volume size not read", zap.Uint32("volume", v.VolumeID), zap.Error(err))
			return "", err
		}
		log.Debug("StartShip: volume size", zap.Uint32("volume", v.VolumeID), zap.Uint64("size_bytes", n))
		sizes[v.VolumeID] = n
	}

	j := &database.StorageJob{ID: newJobID(), Kind: database.JobShip, State: database.JobRunning,
		Resource: req.Resource, Node: node, Target: req.Target, TargetName: req.Name, TargetPool: req.Pool,
		Progress: "starting", StartedAt: time.Now()}
	if err := c.db.SaveStorageJob(ctx, j); err != nil {
		log.Debug("StartShip: job not saved", zap.Error(err))
		return "", err
	}
	log.Debug("StartShip: job started", zap.String("job", j.ID), zap.String("node", node), zap.String("haify", haify))
	go bm.runShip(c.ctx, j, shipPlan{req: req, info: info, node: node, host: host, sizes: sizes, haify: haify})
	return j.ID, nil
}

func (bm *BackupManager) shipProgress(ctx context.Context, j *database.StorageJob, msg string) {
	j.Progress, j.UpdatedAt = msg, time.Now()
	err := bm.controller.db.SaveStorageJob(ctx, j)
	ctrlLogOrGlobal(bm.controller).Debug("shipProgress: progress", zap.String("job", j.ID), zap.String("progress", msg), zap.Error(err))
}

// runShip carries a ship out and records how it ended.
func (bm *BackupManager) runShip(ctx context.Context, j *database.StorageJob, p shipPlan) {
	log := bm.controller.logger.With(zap.String("resource", p.req.Resource), zap.String("target", p.req.Target))
	log.Debug("runShip: running", zap.String("job", j.ID), zap.String("node", p.node), zap.Int("volumes", len(p.sizes)))
	start := time.Now()
	err := bm.ship(ctx, j, p)
	j.UpdatedAt = time.Now()
	if err != nil {
		j.State, j.Message = database.JobFailed, err.Error()
		log.Error("Ship failed", zap.Error(err))
	} else {
		j.State, j.Progress = database.JobDone, "done"
		j.Message = fmt.Sprintf("copied to %s as %s", p.req.Target, p.req.Name)
		log.Info("Ship finished", zap.String("as", p.req.Name))
	}
	serr := bm.controller.db.SaveStorageJob(context.WithoutCancel(ctx), j)
	log.Debug("runShip: ended", zap.String("job", j.ID), zap.String("state", j.State), zap.Duration("took", time.Since(start)),
		zap.NamedError("save_error", serr))
}

func (bm *BackupManager) ship(ctx context.Context, j *database.StorageJob, p shipPlan) (err error) {
	log := ctrlLogOrGlobal(bm.controller)
	log.Debug("ship: dialing target", zap.String("target", p.req.Target))
	target, err := dialShipTarget(p.req.Target, p.req.TargetToken)
	if err != nil {
		log.Debug("ship: target not reached", zap.String("target", p.req.Target), zap.Error(err))
		return err
	}
	defer func() { _ = target.Close() }()

	bm.shipProgress(ctx, j, "taking snapshots")
	snaps, err := bm.snapshotVolumes(ctx, p.host, p.info, time.Now())
	defer bm.removeSnapshots(context.WithoutCancel(ctx), p.host, p.info, snaps)
	if err != nil {
		log.Debug("ship: snapshots failed", zap.String("host", p.host), zap.Error(err))
		return err
	}
	log.Debug("ship: snapshots taken", zap.Int("count", len(snaps)))

	ids := make([]uint32, 0, len(p.info.Volumes))
	pools := map[uint32]string{}
	for _, v := range p.info.Volumes {
		ids = append(ids, v.VolumeID)
		pools[v.VolumeID] = v.Pool
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	specs := make([]*haifypb.VolumeSpec, 0, len(ids))
	for _, id := range ids {
		specs = append(specs, &haifypb.VolumeSpec{SizeBytes: p.sizes[id], Pool: p.req.Pool})
	}
	bm.shipProgress(ctx, j, "creating "+p.req.Name+" on "+p.req.Target)
	start := time.Now()
	if err := target.CreateResourceRequest(ctx, &haifypb.CreateResourceRequest{
		Name: p.req.Name, Protocol: "C", Replicas: p.req.Replicas, Volumes: specs,
		Labels: map[string]string{"haify.ship/from": p.req.Resource},
	}); err != nil {
		log.Debug("ship: target resource not created", zap.String("name", p.req.Name), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("create %s on %s: %w", p.req.Name, p.req.Target, err)
	}
	log.Debug("ship: target resource created", zap.String("name", p.req.Name), zap.Int("volumes", len(specs)),
		zap.Duration("took", time.Since(start)))
	defer func() {
		if err != nil {
			// A partial copy is worse than none: nothing would say it is one.
			derr := target.DeleteResource(context.WithoutCancel(ctx), p.req.Name)
			log.Debug("ship: failed, deleting the partial copy", zap.String("name", p.req.Name), zap.NamedError("delete_error", derr))
			if derr != nil {
				err = fmt.Errorf("%w; and the partial copy %s on %s could not be deleted: %v", err, p.req.Name, p.req.Target, derr)
			}
		}
	}()

	for i, id := range ids {
		log.Debug("ship: sending volume", zap.Uint32("volume", id), zap.String("snapshot", snaps[id]), zap.Bool("last", i == len(ids)-1))
		if err := bm.shipVolume(ctx, j, p, target, id, fmt.Sprintf("/dev/%s/%s", pools[id], snaps[id]), i == len(ids)-1); err != nil {
			return err
		}
	}
	return nil
}

// shipVolume sends one volume and checks both ends saw the same bytes.
func (bm *BackupManager) shipVolume(ctx context.Context, j *database.StorageJob, p shipPlan, target ShipTarget,
	vol uint32, device string, last bool) error {
	log := ctrlLogOrGlobal(bm.controller)
	rm := bm.controller.resources
	token, err := ship.NewToken()
	if err != nil {
		log.Debug("shipVolume: token not generated", zap.Error(err))
		return err
	}
	start := time.Now()
	prep, err := target.PrepareShipReceive(ctx, p.req.Name, vol, token)
	if err != nil {
		log.Debug("shipVolume: receiver not opened", zap.Uint32("volume", vol), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("open a receiver for volume %d: %w", vol, err)
	}
	log.Debug("shipVolume: receiver open", zap.Uint32("volume", vol), zap.String("address", prep.Address),
		zap.Uint32("port", prep.Port), zap.String("fingerprint", prep.Fingerprint), zap.Duration("took", time.Since(start)))
	args := []string{"--device", device, "--size", strconv.FormatUint(p.sizes[vol], 10),
		"--to", fmt.Sprintf("%s:%d", prep.Address, prep.Port), "--fingerprint", prep.Fingerprint}
	defer func() {
		_, cerr := rm.deployment.Exec(context.WithoutCancel(ctx), []string{p.host},
			"sudo sh -c '"+shipCleanupScript("send", p.req.Resource, vol)+"'")
		log.Debug("shipVolume: sender cleaned up", zap.String("host", p.host), zap.Uint32("volume", vol), zap.Error(cerr))
	}()
	start = time.Now()
	res, err := rm.deployment.Exec(ctx, []string{p.host},
		"echo "+base64Std(shipStartScript(p.haify, "send", p.req.Resource, vol, token, args, "progress"))+" | base64 -d | sudo /bin/bash")
	if err := execFailure(res, err); err != nil {
		_, ferr := target.FinishShipReceive(ctx, p.req.Name, vol, last)
		log.Debug("shipVolume: sender did not start, receiver closed", zap.String("host", p.host), zap.Uint32("volume", vol),
			zap.Duration("took", time.Since(start)), zap.Error(err), zap.NamedError("finish_error", ferr))
		return fmt.Errorf("start sending volume %d from %s: %w", vol, p.node, err)
	}
	log.Debug("shipVolume: sender started", zap.String("host", p.host), zap.Uint32("volume", vol), zap.Duration("took", time.Since(start)))

	// The sender reports progress every few seconds; a transfer has no other
	// upper bound, so the wait is as long as the bytes keep moving.
	var sent *shipNodeResult
	for sent == nil {
		r, found, err := rm.readShipResult(ctx, p.host, "send", p.req.Resource, vol, 10*time.Second)
		if err != nil {
			log.Debug("shipVolume: sender result unreadable", zap.Uint32("volume", vol), zap.Error(err))
			return err
		}
		if found {
			sent = r
			break
		}
		prog, _ := rm.deployment.Exec(ctx, []string{p.host}, "sudo cat "+shipStateDir("send", p.req.Resource, vol)+"/progress 2>/dev/null; true")
		var n uint64
		for _, h := range prog.Hosts {
			n, _ = strconv.ParseUint(strings.TrimSpace(h.Output), 10, 64)
		}
		log.Debug("shipVolume: sending", zap.Uint32("volume", vol), zap.Uint64("sent_bytes", n), zap.Uint64("size_bytes", p.sizes[vol]))
		bm.shipProgress(ctx, j, fmt.Sprintf("volume %d: %s of %s", vol, util.FormatBytes(n), util.FormatBytes(p.sizes[vol])))
	}
	got, ferr := target.FinishShipReceive(ctx, p.req.Name, vol, last)
	log.Debug("shipVolume: both ends finished", zap.Uint32("volume", vol), zap.Bool("sent_ok", sent.OK),
		zap.Uint64("sent_bytes", sent.Bytes), zap.Bool("received_ok", got.GetSuccess()), zap.Uint64("received_bytes", got.GetBytes()),
		zap.Bool("sha256_match", got.GetSha256() == sent.SHA256), zap.NamedError("finish_error", ferr))
	switch {
	case !sent.OK:
		return fmt.Errorf("sending volume %d failed: %s", vol, sent.Error)
	case ferr != nil:
		return fmt.Errorf("volume %d: %w", vol, ferr)
	case !got.GetSuccess():
		return fmt.Errorf("receiving volume %d failed: %s", vol, got.GetMessage())
	case got.GetSha256() != sent.SHA256 || got.GetBytes() != p.sizes[vol]:
		return fmt.Errorf("volume %d arrived different: sent %d bytes (sha256 %s), received %d (sha256 %s)",
			vol, p.sizes[vol], sent.SHA256, got.GetBytes(), got.GetSha256())
	}
	bm.shipProgress(ctx, j, fmt.Sprintf("volume %d: %s copied, sha256 %s", vol, util.FormatBytes(p.sizes[vol]), sent.SHA256[:12]))
	return nil
}

// failInterruptedShips marks ships a previous controller was running as
// failed: their sender died with it, and the partial copy on the other side
// has to be shipped again.
func (bm *BackupManager) failInterruptedShips(ctx context.Context) {
	log := ctrlLogOrGlobal(bm.controller)
	jobs, err := bm.controller.db.ListStorageJobs(ctx)
	if err != nil {
		log.Debug("failInterruptedShips: job list failed", zap.Error(err))
		return
	}
	failed := 0
	for _, j := range jobs {
		if j.Kind == database.JobShip && j.State == database.JobRunning {
			j.State, j.UpdatedAt = database.JobFailed, time.Now()
			j.Message = fmt.Sprintf("the controller restarted during the transfer; delete %s on %s and ship again", j.TargetName, j.Target)
			serr := bm.controller.db.SaveStorageJob(ctx, j)
			log.Debug("failInterruptedShips: ship marked failed", zap.String("job", j.ID), zap.String("resource", j.Resource),
				zap.String("target", j.Target), zap.Error(serr))
			failed++
		}
	}
	log.Debug("failInterruptedShips: done", zap.Int("jobs", len(jobs)), zap.Int("failed", failed))
}
