package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// Choosing the replica a backup reads.

// pickBackupNode chooses which replica to read.
//
// An explicitly named node is honoured as given — an operator asking for the DR
// node knows what they are asking for. Otherwise a Secondary is preferred so
// the snapshot's copy-on-write cost lands away from the node serving the
// workload, and the DR node is skipped: under protocol A it may be behind, and
// a backup whose point in time is "somewhere near then" is not one.
func (bm *BackupManager) pickBackupNode(info *ResourceInfo, requested string) (string, error) {
	log := bm.controller.traceLogger()
	log.Debug("BackupManager.pickBackupNode: choosing replica to read", zap.String("resource", info.Name),
		zap.String("requested", requested))
	if requested != "" {
		for _, n := range info.Nodes {
			if n == requested {
				log.Debug("BackupManager.pickBackupNode: requested node holds a replica", zap.String("node", requested))
				return requested, nil
			}
		}
		log.Debug("BackupManager.pickBackupNode: requested node holds no replica", zap.String("node", requested),
			zap.Strings("nodes", info.Nodes))
		return "", fmt.Errorf("node %q holds no replica of %q", requested, info.Name)
	}
	if c := backupCandidates(info); len(c) > 0 {
		log.Debug("BackupManager.pickBackupNode: chose first candidate", zap.String("node", c[0]), zap.Strings("candidates", c))
		return c[0], nil
	}
	log.Debug("BackupManager.pickBackupNode: no UpToDate replica", zap.String("resource", info.Name))
	return "", fmt.Errorf(
		"no node holds an UpToDate replica of %q; back up from a healthy node or name one explicitly", info.Name)
}

// backupCandidates lists the replicas a backup may read, in order of
// preference: UpToDate Secondaries, then the Primary, never the DR replica.
func backupCandidates(info *ResourceInfo) []string {
	log := zap.L()
	var secondaries, primaries []string
	for _, n := range info.Nodes {
		if info.WANMode && n == info.DRNode {
			log.Debug("backupCandidates: skipping DR replica", zap.String("resource", info.Name), zap.String("node", n))
			continue
		}
		st := info.NodeStates[n]
		if st == nil || st.DiskState != "UpToDate" {
			log.Debug("backupCandidates: skipping replica not UpToDate", zap.String("resource", info.Name),
				zap.String("node", n), zap.Bool("has_state", st != nil))
			continue
		}
		log.Debug("backupCandidates: candidate", zap.String("resource", info.Name), zap.String("node", n),
			zap.String("role", st.Role))
		if st.Role == "Primary" {
			primaries = append(primaries, n)
		} else {
			secondaries = append(secondaries, n)
		}
	}
	return append(secondaries, primaries...)
}

// nodeWithSnapshotRoom returns the first candidate whose volume groups can hold
// the backup's snapshots, or "" to leave the choice to pickBackupNode.
//
// A thin snapshot takes its space from the pool as blocks change, but a thick
// one reserves its copy-on-write area up front from the volume group, and a
// nearly full group refuses it outright. Picking such a node fails the backup
// when a replica next to it had room.
func (bm *BackupManager) nodeWithSnapshotRoom(ctx context.Context, info *ResourceInfo) string {
	log := bm.controller.traceLogger()
	candidates := backupCandidates(info)
	log.Debug("BackupManager.nodeWithSnapshotRoom: checking candidates", zap.String("resource", info.Name),
		zap.Strings("candidates", candidates))
	for _, n := range candidates {
		if bm.snapshotRoom(ctx, bm.controller.ResolveHost(n), info) {
			log.Debug("BackupManager.nodeWithSnapshotRoom: node has room", zap.String("node", n))
			return n
		}
		bm.controller.logger.Info("Backup: skipping a replica without room for a thick snapshot",
			zap.String("resource", info.Name), zap.String("node", n))
	}
	log.Debug("BackupManager.nodeWithSnapshotRoom: no candidate has room, leaving choice to pickBackupNode",
		zap.String("resource", info.Name))
	return ""
}

// snapshotRoom reports whether host's volume groups can take one snapshot of
// every volume of info. A query that fails counts as room: the snapshot then
// fails with LVM's own message, which is more use than a guess.
func (bm *BackupManager) snapshotRoom(ctx context.Context, host string, info *ResourceInfo) bool {
	log := bm.controller.traceLogger()
	dep := bm.controller.deployment
	need := map[string]uint64{}
	for _, v := range info.Volumes {
		start := time.Now()
		thin, err := dep.LVIsThin(ctx, host, v.Pool, v.BackingVolume)
		log.Debug("BackupManager.snapshotRoom: checked volume", zap.String("host", host), zap.String("pool", v.Pool),
			zap.String("volume", v.BackingVolume), zap.Bool("thin", thin), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil || thin {
			continue
		}
		need[v.Pool] += cowBytes(v.SizeGB)
	}
	for vg, n := range need {
		start := time.Now()
		free, err := dep.VGFreeBytes(ctx, host, vg)
		log.Debug("BackupManager.snapshotRoom: volume group free space", zap.String("host", host), zap.String("pool", vg),
			zap.Uint64("free_bytes", free), zap.Uint64("need_bytes", n), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err == nil && free < n {
			log.Debug("BackupManager.snapshotRoom: no room for thick snapshots", zap.String("host", host), zap.String("pool", vg))
			return false
		}
	}
	log.Debug("BackupManager.snapshotRoom: room for snapshots", zap.String("host", host), zap.Int("thick_pools", len(need)))
	return true
}

// cowBytes is cowSize in bytes.
func cowBytes(originGB uint64) uint64 {
	mb := originGB * 1024 / 5
	if mb < 256 {
		mb = 256
	}
	return mb << 20
}
