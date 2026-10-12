package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

func resourceRenameCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "rename <resource> <new-name>",
		Short: "Rename a resource that is not Primary anywhere and that nothing refers to by name",
		Long: `Rename a resource: it goes down on every node, its backing volumes and DRBD
config are renamed, and it comes back up under the new name with its data,
node ids and port unchanged. Refused while it is Primary anywhere, and while
an HA config, gateway, snapshot schedule, backups, snapshots, WAN replication
or encryption refer to the old name.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceRenameCommand: running command", zap.Strings("args", args))
			c, conn, err := newResourceGRPCClient()
			if err != nil {
				log.Debug("resourceRenameCommand: newResourceGRPCClient failed", zap.Error(err))
				return err
			}
			defer func() { _ = conn.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			r, err := c.RenameResource(ctx, &haifypb.RenameResourceRequest{Name: args[0], NewName: args[1]})
			if err != nil {
				log.Debug("resourceRenameCommand: RenameResource failed", zap.Error(err))
				return err
			}
			if !r.Success {
				log.Debug("resourceRenameCommand: controller reported failure", zap.String("message", r.Message))
				return fmt.Errorf("%s", r.Message)
			}
			fmt.Println(r.Message)
			log.Debug("resourceRenameCommand: done", zap.String("resource", args[0]), zap.String("new_name", args[1]))
			return nil
		},
	}
}
