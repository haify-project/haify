package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

type backupScheduleOut struct {
	Name       string `json:"name" jsonschema:"resource@target"`
	Resource   string `json:"resource"`
	Target     string `json:"target"`
	Cron       string `json:"cron"`
	Enabled    bool   `json:"enabled"`
	KeepHourly int32  `json:"keep_hourly,omitempty"`
	KeepDaily  int32  `json:"keep_daily,omitempty"`
	KeepWeekly int32  `json:"keep_weekly,omitempty"`
	KeepMonth  int32  `json:"keep_monthly,omitempty"`
	KeepYearly int32  `json:"keep_yearly,omitempty"`
	LastRun    string `json:"last_run,omitempty"`
	LastBackup string `json:"last_backup,omitempty" jsonschema:"backup id the last run produced"`
	LastError  string `json:"last_error,omitempty" jsonschema:"why the last run produced no backup"`
	NextRun    string `json:"next_run,omitempty"`
}

type backupScheduleListOut struct {
	Schedules []backupScheduleOut `json:"schedules"`
}

type backupScheduleCreateIn struct {
	Resource    string `json:"resource" jsonschema:"resource to back up"`
	Target      string `json:"target" jsonschema:"name of a configured backup target"`
	Cron        string `json:"cron" jsonschema:"standard 5-field cron expression, e.g. '30 2 * * *'"`
	KeepHourly  int32  `json:"keep_hourly,omitempty"`
	KeepDaily   int32  `json:"keep_daily,omitempty"`
	KeepWeekly  int32  `json:"keep_weekly,omitempty"`
	KeepMonthly int32  `json:"keep_monthly,omitempty"`
	KeepYearly  int32  `json:"keep_yearly,omitempty"`
	Disabled    bool   `json:"disabled,omitempty" jsonschema:"save without running it"`
}

type backupImportIn struct {
	Target string `json:"target" jsonschema:"name of a configured backup target"`
	Node   string `json:"node,omitempty" jsonschema:"node to read the target from; empty picks one with rclone"`
}

type backupImportOut struct {
	Imported []string          `json:"imported"`
	Skipped  map[string]string `json:"skipped,omitempty"`
}

func backupScheduleToOut(s *haifypb.BackupScheduleInfo) backupScheduleOut {
	k := s.GetKeep()
	return backupScheduleOut{
		Name: s.Name, Resource: s.Resource, Target: s.Target, Cron: s.Cron, Enabled: s.Enabled,
		KeepHourly: k.GetHourly(), KeepDaily: k.GetDaily(), KeepWeekly: k.GetWeekly(),
		KeepMonth: k.GetMonthly(), KeepYearly: k.GetYearly(),
		LastRun: s.LastRun, LastBackup: s.LastBackup, LastError: s.LastError, NextRun: s.NextRun,
	}
}

// registerBackupScheduleTools adds scheduled backups and importing backups
// from a target.
func (s *Server) registerBackupScheduleTools(srv *mcp.Server) {
	addRead(s, srv, readOnlyTool("haify_backup_schedule_list", "List backup schedules",
		"List the cron-driven backups, their retention, and how the last run went. A schedule with last_error "+
			"set has not produced a backup since that run."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, backupScheduleListOut, error) {
			s.logger.Debug("haify_backup_schedule_list: listing backup schedules")
			list, err := s.client.ListBackupSchedules(ctx)
			if err != nil {
				s.logger.Debug("haify_backup_schedule_list: list failed", zap.Error(err))
				return nil, backupScheduleListOut{}, err
			}
			out := backupScheduleListOut{Schedules: make([]backupScheduleOut, 0, len(list))}
			for _, sc := range list {
				s.logger.Debug("haify_backup_schedule_list: schedule", zap.String("name", sc.Name),
					zap.String("resource", sc.Resource), zap.String("target", sc.Target), zap.String("cron", sc.Cron),
					zap.Bool("enabled", sc.Enabled), zap.Bool("has_last_error", sc.LastError != ""))
				out.Schedules = append(out.Schedules, backupScheduleToOut(sc))
			}
			s.logger.Debug("haify_backup_schedule_list: listed", zap.Int("count", len(out.Schedules)))
			return nil, out, nil
		})

	addWrite(s, srv, writeTool("haify_backup_schedule_create", "Schedule backups of a resource",
		"Create or replace the cron schedule backing a resource up to a target. Retention keeps the newest backup "+
			"per hour/day/week/month/year up to each count, plus every backup a kept incremental is built on."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in backupScheduleCreateIn) (*mcp.CallToolResult, backupScheduleOut, error) {
			s.logger.Debug("haify_backup_schedule_create: creating schedule", zap.String("resource", in.Resource),
				zap.String("target", in.Target), zap.String("cron", in.Cron), zap.Bool("disabled", in.Disabled),
				zap.Int32("keep_hourly", in.KeepHourly), zap.Int32("keep_daily", in.KeepDaily),
				zap.Int32("keep_weekly", in.KeepWeekly), zap.Int32("keep_monthly", in.KeepMonthly),
				zap.Int32("keep_yearly", in.KeepYearly))
			sc, err := s.client.CreateBackupSchedule(ctx, &haifypb.CreateBackupScheduleRequest{
				Resource: in.Resource, Target: in.Target, Cron: in.Cron, Enabled: !in.Disabled,
				Keep: &haifypb.GFSRetention{Hourly: in.KeepHourly, Daily: in.KeepDaily, Weekly: in.KeepWeekly,
					Monthly: in.KeepMonthly, Yearly: in.KeepYearly},
			})
			if err != nil {
				s.logger.Debug("haify_backup_schedule_create: create failed", zap.String("resource", in.Resource),
					zap.String("target", in.Target), zap.Error(err))
				return nil, backupScheduleOut{}, err
			}
			s.logger.Debug("haify_backup_schedule_create: created", zap.String("name", sc.Name), zap.String("next_run", sc.NextRun))
			return nil, backupScheduleToOut(sc), nil
		})

	addWrite(s, srv, destructiveTool("haify_backup_schedule_delete", "Stop scheduled backups",
		"Delete a backup schedule by its resource@target name. The backups it made are kept."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Name string `json:"name" jsonschema:"resource@target"`
		}) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_backup_schedule_delete: deleting schedule", zap.String("name", in.Name))
			if err := s.client.DeleteBackupSchedule(ctx, in.Name); err != nil {
				s.logger.Debug("haify_backup_schedule_delete: delete failed", zap.String("name", in.Name), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_backup_schedule_delete: deleted", zap.String("name", in.Name))
			return nil, ok("backup schedule " + in.Name + " deleted"), nil
		})

	addWrite(s, srv, writeTool("haify_backup_import", "Import backups from a target",
		"Record the backups a target holds that this controller does not know, from their manifests, chains "+
			"included. For a rebuilt controller, or to restore another cluster's backups. Changes nothing on the target."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in backupImportIn) (*mcp.CallToolResult, backupImportOut, error) {
			s.logger.Debug("haify_backup_import: importing backups", zap.String("target", in.Target), zap.String("node", in.Node))
			res, err := s.client.ImportBackups(ctx, in.Target, in.Node)
			if err != nil {
				s.logger.Debug("haify_backup_import: import failed", zap.String("target", in.Target),
					zap.String("node", in.Node), zap.Error(err))
				return nil, backupImportOut{}, err
			}
			s.logger.Debug("haify_backup_import: imported", zap.String("target", in.Target),
				zap.Int("imported", len(res.Imported)), zap.Int("skipped", len(res.Skipped)))
			return nil, backupImportOut{Imported: res.Imported, Skipped: res.Skipped}, nil
		})
}
