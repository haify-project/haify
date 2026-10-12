package main

import (
	"context"
	"fmt"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func resourceMoveVolumeCommand() *cobra.Command {
	var pool string
	var volume int32
	cmd := &cobra.Command{
		Use:   "move-volume <resource>",
		Short: "Move a volume to another pool, one node at a time, while it keeps serving",
		Long: `Each node's copy in turn is detached, recreated in the new pool and filled
again from a peer; the resource stays online and is never more than one copy
short. The new pool must exist on every node holding a copy. LVM snapshots of
the old volume cannot move with it and are deleted, so a resource whose
snapshots are locked is refused. A move that stops part-way carries on from
where it stopped when asked again. Follow it with: haify pool jobs`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceMoveVolumeCommand: running command", zap.Strings("args", args), zap.String("pool", pool), zap.Int32("volume", volume))
			if pool == "" {
				log.Debug("resourceMoveVolumeCommand: --pool missing")
				return fmt.Errorf("--pool is required")
			}
			return withController(time.Minute, func(ctx context.Context, c haifypb.HaifyControllerClient) error {
				r, err := c.MoveVolume(ctx, &haifypb.MoveVolumeRequest{Resource: args[0], VolumeId: volume, Pool: pool})
				if err != nil {
					log.Debug("resourceMoveVolumeCommand: MoveVolume failed", zap.Error(err))
					return err
				}
				log.Debug("resourceMoveVolumeCommand: move requested", zap.Bool("success", r.Success), zap.String("job_id", r.JobId))
				if r.Success {
					fmt.Printf("job %s: ", r.JobId)
				}
				return resultLine(r.Success, r.Message)
			})
		},
	}
	cmd.Flags().StringVar(&pool, "pool", "", "Destination pool")
	cmd.Flags().Int32Var(&volume, "volume", 0, "Volume number")
	return cmd
}
