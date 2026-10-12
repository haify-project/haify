package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func nodeSetPaths() *cobra.Command {
	var clear bool
	cmd := &cobra.Command{
		Use:   "set-paths <node> [<address>,...]",
		Short: "Give a node more replication networks, so DRBD survives losing one link",
		Long: `Each address is the node's IP on one more network replication may use, listed in
the same order on every node (the first on network 2, the second on network 3...).
Every pair of nodes then gets a DRBD path per network both are on, and a
connection stays up while any of them works. The resources the node takes part in
are rewritten and adjusted in place; connections stay up.

  haify node set-paths node1 10.98.0.1
  haify node set-paths node1 --clear`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nodeSetPaths: running command", zap.Strings("args", args), zap.Bool("clear", clear))
			var addrs []string
			switch {
			case clear && len(args) == 2:
				log.Debug("nodeSetPaths: both addresses and --clear given")
				return fmt.Errorf("give addresses or --clear, not both")
			case !clear && len(args) == 1:
				log.Debug("nodeSetPaths: neither addresses nor --clear given")
				return fmt.Errorf("give the addresses, or --clear to remove them")
			case len(args) == 2:
				for _, a := range strings.Split(args[1], ",") {
					if a = strings.TrimSpace(a); a != "" {
						addrs = append(addrs, a)
					}
				}
				log.Debug("nodeSetPaths: setting replication paths", zap.String("node", args[0]), zap.Strings("addresses", addrs))
			default:
				log.Debug("nodeSetPaths: clearing replication paths", zap.String("node", args[0]))
			}
			c, err := newHaifyClient()
			if err != nil {
				log.Debug("nodeSetPaths: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(c)
			resp, err := c.SetNodeReplicationPaths(cmd.Context(), args[0], addrs)
			if err != nil {
				log.Debug("nodeSetPaths: SetNodeReplicationPaths failed", zap.Error(err))
				return err
			}
			log.Debug("nodeSetPaths: result", zap.Bool("success", resp.Success), zap.Strings("resources", resp.Resources), zap.Strings("failed", resp.Failed))
			fmt.Println(resp.Message)
			for _, r := range resp.Resources {
				fmt.Printf("  %-24s rewritten\n", r)
			}
			for _, f := range resp.Failed {
				fmt.Printf("  FAILED %s\n", f)
			}
			if !resp.Success {
				log.Debug("nodeSetPaths: controller reported failure", zap.String("message", resp.Message))
				if len(resp.Resources) == 0 && len(resp.Failed) == 0 {
					log.Debug("nodeSetPaths: nothing was rewritten")
					return fmt.Errorf("%s", resp.Message)
				}
				return fmt.Errorf("%d resource(s) not rewritten; fix them and run resource repair on those", len(resp.Failed))
			}
			log.Debug("nodeSetPaths: done")
			return nil
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "Remove the node's extra replication addresses")
	return cmd
}
