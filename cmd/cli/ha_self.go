package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// haSelfCommand manages high availability of the controller itself: its
// metadata database lives on a DRBD resource and drbd-reactor decides which
// node runs the controller, reachable through a VIP.
func haSelfCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "self",
		Short: "Manage high availability of the Haify controller itself",
	}

	cmd.AddCommand(haSelfEnable())
	cmd.AddCommand(haSelfStatus())
	cmd.AddCommand(haSelfDisable())

	return cmd
}

func haSelfEnable() *cobra.Command {
	var vip string
	var pool string
	var sizeGB uint32
	var port uint32
	var nodes string

	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Make the Haify controller highly available on its own DRBD + drbd-reactor machinery",
		Long: `Provisions a small DRBD resource (haify-meta) for the controller database,
distributes the controller to all nodes, and hands management over to
drbd-reactor: the node holding the DRBD Primary mounts the database, brings
up the VIP, and runs the controller. On node failure the controller fails
over automatically.

The current controller restarts under reactor management during the handoff,
so this command's connection drops by design. Track progress with
'haify ha self status' (against the VIP) or the handoff log on the node.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("haSelfEnable: running command", zap.Strings("args", args), zap.String("vip", vip), zap.String("pool", pool),
				zap.Uint32("size_gb", sizeGB), zap.Uint32("port", port), zap.String("nodes", nodes))
			if vip == "" {
				log.Debug("haSelfEnable: returning early", zap.String("condition", "vip == \"\""))
				return fmt.Errorf("--vip is required (CIDR, e.g. 192.168.1.50/24)")
			}
			if pool == "" {
				log.Debug("haSelfEnable: returning early", zap.String("condition", "pool == \"\""))
				return fmt.Errorf("--pool is required")
			}

			// Resource creation + artifact distribution take a while.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("haSelfEnable: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			var nodeList []string
			if nodes != "" {
				nodeList = strings.Split(nodes, ",")
				log.Debug("haSelfEnable: limiting to nodes", zap.Strings("nodes", nodeList))
			} else {
				log.Debug("haSelfEnable: no --nodes, using every registered node")
			}

			resource, handoffLog, err := haifyClient.EnableSelfHa(ctx, vip, pool, sizeGB, port, nodeList)
			if err != nil {
				log.Debug("haSelfEnable: EnableSelfHa failed", zap.Error(err))
				return fmt.Errorf("failed to enable self-HA: %w", err)
			}

			fmt.Printf("Self-HA handoff started\n")
			fmt.Printf("  Resource:    %s\n", resource)
			fmt.Printf("  VIP:         %s\n", vip)
			fmt.Printf("  Handoff log: %s (on the controller node)\n", handoffLog)
			fmt.Printf("\nThe controller is restarting under drbd-reactor management.\n")
			vipAddr := strings.Split(vip, "/")[0]
			fmt.Printf("Verify with:  haify -c %s:3374 ha self status\n", vipAddr)

			log.Debug("haSelfEnable: done", zap.String("resource", resource), zap.String("handoff_log", handoffLog))
			return nil
		},
	}

	cmd.Flags().StringVar(&vip, "vip", "", "Virtual IP for the controller (CIDR, required)")
	cmd.Flags().StringVar(&pool, "pool", "", "Storage pool backing the metadata resource (required)")
	cmd.Flags().Uint32Var(&sizeGB, "size", 1, "Metadata resource size in GB")
	cmd.Flags().Uint32Var(&port, "port", 7999, "DRBD port for the metadata resource")
	cmd.Flags().StringVar(&nodes, "nodes", "", "Target nodes (comma-separated, default: all registered nodes)")

	return cmd
}

func haSelfStatus() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show controller self-HA status",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("haSelfStatus: running command", zap.Strings("args", args))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("haSelfStatus: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			status, err := haifyClient.GetSelfHaStatus(ctx)
			if err != nil {
				log.Debug("haSelfStatus: GetSelfHaStatus failed", zap.Error(err))
				return fmt.Errorf("failed to get self-HA status: %w", err)
			}

			if !status.Enabled {
				log.Debug("haSelfStatus: returning early", zap.String("condition", "!status.Enabled"))
				fmt.Println("Controller self-HA: disabled (standalone controller)")
				return nil
			}

			log.Debug("haSelfStatus: self-HA enabled", zap.String("resource", status.Resource), zap.String("vip", status.VIP),
				zap.Strings("nodes", status.Nodes), zap.String("active_node", status.ActiveNode))
			fmt.Println("Controller self-HA: enabled")
			fmt.Printf("  Resource:    %s\n", status.Resource)
			fmt.Printf("  VIP:         %s\n", status.VIP)
			fmt.Printf("  Nodes:       %s\n", strings.Join(status.Nodes, ", "))
			if status.ActiveNode != "" {
				fmt.Printf("  Active node: %s\n", status.ActiveNode)
			} else {
				fmt.Printf("  Active node: unknown\n")
			}
			fmt.Printf("\nDetails: haify ha status %s\n", status.Resource)

			log.Debug("haSelfStatus: done")
			return nil
		},
	}
}

func haSelfDisable() *cobra.Command {
	var node string

	cmd := &cobra.Command{
		Use:   "disable",
		Short: "Revert the controller to standalone operation on one node",
		Long: `Removes drbd-reactor management of the controller, copies the database
back to the node-local path on the given node, and re-enables the controller
as a normal systemd service there. The haify-meta resource is kept and can be
removed afterwards with 'haify resource delete haify-meta'.

The managed controller stops during this operation, so this command's
connection drops by design.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("haSelfDisable: running command", zap.Strings("args", args), zap.String("node", node))
			if node == "" {
				log.Debug("haSelfDisable: returning early", zap.String("condition", "node == \"\""))
				return fmt.Errorf("--node is required (the node that keeps running the controller)")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("haSelfDisable: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			if err := haifyClient.DisableSelfHa(ctx, node); err != nil {
				log.Debug("haSelfDisable: DisableSelfHa failed", zap.Error(err))
				return fmt.Errorf("failed to disable self-HA: %w", err)
			}

			fmt.Printf("Self-HA disable started\n")
			fmt.Printf("  Controller will restart standalone on: %s\n", node)
			fmt.Printf("\nVerify with:  haify -c %s:3374 ha self status\n", node)

			log.Debug("haSelfDisable: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&node, "node", "", "Node that keeps running the standalone controller (required)")

	return cmd
}
