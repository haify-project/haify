package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// A frozen schedule keeps snapshotting, prunes nothing, and locks every
// scheduled snapshot of the resource until the freeze ends. The write-anomaly
// detector freezes one when a volume is suddenly rewritten; these freeze one
// by hand, or lift a freeze once the cause is known.

func resourceSnapshotScheduleFreeze() *cobra.Command {
	var resource, reason string
	var hours uint32
	cmd := &cobra.Command{
		Use:   "freeze",
		Short: "Stop all pruning and lock every scheduled snapshot of a resource for a while",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceSnapshotScheduleFreeze: running command", zap.Strings("args", args), zap.String("resource", resource),
				zap.Uint32("hours", hours), zap.String("reason", reason))
			if resource == "" {
				log.Debug("resourceSnapshotScheduleFreeze: --resource missing")
				return fmt.Errorf("--resource is required")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceSnapshotScheduleFreeze: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)
			until, err := haifyClient.FreezeSnapshotSchedule(ctx, resource, hours, reason)
			if err != nil {
				log.Debug("resourceSnapshotScheduleFreeze: FreezeSnapshotSchedule failed", zap.Error(err))
				return fmt.Errorf("failed to freeze snapshot schedule: %w", err)
			}
			fmt.Printf("Snapshot schedule of %q frozen until %s\n", resource, until)
			log.Debug("resourceSnapshotScheduleFreeze: done", zap.String("resource", resource), zap.String("until", until))
			return nil
		},
	}
	cmd.Flags().StringVar(&resource, "resource", "", "Resource whose schedule to freeze")
	cmd.Flags().Uint32Var(&hours, "hours", 0, "How long (default 168, a week); an existing longer freeze is kept")
	cmd.Flags().StringVar(&reason, "reason", "", "Why, shown in schedule list")
	return cmd
}

func resourceSnapshotScheduleUnfreeze() *cobra.Command {
	var resource string
	cmd := &cobra.Command{
		Use:   "unfreeze",
		Short: "End a freeze early (needs a second person's approval when [rbac.approval] is on)",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceSnapshotScheduleUnfreeze: running command", zap.Strings("args", args), zap.String("resource", resource))
			if resource == "" {
				log.Debug("resourceSnapshotScheduleUnfreeze: --resource missing")
				return fmt.Errorf("--resource is required")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceSnapshotScheduleUnfreeze: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)
			if err := haifyClient.UnfreezeSnapshotSchedule(ctx, resource); err != nil {
				log.Debug("resourceSnapshotScheduleUnfreeze: UnfreezeSnapshotSchedule failed", zap.Error(err))
				return fmt.Errorf("failed to unfreeze snapshot schedule: %w", err)
			}
			fmt.Printf("Snapshot schedule of %q unfrozen\n", resource)
			log.Debug("resourceSnapshotScheduleUnfreeze: done")
			return nil
		},
	}
	cmd.Flags().StringVar(&resource, "resource", "", "Resource whose schedule to unfreeze")
	return cmd
}
