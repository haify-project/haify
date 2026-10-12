package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// The rclone session's half of Object Lock (lock.go): locking what is pushed,
// reading it back, and reading the target as it was.

// SetLock implements Session.
func (s *rcloneSession) SetLock(mode LockMode, until time.Time) {
	zap.L().Debug("rcloneSession.SetLock: lock for pushes", zap.String("host", s.host),
		zap.String("target", s.target.Name), zap.String("lock_mode", string(mode)), zap.Time("until", until))
	s.lockMode, s.lockUntil = mode, until
}

// SetReadAt implements Session.
func (s *rcloneSession) SetReadAt(t time.Time) {
	zap.L().Debug("rcloneSession.SetReadAt: reads pinned", zap.String("host", s.host),
		zap.String("target", s.target.Name), zap.Time("read_at", t))
	s.readAt = t
}

// lockFlags are the rclone flags that lock an upload. Both are needed: rclone
// applies nothing when only one is set.
func (s *rcloneSession) lockFlags() string {
	if s.lockMode == LockNone || s.lockUntil.IsZero() || s.target.Kind != KindS3 {
		zap.L().Debug("rcloneSession.lockFlags: upload not locked", zap.String("target", s.target.Name),
			zap.String("lock_mode", string(s.lockMode)), zap.Bool("until_set", !s.lockUntil.IsZero()),
			zap.String("kind", string(s.target.Kind)))
		return ""
	}
	zap.L().Debug("rcloneSession.lockFlags: upload locked", zap.String("target", s.target.Name),
		zap.String("lock_mode", string(s.lockMode)), zap.Time("until", s.lockUntil))
	return fmt.Sprintf(" --s3-object-lock-mode %s --s3-object-lock-retain-until-date %s",
		s.lockMode.s3(), s.lockUntil.UTC().Format(time.RFC3339))
}

// readAtFlag pins a read to the object versions current at readAt. rclone
// refuses writes while it is set, so it is only ever added to reads.
func (s *rcloneSession) readAtFlag() string {
	if s.readAt.IsZero() || s.target.Kind != KindS3 {
		return ""
	}
	zap.L().Debug("rcloneSession.readAtFlag: reading versions at", zap.String("target", s.target.Name),
		zap.Time("read_at", s.readAt))
	return " --s3-version-at " + s.readAt.UTC().Format(time.RFC3339)
}

// LockOf implements Session: the lock the target reports for objectPath, read
// from the object's metadata. This is the proof that a lock was applied —
// some S3-compatible servers accept the lock headers and ignore them.
func (s *rcloneSession) LockOf(ctx context.Context, objectPath string) (LockMode, time.Time, error) {
	cmd := s.rcloneEnv() + " lsjson --metadata " + shellQuote(s.remotePath(objectPath))
	start := time.Now()
	res, err := s.dep.Exec(ctx, []string{s.host}, cmd)
	zap.L().Debug("rcloneSession.LockOf: metadata read returned", zap.String("host", s.host),
		zap.String("target", s.target.Name), zap.String("object", objectPath),
		zap.Bool("success", err == nil && res.AllSuccess()), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return LockNone, time.Time{}, fmt.Errorf("backup: read the lock of %s: %w", objectPath, err)
	}
	if !res.AllSuccess() {
		return LockNone, time.Time{}, fmt.Errorf("backup: read the lock of %s failed: %s", objectPath, res.FailureDetails())
	}
	return parseRcloneLock(res.Output(s.host))
}

// parseRcloneLock reads the Object Lock metadata out of `rclone lsjson
// --metadata` for one object.
func parseRcloneLock(out string) (LockMode, time.Time, error) {
	log := zap.L()
	start := strings.Index(out, "[")
	if start < 0 {
		log.Debug("parseRcloneLock: no json array in output", zap.Int("output_bytes", len(out)))
		return LockNone, time.Time{}, fmt.Errorf("backup: could not read object metadata: %q", strings.TrimSpace(out))
	}
	var entries []struct {
		Metadata map[string]string `json:"Metadata"`
	}
	if err := json.Unmarshal([]byte(out[start:]), &entries); err != nil {
		log.Debug("parseRcloneLock: json unreadable", zap.Error(err))
		return LockNone, time.Time{}, fmt.Errorf("backup: could not read object metadata: %w", err)
	}
	if len(entries) != 1 {
		log.Debug("parseRcloneLock: expected one object", zap.Int("count", len(entries)))
		return LockNone, time.Time{}, fmt.Errorf("backup: expected one object, the target listed %d", len(entries))
	}
	md := entries[0].Metadata
	mode, err := ParseLockMode(md["object-lock-mode"])
	if err != nil {
		log.Debug("parseRcloneLock: unknown lock mode", zap.String("lock_mode", md["object-lock-mode"]), zap.Error(err))
		return LockNone, time.Time{}, err
	}
	if mode == LockNone {
		log.Debug("parseRcloneLock: object not locked")
		return LockNone, time.Time{}, nil
	}
	until, err := time.Parse(time.RFC3339, md["object-lock-retain-until-date"])
	log.Debug("parseRcloneLock: object locked", zap.String("lock_mode", string(mode)),
		zap.String("until", md["object-lock-retain-until-date"]), zap.Error(err))
	if err != nil {
		return LockNone, time.Time{}, fmt.Errorf("backup: unreadable retain-until date %q: %w", md["object-lock-retain-until-date"], err)
	}
	return mode, until, nil
}
