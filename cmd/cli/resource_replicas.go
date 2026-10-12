package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

func resourceRemoveReplica() *cobra.Command {
	var node string
	var yes, lost bool

	cmd := &cobra.Command{
		Use:   "remove-replica <resource> --node <node>",
		Short: "Take a diskful replica out of a running resource",
		Long: `Remove one full copy of a running resource. The inverse of add-replica, for a
node that was added to carry a resource through some maintenance and is not
wanted permanently.

The surviving replicas keep their node-ids, so none of them resyncs; only the
leaving node is torn down, and its volumes for the resource are deleted. It is refused when that node is Primary, when it is
the quorum tiebreaker or the off-site DR, or when fewer than two diskful copies
would remain — unlike a conversion, whose single-copy window closes when the
resync finishes, this is permanent.

--lost is for a node that is gone for good and will not answer. Nothing runs on
it: the survivors' configs drop it and free its slot (forget-peer), and its own
volume and config are left where they are, to be cleaned before it ever
rejoins. It is refused while the node answers over SSH, while any survivor is
still connected to it, or unless the survivors hold quorum and an UpToDate copy
without it. One remaining diskful copy is enough, since the lost one is already
gone; add a replica afterwards.

  haify resource remove-replica haify-meta --node node-d
  haify resource remove-replica db --node node3 --lost --yes`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceRemoveReplica: running command", zap.Strings("args", args), zap.String("node", node), zap.Bool("yes", yes), zap.Bool("lost", lost))
			resource := args[0]
			if node == "" {
				log.Debug("resourceRemoveReplica: --node missing")
				return fmt.Errorf("--node is required")
			}
			if !yes {
				log.Debug("resourceRemoveReplica: no --yes, asking for confirmation", zap.Bool("lost", lost))
				if lost {
					fmt.Printf("This drops %q from %q for good; if it ever comes back it must be cleaned before it rejoins. Re-run with --yes to proceed.\n", node, resource)
				} else {
					fmt.Printf("This destroys the copy of %q on %q. Re-run with --yes to proceed.\n", resource, node)
				}
				return nil
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceRemoveReplica: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			msg, err := haifyClient.RemoveReplicaOptions(ctx, resource, node, lost)
			if err != nil {
				log.Debug("resourceRemoveReplica: RemoveReplicaOptions failed", zap.Error(err))
				return fmt.Errorf("failed to remove replica: %w", err)
			}
			if lost {
				fmt.Println(msg)
			} else {
				fmt.Printf("Replica removed from %q.\n", node)
			}
			fmt.Printf("  haify resource status %s\n", resource)
			log.Debug("resourceRemoveReplica: done", zap.String("resource", resource), zap.String("node", node), zap.Bool("lost", lost))
			return nil
		},
	}
	cmd.Flags().StringVar(&node, "node", "", "Node whose replica is removed")
	cmd.Flags().BoolVar(&yes, "yes", false, "Confirm destroying that node's copy")
	cmd.Flags().BoolVar(&lost, "lost", false, "The node is gone for good: remove it from the survivors without reaching it")
	return cmd
}

func resourceAddReplica() *cobra.Command {
	var node string
	var ignoreFreeSpace, allowUnreachable bool

	cmd := &cobra.Command{
		Use:   "add-replica <resource> --node <node>",
		Short: "Add a diskful local replica to a running resource",
		Long: `Add one more full copy of a running resource, on a node that was not in the
cluster when the resource was created.

The new node joins the synchronous mesh as a peer of every existing replica and
resyncs in the background; the resource keeps serving throughout. On a resource
that also has an off-site DR the new node additionally gets its own WAN leg,
because DRBD 9 is a full mesh — a replica the DR cannot reach would silently end
replication the moment it was promoted.

  haify resource add-replica data --node node4`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceAddReplica: running command", zap.Strings("args", args), zap.String("node", node),
				zap.Bool("ignore_free_space", ignoreFreeSpace), zap.Bool("allow_unreachable", allowUnreachable))
			resource := args[0]
			if node == "" {
				log.Debug("resourceAddReplica: --node missing")
				return fmt.Errorf("--node is required")
			}

			// A full initial sync of the new copy is not quick.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceAddReplica: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			if err := haifyClient.AddReplicaWith(ctx, &haifypb.AddReplicaRequest{Resource: resource, Node: node,
				IgnoreFreeSpace: ignoreFreeSpace, AllowUnreachable: allowUnreachable}); err != nil {
				log.Debug("resourceAddReplica: AddReplicaWith failed", zap.Error(err))
				return fmt.Errorf("failed to add replica: %w", err)
			}
			fmt.Printf("Replica added on %q. Initial sync runs in the background:\n", node)
			fmt.Printf("  haify resource status %s\n", resource)
			log.Debug("resourceAddReplica: done", zap.String("resource", resource), zap.String("node", node))
			return nil
		},
	}
	cmd.Flags().StringVar(&node, "node", "", "Node that will hold the new replica")
	cmd.Flags().BoolVar(&ignoreFreeSpace, "ignore-free-space", false,
		"Add it although the node's pool has less free space than the volume (the sync writes all of it; a full pool drops the new disk)")
	cmd.Flags().BoolVar(&allowUnreachable, "allow-unreachable", false,
		"Add it while a minority of the members do not answer; they get the new config when they are back")
	return cmd
}

// resourceAddDR is deliberately a resource subcommand rather than an `ha` one:
// it changes where the data lives, not how the service fails over.
func resourceAddDR() *cobra.Command {
	var drNode, drEndpoint, egress string
	var wanPort uint32

	cmd := &cobra.Command{
		Use:   "add-dr <resource> --dr-node <node> --dr-endpoint <host-or-ip>",
		Short: "Attach an off-site asynchronous replica to a running resource",
		Long: `Attach an off-site asynchronous replica to a resource that is already running.

Off-site DR used to be a create-time-only choice, which is backwards: it is
exactly the thing you add after a service has proven it matters. This adds it
in place — the existing replicas keep their synchronous LAN mesh and a promoted
resource keeps serving while the remote copy syncs in the background.

The DR node joins over one mTLS haify-proxy leg per replica, using protocol A and
pull-ahead, so a slow or flapping WAN link cannot stall writes at the primary
site. It does not vote: quorum stays a matter for the primary site alone.

  haify resource add-dr data --dr-node dr1 --dr-endpoint 203.0.113.7
  haify resource add-dr web --dr-node dr1 --dr-endpoint dr.example.com --wan-port 6612`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceAddDR: running command", zap.Strings("args", args), zap.String("dr_node", drNode),
				zap.String("dr_endpoint", drEndpoint), zap.Uint32("wan_port", wanPort), zap.String("egress_address", egress))
			resource := args[0]
			if drNode == "" {
				log.Debug("resourceAddDR: --dr-node missing")
				return fmt.Errorf("--dr-node is required")
			}
			if drEndpoint == "" {
				log.Debug("resourceAddDR: --dr-endpoint missing")
				return fmt.Errorf("--dr-endpoint is required (the address the primary site dials)")
			}

			// The DR node must lay down its own backing volumes and run a full
			// initial sync, neither of which is quick.
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceAddDR: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			port, err := haifyClient.AddDR(ctx, resource, drNode, drEndpoint, wanPort, egress)
			if err != nil {
				log.Debug("resourceAddDR: AddDR failed", zap.Error(err))
				return fmt.Errorf("failed to add DR site: %w", err)
			}

			fmt.Printf("DR site %q attached to %q (WAN base port %d)\n", drNode, resource, port)
			fmt.Printf("Initial sync is running in the background; check it with:\n")
			fmt.Printf("  haify resource status %s\n", resource)
			log.Debug("resourceAddDR: done", zap.String("resource", resource), zap.String("dr_node", drNode), zap.Uint32("wan_port", port))
			return nil
		},
	}

	cmd.Flags().StringVar(&drNode, "dr-node", "", "Registered node that will hold the off-site replica")
	cmd.Flags().StringVar(&drEndpoint, "dr-endpoint", "", "Public address of the DR node that the primary site dials")
	cmd.Flags().Uint32Var(&wanPort, "wan-port", 0, "Base haify-proxy WAN port (one per replica from here; 0 auto-allocates)")
	cmd.Flags().StringVar(&egress, "egress-address", "", "Source address the primary site dials out from (optional)")
	return cmd
}

// resourceDRFailover promotes a WAN resource's DR node — a MANUAL disaster-recovery
// action. WAN is protocol A (async), so the DR peer can lag: promoting it may lose
// the writes still in the WAN buffer. This is never automatic (auto-promoting a
// possibly-behind secondary risks data loss); the operator invokes it knowingly.
func resourceDRFailover() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "dr-failover <resource>",
		Short: "Promote a WAN resource's DR node (manual disaster recovery)",
		Long: "Force-promote the DR node of a WAN (async) resource. Use when the primary\n" +
			"site is lost. Because replication is asynchronous, any writes still buffered\n" +
			"in the WAN link at failure time are lost. This action is deliberately manual.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceDRFailover: running command", zap.Strings("args", args), zap.Bool("yes", yes))
			resource := args[0]

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceDRFailover: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			status, err := haifyClient.ResourceStatus(ctx, resource)
			if err != nil {
				log.Debug("resourceDRFailover: ResourceStatus failed", zap.Error(err))
				return fmt.Errorf("failed to get resource status: %w", err)
			}
			if !status.GetWan() {
				log.Debug("resourceDRFailover: not a WAN resource", zap.String("resource", resource))
				return fmt.Errorf("%q is not a WAN resource; use `resource primary` for LAN promotion", resource)
			}
			drNode := status.GetDrNode()
			if drNode == "" {
				log.Debug("resourceDRFailover: no DR node recorded", zap.String("resource", resource))
				return fmt.Errorf("%q has no DR node recorded", resource)
			}
			log.Debug("resourceDRFailover: DR node found", zap.String("resource", resource), zap.String("dr_node", drNode))

			fmt.Printf("DR failover: promote %q on DR node %q.\n", resource, drNode)
			fmt.Printf("WARNING: WAN replication is asynchronous — writes still in the WAN\n")
			fmt.Printf("buffer at failure time will be LOST.\n")
			if !yes {
				log.Debug("resourceDRFailover: no --yes, stopping after the warning")
				fmt.Printf("Re-run with --yes to proceed.\n")
				return nil
			}

			// Force is required: the DR peer may not be UpToDate relative to a lost
			// primary, and a plain promote would refuse.
			if err := haifyClient.SetPrimary(ctx, resource, drNode, true); err != nil {
				log.Debug("resourceDRFailover: SetPrimary failed", zap.Error(err))
				return fmt.Errorf("DR failover failed: %w", err)
			}
			fmt.Printf("Resource %q promoted on DR node %q. Mount its volume(s) and resume service there.\n", resource, drNode)
			log.Debug("resourceDRFailover: done", zap.String("resource", resource), zap.String("dr_node", drNode))
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Confirm the (lossy) DR failover")
	return cmd
}

// resourceDiskless groups attach/detach of diskless data clients — nodes that
// mount a resource with no local replica, accessing it over the DRBD network.
func resourceDiskless() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diskless",
		Short: "Manage diskless data clients (mount a resource with no local replica)",
	}
	cmd.AddCommand(resourceDisklessAttach())
	cmd.AddCommand(resourceDisklessDetach())
	return cmd
}

func resourceDisklessAttach() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <resource> <node>",
		Short: "Attach a node as a diskless client so it can mount the resource over the network",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceDisklessAttach: running command", zap.Strings("args", args))
			resource, node := args[0], args[1]

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceDisklessAttach: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			if err := haifyClient.AttachDisklessClient(ctx, resource, node); err != nil {
				log.Debug("resourceDisklessAttach: AttachDisklessClient failed", zap.Error(err))
				return fmt.Errorf("failed to attach diskless client: %w", err)
			}
			fmt.Printf("Node '%s' attached to '%s' as a diskless client\n", node, resource)
			log.Debug("resourceDisklessAttach: done", zap.String("resource", resource), zap.String("node", node))
			return nil
		},
	}
}

func resourceDisklessDetach() *cobra.Command {
	return &cobra.Command{
		Use:   "detach <resource> <node>",
		Short: "Detach a diskless client from the resource",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceDisklessDetach: running command", zap.Strings("args", args))
			resource, node := args[0], args[1]

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceDisklessDetach: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			if err := haifyClient.DetachDisklessClient(ctx, resource, node); err != nil {
				log.Debug("resourceDisklessDetach: DetachDisklessClient failed", zap.Error(err))
				return fmt.Errorf("failed to detach diskless client: %w", err)
			}
			fmt.Printf("Node '%s' detached from '%s'\n", node, resource)
			log.Debug("resourceDisklessDetach: done", zap.String("resource", resource), zap.String("node", node))
			return nil
		},
	}
}

func resourceDRFailback() *cobra.Command {
	var node string
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "dr-failback <resource>",
		Short: "Move a WAN resource back from its DR node to the primary site after dr-failover",
		Long: "Run it again until it says done. It rejoins the primary-site nodes — what they\n" +
			"wrote after the failover is DISCARDED in favour of the DR's copy — waits for them\n" +
			"to resync from the DR, then makes the primary site Primary again. The DR must be\n" +
			"unmounted for the last step, and each primary-site node for the first.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceDRFailback: running command", zap.Strings("args", args), zap.String("node", node), zap.Duration("wait", wait))
			ctx, cancel := context.WithTimeout(context.Background(), wait+2*time.Minute)
			defer cancel()
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceDRFailback: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)
			resp, err := haifyClient.DRFailback(ctx, args[0], node, uint32(wait.Seconds()))
			if err != nil {
				log.Debug("resourceDRFailback: DRFailback failed", zap.Error(err))
				return err
			}
			log.Debug("resourceDRFailback: failback result", zap.Bool("success", resp.Success), zap.Strings("steps", resp.Steps))
			for _, s := range resp.Steps {
				fmt.Printf("  %s\n", s)
			}
			fmt.Println(resp.Message)
			if !resp.Success {
				log.Debug("resourceDRFailback: failback not finished", zap.String("message", resp.Message))
				return fmt.Errorf("failback did not finish")
			}
			log.Debug("resourceDRFailback: done")
			return nil
		},
	}
	cmd.Flags().StringVar(&node, "node", "", "Primary-site node to make Primary (default: the first)")
	cmd.Flags().DurationVar(&wait, "wait", 0, "How long to wait for the resync before returning (e.g. 10m)")
	return cmd
}
