package csi

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"go.uber.org/zap"
)

// groupSnapshotIDPrefix starts every group snapshot ID. A group ID has two
// parts and a member's (makeSnapshotID) has three, so neither parses as the
// other.
const groupSnapshotIDPrefix = "haifygroup"

// makeGroupSnapshotID encodes a group's backend name. The group has no record
// of its own anywhere: it is the set of member snapshots whose names derive
// from it (memberSnapshotName), and the CO hands their IDs back on Get and
// Delete.
func makeGroupSnapshotID(groupSnap string) string {
	return groupSnapshotIDPrefix + snapshotIDSep + groupSnap
}

// parseGroupSnapshotID splits an ID produced by makeGroupSnapshotID.
func parseGroupSnapshotID(id string) (string, error) {
	prefix, groupSnap, ok := strings.Cut(id, snapshotIDSep)
	if !ok || prefix != groupSnapshotIDPrefix || !strings.HasPrefix(groupSnap, snapshotNamePrefix) ||
		strings.Contains(groupSnap, snapshotIDSep) {
		zap.L().Debug("parseGroupSnapshotID: malformed", zap.String("group_snapshot_id", id))
		return "", fmt.Errorf("malformed group snapshot id %q (want %s/<group>)", id, groupSnapshotIDPrefix)
	}
	return groupSnap, nil
}

// memberSnapshotName names a group's snapshot of one volume. One-node
// snapshots of LVM volumes share the pool's namespace (lvcreate -n <name>), so
// two members in one pool need different names; the suffix is a hash of the
// volume rather than the volume's name to keep the name, and the snapshot ID
// that carries it, short. It still starts with snapshotNamePrefix, so a member
// is listed, deleted and restored like any other snapshot of this driver.
func memberSnapshotName(groupSnap, resource string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(resource))
	return fmt.Sprintf("%s_%08x", groupSnap, h.Sum32())
}

// groupMember is a member snapshot ID split into its parts, checked to belong
// to the group.
type groupMember struct {
	id, resource, node, name string
}

// membersOfGroup checks that every ID is one of groupSnap's members: a member
// ID handed back under the wrong group is the mismatch the spec asks a plugin
// to report rather than act on.
func membersOfGroup(groupSnap string, ids []string) ([]groupMember, error) {
	log := zap.L()
	out := make([]groupMember, 0, len(ids))
	for _, id := range ids {
		resource, node, name, err := parseSnapshotID(id)
		if err != nil {
			log.Debug("membersOfGroup: malformed member id", zap.String("snapshot_id", id), zap.Error(err))
			return nil, err
		}
		if name != memberSnapshotName(groupSnap, resource) {
			log.Debug("membersOfGroup: not a member", zap.String("snapshot_id", id), zap.String("group", groupSnap))
			return nil, fmt.Errorf("snapshot %q is not a member of group snapshot %q", id, makeGroupSnapshotID(groupSnap))
		}
		out = append(out, groupMember{id: id, resource: resource, node: node, name: name})
	}
	log.Debug("membersOfGroup: all belong", zap.String("group", groupSnap), zap.Int("members", len(out)))
	return out, nil
}

// findGroupMembers walks every CSI volume for snapshots of groupSnap. It is
// the slow path, for a CO that did not name the members and for telling a
// group name already used for other volumes from a retry.
func (s *controllerServer) findGroupMembers(ctx context.Context, groupSnap string) ([]*csi.Snapshot, error) {
	start := time.Now()
	all, err := s.backend.ListResources(ctx)
	s.log.Debug("controllerServer.findGroupMembers: listed resources", zap.String("group", groupSnap), zap.Int("count", len(all)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	var out []*csi.Snapshot
	for _, r := range all {
		if !isCSIVolume(r) {
			continue
		}
		want := snapshotIDSep + memberSnapshotName(groupSnap, r.GetName())
		for _, snap := range s.snapshotsOf(ctx, r) {
			if strings.HasSuffix(snap.GetSnapshotId(), want) {
				s.log.Debug("controllerServer.findGroupMembers: found member", zap.String("group", groupSnap),
					zap.String("snapshot_id", snap.GetSnapshotId()))
				out = append(out, snap)
			}
		}
	}
	s.log.Debug("controllerServer.findGroupMembers: done", zap.String("group", groupSnap), zap.Int("members", len(out)),
		zap.Duration("took", time.Since(start)))
	return out, nil
}
