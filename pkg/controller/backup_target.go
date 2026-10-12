package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/backup"
	"github.com/haify-project/haify/pkg/database"
)

// Backup targets: the repositories backups are shipped to.
//
// A target's secret is write-only. It enters through AddTarget, is stored in
// the controller's 0600 database, and is never returned by any read path — a
// credential that can be read back out of the API leaks through every client
// that ever renders it.

// ==================== TARGETS ====================

// AddTarget validates and stores a backup repository. An existing target with
// the same name is replaced, which is how credentials are rotated.
func (bm *BackupManager) AddTarget(ctx context.Context, spec backup.TargetSpec) error {
	log := bm.controller.traceLogger()
	// Only non-secret fields of the spec are traced; the secret by presence.
	log.Debug("BackupManager.AddTarget: start", zap.String("target", spec.Name), zap.String("kind", string(spec.Kind)),
		zap.String("bucket", spec.Bucket), zap.String("endpoint", spec.Endpoint), zap.String("host", spec.Host),
		zap.String("share", spec.Share), zap.Bool("has_user", spec.User != ""), zap.Bool("has_secret", spec.Secret != ""),
		zap.Bool("secret_obscured", spec.SecretIsObscured), zap.String("lock_mode", string(spec.LockMode)),
		zap.Int("lock_days", spec.LockDays))
	if bm.controller.db == nil {
		log.Debug("BackupManager.AddTarget: no database")
		return fmt.Errorf("database not available")
	}
	if err := spec.Validate(); err != nil {
		log.Debug("BackupManager.AddTarget: spec invalid", zap.String("target", spec.Name), zap.Error(err))
		return err
	}
	// An operator-supplied obscured secret that does not decode is a paste
	// error; catching it here beats an authentication failure at backup time
	// with nothing pointing at the cause.
	if spec.SecretIsObscured && spec.Secret != "" {
		if _, err := backup.Reveal(spec.Secret); err != nil {
			log.Debug("BackupManager.AddTarget: obscured secret does not decode", zap.String("target", spec.Name))
			return err
		}
	}
	t := &database.BackupTarget{
		Name: spec.Name, Kind: string(spec.Kind), Prefix: spec.Prefix,
		Bucket: spec.Bucket, Endpoint: spec.Endpoint, Region: spec.Region,
		Host: spec.Host, Share: spec.Share,
		User: spec.User, Secret: spec.Secret, SecretObscured: spec.SecretIsObscured,
		LockMode: string(spec.LockMode), LockDays: spec.LockDays, FullEveryDays: spec.FullEveryDays,
	}
	start := time.Now()
	if err := bm.controller.db.SaveBackupTarget(ctx, t); err != nil {
		log.Debug("BackupManager.AddTarget: db save failed", zap.String("target", spec.Name),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("save backup target: %w", err)
	}
	log.Debug("BackupManager.AddTarget: db saved", zap.String("target", spec.Name), zap.Duration("took", time.Since(start)))
	bm.controller.logger.Info("Backup target saved",
		zap.String("target", spec.Name), zap.String("kind", string(spec.Kind)))
	return nil
}

// ListTargets returns every target. Secrets are not included: the caller is the
// API surface, and a credential that is readable back out of the API is a
// credential that leaks through every client that ever displays it.
func (bm *BackupManager) ListTargets(ctx context.Context) ([]backup.TargetSpec, error) {
	log := bm.controller.traceLogger()
	if bm.controller.db == nil {
		log.Debug("BackupManager.ListTargets: no database")
		return nil, fmt.Errorf("database not available")
	}
	start := time.Now()
	rows, err := bm.controller.db.ListBackupTargets(ctx)
	log.Debug("BackupManager.ListTargets: db list", zap.Int("count", len(rows)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	out := make([]backup.TargetSpec, 0, len(rows))
	for _, r := range rows {
		spec := targetSpecFromDB(r)
		spec.Secret = ""
		out = append(out, spec)
	}
	return out, nil
}

// DeleteTarget removes a target. It refuses while backups still reference it,
// because losing the target definition means losing the ability to restore or
// even to delete those objects. force overrides, for a target whose storage is
// already gone.
func (bm *BackupManager) DeleteTarget(ctx context.Context, name string, force bool) error {
	log := bm.controller.traceLogger()
	log.Debug("BackupManager.DeleteTarget: start", zap.String("target", name), zap.Bool("force", force))
	if bm.controller.db == nil {
		log.Debug("BackupManager.DeleteTarget: no database")
		return fmt.Errorf("database not available")
	}
	if !force {
		start := time.Now()
		backups, err := bm.controller.db.ListBackups(ctx, "", name)
		log.Debug("BackupManager.DeleteTarget: db list backups on target", zap.String("target", name),
			zap.Int("count", len(backups)), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return err
		}
		if len(backups) > 0 {
			return fmt.Errorf(
				"backup target %q still holds %d backup(s); delete them first or pass --force to drop the target definition and orphan them",
				name, len(backups))
		}
	} else {
		log.Debug("BackupManager.DeleteTarget: forced, not checking for backups", zap.String("target", name))
	}
	start := time.Now()
	if err := bm.controller.db.DeleteBackupTarget(ctx, name); err != nil {
		log.Debug("BackupManager.DeleteTarget: db delete failed", zap.String("target", name),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("delete backup target: %w", err)
	}
	log.Debug("BackupManager.DeleteTarget: db deleted", zap.String("target", name), zap.Duration("took", time.Since(start)))
	bm.controller.logger.Info("Backup target deleted", zap.String("target", name), zap.Bool("force", force))
	return nil
}

func targetSpecFromDB(r *database.BackupTarget) backup.TargetSpec {
	return backup.TargetSpec{
		Name: r.Name, Kind: backup.Kind(r.Kind), Prefix: r.Prefix,
		Bucket: r.Bucket, Endpoint: r.Endpoint, Region: r.Region,
		Host: r.Host, Share: r.Share,
		User: r.User, Secret: r.Secret, SecretIsObscured: r.SecretObscured,
		LockMode: backup.LockMode(r.LockMode), LockDays: r.LockDays, FullEveryDays: r.FullEveryDays,
	}
}
