package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

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
	zap.L().Debug("dropDisklessOverride: overrides dropped", zap.String("host", host), zap.Int("count", dropped))
	if dropped == 0 {
		return "", fmt.Errorf("%s has no diskless override in the config", host)
	}
	return strings.Join(out, "\n"), nil
}

// convertDisklessToReplica gives node, already a diskless member of dbRes,
// a disk of its own.
func (rm *ResourceManager) convertDisklessToReplica(ctx context.Context, dbRes *database.Resource, node string, opts AddReplicaOpts) error {
	resource := dbRes.Name
	tlog := rm.controller.traceLogger()
	tlog.Debug("convertDisklessToReplica: start", zap.String("resource", resource), zap.String("node", node),
		zap.Bool("wan_mode", dbRes.WANMode), zap.Bool("encrypted", dbRes.Encrypted),
		zap.Bool("ignore_free_space", opts.IgnoreFreeSpace))
	if dbRes.WANMode {
		tlog.Debug("convertDisklessToReplica: refusing two-site resource", zap.String("resource", resource))
		return fmt.Errorf("%s is a two-site resource; detach %s and add it as a replica instead", resource, node)
	}
	primaries := splitCSV(dbRes.Nodes)
	if len(primaries) == 0 {
		tlog.Debug("convertDisklessToReplica: no replica to copy from", zap.String("resource", resource))
		return fmt.Errorf("resource %q has no replica to copy from", resource)
	}
	if !opts.IgnoreFreeSpace {
		if err := rm.assertPoolRoom(ctx, resource, node); err != nil {
			tlog.Debug("convertDisklessToReplica: pool has no room", zap.String("node", node), zap.Error(err))
			return err
		}
	} else {
		tlog.Debug("convertDisklessToReplica: free-space check skipped", zap.String("node", node))
	}
	if pool, sizeGB := rm.memberPoolAndSize(ctx, resource, ""); pool != "" {
		tlog.Debug("convertDisklessToReplica: checking overcommit", zap.String("pool", pool), zap.Uint64("size_gb", sizeGB))
		if err := rm.assertOvercommit(ctx, pool, []string{node}, sizeGB); err != nil {
			tlog.Debug("convertDisklessToReplica: overcommit refused", zap.String("pool", pool), zap.Error(err))
			return err
		}
	} else {
		tlog.Debug("convertDisklessToReplica: no member pool known, overcommit not checked")
	}
	addr := rm.controller.ResolveHost(node)
	primaryAddrs := make([]string, 0, len(primaries))
	for _, n := range primaries {
		primaryAddrs = append(primaryAddrs, rm.controller.ResolveHost(n))
	}
	tlog.Debug("convertDisklessToReplica: resolved addresses", zap.String("host", addr), zap.Strings("primary_addrs", primaryAddrs))
	if dbRes.Encrypted {
		if err := rm.assertEncryptionSupported(ctx, []string{addr}, []string{node}); err != nil {
			tlog.Debug("convertDisklessToReplica: node cannot encrypt", zap.String("node", node), zap.Error(err))
			return err
		}
	}
	if err := rm.checkPromoterPrereqs(ctx, resource, []string{addr}); err != nil {
		tlog.Debug("convertDisklessToReplica: promoter prerequisites missing", zap.String("host", addr), zap.Error(err))
		return err
	}
	if err := rm.assertBitmapSlotFree(ctx, primaryAddrs, primaries, resource); err != nil {
		tlog.Debug("convertDisklessToReplica: no bitmap slot", zap.String("resource", resource), zap.Error(err))
		return err
	}
	start := time.Now()
	vols, err := rm.controller.db.ListVolumes(ctx, resource)
	tlog.Debug("convertDisklessToReplica: db list volumes", zap.String("resource", resource), zap.Int("count", len(vols)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || len(vols) == 0 {
		return fmt.Errorf("resource %q has no recorded volumes: %v", resource, err)
	}
	sizes, err := rm.primaryBackingSizes(ctx, primaryAddrs, resource, len(vols))
	if err != nil {
		tlog.Debug("convertDisklessToReplica: backing sizes unreadable", zap.Error(err))
		return err
	}
	sizes = replicaBackingSizes(sizes, dbRes.Encrypted)
	tlog.Debug("convertDisklessToReplica: new disk sizes", zap.Uint64s("size_bytes", sizes))

	resPath := fmt.Sprintf("/etc/drbd.d/%s.res", resource)
	start = time.Now()
	read, err := rm.deployment.Exec(ctx, primaryAddrs[:1], "cat "+resPath)
	tlog.Debug("convertDisklessToReplica: read live config", zap.String("host", primaryAddrs[0]), zap.String("path", resPath),
		zap.Strings("failed_hosts", tracedFailedHosts(read)), zap.Duration("took", time.Since(start)), zap.Error(err))
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
		tlog.Debug("convertDisklessToReplica: no diskless override to drop", zap.String("drbd_name", host), zap.Error(err))
		return err
	}
	others := withoutAll(rm.disklessParticipantHosts(ctx, resource), []string{addr})
	peers := append(append([]string(nil), primaryAddrs...), others...)
	all := append(append([]string(nil), peers...), addr)
	tlog.Debug("convertDisklessToReplica: members to update", zap.Strings("peers", peers), zap.Strings("hosts", all))

	log := rm.controller.logger.With(zap.String("resource", resource), zap.String("node", node))
	log.Info("Giving a diskless member a disk of its own")

	created := 0
	succeeded := false
	defer func() {
		if succeeded {
			return
		}
		tlog.Debug("convertDisklessToReplica: failed part way, rolling back", zap.Int("volumes_created", created))
		rm.undoDisklessConversion(context.WithoutCancel(ctx), resource, resPath, original, peers, addr, vols[:created], dbRes.Encrypted)
	}()
	for i, v := range vols {
		tlog.Debug("convertDisklessToReplica: creating backing volume", zap.String("host", addr), zap.String("pool", v.Pool),
			zap.String("volume", v.VolumeName), zap.Uint64("size_bytes", sizes[i]))
		if err := rm.createBackingVolumeOn(ctx, addr, v.Pool, v.VolumeName, sizes[i]); err != nil {
			tlog.Debug("convertDisklessToReplica: backing volume creation failed", zap.String("volume", v.VolumeName), zap.Error(err))
			return fmt.Errorf("create backing volume %s/%s on %s: %w", v.Pool, v.VolumeName, node, err)
		}
		created++
		if dbRes.Encrypted {
			if err := rm.encryptBackingVolumeOn(ctx, addr, node, v.Pool, v.VolumeName,
				fmt.Sprintf("/dev/%s/%s", v.Pool, v.VolumeName)); err != nil {
				tlog.Debug("convertDisklessToReplica: encrypting volume failed", zap.String("volume", v.VolumeName), zap.Error(err))
				return err
			}
		}
	}
	start = time.Now()
	_, err = rm.deployment.DistributeConfig(ctx, all, updated, resPath)
	tlog.Debug("convertDisklessToReplica: distribute config", zap.Strings("hosts", all), zap.String("path", resPath),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("distribute the config: %w", err)
	}
	if err := rm.execAllSuccess(ctx, []string{addr},
		fmt.Sprintf("sudo drbdadm create-md --max-peers=%d --force %s", deployment.DefaultMaxPeers, resource),
		"create metadata on "+node); err != nil {
		tlog.Debug("convertDisklessToReplica: create-md failed", zap.String("host", addr), zap.Error(err))
		return err
	}
	adjust := "sudo drbdadm adjust " + resource
	if err := rm.execAllSuccess(ctx, peers, adjust, "tell the peers "+node+" has a disk"); err != nil {
		tlog.Debug("convertDisklessToReplica: adjusting peers failed", zap.Strings("hosts", peers), zap.Error(err))
		return err
	}
	if err := rm.execAllSuccess(ctx, []string{addr}, adjust, "attach the new disk on "+node); err != nil {
		tlog.Debug("convertDisklessToReplica: attaching the new disk failed", zap.String("host", addr), zap.Error(err))
		return err
	}
	// A diskless member that joined by attach or set-tiebreaker may never have
	// had the boot unit, and an encrypted disk now depends on it to open.
	if err := rm.ensureBootUnitOnMembers(ctx, resource, []string{addr}, dbRes.Encrypted); err != nil {
		tlog.Debug("convertDisklessToReplica: boot unit install failed", zap.String("host", addr), zap.Error(err))
		return err
	}
	succeeded = true

	dbRes.Nodes = strings.Join(append(primaries, node), ",")
	dbRes.DisklessClients = strings.Join(slices.DeleteFunc(splitCSV(dbRes.DisklessClients), func(n string) bool { return n == node }), ",")
	dbRes.DisklessNodes = strings.Join(slices.DeleteFunc(splitCSV(dbRes.DisklessNodes), func(n string) bool { return n == node }), ",")
	start = time.Now()
	err = rm.controller.db.SaveResource(ctx, dbRes)
	tlog.Debug("convertDisklessToReplica: db save resource", zap.String("resource", resource), zap.String("nodes", dbRes.Nodes),
		zap.String("diskless_clients", dbRes.DisklessClients), zap.String("diskless_nodes", dbRes.DisklessNodes),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("record %s as a replica: %w", node, err)
	}
	log.Info("Diskless member now has a disk; it resyncs from its peers in the background")
	if err := rm.SyncPromoters(ctx, resource); err != nil {
		tlog.Debug("convertDisklessToReplica: promoter sync failed", zap.String("resource", resource), zap.Error(err))
		return fmt.Errorf("%s has a disk now, but the resource's promoters could not be placed there: %w; "+
			"fix that and run `haify resource repair %s`", node, err, resource)
	}
	tlog.Debug("convertDisklessToReplica: done", zap.String("resource", resource), zap.String("node", node))
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
	log.Debug("undoDisklessConversion: start", zap.Strings("hosts", all), zap.Int("volumes", len(vols)), zap.Bool("encrypted", encrypted))
	start := time.Now()
	_, err := rm.deployment.DistributeConfig(ctx, all, original, resPath)
	log.Debug("undoDisklessConversion: restore config", zap.String("path", resPath), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		log.Error("Rollback: restore the config", zap.Error(err))
	}
	adjust := "sudo drbdadm adjust " + resource
	start = time.Now()
	_, err = rm.deployment.Exec(ctx, []string{addr}, adjust)
	log.Debug("undoDisklessConversion: adjust node", zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		log.Error("Rollback: detach the new disk", zap.Error(err))
	}
	start = time.Now()
	_, err = rm.deployment.Exec(ctx, peers, adjust)
	log.Debug("undoDisklessConversion: adjust peers", zap.Strings("hosts", peers), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		log.Error("Rollback: re-adjust the peers", zap.Error(err))
	}
	for _, v := range vols {
		log.Debug("undoDisklessConversion: removing backing volume", zap.String("pool", v.Pool), zap.String("volume", v.VolumeName))
		if encrypted {
			rm.closeBackingVolumeOn(ctx, []string{addr}, v.Pool, v.VolumeName)
		}
		start := time.Now()
		_, err := rm.deployment.Exec(ctx, []string{addr}, fmt.Sprintf("sudo lvremove -f %s/%s", v.Pool, v.VolumeName))
		log.Debug("undoDisklessConversion: lvremove done", zap.String("volume", v.VolumeName), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			log.Error("Rollback: remove a backing volume", zap.String("volume", v.Pool+"/"+v.VolumeName), zap.Error(err))
		}
	}
	log.Debug("undoDisklessConversion: done")
}
