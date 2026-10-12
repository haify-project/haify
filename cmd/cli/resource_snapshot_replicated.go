package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// Resource snapshots: every volume on every diskful replica at the same
// instant, with I/O suspended across them, so a rollback brings all replicas
// back together and resyncs nothing. `haify resource snapshot create` takes one
// on one node; this takes one of the whole resource.

func resourceSnapshotReplicated() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "replicated",
		Short: "Snapshots of a whole resource on every replica (rollback without resync)",
	}
	var resource, name string
	run := func(use, short string, needName bool, fn func(ctx context.Context, c haifypb.HaifyControllerClient) error) *cobra.Command {
		sub := &cobra.Command{
			Use:   use,
			Short: short,
			RunE: func(cmd *cobra.Command, args []string) error {
				log := zap.L()
				log.Debug("resourceSnapshotReplicated: running command", zap.String("command", use), zap.Strings("args", args),
					zap.String("resource", resource), zap.String("name", name))
				if resource == "" || (needName && name == "") {
					log.Debug("resourceSnapshotReplicated: --resource or --name missing", zap.Bool("need_name", needName))
					return fmt.Errorf("--resource and --name are required")
				}
				c, conn, err := newResourceGRPCClient()
				if err != nil {
					log.Debug("resourceSnapshotReplicated: newResourceGRPCClient failed", zap.Error(err))
					return err
				}
				defer func() { _ = conn.Close() }()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()
				err = fn(ctx, c)
				log.Debug("resourceSnapshotReplicated: done", zap.String("command", use), zap.Error(err))
				return err
			},
		}
		sub.Flags().StringVar(&resource, "resource", "", "Resource")
		if needName {
			sub.Flags().StringVar(&name, "name", "", "Snapshot name")
		}
		return sub
	}
	result := func(ok bool, msg string) error {
		if !ok {
			zap.L().Debug("resourceSnapshotReplicated: controller reported failure", zap.String("message", msg))
			return fmt.Errorf("%s", msg)
		}
		fmt.Println(msg)
		return nil
	}
	cmd.AddCommand(
		run("create", "Snapshot every volume on every replica", true, func(ctx context.Context, c haifypb.HaifyControllerClient) error {
			r, err := c.CreateResourceSnapshot(ctx, &haifypb.CreateResourceSnapshotRequest{Resource: resource, Name: name})
			if err != nil {
				zap.L().Debug("resourceSnapshotReplicated: CreateResourceSnapshot failed", zap.Error(err))
				return err
			}
			return result(r.Success, r.Message)
		}),
		run("list", "List a resource's snapshots", false, func(ctx context.Context, c haifypb.HaifyControllerClient) error {
			r, err := c.ListResourceSnapshots(ctx, &haifypb.ListResourceSnapshotsRequest{Resource: resource})
			if err != nil {
				zap.L().Debug("resourceSnapshotReplicated: ListResourceSnapshots failed", zap.Error(err))
				return err
			}
			if !r.Success {
				zap.L().Debug("resourceSnapshotReplicated: controller reported failure", zap.String("message", r.Message))
				return fmt.Errorf("%s", r.Message)
			}
			zap.L().Debug("resourceSnapshotReplicated: snapshots", zap.String("resource", resource), zap.Int("count", len(r.Names)))
			for _, n := range r.Names {
				fmt.Println(n)
			}
			return nil
		}),
		run("rollback", "Roll every replica back (the resource must not be Primary anywhere)", true, func(ctx context.Context, c haifypb.HaifyControllerClient) error {
			r, err := c.RollbackResourceSnapshot(ctx, &haifypb.RollbackResourceSnapshotRequest{Resource: resource, Name: name})
			if err != nil {
				zap.L().Debug("resourceSnapshotReplicated: RollbackResourceSnapshot failed", zap.Error(err))
				return err
			}
			return result(r.Success, r.Message)
		}),
		run("delete", "Delete a snapshot from every replica", true, func(ctx context.Context, c haifypb.HaifyControllerClient) error {
			r, err := c.DeleteResourceSnapshot(ctx, &haifypb.DeleteResourceSnapshotRequest{Resource: resource, Name: name})
			if err != nil {
				zap.L().Debug("resourceSnapshotReplicated: DeleteResourceSnapshot failed", zap.Error(err))
				return err
			}
			return result(r.Success, r.Message)
		}),
	)
	return cmd
}
