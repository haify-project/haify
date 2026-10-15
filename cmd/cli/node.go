package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func nodeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Node management (storage nodes)",
	}

	cmd.AddCommand(nodeList())
	cmd.AddCommand(nodeLostCommand(), nodeRestoreCommand())
	cmd.AddCommand(nodeGet())
	cmd.AddCommand(nodeRegister())
	cmd.AddCommand(nodeUnregister())
	cmd.AddCommand(nodeLabel())
	cmd.AddCommand(nodeSetAddress())
	cmd.AddCommand(nodeDrain())
	cmd.AddCommand(nodeUndrain())
	cmd.AddCommand(nodeDisks())
	cmd.AddCommand(nodeSetPaths())

	return cmd
}

func nodeList() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all nodes in the cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nodeList: running command", zap.Strings("args", args))
			ctx := cmd.Context()

			// Create Haify client
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nodeList: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			// List nodes
			nodes, err := haifyClient.ListNodes(ctx)
			if err != nil {
				log.Debug("nodeList: ListNodes failed", zap.Error(err))
				return fmt.Errorf("failed to list nodes: %w", err)
			}

			if len(nodes) == 0 {
				log.Debug("nodeList: no nodes registered")
				fmt.Println("No nodes registered")
				return nil
			}

			// Print nodes in table format
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			// Writes to the command's own output stream are best-effort. The only ways
			// they fail are a closed pipe (`haify ... | head`) or a full disk, neither of
			// which this command can report anywhere the operator is still looking, and
			// treating them as errors would report a successful operation as failed.
			_, _ = fmt.Fprintln(w, "NAME\tADDRESS\tSTATE\tVERSION")

			for _, node := range nodes {
				log.Debug("nodeList: node", zap.String("node", node.Name), zap.String("address", node.Address),
					zap.String("state", node.State), zap.Int64("offline_since", node.OfflineSince))
				// Strip port from address for display
				displayAddr := node.Address
				if idx := strings.LastIndex(node.Address, ":"); idx != -1 {
					displayAddr = node.Address[:idx]
				}
				state := node.State
				if node.OfflineSince > 0 {
					state += " since " + formatLastSeen(node.OfflineSince)
				}
				version := node.Version
				if node.LocalAgent && version == "" {
					version = "local agent"
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
					node.Name,
					displayAddr,
					state,
					version)
			}

			// Flush pushes the buffered table to stdout; like the Fprint calls above it
			// is best-effort, and a write failure here says nothing about whether the
			// operation the operator asked for succeeded.
			_ = w.Flush()

			log.Debug("nodeList: done", zap.Int("count", len(nodes)))
			return nil
		},
	}

	return cmd
}

func nodeGet() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <node>",
		Short: "Get node details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nodeGet: running command", zap.Strings("args", args))
			ctx := cmd.Context()
			nodeRef := args[0]

			// Create Haify client
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nodeGet: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			foundNode, err := haifyClient.GetNode(ctx, nodeRef)
			if err != nil {
				log.Debug("nodeGet: GetNode failed", zap.Error(err))
				return fmt.Errorf("failed to get node: %w", err)
			}

			log.Debug("nodeGet: node found", zap.String("node", foundNode.Name), zap.String("address", foundNode.Address),
				zap.String("replication_address", foundNode.ReplicationAddress), zap.Strings("replication_paths", foundNode.ReplicationPaths),
				zap.String("state", foundNode.State), zap.Int("labels", len(foundNode.Labels)))
			// Print node details
			fmt.Printf("Address:   %s\n", foundNode.Address)
			if r := foundNode.ReplicationAddress; r != "" && r != foundNode.Address {
				fmt.Printf("DRBD:      %s\n", r)
			}
			if len(foundNode.ReplicationPaths) > 0 {
				fmt.Printf("DRBD also: %s\n", strings.Join(foundNode.ReplicationPaths, ", "))
			}
			fmt.Printf("Hostname:  %s\n", foundNode.Hostname)
			fmt.Printf("State:     %s\n", foundNode.State)
			fmt.Printf("Version:   %s\n", foundNode.Version)
			fmt.Printf("Last Seen: %s\n", formatLastSeen(foundNode.LastSeen))
			if foundNode.OfflineSince > 0 {
				fmt.Printf("Offline since: %s\n", formatLastSeen(foundNode.OfflineSince))
			}
			if len(foundNode.Labels) > 0 {
				fmt.Printf("Labels:    %s\n", formatLabels(foundNode.Labels))
			}

			log.Debug("nodeGet: done")
			return nil
		},
	}

	return cmd
}

// nodeLabel sets key=value labels on a node, used by placement constraints such
// as `resource create --replicas-on-different rack`. An empty value (key=)
// deletes that label.
func nodeLabel() *cobra.Command {
	var replace bool
	cmd := &cobra.Command{
		Use:   "label <node> <key=value> [<key=value>...]",
		Short: "Set labels on a node (e.g. rack=A zone=east); key= deletes a label",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nodeLabel: running command", zap.Strings("args", args), zap.Bool("replace", replace))
			nodeRef := args[0]
			labels := make(map[string]string, len(args)-1)
			for _, kv := range args[1:] {
				k, v, ok := strings.Cut(kv, "=")
				k = strings.TrimSpace(k)
				if !ok || k == "" {
					log.Debug("nodeLabel: label not in key=value form", zap.String("label", kv))
					return fmt.Errorf("invalid label %q (want key=value)", kv)
				}
				labels[k] = strings.TrimSpace(v)
				log.Debug("nodeLabel: label", zap.String("key", k), zap.Bool("delete", strings.TrimSpace(v) == ""))
			}

			ctx := cmd.Context()
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nodeLabel: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			node, err := haifyClient.SetNodeLabels(ctx, nodeRef, labels, replace)
			if err != nil {
				log.Debug("nodeLabel: SetNodeLabels failed", zap.Error(err))
				return fmt.Errorf("failed to set node labels: %w", err)
			}
			fmt.Printf("Node '%s' labels: %s\n", node.Name, formatLabels(node.Labels))
			log.Debug("nodeLabel: done", zap.String("node", node.Name), zap.Int("labels", len(node.Labels)))
			return nil
		},
	}
	cmd.Flags().BoolVar(&replace, "replace", false, "Replace all labels instead of merging")
	return cmd
}

// formatLabels renders a label map as a stable, comma-separated key=value list.
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ", ")
}

func nodeRegister() *cobra.Command {
	var name string
	var address string
	var replicationAddress string

	cmd := &cobra.Command{
		Use:   "register --name <name> --address <ip> [--replication-address <ip>]",
		Short: "Register a storage node",
		Long: "Register a storage node.\n\n" +
			"--address is the management address: the controller reaches the node there\n" +
			"over SSH to run drbdadm/LVM.\n\n" +
			"--replication-address is optional and, when given, is the address DRBD uses\n" +
			"for this node instead. Set it to put replication traffic on a dedicated NIC\n" +
			"or subnet so it does not compete with management or client traffic. Omit it\n" +
			"and replication shares the management address (the historical behavior).",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nodeRegister: running command", zap.Strings("args", args), zap.String("name", name),
				zap.String("address", address), zap.String("replication_address", replicationAddress))
			if name == "" {
				log.Debug("nodeRegister: --name missing")
				return fmt.Errorf("--name is required")
			}
			if address == "" {
				log.Debug("nodeRegister: --address missing")
				return fmt.Errorf("--address is required")
			}

			ctx := cmd.Context()

			// Create Haify client
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nodeRegister: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			// Register node
			node, err := haifyClient.RegisterNodeWithReplicationAddress(ctx, name, address, replicationAddress)
			if err != nil {
				log.Debug("nodeRegister: RegisterNodeWithReplicationAddress failed", zap.Error(err))
				return fmt.Errorf("failed to register node: %w", err)
			}

			fmt.Printf("✓ Node registered successfully\n")
			fmt.Printf("  Name:    %s\n", node.Name)
			fmt.Printf("  Address: %s\n", node.Address)
			if r := node.GetReplicationAddress(); r != "" {
				fmt.Printf("  Replication address: %s (DRBD traffic)\n", r)
			}

			log.Debug("nodeRegister: done", zap.String("node", node.Name), zap.String("address", node.Address),
				zap.String("replication_address", node.GetReplicationAddress()))
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Node name (e.g., node1)")
	cmd.Flags().StringVar(&address, "address", "", "Node management IP address, used for SSH (e.g., 192.168.1.10)")
	cmd.Flags().StringVar(&replicationAddress, "replication-address", "",
		"IP address DRBD should use for this node; empty = same as --address")

	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("address")

	return cmd
}

func nodeUnregister() *cobra.Command {
	var address string

	cmd := &cobra.Command{
		Use:   "unregister <node>",
		Short: "Unregister a storage node",
		Long: `Unregister a storage node from the cluster, by name or address.
This removes the node from the database but does not affect the node itself.

It refuses while the node still holds a replica, tiebreaker or diskless client,
is a WAN resource's DR node, or carries a gateway, and lists each resource and
role so you can move or remove them first.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nodeUnregister: running command", zap.Strings("args", args), zap.String("address", address))
			ref := address
			if len(args) == 1 {
				ref = args[0]
			}
			if ref == "" {
				log.Debug("nodeUnregister: no node named")
				return fmt.Errorf("name the node to unregister: haify node unregister <name|address>")
			}

			ctx := cmd.Context()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nodeUnregister: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			// The controller removes nodes by address; every other node command
			// takes a name, so resolve one here rather than make the operator
			// look the address up.
			node, err := haifyClient.GetNode(ctx, ref)
			if err != nil {
				log.Debug("nodeUnregister: GetNode failed", zap.Error(err))
				return fmt.Errorf("failed to find node %q: %w", ref, err)
			}

			log.Debug("nodeUnregister: node resolved", zap.String("ref", ref), zap.String("node", node.Name), zap.String("address", node.Address))
			if err := haifyClient.UnregisterNode(ctx, node.Address); err != nil {
				log.Debug("nodeUnregister: UnregisterNode failed", zap.Error(err))
				return fmt.Errorf("failed to unregister node: %w", err)
			}

			fmt.Printf("✓ Node unregistered successfully\n")
			fmt.Printf("  Name:    %s\n", node.Name)
			fmt.Printf("  Address: %s\n", node.Address)

			log.Debug("nodeUnregister: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&address, "address", "", "Node address (same as passing it as the argument)")

	return cmd
}

func nodeDrain() *cobra.Command {
	return &cobra.Command{
		Use:   "drain <node>",
		Short: "Mark a node maintenance and move every Primary resource off it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nodeDrain: running command", zap.Strings("args", args))
			node := args[0]
			ctx, cancel := context.WithTimeout(context.Background(), nodeOpTimeout)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nodeDrain: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			moved, err := haifyClient.DrainNode(ctx, node)
			if err != nil {
				log.Debug("nodeDrain: DrainNode failed", zap.String("node", node), zap.Strings("moved", moved), zap.Error(err))
				for _, r := range moved {
					fmt.Printf("  moved: %s\n", r)
				}
				return fmt.Errorf("drain incomplete: %w", err)
			}

			log.Debug("nodeDrain: node drained", zap.String("node", node), zap.Strings("moved", moved))
			if len(moved) == 0 {
				fmt.Printf("✓ Node %q drained (no active primaries to move)\n", node)
			} else {
				fmt.Printf("✓ Node %q drained — %d resource(s) moved:\n", node, len(moved))
				for _, r := range moved {
					fmt.Printf("  - %s\n", r)
				}
			}
			fmt.Printf("  Node is now in maintenance mode. Run \"haify node undrain %s\" when ready.\n", node)
			log.Debug("nodeDrain: done")
			return nil
		},
	}
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

// formatLastSeen renders a node's last-contact Unix time for a person: the
// local time and how long ago it was.
func formatLastSeen(unix int64) string {
	if unix <= 0 {
		return "never"
	}
	t := time.Unix(unix, 0)
	return fmt.Sprintf("%s (%s ago)", t.Format("2006-01-02 15:04:05 MST"), time.Since(t).Round(time.Second))
}

func nodeSetAddress() *cobra.Command {
	var replication string
	cmd := &cobra.Command{
		Use:   "set-address <node> <new-address> | <node>=<address> [<node>=<address> ...]",
		Short: "Renumber nodes: move them to new IPs everywhere Haify records one",
		Long: "Each node must already answer on its new address. The registry, /etc/hosts on\n" +
			"the nodes and the DRBD config of every resource they take part in are rewritten;\n" +
			"each resource reconnects on the new addresses.\n\n" +
			"When several nodes changed address — every node got a new DHCP lease — give\n" +
			"them all in one command (node1=10.0.0.5 node2=10.0.0.6 ...): one at a time cannot\n" +
			"work then, since each node's resources would be rewritten through peers still\n" +
			"known only by their old addresses.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nodeSetAddress: running command", zap.Strings("args", args), zap.String("replication_address", replication))
			var moves []*haifypb.NodeAddressMove
			if strings.Contains(args[0], "=") {
				log.Debug("nodeSetAddress: node=address form, moving several nodes together", zap.Int("count", len(args)))
				if replication != "" {
					log.Debug("nodeSetAddress: --replication-address given with the multi-node form")
					return fmt.Errorf("--replication-address goes with the single-node form")
				}
				for _, a := range args {
					node, addr, ok := strings.Cut(a, "=")
					if !ok || node == "" || addr == "" {
						log.Debug("nodeSetAddress: pair not in node=address form", zap.String("pair", a))
						return fmt.Errorf("%q is not node=address", a)
					}
					log.Debug("nodeSetAddress: move", zap.String("node", node), zap.String("address", addr))
					moves = append(moves, &haifypb.NodeAddressMove{Node: node, Address: addr})
				}
			} else if len(args) != 2 {
				log.Debug("nodeSetAddress: single-node form needs exactly two arguments", zap.Int("args", len(args)))
				return fmt.Errorf("give <node> <new-address>, or node=address pairs")
			}
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nodeSetAddress: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)
			var resp *haifypb.SetNodeAddressResponse
			if moves != nil {
				resp, err = haifyClient.SetNodeAddresses(cmd.Context(), moves)
			} else {
				resp, err = haifyClient.SetNodeAddress(cmd.Context(), args[0], args[1], replication)
			}
			if err != nil {
				log.Debug("nodeSetAddress: renumber call failed", zap.Bool("multi", moves != nil), zap.Error(err))
				return err
			}
			log.Debug("nodeSetAddress: renumber result", zap.Bool("success", resp.Success), zap.Strings("resources", resp.Resources),
				zap.Strings("failed", resp.Failed))
			if resp.Success || len(resp.Resources) > 0 || len(resp.Failed) > 0 {
				fmt.Println(resp.Message)
			}
			for _, r := range resp.Resources {
				fmt.Printf("  %-24s on the new address\n", r)
			}
			for _, f := range resp.Failed {
				fmt.Printf("  FAILED %s\n", f)
			}
			if !resp.Success {
				log.Debug("nodeSetAddress: controller reported failure", zap.String("message", resp.Message))
				if len(resp.Resources) == 0 && len(resp.Failed) == 0 {
					log.Debug("nodeSetAddress: nothing was renumbered")
					return fmt.Errorf("%s", resp.Message)
				}
				return fmt.Errorf("renumbered, but %d step(s) failed; fix them and run resource repair on those resources", len(resp.Failed))
			}
			log.Debug("nodeSetAddress: done")
			return nil
		},
	}
	cmd.Flags().StringVar(&replication, "replication-address", "", "Move DRBD traffic to this address too (default: keep the node's current arrangement)")
	return cmd
}
