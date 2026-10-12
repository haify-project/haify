package main

import (
	"context"
	"fmt"
	"time"

	"github.com/haify-project/haify/pkg/util"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// poolAddCache puts an SSD in front of one node's pool with lvmcache.
func poolAddCache() *cobra.Command {
	var node, pool, device, mode string

	cmd := &cobra.Command{
		Use:   "add-cache --node <node> --pool <pool> --device <dev> [--mode writeback]",
		Short: "Put an SSD/NVMe device in front of a node's pool (lvmcache)",
		Long: `Attach a fast device as a cache in front of one node's LVM thin pool.

The whole device is consumed and the cache serves every volume in the pool. The
cache is local to the node, so each replica of a resource is tiered on its own —
caching one node of a three-way resource is a complete operation, not half of one.

MODES

  writethrough (default)  A write is acknowledged only once it has reached the
                          slow disk. The SSD accelerates reads and holds nothing
                          that is not already durable. Losing it costs
                          performance and nothing else.

  writeback               A write is acknowledged as soon as it is on the SSD and
                          is written down to the slow disk later. Losing that one
                          SSD — device failure, or the node destroyed — loses every
                          write it had accepted but not yet destaged, and only a
                          replica that happens to hold those writes can give them
                          back, which a resyncing peer or a correlated failure
                          will not.

Run 'haify pool get' to see how much of a writeback cache is dirty, which is the
size of that window at any moment.

The command refuses when the device is already in use or carries a signature,
when it is smaller than 4 GiB, when the pool is not an LVM thin pool, and when
the pool already has a cache.

  haify pool add-cache --node node1 --pool pool0 --device /dev/nvme0n1`,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("poolAddCache: running command", zap.Strings("args", args), zap.String("node", node), zap.String("pool", pool),
				zap.String("device", device), zap.String("mode", mode))
			if node == "" || pool == "" || device == "" {
				log.Debug("poolAddCache: --node, --pool or --device missing")
				return fmt.Errorf("--node, --pool and --device are all required")
			}

			// lvconvert has to migrate the pool's existing metadata into the
			// cache before it returns, which is minutes on a large pool.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("poolAddCache: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			applied, size, err := haifyClient.AddPoolCache(ctx, node, pool, device, mode)
			if err != nil {
				log.Debug("poolAddCache: AddPoolCache failed", zap.Error(err))
				return fmt.Errorf("failed to add cache: %w", err)
			}

			log.Debug("poolAddCache: cache attached", zap.String("node", node), zap.String("pool", pool), zap.String("mode", applied), zap.Uint64("size_bytes", size))
			fmt.Printf("Cache attached to %s on %s: %s of %s, %s mode.\n",
				pool, node, util.FormatBytes(size), device, applied)
			if applied == "writeback" {
				log.Debug("poolAddCache: writeback mode, printing the data-loss warning")
				fmt.Printf("Writes are acknowledged from %s before they reach the slow disk.\n", device)
				fmt.Printf("If that device is lost, its undestaged writes are gone unless a replica has them.\n")
			}
			log.Debug("poolAddCache: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&node, "node", "", "Node whose pool gets the cache")
	cmd.Flags().StringVar(&pool, "pool", "", "Pool (volume group) to put the cache in front of")
	cmd.Flags().StringVar(&device, "device", "", "Fast block device to spend entirely on cache, e.g. /dev/nvme0n1")
	cmd.Flags().StringVar(&mode, "mode", "", "writethrough (default) or writeback. "+
		"writeback acks writes from the SSD, so losing that one device loses every write it has not "+
		"yet written down to the slow disk, recoverable only from a replica that still has them")
	return cmd
}

// poolRemoveCache flushes a pool's cache and gives the device back.
func poolRemoveCache() *cobra.Command {
	var node, pool string

	cmd := &cobra.Command{
		Use:   "remove-cache --node <node> --pool <pool>",
		Short: "Flush and detach a pool's cache, releasing its device",
		Long: `Detach the cache from one node's pool and take its device back out of the
volume group.

A writeback cache is flushed to the slow disk before it is detached, and the
detach is verified afterwards: if any of it is still there the command fails and
says so, because "the cache is gone" is what tells an operator the SSD is safe
to pull.

If the cache device has already failed there is nothing to flush from, and this
command refuses rather than discarding the writes on your behalf; it names the
lvconvert --uncache --force needed to accept that loss deliberately.

  haify pool remove-cache --node node1 --pool pool0`,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("poolRemoveCache: running command", zap.Strings("args", args), zap.String("node", node), zap.String("pool", pool))
			if node == "" || pool == "" {
				log.Debug("poolRemoveCache: --node or --pool missing")
				return fmt.Errorf("--node and --pool are both required")
			}

			// Flushing a full writeback cache to a slow disk is bounded by that
			// disk's write throughput, not by anything this command controls.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("poolRemoveCache: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			if err := haifyClient.RemovePoolCache(ctx, node, pool); err != nil {
				log.Debug("poolRemoveCache: RemovePoolCache failed", zap.Error(err))
				return fmt.Errorf("failed to remove cache: %w", err)
			}

			fmt.Printf("Cache on %s on %s flushed, detached, and its device released.\n", pool, node)
			log.Debug("poolRemoveCache: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&node, "node", "", "Node whose pool cache is removed")
	cmd.Flags().StringVar(&pool, "pool", "", "Pool (volume group) to uncache")
	return cmd
}
