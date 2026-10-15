package csi

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// snapshotBackupMarker is the fourth part of the ID of a snapshot that is also
// shipped off-cluster as a Haify backup: "<resource>/<node>/<snapshot>/backup".
//
// The marker is all the ID needs: the backup's id follows from the other parts
// (backupIDFor), and is the same in every cluster that imports the target's
// backups, while the target's name is not — so the target is deliberately
// left out. Three-part IDs, from before backups and from snapshot classes
// without backupTarget, keep their meaning: a local snapshot only.
const snapshotBackupMarker = "backup"

// snapshotRef is a parsed CSI snapshot ID.
type snapshotRef struct {
	resource string // the source volume
	node     string // the node whose replica holds the local snapshot
	name     string // the local snapshot's name
	// backedUp says the snapshot is also a Haify backup on a target, which a
	// restore falls back to when the local snapshot is gone.
	backedUp bool
}

// id renders the ref as the CSI snapshot ID it was parsed from.
func (r snapshotRef) id() string {
	if r.backedUp {
		return makeSnapshotID(r.resource, r.node, r.name) + snapshotIDSep + snapshotBackupMarker
	}
	return makeSnapshotID(r.resource, r.node, r.name)
}

// backupID is the id of the Haify backup of a backed-up snapshot. The
// controller names a backup of an existing snapshot "<resource>_<snapshot>".
func (r snapshotRef) backupID() string {
	return backupIDFor(r.resource, r.name)
}

// backupIDFor is the controller's id for the backup of snapshot of resource.
func backupIDFor(resource, snapshot string) string {
	return resource + "_" + snapshot
}

// parseSnapshotRef parses both ID formats.
func parseSnapshotRef(id string) (snapshotRef, error) {
	parts := strings.Split(id, snapshotIDSep)
	backedUp := len(parts) == 4 && parts[3] == snapshotBackupMarker
	if backedUp {
		parts = parts[:3]
	}
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		zap.L().Debug("parseSnapshotRef: malformed", zap.String("snapshot_id", id), zap.Int("parts", len(parts)))
		return snapshotRef{}, fmt.Errorf("malformed snapshot id %q (want <resource>/<node>/<snapshot>[/%s])", id, snapshotBackupMarker)
	}
	ref := snapshotRef{resource: parts[0], node: parts[1], name: parts[2], backedUp: backedUp}
	zap.L().Debug("parseSnapshotRef: parsed", zap.String("snapshot_id", id), zap.String("resource", ref.resource),
		zap.String("node", ref.node), zap.String("snapshot", ref.name), zap.Bool("backed_up", ref.backedUp))
	return ref, nil
}

// refFromBackupID recovers the snapshot a CSI-made backup was taken of, from
// its id and the resource and node it records. ok is false for a backup the
// driver did not make: one taken by hand or by a schedule.
func refFromBackupID(id, resource, node string) (snapshotRef, bool) {
	name, found := strings.CutPrefix(id, resource+"_")
	if !found || !strings.HasPrefix(name, snapshotNamePrefix) || node == "" || strings.Contains(name, snapshotIDSep) {
		return snapshotRef{}, false
	}
	return snapshotRef{resource: resource, node: node, name: name, backedUp: true}, true
}
