package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/backup"
	"github.com/haify-project/haify/pkg/database"
)

// Backups on a target with S3 Object Lock (pkg/backup/lock.go).
//
// The lock is set when an object is written and cannot be extended by rclone
// afterwards, which shapes how chains work. An incremental is useless without
// every backup down to its full one, so no link may expire before a backup
// built on it. Every backup of a chain therefore carries the same date: chain
// start + full_every_days + lock_days. A backup may join the chain only while
// that date still covers it for lock_days; after that the next one is full and
// starts a new chain. Each backup is locked for at least lock_days, a chain's
// first backups for up to full_every_days longer.
//
// Locked objects cannot be deleted, so neither can the backup: delete and
// retention leave it alone until the date passes. And they are read as of the
// time they were written, so an attacker who could not delete them but could
// write a new version over them, or a delete marker on top, changes nothing a
// restore sees.

// readAtSlack is how long after a backup finished its objects are read as of.
// The finish time is the controller's clock and the versions are dated by the
// object store's, so the margin absorbs any skew between the two; an
// overwrite within it is the only one a restore would see.
const readAtSlack = time.Hour

// chainRetainUntil is the lock date of a new backup: its chain's, or a new
// chain's when it is full.
func chainRetainUntil(spec backup.TargetSpec, parent *database.Backup, started time.Time) time.Time {
	if parent != nil && !parent.RetainUntil.IsZero() {
		zap.L().Debug("chainRetainUntil: joining parent's chain date", zap.String("parent", parent.ID),
			zap.Time("retain_until", parent.RetainUntil))
		return parent.RetainUntil
	}
	until := started.AddDate(0, 0, spec.FullEvery()+spec.LockDays).UTC().Truncate(time.Second)
	zap.L().Debug("chainRetainUntil: new chain date", zap.Int("full_every_days", spec.FullEvery()),
		zap.Int("lock_days", spec.LockDays), zap.Time("retain_until", until))
	return until
}

// lockedChainRefusal says why a backup on a locked target cannot be an
// incremental on parent, or "" when it can.
func lockedChainRefusal(spec backup.TargetSpec, parent *database.Backup, started time.Time) string {
	log := zap.L()
	if !spec.Locked() {
		return ""
	}
	if parent.LockMode != string(spec.LockMode) || parent.RetainUntil.IsZero() {
		log.Debug("lockedChainRefusal: parent not locked like the target", zap.String("parent", parent.ID),
			zap.String("parent_lock_mode", parent.LockMode), zap.String("lock_mode", string(spec.LockMode)))
		return "the last backup is not locked the way the target now locks; starting a locked chain"
	}
	if started.AddDate(0, 0, spec.LockDays).After(parent.RetainUntil) {
		log.Debug("lockedChainRefusal: chain lock no longer covers a new backup", zap.String("parent", parent.ID),
			zap.Time("retain_until", parent.RetainUntil), zap.Int("lock_days", spec.LockDays))
		return fmt.Sprintf("the chain is locked until %s, which no longer covers %d more days; starting a new chain",
			parent.RetainUntil.UTC().Format(time.RFC3339), spec.LockDays)
	}
	log.Debug("lockedChainRefusal: may join locked chain", zap.String("parent", parent.ID))
	return ""
}

// verifyLocks proves every object of a locked backup really is locked until
// its date. Some S3-compatible servers accept the lock headers and store the
// object unlocked; that backup would look immutable and be deletable by
// anyone with haify's keys, so it is failed instead.
func verifyLocks(ctx context.Context, sess backup.Session, rec *database.Backup, objects []string) error {
	log := zap.L()
	if rec.LockMode == "" {
		log.Debug("verifyLocks: backup not locked, nothing to verify", zap.String("backup", rec.ID))
		return nil
	}
	log.Debug("verifyLocks: verifying object locks", zap.String("backup", rec.ID), zap.String("lock_mode", rec.LockMode),
		zap.Time("retain_until", rec.RetainUntil), zap.Int("count", len(objects)))
	for _, obj := range objects {
		start := time.Now()
		mode, until, err := sess.LockOf(ctx, obj)
		log.Debug("verifyLocks: read object lock", zap.String("object", obj), zap.String("mode", string(mode)),
			zap.Time("until", until), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return fmt.Errorf("could not confirm %s is locked: %w", obj, err)
		}
		if string(mode) != rec.LockMode || until.Before(rec.RetainUntil.Add(-time.Second)) {
			log.Debug("verifyLocks: object not locked as asked", zap.String("object", obj))
			got := "no lock"
			if mode != backup.LockNone {
				got = fmt.Sprintf("%s until %s", mode, until.UTC().Format(time.RFC3339))
			}
			return fmt.Errorf("the target stored %s with %s instead of %s until %s: the bucket must have Object Lock enabled, "+
				"and the server must honour the lock headers; this backup is not immutable and was failed",
				obj, got, rec.LockMode, rec.RetainUntil.UTC().Format(time.RFC3339))
		}
	}
	return nil
}

// readAtFor is the time a backup's objects are read as of: the --as-of time it
// was imported at, shortly after it finished when it is locked, and the
// current versions otherwise.
func readAtFor(b *database.Backup) time.Time {
	switch {
	case !b.ReadAt.IsZero():
		zap.L().Debug("readAtFor: reading as of import time", zap.String("backup", b.ID), zap.Time("read_at", b.ReadAt))
		return b.ReadAt
	case b.LockMode != "" && !b.FinishedAt.IsZero():
		zap.L().Debug("readAtFor: locked backup, reading as of finish", zap.String("backup", b.ID),
			zap.Time("read_at", b.FinishedAt.Add(readAtSlack)))
		return b.FinishedAt.Add(readAtSlack)
	default:
		zap.L().Debug("readAtFor: reading current versions", zap.String("backup", b.ID))
		return time.Time{}
	}
}

// lockedNow reports whether a backup's objects are still locked.
func lockedNow(b *database.Backup, now time.Time) bool {
	return b.LockMode != "" && b.RetainUntil.After(now)
}

// assertDeletable refuses to delete a backup whose objects are locked. With
// force the record may still go — for a target that no longer exists — but
// the objects are not touched: removing them would only hide them behind a
// delete marker until the lock expires, and report them gone.
func assertDeletable(b *database.Backup, force bool, now time.Time) (touchObjects bool, err error) {
	if !lockedNow(b, now) {
		zap.L().Debug("assertDeletable: backup not locked", zap.String("backup", b.ID))
		return true, nil
	}
	if !force {
		zap.L().Debug("assertDeletable: backup locked, refusing", zap.String("backup", b.ID),
			zap.Time("retain_until", b.RetainUntil))
		return false, fmt.Errorf("backup %q is locked (%s) until %s and cannot be deleted before then, by haify or anyone using its credentials; "+
			"--force drops only the record and leaves the locked objects on the target", b.ID, b.LockMode,
			b.RetainUntil.UTC().Format(time.RFC3339))
	}
	zap.L().Debug("assertDeletable: backup locked, forced: dropping record only", zap.String("backup", b.ID))
	return false, nil
}
