package controller

import (
	"context"
	"fmt"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/backup"
	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
)

// ==================== BACKUP OPERATIONS ====================
//
// The RPC surface for off-cluster backup shipping. Two rules shape it:
// a target's secret is write-only (it enters through AddBackupTarget and is
// never rendered back), and a backup is only reported as restorable in the
// "completed" state.

func (s *Server) AddBackupTarget(ctx context.Context, req *haifypb.AddBackupTargetRequest) (*haifypb.AddBackupTargetResponse, error) {
	log := s.serverLog()
	// Only safe fields: the secret is logged as present or not, never its value.
	log.Debug("AddBackupTarget: saving target", zap.String("target", req.Name), zap.String("kind", req.Kind),
		zap.String("bucket", req.Bucket), zap.String("host", req.Host), zap.String("share", req.Share),
		zap.Bool("has_user", req.User != ""), zap.Bool("has_secret", req.Secret != ""),
		zap.String("lock_mode", req.LockMode), zap.Uint32("lock_days", req.LockDays))
	kind, err := backup.ParseKind(req.Kind)
	if err != nil {
		log.Debug("AddBackupTarget: bad kind", zap.String("kind", req.Kind), zap.Error(err))
		return &haifypb.AddBackupTargetResponse{Success: false, Message: err.Error()}, nil
	}
	mode, err := backup.ParseLockMode(req.LockMode)
	if err != nil {
		log.Debug("AddBackupTarget: bad lock mode", zap.String("lock_mode", req.LockMode), zap.Error(err))
		return &haifypb.AddBackupTargetResponse{Success: false, Message: err.Error()}, nil
	}
	spec := backup.TargetSpec{
		Name: req.Name, Kind: kind, Prefix: req.Prefix,
		Bucket: req.Bucket, Endpoint: req.Endpoint, Region: req.Region,
		Host: req.Host, Share: req.Share,
		User: req.User, Secret: req.Secret, SecretIsObscured: req.SecretObscured,
		LockMode: mode, LockDays: int(req.LockDays), FullEveryDays: int(req.FullEveryDays),
	}
	if err := s.ctrl.backups.AddTarget(ctx, spec); err != nil {
		log.Debug("AddBackupTarget: failed", zap.String("target", req.Name), zap.Error(err))
		return &haifypb.AddBackupTargetResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("AddBackupTarget: saved", zap.String("target", spec.Name), zap.String("kind", string(kind)))
	return &haifypb.AddBackupTargetResponse{
		Success: true,
		Message: fmt.Sprintf("Backup target %q saved (%s)", spec.Name, spec.Describe()),
	}, nil
}

func (s *Server) ListBackupTargets(ctx context.Context, req *haifypb.ListBackupTargetsRequest) (*haifypb.ListBackupTargetsResponse, error) {
	log := s.serverLog()
	log.Debug("ListBackupTargets: listing")
	targets, err := s.ctrl.backups.ListTargets(ctx)
	if err != nil {
		log.Debug("ListBackupTargets: failed", zap.Error(err))
		return &haifypb.ListBackupTargetsResponse{Success: false, Message: err.Error()}, nil
	}
	out := make([]*haifypb.BackupTargetInfo, 0, len(targets))
	for _, t := range targets {
		log.Debug("ListBackupTargets: target", zap.String("target", t.Name), zap.String("kind", string(t.Kind)))
		out = append(out, &haifypb.BackupTargetInfo{
			Name: t.Name, Kind: string(t.Kind), Prefix: t.Prefix,
			Bucket: t.Bucket, Endpoint: t.Endpoint, Region: t.Region,
			Host: t.Host, Share: t.Share, User: t.User,
			Description: t.Describe(),
			LockMode:    string(t.LockMode), LockDays: uint32(t.LockDays), FullEveryDays: uint32(t.FullEveryDays),
		})
	}
	log.Debug("ListBackupTargets: listed", zap.Int("count", len(out)))
	return &haifypb.ListBackupTargetsResponse{
		Success: true, Message: "Backup targets listed successfully", Targets: out,
	}, nil
}

func (s *Server) DeleteBackupTarget(ctx context.Context, req *haifypb.DeleteBackupTargetRequest) (*haifypb.DeleteBackupTargetResponse, error) {
	log := s.serverLog()
	log.Debug("DeleteBackupTarget: deleting", zap.String("target", req.Name), zap.Bool("force", req.Force))
	if err := s.ctrl.backups.DeleteTarget(ctx, req.Name, req.Force); err != nil {
		log.Debug("DeleteBackupTarget: failed", zap.String("target", req.Name), zap.Error(err))
		return &haifypb.DeleteBackupTargetResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("DeleteBackupTarget: deleted", zap.String("target", req.Name))
	return &haifypb.DeleteBackupTargetResponse{
		Success: true, Message: fmt.Sprintf("Backup target %q deleted", req.Name),
	}, nil
}

func (s *Server) CreateBackup(ctx context.Context, req *haifypb.CreateBackupRequest) (*haifypb.CreateBackupResponse, error) {
	log := s.serverLog()
	log.Debug("CreateBackup: backing up", zap.String("resource", req.Resource), zap.String("target", req.Target),
		zap.String("node", req.Node), zap.Bool("full", req.Full))
	rec, err := s.ctrl.backups.CreateBackup(ctx, req.Resource, req.Target, req.Node, req.Full)
	if err != nil {
		log.Debug("CreateBackup: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.CreateBackupResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("CreateBackup: completed", zap.String("backup", rec.ID), zap.String("kind", rec.Kind),
		zap.Uint64("total_bytes", rec.TotalBytes))
	return &haifypb.CreateBackupResponse{
		Success: true,
		Message: backupSummary(rec),
		Backup:  backupToProto(rec),
	}, nil
}

func (s *Server) ListBackups(ctx context.Context, req *haifypb.ListBackupsRequest) (*haifypb.ListBackupsResponse, error) {
	log := s.serverLog()
	log.Debug("ListBackups: listing", zap.String("resource", req.Resource), zap.String("target", req.Target))
	backups, err := s.ctrl.backups.ListBackups(ctx, req.Resource, req.Target)
	if err != nil {
		log.Debug("ListBackups: failed", zap.Error(err))
		return &haifypb.ListBackupsResponse{Success: false, Message: err.Error()}, nil
	}
	out := make([]*haifypb.BackupInfo, 0, len(backups))
	for _, b := range backups {
		log.Debug("ListBackups: backup", zap.String("backup", b.ID), zap.String("resource", b.Resource),
			zap.String("state", b.State))
		out = append(out, backupToProto(b))
	}
	log.Debug("ListBackups: listed", zap.Int("count", len(out)))
	return &haifypb.ListBackupsResponse{
		Success: true, Message: "Backups listed successfully", Backups: out,
	}, nil
}

func (s *Server) RestoreBackup(ctx context.Context, req *haifypb.RestoreBackupRequest) (*haifypb.RestoreBackupResponse, error) {
	log := s.serverLog()
	log.Debug("RestoreBackup: restoring", zap.String("backup", req.Id), zap.String("resource", req.Resource), zap.String("node", req.Node))
	rec, err := s.ctrl.backups.RestoreBackup(ctx, req.Id, req.Resource, req.Node)
	if err != nil {
		log.Debug("RestoreBackup: failed", zap.String("backup", req.Id), zap.Error(err))
		return &haifypb.RestoreBackupResponse{Success: false, Message: err.Error()}, nil
	}
	target := req.Resource
	if target == "" {
		log.Debug("RestoreBackup: no resource given, restored into the original", zap.String("resource", rec.Resource))
		target = rec.Resource
	}
	log.Debug("RestoreBackup: restored", zap.String("backup", rec.ID), zap.String("resource", target),
		zap.Uint64("total_bytes", rec.TotalBytes))
	return &haifypb.RestoreBackupResponse{
		Success: true,
		Message: fmt.Sprintf("Backup %s restored into %q (%s)", rec.ID, target, formatBytes(rec.TotalBytes)),
		Backup:  backupToProto(rec),
	}, nil
}

func (s *Server) DeleteBackup(ctx context.Context, req *haifypb.DeleteBackupRequest) (*haifypb.DeleteBackupResponse, error) {
	log := s.serverLog()
	log.Debug("DeleteBackup: deleting", zap.String("backup", req.Id), zap.String("node", req.Node), zap.Bool("force", req.Force))
	if err := s.ctrl.backups.DeleteBackup(ctx, req.Id, req.Node, req.Force); err != nil {
		log.Debug("DeleteBackup: failed", zap.String("backup", req.Id), zap.Error(err))
		return &haifypb.DeleteBackupResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("DeleteBackup: deleted", zap.String("backup", req.Id))
	return &haifypb.DeleteBackupResponse{
		Success: true, Message: fmt.Sprintf("Backup %s deleted", req.Id),
	}, nil
}

// backupToProto renders a backup record for the API.
func backupToProto(b *database.Backup) *haifypb.BackupInfo {
	if b == nil {
		zap.L().Debug("backupToProto: no backup record")
		return nil
	}
	info := &haifypb.BackupInfo{
		Id: b.ID, Resource: b.Resource, Target: b.Target, Node: b.Node,
		Backend: b.Backend, State: b.State, Error: b.Error, Prefix: b.Prefix,
		TotalBytes: b.TotalBytes, Kind: b.Kind, Parent: b.Parent, Schedule: b.Schedule,
	}
	if info.Kind == "" {
		info.Kind = database.BackupKindFull
	}
	if !b.StartedAt.IsZero() {
		info.StartedAt = b.StartedAt.UTC().Format(time.RFC3339)
	}
	if !b.FinishedAt.IsZero() {
		info.FinishedAt = b.FinishedAt.UTC().Format(time.RFC3339)
	}
	info.LockMode = b.LockMode
	if !b.RetainUntil.IsZero() {
		info.RetainUntil = b.RetainUntil.UTC().Format(time.RFC3339)
	}
	if !b.ReadAt.IsZero() {
		info.ReadAt = b.ReadAt.UTC().Format(time.RFC3339)
	}
	for _, v := range b.Volumes {
		info.Volumes = append(info.Volumes, &haifypb.BackupVolumeInfo{
			VolumeId: v.VolumeID, Object: v.Object, Bytes: v.Bytes,
			Pool: v.Pool, BackingVolume: v.BackingVolume,
			Ranges: v.Ranges, ChangedBytes: v.ChangedBytes,
		})
	}
	zap.L().Debug("backupToProto: rendered backup", zap.String("backup", b.ID), zap.String("kind", info.Kind),
		zap.Int("volumes", len(info.Volumes)))
	return info
}

// backupSummary says what a finished backup holds.
func backupSummary(rec *database.Backup) string {
	if rec.Kind != database.BackupKindIncremental {
		zap.L().Debug("backupSummary: full backup", zap.String("backup", rec.ID))
		return fmt.Sprintf("Backup %s of %q completed: full, %s", rec.ID, rec.Resource, formatBytes(rec.TotalBytes))
	}
	var changed uint64
	for _, v := range rec.Volumes {
		changed += v.ChangedBytes
	}
	zap.L().Debug("backupSummary: incremental backup", zap.String("backup", rec.ID), zap.String("parent", rec.Parent),
		zap.Uint64("changed_bytes", changed))
	return fmt.Sprintf("Backup %s of %q completed: incremental on %s, %s changed of %s",
		rec.ID, rec.Resource, rec.Parent, formatBytes(changed), formatBytes(rec.TotalBytes))
}
