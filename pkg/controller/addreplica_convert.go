package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
)

// Giving a diskless member — a data client or the quorum tiebreaker — a disk
// of its own, in place. The node keeps its node-id, its connections and, for
// a client, whatever it has open: a VM or a mounted filesystem on a diskless
// Primary carries on writing while the new disk attaches and resyncs from the
// peers. Removing the client and adding a replica instead would have cut it
// off.
//
// DRBD refuses the attach ("marked as intentional diskless on a peer") until
// every peer has been told the node has a disk, so the peers are adjusted
// before the node itself.

// dropDisklessOverride removes the `disk none` volume overrides from host's
// `on` stanza, which leaves it the resource-level disks every replica has.
func dropDisklessOverride(content, host string) (string, error) {
	lines := strings.Split(content, "\n")
	var out []string
	depth, inHost, dropped := 0, false, 0
	var pending []string // a volume block inside host's stanza, held until we know what it is
	for _, line := range lines {
		t := strings.TrimSpace(line)
		opens, closes := strings.Count(line, "{"), strings.Count(line, "}")
		switch {
		case !inHost && depth == 1 && (t == "on "+host+" {" || strings.HasPrefix(t, "on "+host+" {")):
			inHost = true
			out = append(out, line)
		case inHost && pending == nil && depth == 2 && strings.HasPrefix(t, "volume ") && opens > 0:
			pending = []string{line}
		case pending != nil:
			pending = append(pending, line)
			if depth+opens-closes == 2 { // the volume block closed
				if slices.ContainsFunc(pending, func(l string) bool {
					f := strings.Fields(l)
					return len(f) == 2 && f[0] == "disk" && f[1] == "none;"
				}) {
					dropped++
				} else {
					out = append(out, pending...)
				}
				pending = nil
			}
		default:
			out = append(out, line)
		}
		depth += opens - closes
		if inHost && depth <= 1 {
			inHost = false
		}
	}
	if dropped == 0 {
		return "", fmt.Errorf("%s has no diskless override in the config", host)
	}
	return strings.Join(out, "\n"), nil
}

// convertDisklessToReplica gives node, already a diskless member of dbRes,
// a disk of its own.
func (rm *ResourceManager) convertDisklessToReplica(ctx context.Context, dbRes *database.Resource, node string, opts AddReplicaOpts) error {
	resource := dbRes.Name
	if dbRes.WANMode {
		return fmt.Errorf("%s is a two-site resource; detach %s and add it as a replica instead", resource, node)
	}
	primaries := splitCSV(dbRes.Nodes)
	if len(primaries) == 0 {
		return fmt.Errorf("resource %q has no replica to copy from", resource)
	}
	if !opts.IgnoreFreeSpace {
		if err := rm.assertPoolRoom(ctx, resource, node); err != nil {
			return err
		}
	}
	if pool, sizeGB := rm.memberPoolAndSize(ctx, resource, ""); pool != "" {
		if err := rm.assertOvercommit(ctx, pool, []string{node}, sizeGB); err != nil {
			return err
		}
	}
	addr := rm.controller.ResolveHost(node)
	primaryAddrs := make([]string, 0, len(primaries))
	for _, n := range primaries {
		primaryAddrs = append(primaryAddrs, rm.controller.ResolveHost(n))
	}
	if dbRes.Encrypted {
		if err := rm.assertEncryptionSupported(ctx, []string{addr}, []string{node}); err != nil {
			return err
		}
	}
	if err := rm.checkPromoterPrereqs(ctx, resource, []string{addr}); err != nil {
		return err
	}
	if err := rm.assertBitmapSlotFree(ctx, primaryAddrs, primaries, resource); err != nil {
		return err
	}
	vols, err := rm.controller.db.ListVolumes(ctx, resource)
	if err != nil || len(vols) == 0 {
		return fmt.Errorf("resource %q has no recorded volumes: %v", resource, err)
	}
	sizes, err := rm.primaryBackingSizes(ctx, primaryAddrs, resource, len(vols))
	if err != nil {
		return err
	}
	sizes = replicaBackingSizes(sizes, dbRes.Encrypted)

	resPath := fmt.Sprintf("/etc/drbd.d/%s.res", resource)
	read, err := rm.deployment.Exec(ctx, primaryAddrs[:1], "cat "+resPath)
	if err != nil || !read.AllSuccess() {
		return fmt.Errorf("read the config of %s: %v", resource, err)
	}
	var original string
	for _, h := range read.Hosts {
		original = h.Output
	}
	host := rm.controller.nodes.GetDRBDNameByRef(node)
	updated, err := dropDisklessOverride(original, host)
	if err != nil {
		return err
	}
	others := withoutAll(rm.disklessParticipantHosts(ctx, resource), []string{addr})
	peers := append(append([]string(nil), primaryAddrs...), others...)
	all := append(append([]string(nil), peers...), addr)

	log := rm.controller.logger.With(zap.String("resource", resource), zap.String("node", node))
	log.Info("Giving a diskless member a disk of its own")

	created := 0
	succeeded := false
	defer func() {
		if succeeded {
			return
		}
		rm.undoDisklessConversion(context.WithoutCancel(ctx), resource, resPath, original, peers, addr, vols[:created], dbRes.Encrypted)
	}()
	for i, v := range vols {
		if err := rm.createBackingVolumeOn(ctx, addr, v.Pool, v.VolumeName, sizes[i]); err != nil {
			return fmt.Errorf("create backing volume %s/%s on %s: %w", v.Pool, v.VolumeName, node, err)
		}
		created++
		if dbRes.Encrypted {
			if err := rm.encryptBackingVolumeOn(ctx, addr, node, v.Pool, v.VolumeName,
				fmt.Sprintf("/dev/%s/%s", v.Pool, v.VolumeName)); err != nil {
				return err
			}
		}
	}
	if _, err := rm.deployment.DistributeConfig(ctx, all, updated, resPath); err != nil {
		return fmt.Errorf("distribute the config: %w", err)
	}
	if err := rm.execAllSuccess(ctx, []string{addr},
		fmt.Sprintf("sudo drbdadm create-md --max-peers=%d --force %s", deployment.DefaultMaxPeers, resource),
		"create metadata on "+node); err != nil {
		return err
	}
	adjust := "sudo drbdadm adjust " + resource
	if err := rm.execAllSuccess(ctx, peers, adjust, "tell the peers "+node+" has a disk"); err != nil {
		return err
	}
	if err := rm.execAllSuccess(ctx, []string{addr}, adjust, "attach the new disk on "+node); err != nil {
		return err
	}
	// A diskless member that joined by attach or set-tiebreaker may never have
	// had the boot unit, and an encrypted disk now depends on it to open.
	if err := rm.ensureBootUnitOnMembers(ctx, resource, []string{addr}, dbRes.Encrypted); err != nil {
		return err
	}
	succeeded = true

	dbRes.Nodes = strings.Join(append(primaries, node), ",")
	dbRes.DisklessClients = strings.Join(slices.DeleteFunc(splitCSV(dbRes.DisklessClients), func(n string) bool { return n == node }), ",")
	dbRes.DisklessNodes = strings.Join(slices.DeleteFunc(splitCSV(dbRes.DisklessNodes), func(n string) bool { return n == node }), ",")
	if err := rm.controller.db.SaveResource(ctx, dbRes); err != nil {
		return fmt.Errorf("record %s as a replica: %w", node, err)
	}
	log.Info("Diskless member now has a disk; it resyncs from its peers in the background")
	if err := rm.SyncPromoters(ctx, resource); err != nil {
		return fmt.Errorf("%s has a disk now, but the resource's promoters could not be placed there: %w; "+
			"fix that and run `haify resource repair %s`", node, err, resource)
	}
	return nil
}

// undoDisklessConversion puts the node back to diskless: the old config on
// every member, the node first so it lets go of its disk before the peers
// mark it diskless again, then the new volumes go.
func (rm *ResourceManager) undoDisklessConversion(ctx context.Context, resource, resPath, original string,
	peers []string, addr string, vols []*database.Volume, encrypted bool) {
	log := rm.controller.logger.With(zap.String("resource", resource), zap.String("node", addr))
	log.Warn("Rolling back the diskless member's new disk")
	all := append(append([]string(nil), peers...), addr)
	if _, err := rm.deployment.DistributeConfig(ctx, all, original, resPath); err != nil {
		log.Error("Rollback: restore the config", zap.Error(err))
	}
	adjust := "sudo drbdadm adjust " + resource
	if _, err := rm.deployment.Exec(ctx, []string{addr}, adjust); err != nil {
		log.Error("Rollback: detach the new disk", zap.Error(err))
	}
	if _, err := rm.deployment.Exec(ctx, peers, adjust); err != nil {
		log.Error("Rollback: re-adjust the peers", zap.Error(err))
	}
	for _, v := range vols {
		if encrypted {
			rm.closeBackingVolumeOn(ctx, []string{addr}, v.Pool, v.VolumeName)
		}
		if _, err := rm.deployment.Exec(ctx, []string{addr}, fmt.Sprintf("sudo lvremove -f %s/%s", v.Pool, v.VolumeName)); err != nil {
			log.Error("Rollback: remove a backing volume", zap.String("volume", v.Pool+"/"+v.VolumeName), zap.Error(err))
		}
	}
}
