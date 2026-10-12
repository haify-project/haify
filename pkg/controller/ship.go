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
	existing, err := target.ListResources(ctx)
	if err != nil {
		return fmt.Errorf("ask %s for its resources: %w", address, err)
	}
	for _, r := range existing {
		if r.GetName() == name {
			return fmt.Errorf("%s already has a resource named %s; give another with --as", address, name)
		}
	}
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
	if c.db == nil {
		return "", fmt.Errorf("database not available")
	}
	if req.Target == "" {
		return "", fmt.Errorf("give the receiving controller's address")
	}
	if req.Name == "" {
		req.Name = req.Resource
	}
	info, err := c.resources.GetResource(ctx, req.Resource)
	if err != nil {
		return "", err
	}
	if len(info.Volumes) == 0 {
		return "", fmt.Errorf("resource %q has no volumes", req.Resource)
	}
	backing, err := bm.backingDevices(ctx, req.Resource)
	if err != nil {
		return "", err
	}
	if enc := bm.encryptedVolumes(ctx, req.Resource, backing); len(enc) > 0 {
		return "", fmt.Errorf("%s is encrypted; shipping copies plaintext only, so back it up and restore it there instead", req.Resource)
	}
	for _, v := range info.Volumes {
		if isZFSDevice(backing[v.VolumeID]) || v.Pool == "" || v.BackingVolume == "" {
			return "", fmt.Errorf("%s volume %d is not an LVM volume; only LVM-backed resources can be shipped", req.Resource, v.VolumeID)
		}
	}
	if req.Pool == "" {
		req.Pool = strings.TrimPrefix(info.Volumes[0].Pool, managedNamePrefix)
	}
	if req.Replicas == 0 {
		req.Replicas = uint32(len(info.Nodes))
	}

	target, err := dialShipTarget(req.Target, req.TargetToken)
	if err != nil {
		return "", fmt.Errorf("reach %s: %w", req.Target, err)
	}
	defer func() { _ = target.Close() }()
	if err := checkShipTarget(ctx, target, req.Target, req.Name); err != nil {
		return "", err
	}

	node, err := bm.pickBackupNode(info, "")
	if err != nil {
		return "", err
	}
	host := c.ResolveHost(node)
	haify, err := c.resources.ensureHaifyCLI(ctx, host)
	if err != nil {
		return "", err
	}
	sizes := make(map[uint32]uint64, len(info.Volumes))
	for _, v := range info.Volumes {
		n, err := bm.drbdDeviceBytes(ctx, host, req.Resource, v.VolumeID)
		if err != nil {
			return "", err
		}
		sizes[v.VolumeID] = n
	}

	j := &database.StorageJob{ID: newJobID(), Kind: database.JobShip, State: database.JobRunning,
		Resource: req.Resource, Node: node, Target: req.Target, TargetName: req.Name, TargetPool: req.Pool,
		Progress: "starting", StartedAt: time.Now()}
	if err := c.db.SaveStorageJob(ctx, j); err != nil {
		return "", err
	}
	go bm.runShip(c.ctx, j, shipPlan{req: req, info: info, node: node, host: host, sizes: sizes, haify: haify})
	return j.ID, nil
}

func (bm *BackupManager) shipProgress(ctx context.Context, j *database.StorageJob, msg string) {
	j.Progress, j.UpdatedAt = msg, time.Now()
	_ = bm.controller.db.SaveStorageJob(ctx, j)
}

// runShip carries a ship out and records how it ended.
func (bm *BackupManager) runShip(ctx context.Context, j *database.StorageJob, p shipPlan) {
	log := bm.controller.logger.With(zap.String("resource", p.req.Resource), zap.String("target", p.req.Target))
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
	_ = bm.controller.db.SaveStorageJob(context.WithoutCancel(ctx), j)
}

func (bm *BackupManager) ship(ctx context.Context, j *database.StorageJob, p shipPlan) (err error) {
	target, err := dialShipTarget(p.req.Target, p.req.TargetToken)
	if err != nil {
		return err
	}
	defer func() { _ = target.Close() }()

	bm.shipProgress(ctx, j, "taking snapshots")
	snaps, err := bm.snapshotVolumes(ctx, p.host, p.info, time.Now())
	defer bm.removeSnapshots(context.WithoutCancel(ctx), p.host, p.info, snaps)
	if err != nil {
		return err
	}

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
	if err := target.CreateResourceRequest(ctx, &haifypb.CreateResourceRequest{
		Name: p.req.Name, Protocol: "C", Replicas: p.req.Replicas, Volumes: specs,
		Labels: map[string]string{"haify.ship/from": p.req.Resource},
	}); err != nil {
		return fmt.Errorf("create %s on %s: %w", p.req.Name, p.req.Target, err)
	}
	defer func() {
		if err != nil {
			// A partial copy is worse than none: nothing would say it is one.
			if derr := target.DeleteResource(context.WithoutCancel(ctx), p.req.Name); derr != nil {
				err = fmt.Errorf("%w; and the partial copy %s on %s could not be deleted: %v", err, p.req.Name, p.req.Target, derr)
			}
		}
	}()

	for i, id := range ids {
		if err := bm.shipVolume(ctx, j, p, target, id, fmt.Sprintf("/dev/%s/%s", pools[id], snaps[id]), i == len(ids)-1); err != nil {
			return err
		}
	}
	return nil
}

// shipVolume sends one volume and checks both ends saw the same bytes.
func (bm *BackupManager) shipVolume(ctx context.Context, j *database.StorageJob, p shipPlan, target ShipTarget,
	vol uint32, device string, last bool) error {
	rm := bm.controller.resources
	token, err := ship.NewToken()
	if err != nil {
		return err
	}
	prep, err := target.PrepareShipReceive(ctx, p.req.Name, vol, token)
	if err != nil {
		return fmt.Errorf("open a receiver for volume %d: %w", vol, err)
	}
	args := []string{"--device", device, "--size", strconv.FormatUint(p.sizes[vol], 10),
		"--to", fmt.Sprintf("%s:%d", prep.Address, prep.Port), "--fingerprint", prep.Fingerprint}
	defer func() {
		_, _ = rm.deployment.Exec(context.WithoutCancel(ctx), []string{p.host},
			"sudo sh -c '"+shipCleanupScript("send", p.req.Resource, vol)+"'")
	}()
	res, err := rm.deployment.Exec(ctx, []string{p.host},
		"echo "+base64Std(shipStartScript(p.haify, "send", p.req.Resource, vol, token, args, "progress"))+" | base64 -d | sudo /bin/bash")
	if err := execFailure(res, err); err != nil {
		_, _ = target.FinishShipReceive(ctx, p.req.Name, vol, last)
		return fmt.Errorf("start sending volume %d from %s: %w", vol, p.node, err)
	}

	// The sender reports progress every few seconds; a transfer has no other
	// upper bound, so the wait is as long as the bytes keep moving.
	var sent *shipNodeResult
	for sent == nil {
		r, found, err := rm.readShipResult(ctx, p.host, "send", p.req.Resource, vol, 10*time.Second)
		if err != nil {
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
		bm.shipProgress(ctx, j, fmt.Sprintf("volume %d: %s of %s", vol, util.FormatBytes(n), util.FormatBytes(p.sizes[vol])))
	}
	got, ferr := target.FinishShipReceive(ctx, p.req.Name, vol, last)
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
	jobs, err := bm.controller.db.ListStorageJobs(ctx)
	if err != nil {
		return
	}
	for _, j := range jobs {
		if j.Kind == database.JobShip && j.State == database.JobRunning {
			j.State, j.UpdatedAt = database.JobFailed, time.Now()
			j.Message = fmt.Sprintf("the controller restarted during the transfer; delete %s on %s and ship again", j.TargetName, j.Target)
			_ = bm.controller.db.SaveStorageJob(ctx, j)
		}
	}
}
