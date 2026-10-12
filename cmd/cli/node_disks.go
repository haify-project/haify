package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/haify-project/haify/pkg/util"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// diskStateOrder puts the disks a pool can use first.
var diskStateOrder = map[string]int{"free": 0, "wipe": 1, "pool": 2, "busy": 3}

func nodeDisks() *cobra.Command {
	var node string
	cmd := &cobra.Command{
		Use:   "disks",
		Short: "List the disks on the nodes and whether a pool can use each",
		Long: `Every disk on the nodes, and what it is:

  free  empty; a pool can use it as it is
  wipe  holds only old data (a filesystem, a partition table, a stale array
        or pool label); a pool can use it with --wipe, which erases it
  pool  already in a Haify pool
  busy  in use (mounted, the system disk, another volume group, DRBD); never touched

A disk just put into a server shows up here as free.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nodeDisks: running command", zap.Strings("args", args), zap.String("node", node))
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				log.Debug("nodeDisks: newHaifyClient failed", zap.Error(err))
				return err
			}
			defer closeClient(c)
			disks, err := c.ListNodeDisks(ctx, node)
			if err != nil {
				log.Debug("nodeDisks: ListNodeDisks failed", zap.Error(err))
				return err
			}
			sort.SliceStable(disks, func(i, j int) bool {
				if disks[i].Node != disks[j].Node {
					return disks[i].Node < disks[j].Node
				}
				return diskStateOrder[disks[i].State] < diskStateOrder[disks[j].State]
			})
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NODE\tDISK\tSIZE\tSTATE\tDETAIL\tMODEL\tID")
			var free []string
			for _, d := range disks {
				log.Debug("nodeDisks: disk", zap.String("node", d.Node), zap.String("device", d.Device),
					zap.String("state", d.State), zap.Uint64("size_bytes", d.SizeBytes), zap.String("pool", d.Pool))
				detail := d.Detail
				if d.State == "pool" {
					detail = d.Pool
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.Node, d.Device, util.FormatBytes(d.SizeBytes),
					d.State, detail, strings.TrimSpace(d.Model+" "+d.Serial), d.ById)
				if d.State == "free" {
					free = append(free, d.Node+" "+d.Device)
				}
			}
			if err := w.Flush(); err != nil {
				log.Debug("nodeDisks: Flush failed", zap.Error(err))
				return err
			}
			if len(free) > 0 {
				log.Debug("nodeDisks: free disks found, printing a hint", zap.Strings("free", free))
				f := strings.Fields(free[0])
				fmt.Printf("\nTo give a free disk to a pool: haify pool add --pool <pool> --nodes %s --devices %s\n", f[0], f[1])
			}
			log.Debug("nodeDisks: done", zap.Int("count", len(disks)), zap.Int("free", len(free)))
			return nil
		},
	}
	cmd.Flags().StringVar(&node, "node", "", "Only this node (default: every online node)")
	return cmd
}
