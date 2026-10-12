package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// A brand-new resource on thin storage has nothing to synchronise: every
// replica is a freshly allocated thin LV or zvol, and an unwritten block of
// either reads as zeros. Running the initial sync anyway copies a volume's
// worth of zeros to every peer, and a thin target stores them — each secondary
// replica came out 100% allocated however little had been written, which is
// thin provisioning undone on every node but one, and a sync as long as the
// volume is big.
//
// So on thin storage the replicas are declared identical instead: a new
// current UUID with a cleared bitmap, the way AddVolume already brings up a
// new volume and the way LINSTOR does it. Thick LVM keeps the full sync: a
// freshly allocated LV holds whatever the extents held before, and replicas
// that differ in blocks nobody wrote would fail an online verify and could
// surface stale data.

// backedByZeroReadingStorage reports whether every diskful replica of a new
// resource sits on storage whose unwritten blocks read as zeros, so the
// replicas start identical.
//
// It asks what was actually built rather than trusting storageType: the CLI's
// --storage-type defaults to "lvm", and on a thin pool createBackingVolume
// builds a thin LV anyway, so the flag alone would send a thin resource
// through the full sync this exists to avoid.
//
// An encrypted volume never qualifies. Under LUKS an unwritten block reads as
// that node's decryption of zeros — noise, and different noise on every node,
// since each keeps its own key.
func (rm *ResourceManager) backedByZeroReadingStorage(ctx context.Context, storageType string, nodeIPs []string, vols []resolvedVolume) bool {
	log := rm.controller.traceLogger()
	log.Debug("backedByZeroReadingStorage: checking", zap.String("storage_type", storageType),
		zap.Strings("nodes", nodeIPs), zap.Int("volumes", len(vols)))
	for _, v := range vols {
		if v.encrypted {
			log.Debug("backedByZeroReadingStorage: encrypted volume, full sync needed",
				zap.String("volume", v.volumeName), zap.String("pool", v.pool))
			return false
		}
	}
	switch strings.ToLower(strings.TrimSpace(storageType)) {
	case "zfs", "zfs-thin":
		log.Debug("backedByZeroReadingStorage: zfs storage reads unwritten blocks as zeros",
			zap.String("storage_type", storageType))
		return true
	}
	for _, v := range vols {
		for _, ip := range nodeIPs {
			start := time.Now()
			thin, err := rm.deployment.LVIsThin(ctx, ip, v.pool, v.volumeName)
			log.Debug("backedByZeroReadingStorage: checked whether the LV is thin", zap.String("host", ip),
				zap.String("pool", v.pool), zap.String("volume", v.volumeName), zap.Bool("thin", thin),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			if err != nil || !thin {
				log.Debug("backedByZeroReadingStorage: not thin everywhere, full sync needed",
					zap.String("host", ip), zap.String("volume", v.volumeName))
				return false
			}
		}
	}
	log.Debug("backedByZeroReadingStorage: every replica is thin")
	return true
}

// peerConnectWait bounds how long a new resource may take to connect before
// the initial sync falls back to a full one.
const peerConnectWait = 30 * time.Second

// skipInitialSync marks a brand-new resource UpToDate on every replica without
// syncing, once all of its peers are connected. A peer that is not connected
// when the bitmap is cleared would still get a full sync when it does connect,
// so a resource that does not connect in time returns an error and the caller
// falls back to the ordinary initial sync.
func (rm *ResourceManager) skipInitialSync(ctx context.Context, resource, address string) error {
	log := rm.controller.traceLogger()
	log.Debug("skipInitialSync: waiting for peers to connect", zap.String("resource", resource),
		zap.String("host", address), zap.Duration("wait", peerConnectWait))
	deadline := time.Now().Add(peerConnectWait)
	for {
		start := time.Now()
		res, err := rm.deployment.Exec(ctx, []string{address}, "sudo drbdsetup status "+resource)
		log.Debug("skipInitialSync: drbdsetup status", zap.String("resource", resource), zap.String("host", address),
			zap.Bool("success", err == nil && res.AllSuccess()), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err == nil && res.AllSuccess() {
			out := ""
			for _, h := range res.Hosts {
				out = h.Output
			}
			// Plain `drbdsetup status` names the connection state only when it
			// is not Connected.
			if strings.Contains(out, "role:") && !strings.Contains(out, "connection:") {
				log.Debug("skipInitialSync: all peers connected", zap.String("resource", resource))
				break
			}
			log.Debug("skipInitialSync: peers not all connected yet", zap.String("resource", resource))
		}
		if time.Now().After(deadline) {
			log.Debug("skipInitialSync: peers did not connect in time", zap.String("resource", resource),
				zap.Duration("wait", peerConnectWait))
			return fmt.Errorf("peers of %s did not connect within %s", resource, peerConnectWait)
		}
		select {
		case <-ctx.Done():
			log.Debug("skipInitialSync: context done while waiting", zap.String("resource", resource), zap.Error(ctx.Err()))
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}

	rm.controller.logger.Info("Thin storage: skipping the initial sync (new current UUID, cleared bitmap)",
		zap.String("resource", resource), zap.String("address", address))
	// new-current-uuid takes one volume at a time. The verbose status lists
	// this node's volumes as two-space-indented "volume:N" lines; the
	// deeper-indented ones belong to peers.
	cmd := fmt.Sprintf(`set -e
vols=$(drbdsetup status %[1]s --verbose | grep -oE '^  volume:[0-9]+' | cut -d: -f2)
[ -n "$vols" ]
for v in $vols; do drbdadm new-current-uuid --clear-bitmap %[1]s/$v; done`, resource)
	start := time.Now()
	err := rm.execAllSuccess(ctx, []string{address},
		"echo "+base64Std(cmd)+" | base64 -d | sudo /bin/bash", "skip initial sync")
	log.Debug("skipInitialSync: new-current-uuid --clear-bitmap on every local volume", zap.String("resource", resource),
		zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

func base64Std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
