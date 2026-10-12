package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
)

// Locked scheduled snapshots.
//
// Ransomware that encrypts a volume rewrites every block, so a thin pool's
// snapshots suddenly hold the whole volume's worth of old data and the pool
// fills. Retention then had the worst possible answer: relieveThinPool deleted
// the OLDEST scheduled snapshots first — the clean ones from before the attack
// — and kept the newest two, taken after it. Anyone with an haify token could
// also simply delete the snapshots, the schedule or the resource.
//
// A schedule's lock_days locks the snapshots it takes: until a snapshot is
// lock_days old, nothing haify does deletes it — not retention, not a full
// pool, not a person through the API — and the schedule and resource cannot
// be deleted, nor the lock shortened, while any of its snapshots is locked.
// The lock is measured from the time in the snapshot's own name, which haify
// wrote when it took it.
//
// What it is not: a lock against root on a storage node, who can lvremove
// anything. That needs the backups on a target with S3 Object Lock
// (backup_lock.go). And it trades one risk for another: a pool that fills
// with locked snapshots is not relieved, its replica's writes fail and DRBD
// drops it. relieveThinPool raises that as a critical event so the pool is
// grown before then.

// maxSnapshotLockDays bounds lock_days: a typo of 3650 for 30 would lock a
// pool for a decade with no way back short of root on the nodes.
const maxSnapshotLockDays = 365

// snapshotLockWindow is how long resource's schedule locks its snapshots.
func (c *Controller) snapshotLockWindow(ctx context.Context, resource string) time.Duration {
	log := ctrlLogOrGlobal(c)
	if c.db == nil {
		log.Debug("snapshotLockWindow: no database, no lock", zap.String("resource", resource))
		return 0
	}
	s, err := c.db.GetSnapshotSchedule(ctx, resource)
	if err != nil || s == nil || s.LockDays <= 0 {
		log.Debug("snapshotLockWindow: no locking schedule", zap.String("resource", resource), zap.Error(err))
		return 0
	}
	log.Debug("snapshotLockWindow: schedule locks", zap.String("resource", resource), zap.Int("lock_days", s.LockDays))
	return time.Duration(s.LockDays) * 24 * time.Hour
}

// snapshotLockedUntil says until when a snapshot named name, taken by a
// schedule that locks for window, is locked; zero when it is not locked.
func snapshotLockedUntil(name string, window time.Duration, now time.Time) time.Time {
	if window <= 0 {
		return time.Time{}
	}
	_, ts, ok := parseSnapName(name)
	if !ok {
		return time.Time{}
	}
	if until := ts.Add(window); until.After(now) {
		return until
	}
	return time.Time{}
}

// resourceOfBackingVolume names the resource a backing volume belongs to.
func (c *Controller) resourceOfBackingVolume(ctx context.Context, backing string) string {
	log := ctrlLogOrGlobal(c)
	if c.db == nil {
		log.Debug("resourceOfBackingVolume: no database", zap.String("volume", backing))
		return ""
	}
	resources, err := c.db.ListResources(ctx)
	if err != nil {
		log.Debug("resourceOfBackingVolume: resource list failed", zap.String("volume", backing), zap.Error(err))
		return ""
	}
	for _, r := range resources {
		vols, err := c.db.ListVolumes(ctx, r.Name)
		if err != nil {
			log.Debug("resourceOfBackingVolume: volume list failed", zap.String("resource", r.Name), zap.Error(err))
			continue
		}
		for _, v := range vols {
			if v.VolumeName == backing {
				log.Debug("resourceOfBackingVolume: found", zap.String("volume", backing), zap.String("resource", r.Name))
				return r.Name
			}
		}
	}
	log.Debug("resourceOfBackingVolume: no resource uses it", zap.String("volume", backing))
	return ""
}

// assertSnapshotUnlocked refuses to delete or consume a locked scheduled
// snapshot. Snapshots haify did not schedule carry no lock.
func (c *Controller) assertSnapshotUnlocked(ctx context.Context, name string) error {
	log := ctrlLogOrGlobal(c)
	backing, _, ok := parseSnapName(name)
	if !ok {
		log.Debug("assertSnapshotUnlocked: not a scheduled snapshot, no lock", zap.String("snapshot", name))
		return nil
	}
	resource := c.resourceOfBackingVolume(ctx, backing)
	if resource == "" {
		log.Debug("assertSnapshotUnlocked: no resource, no lock", zap.String("snapshot", name))
		return nil
	}
	if until := snapshotLockedUntil(name, c.snapshotLockWindow(ctx, resource), lockNow()); !until.IsZero() {
		log.Debug("assertSnapshotUnlocked: locked by the schedule", zap.String("snapshot", name), zap.String("resource", resource),
			zap.Time("until", until))
		return fmt.Errorf("snapshot %s is locked by the snapshot schedule of %s until %s and cannot be deleted before then",
			name, resource, until.UTC().Format(time.RFC3339))
	}
	if until, reason := c.resourceFrozenUntil(ctx, resource); !until.IsZero() {
		log.Debug("assertSnapshotUnlocked: schedule frozen", zap.String("snapshot", name), zap.String("resource", resource),
			zap.Time("until", until))
		return fmt.Errorf("the snapshot schedule of %s is frozen until %s (%s): none of its snapshots can be deleted before then",
			resource, until.UTC().Format(time.RFC3339), reason)
	}
	log.Debug("assertSnapshotUnlocked: unlocked", zap.String("snapshot", name), zap.String("resource", resource))
	return nil
}

// resourceSnapshotsLockedUntil is the time until which resource may still hold
// locked snapshots: its schedule's last run plus the lock. Zero when none can
// be locked any more.
func (c *Controller) resourceSnapshotsLockedUntil(ctx context.Context, resource string) time.Time {
	log := ctrlLogOrGlobal(c)
	if c.db == nil {
		log.Debug("resourceSnapshotsLockedUntil: no database", zap.String("resource", resource))
		return time.Time{}
	}
	s, err := c.db.GetSnapshotSchedule(ctx, resource)
	if err != nil || s == nil {
		log.Debug("resourceSnapshotsLockedUntil: no schedule", zap.String("resource", resource), zap.Error(err))
		return time.Time{}
	}
	until := scheduleLockedUntil(s, lockNow())
	log.Debug("resourceSnapshotsLockedUntil: checked", zap.String("resource", resource), zap.Time("until", until))
	return until
}

// scheduleLockedUntil is when the newest snapshot s can have taken stops being
// locked — by its lock, or by a freeze — or zero when that has passed.
func scheduleLockedUntil(s *database.SnapshotSchedule, now time.Time) time.Time {
	until := scheduleFrozenUntil(s, now)
	if s.LockDays <= 0 || s.LastRun.IsZero() {
		return until
	}
	if l := s.LastRun.Add(time.Duration(s.LockDays) * 24 * time.Hour); l.After(now) && l.After(until) {
		return l
	}
	return until
}

// assertResourceUnlocked refuses an operation that would take a resource's
// locked snapshots with it: deleting the resource or one of its volumes.
func (c *Controller) assertResourceUnlocked(ctx context.Context, resource, what string) error {
	if until := c.resourceSnapshotsLockedUntil(ctx, resource); !until.IsZero() {
		ctrlLogOrGlobal(c).Debug("assertResourceUnlocked: refused, snapshots locked", zap.String("resource", resource),
			zap.String("what", what), zap.Time("until", until))
		return fmt.Errorf("%s would delete snapshots of %s that its snapshot schedule locks until %s; that is refused until then",
			what, resource, until.UTC().Format(time.RFC3339))
	}
	ctrlLogOrGlobal(c).Debug("assertResourceUnlocked: no locked snapshots", zap.String("resource", resource), zap.String("what", what))
	return nil
}

// checkScheduleLockChange validates a schedule's new lock against the one it
// replaces: it may be raised at any time, but not lowered or removed while
// snapshots are locked — that would unlock them.
func checkScheduleLockChange(old *database.SnapshotSchedule, lockDays int, now time.Time) error {
	log := zap.L()
	if lockDays < 0 || lockDays > maxSnapshotLockDays {
		log.Debug("checkScheduleLockChange: lock days out of range", zap.Int("lock_days", lockDays))
		return fmt.Errorf("lock days must be between 0 and %d", maxSnapshotLockDays)
	}
	if old == nil || lockDays >= old.LockDays {
		log.Debug("checkScheduleLockChange: new or raised lock, allowed", zap.Int("lock_days", lockDays))
		return nil
	}
	if until := scheduleLockedUntil(old, now); !until.IsZero() {
		log.Debug("checkScheduleLockChange: lowering refused while locked", zap.String("resource", old.Resource),
			zap.Int("old_lock_days", old.LockDays), zap.Int("lock_days", lockDays), zap.Time("until", until))
		return fmt.Errorf("the schedule of %s locks its snapshots for %d days and the newest stay locked until %s; "+
			"the lock can be raised but not lowered before then", old.Resource, old.LockDays, until.UTC().Format(time.RFC3339))
	}
	log.Debug("checkScheduleLockChange: lowering allowed, nothing locked", zap.String("resource", old.Resource),
		zap.Int("lock_days", lockDays))
	return nil
}

// unlockedSnaps drops the snapshots that are still locked.
func unlockedSnaps(snaps []scheduledSnap, window time.Duration, now time.Time) []scheduledSnap {
	if window <= 0 {
		return snaps
	}
	out := snaps[:0:0]
	for _, s := range snaps {
		if snapshotLockedUntil(s.Name, window, now).IsZero() {
			out = append(out, s)
		}
	}
	zap.L().Debug("unlockedSnaps: filtered", zap.Int("snapshots", len(snaps)), zap.Int("unlocked", len(out)),
		zap.Duration("window", window))
	return out
}

// lockPreservingMerge returns the merge to run for restoring snapshotName
// into backing. For a snapshot that is not locked that is the plain merge.
// A locked thin snapshot is merged and then taken again from the restored
// volume under the same name: the volume is exactly the snapshot's content at
// that moment (the resource is down), and the name carries the time the lock
// is measured from, so the lock is unchanged. A locked thick snapshot is
// refused: re-taking it would need a copy-on-write reservation sized for a
// volume about to diverge, and failing that would lose it.
func (sm *SnapshotManager) lockPreservingMerge(ctx context.Context, address, vg, lv, snapshotName, snapshotPath, backing string) (func() error, error) {
	log := ctrlLogOrGlobal(sm.controller)
	plain := func() error { return sm.mergeSnapshot(ctx, address, snapshotPath, backing) }
	if err := sm.controller.assertSnapshotUnlocked(ctx, snapshotName); err == nil {
		log.Debug("lockPreservingMerge: unlocked, plain merge", zap.String("snapshot", snapshotName))
		return plain, nil
	}
	start := time.Now()
	thin, err := sm.controller.deployment.LVIsThin(ctx, address, vg, snapshotName)
	if err != nil || !thin {
		log.Debug("lockPreservingMerge: locked thick snapshot, refused", zap.String("snapshot", snapshotName),
			zap.Bool("thin", thin), zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, fmt.Errorf("snapshot %s is locked, and restoring merges it away; only a locked thin snapshot can be kept "+
			"through a restore", snapshotName)
	}
	log.Debug("lockPreservingMerge: locked thin snapshot, merge then retake", zap.String("snapshot", snapshotName))
	return func() error {
		if err := plain(); err != nil {
			return err
		}
		start := time.Now()
		res, err := sm.controller.deployment.LVCreateThinSnapshot(ctx, []string{address}, vg, lv, snapshotName)
		if err == nil && res != nil && !res.AllSuccess() {
			err = fmt.Errorf("%s", res.FailureDetails())
		}
		if err != nil {
			log.Debug("lockPreservingMerge: retake failed", zap.String("snapshot", snapshotName),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			return fmt.Errorf("restored, but the locked snapshot %s could not be taken again: %w", snapshotName, err)
		}
		log.Debug("lockPreservingMerge: locked snapshot retaken", zap.String("snapshot", snapshotName), zap.Duration("took", time.Since(start)))
		return nil
	}, nil
}

// assertRollbackKeepsLocks refuses a ZFS rollback to snapshotName when it
// would destroy a locked snapshot: `zfs rollback -r` destroys every snapshot
// taken after the one rolled back to.
func (sm *SnapshotManager) assertRollbackKeepsLocks(ctx context.Context, address, dataset, snapshotName string) error {
	log := ctrlLogOrGlobal(sm.controller)
	backing := dataset[strings.LastIndex(dataset, "/")+1:]
	resource := sm.controller.resourceOfBackingVolume(ctx, backing)
	window := sm.controller.snapshotLockWindow(ctx, resource)
	if resource == "" || window <= 0 {
		log.Debug("assertRollbackKeepsLocks: no lock to keep", zap.String("dataset", dataset), zap.String("resource", resource))
		return nil
	}
	start := time.Now()
	res, err := sm.controller.deployment.Exec(ctx, []string{address},
		fmt.Sprintf("sudo zfs list -t snapshot -H -o name -s creation -d 1 %s", dataset))
	if err != nil || res == nil || !res.AllSuccess() {
		log.Debug("assertRollbackKeepsLocks: snapshot list failed, refusing", zap.String("host", address),
			zap.String("dataset", dataset), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("could not list the snapshots a rollback of %s would destroy; refusing in case one is locked", dataset)
	}
	after, now := false, lockNow()
	var later []string
	for _, line := range execLines(res, address) {
		_, name, ok := strings.Cut(strings.TrimSpace(line), "@")
		if !ok {
			continue
		}
		if after {
			if until := snapshotLockedUntil(name, window, now); !until.IsZero() {
				log.Debug("assertRollbackKeepsLocks: a later snapshot is locked, refusing", zap.String("snapshot", name),
					zap.Time("until", until))
				return fmt.Errorf("rolling %s back to %s destroys every later snapshot, and %s is locked until %s",
					dataset, snapshotName, name, until.UTC().Format(time.RFC3339))
			}
			later = append(later, name)
		}
		if name == snapshotName {
			after = true
		}
	}
	log.Debug("assertRollbackKeepsLocks: later snapshots unlocked, releasing holds", zap.String("dataset", dataset),
		zap.Strings("later", later))
	// Their locks have passed, but a hold placed under one would still make
	// the rollback fail.
	for _, name := range later {
		sm.controller.releaseZFSLockHold(ctx, address, dataset+"@"+name)
	}
	return nil
}
