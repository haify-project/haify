package controller

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Restoring a snapshot in place rolls one node's backing volume back in time.
// It used to be exactly that and nothing more — `lvconvert --merge` on the node
// holding the snapshot — which on a replicated volume is the one thing that
// must not happen:
//
//   - The other replicas keep the newer data and DRBD is never told. Both sides
//     go on reporting UpToDate while holding different contents, and a read
//     returns whichever replica serves it.
//   - With the volume in use the merge does not even happen then: LVM defers it
//     to the origin's next activation, so the rollback lands later, on a reboot,
//     with nobody watching.
//
// A restore therefore takes the resource down, merges while nothing holds the
// volume, and makes every other replica of that volume resync from the node it
// was restored on. The peers' metadata for the volume is wiped for that: left
// in place, the peers' newer generation would win the reconnect and resync the
// restored node back to the data the restore was meant to undo.

// RestoreSnapshot merges snapshotName back into volume ("<vg>/<lv>") on node.
func (sm *SnapshotManager) RestoreSnapshot(ctx context.Context, volume, snapshotName, node string) error {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("RestoreSnapshot: restoring", zap.String("volume", volume), zap.String("snapshot", snapshotName), zap.String("node", node))
	address := sm.controller.ResolveHost(node)
	vg, lv := parseVolumePath(volume)
	if vg == "" || lv == "" {
		log.Debug("RestoreSnapshot: volume not pool/volume", zap.String("volume", volume))
		return fmt.Errorf("volume must be <pool>/<volume>, got %q", volume)
	}
	snapshotPath := fmt.Sprintf("/dev/%s/%s", vg, snapshotName)
	backing := fmt.Sprintf("/dev/%s/%s", vg, lv)

	sm.controller.logger.Info("Restoring snapshot",
		zap.String("volume", volume), zap.String("snapshot", snapshotName), zap.String("node", node))

	// A merge consumes the snapshot it merges. A locked one is put back,
	// under the same name and with the same content, as soon as the merge is
	// done (see keepLockedSnapshot).
	merge, err := sm.lockPreservingMerge(ctx, address, vg, lv, snapshotName, snapshotPath, backing)
	if err != nil {
		log.Debug("RestoreSnapshot: merge refused", zap.String("snapshot", snapshotName), zap.Error(err))
		return err
	}

	resource, disk, err := sm.resourceBackedBy(ctx, address, backing)
	if err != nil {
		log.Debug("RestoreSnapshot: resource lookup failed", zap.String("backing", backing), zap.Error(err))
		return err
	}
	if resource == "" {
		// Not under DRBD: a plain LV has no replicas to keep in step.
		log.Debug("RestoreSnapshot: plain LV, merging in place", zap.String("backing", backing))
		return merge()
	}
	log.Debug("RestoreSnapshot: replicated volume", zap.String("resource", resource), zap.String("disk", disk),
		zap.Bool("encrypted", disk != backing))
	return sm.restoreReplicated(ctx, resource, address, node, disk, func() error {
		// An encrypted volume's LV is held open by its LUKS container even
		// with DRBD down, so LVM would only schedule the merge — the deferred
		// rollback this path exists to prevent. Close the container around
		// the merge and reopen it with the node's own key.
		if disk != backing {
			name := strings.TrimPrefix(disk, "/dev/mapper/")
			log.Debug("RestoreSnapshot: closing the LUKS container for the merge", zap.String("container", name))
			if err := sm.controller.resources.execAllSuccess(ctx, []string{address},
				"sudo cryptsetup close "+name, "close the LUKS container "+name); err != nil {
				log.Debug("RestoreSnapshot: LUKS close failed", zap.String("container", name), zap.Error(err))
				return err
			}
			defer func() {
				// The key file path is logged, never the key.
				_, oerr := sm.controller.deployment.Exec(ctx, []string{address}, fmt.Sprintf(
					"sudo cryptsetup open --type luks --key-file %s %s %s", luksKeyPath(name), backing, name))
				log.Debug("RestoreSnapshot: LUKS container reopened", zap.String("container", name), zap.Error(oerr))
			}()
		}
		return merge()
	})
}

// RestoreZFSSnapshot rolls dataset back to snapshotName on node, keeping the
// resource's other replicas in step the same way RestoreSnapshot does.
func (sm *SnapshotManager) RestoreZFSSnapshot(ctx context.Context, dataset, snapshotName, node string) error {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("RestoreZFSSnapshot: rolling back", zap.String("dataset", dataset), zap.String("snapshot", snapshotName), zap.String("node", node))
	address := sm.controller.ResolveHost(node)
	backing := "/dev/zvol/" + dataset
	if err := sm.assertRollbackKeepsLocks(ctx, address, dataset, snapshotName); err != nil {
		log.Debug("RestoreZFSSnapshot: rollback would destroy a locked snapshot", zap.Error(err))
		return err
	}
	rollback := func() error {
		start := time.Now()
		res, err := sm.controller.deployment.ZFSRollback(ctx, []string{address}, dataset, snapshotName)
		if err != nil {
			log.Debug("RestoreZFSSnapshot: zfs rollback errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
			return fmt.Errorf("failed to restore ZFS snapshot: %w", err)
		}
		if !res.AllSuccess() {
			log.Debug("RestoreZFSSnapshot: zfs rollback failed", zap.String("host", address), zap.Duration("took", time.Since(start)))
			return fmt.Errorf("failed to restore ZFS snapshot: %s", res.FailureDetails())
		}
		log.Debug("RestoreZFSSnapshot: rolled back", zap.String("dataset", dataset), zap.Duration("took", time.Since(start)))
		return nil
	}
	resource, disk, err := sm.resourceBackedBy(ctx, address, backing)
	if err != nil {
		log.Debug("RestoreZFSSnapshot: resource lookup failed", zap.String("backing", backing), zap.Error(err))
		return err
	}
	if resource == "" {
		log.Debug("RestoreZFSSnapshot: plain zvol, rolling back in place", zap.String("dataset", dataset))
		return rollback()
	}
	log.Debug("RestoreZFSSnapshot: replicated volume", zap.String("resource", resource), zap.String("disk", disk))
	return sm.restoreReplicated(ctx, resource, address, node, disk, rollback)
}

// RestoreLVMSnapshotByName restores a snapshot known only by its name within
// vg, asking the node which LV it is a snapshot of.
func (sm *SnapshotManager) RestoreLVMSnapshotByName(ctx context.Context, vg, snapshotName, node string) error {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("RestoreLVMSnapshotByName: looking up origin", zap.String("pool", vg), zap.String("snapshot", snapshotName), zap.String("node", node))
	address := sm.controller.ResolveHost(node)
	start := time.Now()
	res, err := sm.controller.deployment.Exec(ctx, []string{address},
		fmt.Sprintf("sudo lvs --noheadings -o origin %s/%s", vg, snapshotName))
	if err != nil {
		log.Debug("RestoreLVMSnapshotByName: lvs errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("look up the origin of %s/%s: %w", vg, snapshotName, err)
	}
	origin := ""
	for _, h := range res.Hosts {
		if !h.Success {
			log.Debug("RestoreLVMSnapshotByName: lvs failed", zap.String("host", address), zap.Duration("took", time.Since(start)))
			return fmt.Errorf("failed to find snapshot %s/%s on %s: %s", vg, snapshotName, node, strings.TrimSpace(h.Output))
		}
		origin = strings.TrimSpace(h.Output)
	}
	if origin == "" {
		log.Debug("RestoreLVMSnapshotByName: not a snapshot", zap.String("snapshot", snapshotName))
		return fmt.Errorf("failed to restore %s/%s: not a snapshot on %s", vg, snapshotName, node)
	}
	log.Debug("RestoreLVMSnapshotByName: origin found", zap.String("origin", origin), zap.Duration("took", time.Since(start)))
	return sm.RestoreSnapshot(ctx, vg+"/"+origin, snapshotName, node)
}

// resourceBackedBy names the DRBD resource whose config uses backing as a
// disk on this node, or "" when none does. An encrypted volume is configured
// by its LUKS container rather than the LV, so that path is looked for too;
// disk is whichever the config names.
func (sm *SnapshotManager) resourceBackedBy(ctx context.Context, address, backing string) (resource, disk string, err error) {
	log := ctrlLogOrGlobal(sm.controller)
	candidates := []string{backing}
	if vg, lv := parseVolumePath(strings.TrimPrefix(backing, "/dev/")); vg != "" && !strings.HasPrefix(backing, "/dev/zvol/") {
		candidates = append(candidates, luksMapperPath(vg, lv))
	}
	log.Debug("resourceBackedBy: looking up", zap.String("host", address), zap.Strings("candidates", candidates))
	for _, disk := range candidates {
		cmd := fmt.Sprintf("grep -lE '^[[:space:]]*disk[[:space:]]+%s;' /etc/drbd.d/*.res 2>/dev/null || true",
			regexp.QuoteMeta(disk))
		start := time.Now()
		res, err := sm.controller.deployment.Exec(ctx, []string{address}, cmd)
		if err != nil {
			log.Debug("resourceBackedBy: grep errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
			return "", "", fmt.Errorf("look up the resource using %s: %w", backing, err)
		}
		for _, h := range res.Hosts {
			if !h.Success {
				log.Debug("resourceBackedBy: grep failed", zap.String("host", address), zap.Duration("took", time.Since(start)))
				return "", "", fmt.Errorf("failed to look up the resource using %s: %s", backing, strings.TrimSpace(h.Output))
			}
			for _, f := range strings.Fields(h.Output) {
				resource := strings.TrimSuffix(filepath.Base(f), ".res")
				log.Debug("resourceBackedBy: found", zap.String("disk", disk), zap.String("resource", resource))
				return resource, disk, nil
			}
		}
	}
	log.Debug("resourceBackedBy: not under DRBD", zap.String("backing", backing))
	return "", "", nil
}

// restoreReplicated runs rollback with resource down on every replica, then
// resyncs the other replicas from address. disk is the device the resource's
// config names for the restored volume — the LV, a zvol, or a LUKS container.
func (sm *SnapshotManager) restoreReplicated(ctx context.Context, resource, address, node, disk string, rollback func() error) error {
	log := ctrlLogOrGlobal(sm.controller)
	log.Debug("restoreReplicated: restoring", zap.String("resource", resource), zap.String("node", node), zap.String("disk", disk))
	rm := sm.controller.resources
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		log.Debug("restoreReplicated: hosts not resolved", zap.String("resource", resource), zap.Error(err))
		return err
	}
	var peers []string
	for _, h := range hosts {
		if h != address {
			peers = append(peers, h)
		}
	}
	log.Debug("restoreReplicated: replicas", zap.Strings("hosts", hosts), zap.Strings("peers", peers))

	// Which volume of the resource this backing volume is.
	start := time.Now()
	cfg, err := sm.controller.deployment.Exec(ctx, []string{address}, "cat /etc/drbd.d/"+resource+".res")
	if err != nil || !cfg.AllSuccess() {
		log.Debug("restoreReplicated: config not read", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("read the config of %s: %v", resource, err)
	}
	volID := -1
	for _, h := range cfg.Hosts {
		for _, v := range parseResourceConfigVolumes(h.Output) {
			if v.DiskPath == disk {
				volID = v.VolumeID
			}
		}
	}
	if volID < 0 {
		log.Debug("restoreReplicated: disk is not a volume of the resource", zap.String("disk", disk))
		return fmt.Errorf("%s is not a volume of %s", disk, resource)
	}
	log.Debug("restoreReplicated: volume found", zap.Int("volume", volID))

	// Nothing may be using the resource: a restore under a mounted filesystem
	// is a filesystem changing underneath its own cache.
	start = time.Now()
	st, err := sm.controller.deployment.Exec(ctx, hosts, "sudo drbdsetup status "+resource)
	if err != nil {
		log.Debug("restoreReplicated: status errored", zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("read the state of %s: %w", resource, err)
	}
	for host, h := range st.Hosts {
		if strings.HasPrefix(strings.TrimSpace(h.Output), resource+" role:Primary") {
			name := sm.controller.NodeName(host)
			log.Debug("restoreReplicated: resource is Primary, refusing", zap.String("node", name))
			return fmt.Errorf("%s is Primary on %s: unmount it and demote it (haify resource secondary %s %s) before restoring",
				resource, name, resource, name)
		}
	}
	log.Debug("restoreReplicated: no Primary, taking it down", zap.Duration("took", time.Since(start)))

	down := "sudo drbdadm down " + resource
	if err := rm.execAllSuccess(ctx, hosts, down, "take "+resource+" down for the restore"); err != nil {
		log.Debug("restoreReplicated: down failed", zap.Error(err))
		return err
	}
	if err := rollback(); err != nil {
		// The volume is untouched; bring the resource back as it was.
		_, uerr := sm.controller.deployment.Exec(ctx, hosts, "sudo drbdadm up "+resource)
		log.Debug("restoreReplicated: rollback failed, resource brought back up", zap.Error(err), zap.NamedError("up_error", uerr))
		return err
	}
	log.Debug("restoreReplicated: rolled back, bringing the restored node up", zap.String("node", node))
	if err := rm.execAllSuccess(ctx, []string{address}, "sudo drbdadm up "+resource,
		"bring "+resource+" up on "+node); err != nil {
		log.Debug("restoreReplicated: up on the restored node failed", zap.Error(err))
		return err
	}
	// The peers lose their generation for this volume, so they cannot claim
	// to be newer, and sync it from the restored node.
	wipe := fmt.Sprintf("yes yes | sudo drbdadm create-md --force %s/%d && sudo drbdadm up %s", resource, volID, resource)
	if len(peers) > 0 {
		log.Debug("restoreReplicated: resetting the peers' metadata", zap.Strings("peers", peers), zap.Int("volume", volID))
		if err := rm.execAllSuccess(ctx, peers, wipe, "reset the other replicas of "+resource); err != nil {
			log.Debug("restoreReplicated: peer reset failed", zap.Error(err))
			return err
		}
	}

	sm.controller.logger.Info("Snapshot restored; the other replicas resync from the restored node",
		zap.String("resource", resource), zap.Int("volume", volID), zap.String("node", node))
	return nil
}

// mergeSnapshot merges a snapshot into its origin and makes sure it happened.
// LVM defers a merge into an active origin to its next activation, and says
// so rather than failing, so the snapshot's disappearance is what is checked;
// cycling the origin's activation starts a deferred merge.
func (sm *SnapshotManager) mergeSnapshot(ctx context.Context, address, snapshotPath, backing string) error {
	log := ctrlLogOrGlobal(sm.controller)
	snapLV := strings.TrimPrefix(snapshotPath, "/dev/")
	originLV := strings.TrimPrefix(backing, "/dev/")
	log.Debug("mergeSnapshot: merging", zap.String("host", address), zap.String("snapshot", snapLV), zap.String("origin", originLV))
	// A merge an earlier restore left pending (attr "S": merging snapshot)
	// is finished rather than refused.
	script := fmt.Sprintf(`set -e
if ! lvconvert --merge %[1]s; then
  lvs --noheadings -o lv_attr %[2]s | grep -q '^ *S' || exit 1
fi
if lvs %[2]s >/dev/null 2>&1; then
  lvchange -an %[3]s
  lvchange -ay %[3]s
fi
for i in $(seq 1 60); do
  lvs %[2]s >/dev/null 2>&1 || exit 0
  sleep 1
done
echo "the merge of %[2]s into %[3]s did not complete" >&2
exit 1`, snapshotPath, snapLV, originLV)
	cmd := "echo " + base64Std(script) + " | base64 -d | sudo /bin/bash"
	start := time.Now()
	res, err := sm.controller.deployment.Exec(ctx, []string{address}, cmd)
	if err != nil {
		log.Debug("mergeSnapshot: merge errored", zap.String("host", address), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to restore snapshot: %w", err)
	}
	if !res.AllSuccess() {
		log.Debug("mergeSnapshot: merge failed", zap.String("host", address), zap.Duration("took", time.Since(start)))
		return fmt.Errorf("failed to restore snapshot: %s", res.FailureDetails())
	}
	log.Debug("mergeSnapshot: merged", zap.String("snapshot", snapLV), zap.Duration("took", time.Since(start)))
	return nil
}
