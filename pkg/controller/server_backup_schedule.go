package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
)

// RPCs for scheduled backups and for importing backups from a target.

func (s *Server) CreateBackupSchedule(ctx context.Context, req *haifypb.CreateBackupScheduleRequest) (*haifypb.CreateBackupScheduleResponse, error) {
	log := s.serverLog()
	log.Debug("CreateBackupSchedule: saving schedule", zap.String("resource", req.Resource), zap.String("target", req.Target),
		zap.String("cron", req.Cron), zap.Bool("enabled", req.Enabled))
	sc, err := s.ctrl.schedules.CreateBackupSchedule(ctx, req.Resource, req.Target, req.Cron, gfsFromProto(req.Keep), req.Enabled)
	if err != nil {
		log.Debug("CreateBackupSchedule: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.CreateBackupScheduleResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("CreateBackupSchedule: saved", zap.String("schedule", sc.Name))
	return &haifypb.CreateBackupScheduleResponse{
		Success:  true,
		Message:  fmt.Sprintf("Backup schedule %s saved", sc.Name),
		Schedule: backupScheduleToProto(sc, time.Now()),
	}, nil
}

func (s *Server) ListBackupSchedules(ctx context.Context, _ *haifypb.ListBackupSchedulesRequest) (*haifypb.ListBackupSchedulesResponse, error) {
	log := s.serverLog()
	log.Debug("ListBackupSchedules: listing")
	schedules, err := s.ctrl.schedules.ListBackupSchedules(ctx)
	if err != nil {
		log.Debug("ListBackupSchedules: failed", zap.Error(err))
		return &haifypb.ListBackupSchedulesResponse{Success: false, Message: err.Error()}, nil
	}
	now := time.Now()
	out := make([]*haifypb.BackupScheduleInfo, 0, len(schedules))
	for _, sc := range schedules {
		log.Debug("ListBackupSchedules: schedule", zap.String("schedule", sc.Name), zap.String("resource", sc.Resource),
			zap.String("target", sc.Target), zap.Bool("enabled", sc.Enabled))
		out = append(out, backupScheduleToProto(sc, now))
	}
	log.Debug("ListBackupSchedules: listed", zap.Int("count", len(out)))
	return &haifypb.ListBackupSchedulesResponse{Success: true, Message: "Backup schedules listed", Schedules: out}, nil
}

func (s *Server) DeleteBackupSchedule(ctx context.Context, req *haifypb.DeleteBackupScheduleRequest) (*haifypb.DeleteBackupScheduleResponse, error) {
	log := s.serverLog()
	log.Debug("DeleteBackupSchedule: deleting", zap.String("schedule", req.Name))
	if err := s.ctrl.schedules.DeleteBackupSchedule(ctx, req.Name); err != nil {
		log.Debug("DeleteBackupSchedule: failed", zap.String("schedule", req.Name), zap.Error(err))
		return &haifypb.DeleteBackupScheduleResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("DeleteBackupSchedule: deleted", zap.String("schedule", req.Name))
	return &haifypb.DeleteBackupScheduleResponse{Success: true, Message: fmt.Sprintf("Backup schedule %s deleted", req.Name)}, nil
}

func (s *Server) RunBackupSchedule(ctx context.Context, req *haifypb.RunBackupScheduleRequest) (*haifypb.RunBackupScheduleResponse, error) {
	log := s.serverLog()
	log.Debug("RunBackupSchedule: running now", zap.String("schedule", req.Name))
	sc, err := s.ctrl.schedules.RunBackupSchedule(ctx, req.Name)
	if err != nil {
		log.Debug("RunBackupSchedule: failed to run", zap.String("schedule", req.Name), zap.Error(err))
		return &haifypb.RunBackupScheduleResponse{Success: false, Message: err.Error()}, nil
	}
	if sc.LastError != "" {
		log.Debug("RunBackupSchedule: backup failed", zap.String("schedule", req.Name), zap.String("last_error", sc.LastError))
		return &haifypb.RunBackupScheduleResponse{
			Success: false, Message: "backup failed: " + sc.LastError, Schedule: backupScheduleToProto(sc, time.Now()),
		}, nil
	}
	log.Debug("RunBackupSchedule: backup completed", zap.String("schedule", req.Name), zap.String("backup", sc.LastBackup))
	return &haifypb.RunBackupScheduleResponse{
		Success: true, Message: fmt.Sprintf("Backup %s completed", sc.LastBackup), Schedule: backupScheduleToProto(sc, time.Now()),
	}, nil
}

func (s *Server) ImportBackups(ctx context.Context, req *haifypb.ImportBackupsRequest) (*haifypb.ImportBackupsResponse, error) {
	log := s.serverLog()
	log.Debug("ImportBackups: importing", zap.String("target", req.Target), zap.String("node", req.Node), zap.String("as_of", req.AsOf))
	var asOf time.Time
	if req.AsOf != "" {
		t, perr := time.Parse(time.RFC3339, req.AsOf)
		if perr != nil {
			log.Debug("ImportBackups: as_of is not RFC3339", zap.String("as_of", req.AsOf), zap.Error(perr))
			return &haifypb.ImportBackupsResponse{Success: false, Message: fmt.Sprintf("as_of %q is not an RFC3339 time", req.AsOf)}, nil
		}
		asOf = t
	}
	res, err := s.ctrl.backups.ImportBackups(ctx, req.Target, req.Node, asOf)
	if err != nil {
		log.Debug("ImportBackups: failed", zap.String("target", req.Target), zap.Error(err))
		return &haifypb.ImportBackupsResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("ImportBackups: imported", zap.String("target", req.Target), zap.Int("imported", len(res.Imported)),
		zap.Int("skipped", len(res.Skipped)))
	return &haifypb.ImportBackupsResponse{
		Success:  true,
		Message:  fmt.Sprintf("Imported %d backup(s) from %s; %d skipped", len(res.Imported), req.Target, len(res.Skipped)),
		Imported: res.Imported,
		Skipped:  res.Skipped,
	}, nil
}

func backupScheduleToProto(sc *database.BackupSchedule, now time.Time) *haifypb.BackupScheduleInfo {
	info := &haifypb.BackupScheduleInfo{
		Name: sc.Name, Resource: sc.Resource, Target: sc.Target, Cron: sc.Cron, Enabled: sc.Enabled,
		Keep: gfsToProto(sc.Keep), LastBackup: sc.LastBackup, LastError: strings.TrimSpace(sc.LastError),
	}
	if !sc.LastRun.IsZero() {
		info.LastRun = sc.LastRun.UTC().Format(time.RFC3339)
	}
	if sc.Enabled {
		if next := NextRun(sc.Cron, now); !next.IsZero() {
			info.NextRun = next.UTC().Format(time.RFC3339)
		}
	}
	zap.L().Debug("backupScheduleToProto: rendered schedule", zap.String("schedule", sc.Name),
		zap.Bool("enabled", sc.Enabled), zap.String("next_run", info.NextRun))
	return info
}
