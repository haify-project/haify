package main

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// maintenanceOrigin says who drained a node and why, "" for a node that is
// not drained or was drained by nobody on record.
func maintenanceOrigin(n *haifypb.NodeInfo) string {
	by, reason := n.GetMaintenanceBy(), n.GetMaintenanceReason()
	switch {
	case by != "" && reason != "":
		return by + ": " + reason
	case by != "":
		return by
	}
	return reason
}

func nodeDrain() *cobra.Command {
	var keepInUse bool
	var reason string
	cmd := &cobra.Command{
		Use:   "drain <node>",
		Short: "Mark a node maintenance and move every Primary resource off it",
		Long: `Take a node out of service ahead of maintenance.

The node is marked maintenance first: from then on it gets no new replicas.
Then every resource that is Primary on it is moved to another replica.

With --keep-in-use only the resources whose Primary Haify itself decides are
moved: gateways, HA services, apps and the controller's own metadata. A
resource that Proxmox VE, the Kubernetes CSI driver, OpenStack Cinder, XCP-ng
or OpenNebula keeps Primary for a VM or a Pod, and any device that is open,
stays where it is and is listed; it moves when what uses it leaves the node.
Run the command again without the option once the node is empty.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nodeDrain: running command", zap.Strings("args", args), zap.Bool("keep_in_use", keepInUse), zap.String("reason", reason))
			node := args[0]
			ctx, cancel := context.WithTimeout(context.Background(), nodeOpTimeout)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nodeDrain: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			resp, err := haifyClient.DrainNodeWith(ctx, &haifypb.DrainNodeRequest{Name: node, KeepInUse: keepInUse, Reason: reason})
			moved, left := resp.GetResourcesMoved(), resp.GetResourcesLeft()
			if err != nil {
				log.Debug("nodeDrain: DrainNode failed", zap.String("node", node), zap.Strings("moved", moved), zap.Error(err))
				for _, r := range moved {
					fmt.Printf("  moved: %s\n", r)
				}
				for _, l := range left {
					fmt.Printf("  left:  %s (%s)\n", l.GetResource(), l.GetReason())
				}
				return fmt.Errorf("drain incomplete: %w", err)
			}

			log.Debug("nodeDrain: node drained", zap.String("node", node), zap.Strings("moved", moved), zap.Int("left", len(left)))
			if len(moved) == 0 {
				fmt.Printf("✓ Node %q drained (no active primaries to move)\n", node)
			} else {
				fmt.Printf("✓ Node %q drained — %d resource(s) moved:\n", node, len(moved))
				for _, r := range moved {
					fmt.Printf("  - %s\n", r)
				}
			}
			if len(left) > 0 {
				fmt.Printf("  %d resource(s) are still Primary on it and were left where they are:\n", len(left))
				for _, l := range left {
					fmt.Printf("  - %s: %s\n", l.GetResource(), l.GetReason())
				}
			}
			fmt.Printf("  Node is now in maintenance mode. Run \"haify node undrain %s\" when ready.\n", node)
			log.Debug("nodeDrain: done")
			return nil
		},
	}
	cmd.Flags().BoolVar(&keepInUse, "keep-in-use", false,
		"Move only what Haify itself keeps Primary; leave resources a hypervisor or Kubernetes holds, and open devices, where they are")
	cmd.Flags().StringVar(&reason, "reason", "", "Why the node is drained, shown in the node list")
	return cmd
}

func nodeUndrain() *cobra.Command {
	return &cobra.Command{
		Use:   "undrain <node>",
		Short: "Return a drained node to active service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nodeUndrain: running command", zap.Strings("args", args))
			node := args[0]
			ctx, cancel := context.WithTimeout(context.Background(), nodeOpTimeout)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nodeUndrain: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			if err := haifyClient.UndrainNode(ctx, node); err != nil {
				log.Debug("nodeUndrain: UndrainNode failed", zap.Error(err))
				return fmt.Errorf("undrain failed: %w", err)
			}

			fmt.Printf("✓ Node %q returned to service\n", node)
			log.Debug("nodeUndrain: done")
			return nil
		},
	}
}
