package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// poolConvertThin rebuilds one node's pool as an LVM thin pool, in place.
func poolConvertThin() *cobra.Command {
	var node, pool string

	cmd := &cobra.Command{
		Use:   "convert-thin --node <node> --pool <pool>",
		Short: "Rebuild a node's LVM pool as a thin pool, in place",
		Long: `Rebuild one node's LVM pool as a thin pool without recreating its resources.

A thick pool cannot hold a snapshot history. LVM makes every snapshot reserve a
copy-on-write area up front — Haify reserves 20% of the origin — so a 10 GiB pool
backing a 6 GiB volume fits two snapshots whether or not anything ever changes.
A thin snapshot costs only the blocks that diverge.

This destroys the node's backing volumes and resyncs them in full from the
peers, so run it on ONE node at a time and let each resync finish first. The
resource keeps serving throughout: the node goes diskless for the duration and
its peers answer. The command refuses to start when the node holds a Primary,
when any peer is not UpToDate, when a resync is already running, or when the
node's copy is one of only two.

Note that the rebuilt volume ends up fully allocated — DRBD's resync writes
every block, zeroes included — so the pool is sized for the whole origin plus
headroom, not for the live data.

  haify pool convert-thin --node node2 --pool haify_pool0`,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("poolConvertThin: running command", zap.Strings("args", args), zap.String("node", node), zap.String("pool", pool))
			if node == "" || pool == "" {
				log.Debug("poolConvertThin: --node or --pool missing")
				return fmt.Errorf("--node and --pool are both required")
			}

			// A full resync of every volume on the node, from scratch.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("poolConvertThin: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			if err := haifyClient.ConvertPoolToThin(ctx, node, pool); err != nil {
				log.Debug("poolConvertThin: ConvertPoolToThin failed", zap.Error(err))
				return fmt.Errorf("failed to convert pool: %w", err)
			}

			fmt.Printf("Pool %s on %s rebuilt as thin.\n", pool, node)
			fmt.Printf("Its volumes are resyncing from their peers; watch with:\n")
			fmt.Printf("  haify resource status <resource>\n")
			fmt.Printf("Wait for every volume to read UpToDate before converting the next node.\n")
			log.Debug("poolConvertThin: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&node, "node", "", "Node whose pool will be rebuilt")
	cmd.Flags().StringVar(&pool, "pool", "", "Pool (volume group) to rebuild as thin")
	return cmd
}
