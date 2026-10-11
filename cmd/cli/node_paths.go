package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
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
			var addrs []string
			switch {
			case clear && len(args) == 2:
				return fmt.Errorf("give addresses or --clear, not both")
			case !clear && len(args) == 1:
				return fmt.Errorf("give the addresses, or --clear to remove them")
			case len(args) == 2:
				for _, a := range strings.Split(args[1], ",") {
					if a = strings.TrimSpace(a); a != "" {
						addrs = append(addrs, a)
					}
				}
			}
			c, err := newHaifyClient()
			if err != nil {
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(c)
			resp, err := c.SetNodeReplicationPaths(cmd.Context(), args[0], addrs)
			if err != nil {
				return err
			}
			fmt.Println(resp.Message)
			for _, r := range resp.Resources {
				fmt.Printf("  %-24s rewritten\n", r)
			}
			for _, f := range resp.Failed {
				fmt.Printf("  FAILED %s\n", f)
			}
			if !resp.Success {
				if len(resp.Resources) == 0 && len(resp.Failed) == 0 {
					return fmt.Errorf("%s", resp.Message)
				}
				return fmt.Errorf("%d resource(s) not rewritten; fix them and run resource repair on those", len(resp.Failed))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "Remove the node's extra replication addresses")
	return cmd
}
