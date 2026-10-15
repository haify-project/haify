package controller

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Snapshot groups: one-node snapshots of several volumes, possibly of several
// resources, taken at the same instant.
//
// An application that keeps its data on more than one volume — a database
// with its write-ahead log on a volume of its own, say — is only recoverable
// from snapshots that agree on a point in time. Snapshots taken one after the
// other do not: the log can be newer than the tables. So the group suspends
// I/O on every member's resource first (the same suspend, watchdog and resume
// as a resource snapshot, resource_snapshot.go), takes every snapshot while
// nothing is written anywhere, and resumes. What the snapshots then hold is
// what a power cut at that instant would have left: crash-consistent across
// all of them.
//
// Each member is an ordinary one-node snapshot, exactly what CreateSnapshot
// takes, so everything that works on one (listing, deleting, populating a new
// volume from it) works on a member. The CSI driver's VolumeGroupSnapshot is
// built on this.

// snapshotGroupMargin is how much of the watchdog's minute a group may not
// use. The first resource's watchdog was armed before anything else happened;
// a group that has run so close to its end may have had I/O resumed under it.
const snapshotGroupMargin = 5 * time.Second

// snapshotGroupMu serialises groups. Two groups sharing a resource would
// otherwise resume it for each other: the first to finish resumes I/O in the
// middle of the second's snapshots.
var snapshotGroupMu sync.Mutex

// The names a member brings end up on a shell command line on the node.
var (
	snapshotGroupNameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]{0,127}$`)
	snapshotGroupVolumeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]*/[A-Za-z0-9][A-Za-z0-9_.+-]*$`)
)

// SnapshotGroupMember is one volume of a snapshot group.
type SnapshotGroupMember struct {
	// Resource is the DRBD resource whose I/O is suspended.
	Resource string
	// Volume ("<pool>/<backing>"), Name and Node are as CreateSnapshot takes
	// them.
	Volume string
	Name   string
	Node   string
}

// validateSnapshotGroup checks every member before anything is suspended, and
// returns the resources in the order they are first named.
func (rm *ResourceManager) validateSnapshotGroup(ctx context.Context, members []SnapshotGroupMember) ([]string, error) {
	log := rm.resourcesLog()
	log.Debug("validateSnapshotGroup: start", zap.Int("members", len(members)))
	if len(members) == 0 {
		log.Debug("validateSnapshotGroup: no members")
		return nil, fmt.Errorf("a snapshot group needs at least one member")
	}
	if rm.deployment == nil || rm.controller.db == nil || rm.controller.snapshots == nil {
		log.Debug("validateSnapshotGroup: no deployment client, database or snapshot manager")
		return nil, fmt.Errorf("deployment client or database not available")
	}
	seen := map[string]bool{}
	var resources []string
	backings := map[string]map[string]bool{} // resource -> "<pool>/<backing>" of its volumes
	for _, m := range members {
		if !resourceNameRe.MatchString(m.Resource) || !snapshotGroupVolumeRe.MatchString(m.Volume) ||
			!snapshotGroupNameRe.MatchString(m.Name) || m.Node == "" {
			log.Debug("validateSnapshotGroup: malformed member", zap.String("resource", m.Resource), zap.String("volume", m.Volume),
				zap.String("snapshot", m.Name), zap.String("node", m.Node))
			return nil, fmt.Errorf("member %q/%q: resource, volume (<pool>/<backing>), snapshot name and node are required "+
				"(names: letters, digits, _ . + -)", m.Resource, m.Volume)
		}
		key := m.Volume + "@" + m.Node
		if seen[key] {
			log.Debug("validateSnapshotGroup: volume named twice", zap.String("volume", m.Volume), zap.String("node", m.Node))
			return nil, fmt.Errorf("volume %s on %s is named twice", m.Volume, m.Node)
		}
		seen[key] = true
		if _, known := backings[m.Resource]; !known {
			start := time.Now()
			dbRes, err := rm.controller.db.GetResource(ctx, m.Resource)
			log.Debug("validateSnapshotGroup: looked up resource", zap.String("resource", m.Resource),
				zap.Bool("found", err == nil && dbRes != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
			if err != nil || dbRes == nil {
				return nil, fmt.Errorf("resource %q not found", m.Resource)
			}
			vols, err := rm.controller.db.ListVolumes(ctx, m.Resource)
			log.Debug("validateSnapshotGroup: listed volumes", zap.String("resource", m.Resource), zap.Int("volumes", len(vols)),
				zap.Error(err))
			if err != nil {
				return nil, fmt.Errorf("list volumes of %s: %w", m.Resource, err)
			}
			set := map[string]bool{}
			for _, v := range vols {
				set[v.Pool+"/"+v.VolumeName] = true
			}
			backings[m.Resource] = set
			resources = append(resources, m.Resource)
		}
		// Suspending one resource and snapshotting another's volume would
		// take that volume with its writes still flowing.
		if !backings[m.Resource][m.Volume] {
			log.Debug("validateSnapshotGroup: volume is not the resource's", zap.String("resource", m.Resource),
				zap.String("volume", m.Volume))
			return nil, fmt.Errorf("%s is not a volume of resource %s", m.Volume, m.Resource)
		}
	}
	log.Debug("validateSnapshotGroup: valid", zap.Strings("resources", resources))
	return resources, nil
}

// suspendedResource is one resource a group has suspended.
type suspendedResource struct {
	resource string
	hosts    []string
	watchdog string
}

// CreateSnapshotGroup takes every member's snapshot with I/O suspended on all
// of the members' resources. It is all or nothing: when one snapshot fails,
// or the group takes so long that a watchdog may have resumed I/O, the
// snapshots already taken are deleted.
func (rm *ResourceManager) CreateSnapshotGroup(ctx context.Context, members []SnapshotGroupMember) error {
	log := rm.resourcesLog()
	log.Debug("CreateSnapshotGroup: start", zap.Int("members", len(members)))
	resources, err := rm.validateSnapshotGroup(ctx, members)
	if err != nil {
		return err
	}

	snapshotGroupMu.Lock()
	defer snapshotGroupMu.Unlock()

	var suspended []suspendedResource
	// Resume in the reverse order, on contexts of their own (resumeAfterSnapshot):
	// a cancelled request must not leave anything suspended until the watchdog.
	defer func() {
		for i := len(suspended) - 1; i >= 0; i-- {
			s := suspended[i]
			rm.resumeAfterSnapshot(s.resource, s.hosts, s.watchdog)
		}
	}()

	firstArmed := time.Now()
	for _, res := range resources {
		hosts, err := rm.resourceHosts(ctx, res)
		if err != nil {
			log.Debug("CreateSnapshotGroup: no hosts", zap.String("resource", res), zap.Error(err))
			return fmt.Errorf("hosts of %s: %w", res, err)
		}
		start := time.Now()
		unit, onHosts, err := rm.suspendForSnapshot(ctx, res, hosts)
		log.Debug("CreateSnapshotGroup: suspended resource", zap.String("resource", res), zap.Strings("hosts", onHosts),
			zap.String("unit", unit), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return fmt.Errorf("suspend I/O of %s: %w", res, err)
		}
		suspended = append(suspended, suspendedResource{resource: res, hosts: onHosts, watchdog: unit})
	}

	var made []SnapshotGroupMember
	for _, m := range members {
		start := time.Now()
		err := rm.controller.snapshots.CreateSnapshot(ctx, m.Volume, m.Name, m.Node)
		log.Debug("CreateSnapshotGroup: member snapshot", zap.String("resource", m.Resource), zap.String("volume", m.Volume),
			zap.String("snapshot", m.Name), zap.String("node", m.Node), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			rm.undoSnapshotGroup(made)
			return fmt.Errorf("snapshot %s of %s on %s: %w", m.Name, m.Volume, m.Node, err)
		}
		made = append(made, m)
	}

	if elapsed := time.Since(firstArmed); elapsed >= resourceSnapshotWatchdog-snapshotGroupMargin {
		log.Debug("CreateSnapshotGroup: too slow, a watchdog may have resumed I/O", zap.Duration("elapsed", elapsed),
			zap.Duration("watchdog", resourceSnapshotWatchdog))
		rm.undoSnapshotGroup(made)
		return fmt.Errorf("the group took %s, close to the %s after which I/O resumes by itself, so its snapshots may not "+
			"be from one instant; they were deleted. Snapshot fewer volumes at once", elapsed.Round(time.Second), resourceSnapshotWatchdog)
	}
	rm.controller.logger.Info("Snapshot group taken", zap.Int("members", len(members)), zap.Strings("resources", resources),
		zap.Duration("took", time.Since(firstArmed)))
	return nil
}

// undoSnapshotGroup deletes the members already taken. It runs on its own
// context: the request's may be what failed.
func (rm *ResourceManager) undoSnapshotGroup(made []SnapshotGroupMember) {
	log := rm.resourcesLog()
	log.Debug("undoSnapshotGroup: deleting partial group", zap.Int("made", len(made)))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, m := range made {
		start := time.Now()
		err := rm.controller.snapshots.DeleteSnapshot(ctx, m.Volume, m.Name, m.Node)
		log.Debug("undoSnapshotGroup: deleted member", zap.String("volume", m.Volume), zap.String("snapshot", m.Name),
			zap.String("node", m.Node), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			rm.controller.logger.Warn("A partial snapshot group could not be removed; delete it by hand",
				zap.String("volume", m.Volume), zap.String("snapshot", m.Name), zap.String("node", m.Node), zap.Error(err))
		}
	}
}
