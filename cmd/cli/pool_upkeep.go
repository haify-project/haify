package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/haify-project/haify/pkg/client"
	"github.com/haify-project/haify/pkg/util"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// Pool upkeep: trimming, the disks under the pools, and taking disks out.

func addPoolUpkeepCommands(cmd *cobra.Command) {
	cmd.AddCommand(poolTrim(), poolDisks(), poolRemoveDisk(), poolReplaceDisk(), poolJobs())
}

func poolTrim() *cobra.Command {
	var node string
	cmd := &cobra.Command{
		Use:   "trim",
		Short: "Return the space filesystems no longer use to the thin pools, now",
		Long: `Trims every mounted filesystem on a DRBD device, on the node serving it. The
discards reach every replica, so each node's thin pool gets back the blocks the
filesystem freed — and the zeros a full resync wrote. The controller also does
this on [storage.thin] trim_schedule (daily by default).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("poolTrim: running command", zap.Strings("args", args), zap.String("node", node))
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				log.Debug("poolTrim: newHaifyClient failed", zap.Error(err))
				return err
			}
			defer closeClient(c)
			resp, err := c.TrimPools(ctx, node)
			if err != nil {
				log.Debug("poolTrim: TrimPools failed", zap.Error(err))
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NODE\tFILESYSTEM\tTRIMMED")
			for _, r := range resp.Results {
				log.Debug("poolTrim: trim result", zap.String("node", r.Node), zap.String("mount", r.Mount), zap.Uint64("bytes", r.Bytes), zap.String("error", r.Error))
				got := util.FormatBytes(r.Bytes)
				if r.Error != "" {
					got = "failed: " + r.Error
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", r.Node, r.Mount, got)
			}
			_ = w.Flush()
			fmt.Println(resp.Message)
			log.Debug("poolTrim: done", zap.Int("count", len(resp.Results)))
			return nil
		},
	}
	cmd.Flags().StringVar(&node, "node", "", "Only this node (default: every online node)")
	return cmd
}

func poolDisks() *cobra.Command {
	var pool, node string
	cmd := &cobra.Command{
		Use:   "disks",
		Short: "List the disks under the pools and their health (SMART / NVMe)",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("poolDisks: running command", zap.Strings("args", args), zap.String("pool", pool), zap.String("node", node))
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				log.Debug("poolDisks: newHaifyClient failed", zap.Error(err))
				return err
			}
			defer closeClient(c)
			disks, err := c.ListPoolDisks(ctx, pool, node)
			if err != nil {
				log.Debug("poolDisks: ListPoolDisks failed", zap.Error(err))
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NODE\tPOOL\tDISK\tSIZE\tUSED\tHEALTH\tDETAIL")
			log.Debug("poolDisks: printing disks", zap.Int("count", len(disks)))
			for _, d := range disks {
				log.Debug("poolDisks: disk", zap.String("node", d.Node), zap.String("pool", d.Pool), zap.String("device", d.Device), zap.String("health", d.Health))
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.Node, d.Pool, d.Device, util.FormatBytes(d.SizeBytes),
					util.FormatBytes(d.UsedBytes), d.Health, strings.TrimSpace(d.HealthDetail+" "+d.Model))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&pool, "pool", "", "Only this pool")
	cmd.Flags().StringVar(&node, "node", "", "Only this node")
	return cmd
}

func poolRemoveDisk() *cobra.Command {
	var pool, node, disk string
	cmd := &cobra.Command{
		Use:   "remove-disk",
		Short: "Move a disk's data onto the pool's other disks, then take it out",
		Long: `The data moves first (pvmove), while the pool stays in use; the disk leaves the
pool only when that is done. The other disks need room for what it holds; when
they have none, use replace-disk. Follow it with: haify pool jobs`,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("poolRemoveDisk: running command", zap.Strings("args", args), zap.String("pool", pool), zap.String("node", node), zap.String("disk", disk))
			return runJob(func(ctx context.Context, c *client.HaifyClient) (string, error) {
				resp, err := c.RemovePoolDisk(ctx, pool, node, disk)
				if err != nil {
					log.Debug("poolRemoveDisk: RemovePoolDisk failed", zap.Error(err))
					return "", err
				}
				log.Debug("poolRemoveDisk: job started", zap.String("job_id", resp.JobId))
				return fmt.Sprintf("job %s: %s", resp.JobId, resp.Message), nil
			})
		},
	}
	cmd.Flags().StringVar(&pool, "pool", "", "Pool name (required)")
	cmd.Flags().StringVar(&node, "node", "", "Node (required)")
	cmd.Flags().StringVar(&disk, "disk", "", "Disk to take out, as the pool knows it (haify pool disks)")
	return cmd
}

func poolReplaceDisk() *cobra.Command {
	var pool, node, disk, newDisk string
	var wipe bool
	cmd := &cobra.Command{
		Use:   "replace-disk",
		Short: "Move a disk's data to a new disk, then take the old one out",
		Long: `For a disk that is failing or too small. The new disk joins the pool, the old
one's data moves to it, and the old disk leaves the pool. The pool stays in use throughout.
Follow it with: haify pool jobs`,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("poolReplaceDisk: running command", zap.Strings("args", args), zap.String("pool", pool), zap.String("node", node),
				zap.String("disk", disk), zap.String("new_disk", newDisk), zap.Bool("wipe", wipe))
			return runJob(func(ctx context.Context, c *client.HaifyClient) (string, error) {
				resp, err := c.ReplacePoolDisk(ctx, pool, node, disk, newDisk, wipe)
				if err != nil {
					log.Debug("poolReplaceDisk: ReplacePoolDisk failed", zap.Error(err))
					return "", err
				}
				log.Debug("poolReplaceDisk: job started", zap.String("job_id", resp.JobId))
				return fmt.Sprintf("job %s: %s", resp.JobId, resp.Message), nil
			})
		},
	}
	cmd.Flags().StringVar(&pool, "pool", "", "Pool name (required)")
	cmd.Flags().StringVar(&node, "node", "", "Node (required)")
	cmd.Flags().StringVar(&disk, "disk", "", "Disk to replace")
	cmd.Flags().StringVar(&newDisk, "new-disk", "", "Empty disk to move its data to (haify node disks)")
	cmd.Flags().BoolVar(&wipe, "wipe", false, "Erase the new disk if it holds only old data; without it such a disk is refused")
	return cmd
}

func poolJobs() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "Long storage jobs: disks being emptied or replaced, volumes changing pool",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("poolJobs: running command", zap.Strings("args", args), zap.Bool("all", all))
			ctx, cancel := context.WithTimeout(context.Background(), nodeOpTimeout)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				log.Debug("poolJobs: newHaifyClient failed", zap.Error(err))
				return err
			}
			defer closeClient(c)
			jobs, err := c.ListStorageJobs(ctx, all)
			if err != nil {
				log.Debug("poolJobs: ListStorageJobs failed", zap.Error(err))
				return err
			}
			if len(jobs) == 0 {
				log.Debug("poolJobs: no jobs")
				fmt.Println("no storage job running")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "JOB\tKIND\tSTATE\tSUBJECT\tPROGRESS\tSTARTED")
			for _, j := range jobs {
				log.Debug("poolJobs: job", zap.String("job_id", j.Id), zap.String("kind", j.Kind), zap.String("state", j.State),
					zap.String("subject", j.Subject), zap.String("progress", j.Progress))
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", j.Id, j.Kind, j.State, j.Subject, j.Progress,
					time.Unix(j.StartedUnix, 0).Format("01-02 15:04"))
			}
			_ = w.Flush()
			for _, j := range jobs {
				if j.Message != "" {
					fmt.Printf("%s: %s\n", j.Id, j.Message)
				}
			}
			log.Debug("poolJobs: done", zap.Int("count", len(jobs)))
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Include finished jobs (kept 14 days)")
	return cmd
}

// runJob connects, starts a job and prints what it said.
func runJob(start func(context.Context, *client.HaifyClient) (string, error)) error {
	log := zap.L()
	log.Debug("runJob: starting storage job")
	ctx, cancel := context.WithTimeout(context.Background(), nodeOpTimeout)
	defer cancel()
	c, err := newHaifyClient()
	if err != nil {
		log.Debug("runJob: newHaifyClient failed", zap.Error(err))
		return err
	}
	defer closeClient(c)
	msg, err := start(ctx, c)
	if err != nil {
		log.Debug("runJob: start failed", zap.Error(err))
		return err
	}
	fmt.Println(msg)
	log.Debug("runJob: done")
	return nil
}
