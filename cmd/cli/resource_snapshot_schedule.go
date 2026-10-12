package main

import (
	"context"
	"fmt"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// resourceSnapshotSchedule manages cron-driven snapshot schedules with GFS retention.
func resourceSnapshotSchedule() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Scheduled snapshots with GFS retention",
	}
	cmd.AddCommand(resourceSnapshotScheduleCreate())
	cmd.AddCommand(resourceSnapshotScheduleList())
	cmd.AddCommand(resourceSnapshotScheduleDelete())
	cmd.AddCommand(resourceSnapshotScheduleFreeze(), resourceSnapshotScheduleUnfreeze())
	return cmd
}

func resourceSnapshotScheduleCreate() *cobra.Command {
	var resource, cronExpr string
	var hourly, daily, weekly, monthly, yearly int
	var disabled bool
	var lockDays uint32

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create or replace a snapshot schedule for a resource",
		Long: `Create or replace a snapshot schedule for a resource.

--lock-days N locks every snapshot the schedule takes for N days: retention, a
full thin pool and API deletes all leave it alone until then, and the schedule
and resource cannot be deleted, nor the lock lowered, while any is locked. It is
what keeps the clean snapshots from before a volume was encrypted: a pool
filling fast used to give up its oldest snapshots first. Size the pool for N
days of change; a pool that fills with locked snapshots raises a critical
pool.snapshots_locked event. Root on a storage node can still remove them —
for that, back up to a locked S3 target. Without the flag a replaced schedule
keeps its lock.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceSnapshotScheduleCreate: running command", zap.Strings("args", args), zap.String("resource", resource),
				zap.String("cron", cronExpr), zap.Int("keep_hourly", hourly), zap.Int("keep_daily", daily), zap.Int("keep_weekly", weekly),
				zap.Int("keep_monthly", monthly), zap.Int("keep_yearly", yearly), zap.Bool("disabled", disabled), zap.Uint32("lock_days", lockDays))
			if resource == "" {
				log.Debug("resourceSnapshotScheduleCreate: --resource missing")
				return fmt.Errorf("--resource is required")
			}
			if cronExpr == "" {
				log.Debug("resourceSnapshotScheduleCreate: --cron missing")
				return fmt.Errorf("--cron is required (standard 5-field cron, e.g. \"0 * * * *\")")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceSnapshotScheduleCreate: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			keep := &haifypb.GFSRetention{
				Hourly:  int32(hourly),
				Daily:   int32(daily),
				Weekly:  int32(weekly),
				Monthly: int32(monthly),
				Yearly:  int32(yearly),
			}
			var lock *uint32
			if cmd.Flags().Changed("lock-days") {
				log.Debug("resourceSnapshotScheduleCreate: --lock-days given", zap.Uint32("lock_days", lockDays))
				lock = &lockDays
			} else {
				log.Debug("resourceSnapshotScheduleCreate: no --lock-days, a replaced schedule keeps its lock")
			}
			if err := haifyClient.CreateSnapshotScheduleLocked(ctx, resource, cronExpr, keep, !disabled, lock); err != nil {
				log.Debug("resourceSnapshotScheduleCreate: CreateSnapshotScheduleLocked failed", zap.Error(err))
				return fmt.Errorf("failed to create snapshot schedule: %w", err)
			}
			fmt.Printf("Snapshot schedule for %q created (cron=%q)\n", resource, cronExpr)
			log.Debug("resourceSnapshotScheduleCreate: done", zap.String("resource", resource))
			return nil
		},
	}
	cmd.Flags().StringVar(&resource, "resource", "", "Target resource (required)")
	cmd.Flags().StringVar(&cronExpr, "cron", "", "Standard 5-field cron expression (required)")
	cmd.Flags().IntVar(&hourly, "keep-hourly", 0, "Hourly snapshots to retain")
	cmd.Flags().IntVar(&daily, "keep-daily", 0, "Daily snapshots to retain")
	cmd.Flags().IntVar(&weekly, "keep-weekly", 0, "Weekly snapshots to retain")
	cmd.Flags().IntVar(&monthly, "keep-monthly", 0, "Monthly snapshots to retain")
	cmd.Flags().IntVar(&yearly, "keep-yearly", 0, "Yearly snapshots to retain")
	cmd.Flags().BoolVar(&disabled, "disabled", false, "Create the schedule disabled")
	cmd.Flags().Uint32Var(&lockDays, "lock-days", 0, "Lock every snapshot the schedule takes for this many days (see above)")
	return cmd
}

func resourceSnapshotScheduleList() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List snapshot schedules",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceSnapshotScheduleList: running command", zap.Strings("args", args))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceSnapshotScheduleList: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			schedules, err := haifyClient.ListSnapshotSchedules(ctx)
			if err != nil {
				log.Debug("resourceSnapshotScheduleList: ListSnapshotSchedules failed", zap.Error(err))
				return fmt.Errorf("failed to list snapshot schedules: %w", err)
			}
			if len(schedules) == 0 {
				log.Debug("resourceSnapshotScheduleList: no schedules")
				fmt.Println("No snapshot schedules found")
				return nil
			}
			for _, s := range schedules {
				log.Debug("resourceSnapshotScheduleList: schedule", zap.String("schedule", s.Name), zap.String("resource", s.Resource),
					zap.Bool("enabled", s.Enabled), zap.Uint32("lock_days", s.LockDays), zap.String("frozen_until", s.FrozenUntil))
				state := "enabled"
				if !s.Enabled {
					state = "disabled"
				}
				k := s.Keep
				fmt.Printf("%s (resource=%s, cron=%q, %s)\n", s.Name, s.Resource, s.Cron, state)
				fmt.Printf("  keep: hourly=%d daily=%d weekly=%d monthly=%d yearly=%d\n",
					k.GetHourly(), k.GetDaily(), k.GetWeekly(), k.GetMonthly(), k.GetYearly())
				if s.LastRun != "" {
					fmt.Printf("  last run: %s\n", s.LastRun)
				}
				if s.NextRun != "" {
					fmt.Printf("  next run: %s\n", s.NextRun)
				}
				if s.LockDays > 0 {
					fmt.Printf("  snapshots locked for %d days", s.LockDays)
					if s.LockedUntil != "" {
						fmt.Printf("; newest locked until %s", s.LockedUntil)
					}
					fmt.Println()
				}
				if s.FrozenUntil != "" {
					fmt.Printf("  FROZEN until %s (%s): nothing is pruned or deleted\n", s.FrozenUntil, s.FrozenReason)
				}
			}
			log.Debug("resourceSnapshotScheduleList: done", zap.Int("count", len(schedules)))
			return nil
		},
	}
	return cmd
}

func resourceSnapshotScheduleDelete() *cobra.Command {
	var name, resourceFlag string
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a snapshot schedule (existing snapshots are kept)",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceSnapshotScheduleDelete: running command", zap.Strings("args", args), zap.String("name", name), zap.String("resource", resourceFlag))
			if name == "" {
				name = resourceFlag
			}
			if name == "" {
				log.Debug("resourceSnapshotScheduleDelete: --resource missing")
				return fmt.Errorf("--resource is required")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceSnapshotScheduleDelete: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			if err := haifyClient.DeleteSnapshotSchedule(ctx, name); err != nil {
				log.Debug("resourceSnapshotScheduleDelete: DeleteSnapshotSchedule failed", zap.Error(err))
				return fmt.Errorf("failed to delete snapshot schedule: %w", err)
			}
			fmt.Printf("Snapshot schedule %q deleted\n", name)
			log.Debug("resourceSnapshotScheduleDelete: done")
			return nil
		},
	}
	// --resource matches schedule create; --name is the older spelling.
	cmd.Flags().StringVar(&resourceFlag, "resource", "", "Resource whose schedule to delete (required)")
	cmd.Flags().StringVar(&name, "name", "", "Same as --resource")
	_ = cmd.Flags().MarkHidden("name")
	return cmd
}
