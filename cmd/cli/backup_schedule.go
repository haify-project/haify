package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"

	"github.com/spf13/cobra"
)

func backupScheduleCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Back resources up on a cron, with GFS retention",
		Long: `Run backups on a cron and prune old ones.

One schedule per resource and target. The active controller runs it; after a
controller failover the new one carries on. A run still going when the next
one is due skips that tick.

Retention counts the schedule's completed backups the way snapshot schedules
count snapshots (--keep-daily 7 keeps the newest backup of each of the last 7
days), with one addition: an incremental is useless without every backup down
to its full one, so a kept backup keeps its whole chain. A daily schedule
keeping 7 can therefore hold up to a month of backups — the chain restarts with
a full backup after 30 incrementals.

A failed run raises a backup.failed event, resolved by the next completed run.`,
	}
	cmd.AddCommand(backupScheduleCreateCommand(), backupScheduleListCommand(),
		backupScheduleDeleteCommand(), backupScheduleRunCommand())
	return cmd
}

func backupScheduleCreateCommand() *cobra.Command {
	var resource, target, cronExpr string
	var hourly, daily, weekly, monthly, yearly int
	var disabled bool
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create or replace the schedule backing a resource up to a target",
		Example: `  haify backup schedule create --resource db --target offsite \
      --cron "30 2 * * *" --keep-daily 7 --keep-weekly 4 --keep-monthly 6`,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("backupScheduleCreateCommand: running command", zap.Strings("args", args), zap.String("resource", resource),
				zap.String("target", target), zap.String("cron", cronExpr), zap.Int("keep_hourly", hourly), zap.Int("keep_daily", daily),
				zap.Int("keep_weekly", weekly), zap.Int("keep_monthly", monthly), zap.Int("keep_yearly", yearly), zap.Bool("disabled", disabled))
			if resource == "" || target == "" || cronExpr == "" {
				log.Debug("backupScheduleCreateCommand: required flag missing")
				return fmt.Errorf("--resource, --target and --cron are required")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				log.Debug("backupScheduleCreateCommand: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(c)
			sc, err := c.CreateBackupSchedule(ctx, &haifypb.CreateBackupScheduleRequest{
				Resource: resource, Target: target, Cron: cronExpr, Enabled: !disabled,
				Keep: &haifypb.GFSRetention{Hourly: int32(hourly), Daily: int32(daily), Weekly: int32(weekly),
					Monthly: int32(monthly), Yearly: int32(yearly)},
			})
			if err != nil {
				log.Debug("backupScheduleCreateCommand: CreateBackupSchedule failed", zap.Error(err))
				return fmt.Errorf("failed to save backup schedule: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Backup schedule %s saved (cron %q, next run %s)\n",
				sc.Name, sc.Cron, orDash(sc.NextRun))
			log.Debug("backupScheduleCreateCommand: done", zap.String("schedule", sc.Name), zap.String("next_run", sc.NextRun))
			return nil
		},
	}
	cmd.Flags().StringVar(&resource, "resource", "", "Resource to back up (required)")
	cmd.Flags().StringVar(&target, "target", "", "Backup target (required)")
	cmd.Flags().StringVar(&cronExpr, "cron", "", "Standard 5-field cron expression, in the controller's time zone (required)")
	cmd.Flags().IntVar(&hourly, "keep-hourly", 0, "Hourly backups to retain")
	cmd.Flags().IntVar(&daily, "keep-daily", 0, "Daily backups to retain")
	cmd.Flags().IntVar(&weekly, "keep-weekly", 0, "Weekly backups to retain")
	cmd.Flags().IntVar(&monthly, "keep-monthly", 0, "Monthly backups to retain")
	cmd.Flags().IntVar(&yearly, "keep-yearly", 0, "Yearly backups to retain")
	cmd.Flags().BoolVar(&disabled, "disabled", false, "Save the schedule without running it")
	return cmd
}

func backupScheduleListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List backup schedules and how their last run went",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("backupScheduleListCommand: running command", zap.Strings("args", args))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				log.Debug("backupScheduleListCommand: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(c)
			schedules, err := c.ListBackupSchedules(ctx)
			if err != nil {
				log.Debug("backupScheduleListCommand: ListBackupSchedules failed", zap.Error(err))
				return fmt.Errorf("failed to list backup schedules: %w", err)
			}
			out := cmd.OutOrStdout()
			if len(schedules) == 0 {
				log.Debug("backupScheduleListCommand: no schedules")
				_, _ = fmt.Fprintln(out, "No backup schedules")
				return nil
			}
			for _, s := range schedules {
				log.Debug("backupScheduleListCommand: schedule", zap.String("schedule", s.Name), zap.Bool("enabled", s.Enabled),
					zap.String("last_run", s.LastRun), zap.Bool("last_failed", s.LastError != ""))
				state := "enabled"
				if !s.Enabled {
					state = "disabled"
				}
				k := s.Keep
				_, _ = fmt.Fprintf(out, "%s  cron=%q  %s\n", s.Name, s.Cron, state)
				_, _ = fmt.Fprintf(out, "  keep: hourly=%d daily=%d weekly=%d monthly=%d yearly=%d\n",
					k.GetHourly(), k.GetDaily(), k.GetWeekly(), k.GetMonthly(), k.GetYearly())
				switch {
				case s.LastError != "":
					_, _ = fmt.Fprintf(out, "  last run: %s FAILED: %s\n", s.LastRun, s.LastError)
				case s.LastRun != "":
					_, _ = fmt.Fprintf(out, "  last run: %s -> %s\n", s.LastRun, s.LastBackup)
				}
				if s.NextRun != "" {
					_, _ = fmt.Fprintf(out, "  next run: %s\n", s.NextRun)
				}
			}
			log.Debug("backupScheduleListCommand: done", zap.Int("count", len(schedules)))
			return nil
		},
	}
}

func backupScheduleDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <resource@target>",
		Short: "Delete a backup schedule (its backups are kept)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("backupScheduleDeleteCommand: running command", zap.Strings("args", args))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				log.Debug("backupScheduleDeleteCommand: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(c)
			if err := c.DeleteBackupSchedule(ctx, args[0]); err != nil {
				log.Debug("backupScheduleDeleteCommand: DeleteBackupSchedule failed", zap.Error(err))
				return fmt.Errorf("failed to delete backup schedule: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Backup schedule %s deleted\n", args[0])
			log.Debug("backupScheduleDeleteCommand: done")
			return nil
		},
	}
}

func backupScheduleRunCommand() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "run <resource@target>",
		Short: "Run a schedule now, retention included, and wait for it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("backupScheduleRunCommand: running command", zap.Strings("args", args), zap.Duration("timeout", timeout))
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				log.Debug("backupScheduleRunCommand: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(c)
			sc, err := c.RunBackupSchedule(ctx, args[0])
			if err != nil {
				log.Debug("backupScheduleRunCommand: RunBackupSchedule failed", zap.Error(err))
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Backup %s completed\n", sc.LastBackup)
			log.Debug("backupScheduleRunCommand: done", zap.String("backup_id", sc.LastBackup))
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 24*time.Hour, "Give up if the backup takes longer than this")
	return cmd
}

func backupImportCommand() *cobra.Command {
	var target, node, asOf string
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Rebuild backup records from what a target holds",
		Long: `Read every backup manifest on a target and record the backups this
controller does not know yet, incremental chains included.

Use it when the controller's database is gone — a rebuilt cluster — or to
restore another cluster's backups: add the same bucket and prefix as a target,
import, create a resource at least as large as the backup, and
` + "`backup restore <id> --resource <new>`" + `.

A backup whose images are not all on the target is skipped and named. Only the
newest backup of each resource can carry on as the base of the next
incremental, and only when its snapshot is still on that node of this cluster;
otherwise the next backup to the target is full.

--as-of reads the target as it was at a time (RFC3339): the object versions
current then, before any overwrite or delete since. It is the way back after
someone with the target's credentials deleted the backups — on a versioned S3
bucket a delete only hides them — and restores of what it imports read those
same versions.

  haify backup import --target vault --as-of 2026-10-01T00:00:00Z`,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("backupImportCommand: running command", zap.Strings("args", args), zap.String("target", target),
				zap.String("node", node), zap.String("as_of", asOf))
			if target == "" {
				log.Debug("backupImportCommand: --target missing")
				return fmt.Errorf("--target is required")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				log.Debug("backupImportCommand: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(c)
			res, err := c.ImportBackupsAsOf(ctx, target, node, asOf)
			if err != nil {
				log.Debug("backupImportCommand: ImportBackupsAsOf failed", zap.Error(err))
				return fmt.Errorf("import failed: %w", err)
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "Imported %d backup(s) from %s\n", len(res.Imported), target)
			for _, id := range res.Imported {
				_, _ = fmt.Fprintf(out, "  + %s\n", id)
			}
			keys := make([]string, 0, len(res.Skipped))
			for k := range res.Skipped {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				_, _ = fmt.Fprintf(out, "  - %s: %s\n", k, res.Skipped[k])
			}
			log.Debug("backupImportCommand: done", zap.Int("imported", len(res.Imported)), zap.Int("skipped", len(res.Skipped)))
			return nil
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "Target to read (required); add it first with backup target add")
	cmd.Flags().StringVar(&node, "node", "", "Read the target from this node (default: the first registered node with rclone)")
	cmd.Flags().StringVar(&asOf, "as-of", "", "Read the target as it was at this RFC3339 time (versioned S3 targets)")
	return cmd
}
