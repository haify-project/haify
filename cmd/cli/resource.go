package main

import (
	"context"
	"fmt"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/client"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// formatSize formats a size in GB to human-readable string
func formatSize(sizeGB uint64) string {
	if sizeGB == 0 {
		return "0 GB"
	}
	if sizeGB < 1 {
		return "< 1 GB"
	}
	return fmt.Sprintf("%d GB", sizeGB)
}

func resourceCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resource",
		Short: "Resource management (DRBD resources with multiple volumes)",
	}

	cmd.AddCommand(resourceCreate())
	cmd.AddCommand(resourceAdopt())
	cmd.AddCommand(resourceGet())
	cmd.AddCommand(resourceDelete())
	cmd.AddCommand(resourceRenameCommand())
	cmd.AddCommand(resourceShip())
	cmd.AddCommand(resourceMoveReplicaCommand())
	cmd.AddCommand(resourceMoveVolumeCommand())
	cmd.AddCommand(resourceList())
	cmd.AddCommand(resourceAddVolume())
	cmd.AddCommand(resourceRemoveVolume())
	cmd.AddCommand(resourceResizeVolume())
	cmd.AddCommand(resourceSetOptions())
	cmd.AddCommand(resourceGetOptions())
	cmd.AddCommand(resourcePrimary())
	cmd.AddCommand(resourceAddReplica())
	cmd.AddCommand(resourceRemoveReplica())
	cmd.AddCommand(resourceAddDR())
	cmd.AddCommand(resourceDRFailover())
	cmd.AddCommand(resourceDRFailback())
	cmd.AddCommand(resourceVerify())
	cmd.AddCommand(resourceSecondary())
	cmd.AddCommand(resourceDualPrimary())
	cmd.AddCommand(resourceFs())
	cmd.AddCommand(resourceStatus())
	cmd.AddCommand(resourceMount())
	cmd.AddCommand(resourceUnmount())
	cmd.AddCommand(resourcePromote())
	cmd.AddCommand(resourceDemote())
	cmd.AddCommand(resourceDiskless())
	cmd.AddCommand(resourceSnapshot())
	cmd.AddCommand(resourceRepair())
	cmd.AddCommand(resourceLabel())
	cmd.AddCommand(resourceTLSCommand())
	cmd.AddCommand(resourceProfileCommand())
	cmd.AddCommand(resourceSetProfile())

	return cmd
}

func resourceRepair() *cobra.Command {
	return &cobra.Command{
		Use:   "repair <resource>",
		Short: "Bring every node's copy of a resource's DRBD config back into agreement",
		Long: "Rewrite a resource's DRBD config on every participant — diskful replicas, " +
			"quorum tiebreakers and diskless clients — so they agree on the resource's " +
			"volumes, then apply it with `drbdadm adjust`.\n\n" +
			"Use it when a tiebreaker or diskless client of a multi-volume resource stays " +
			"in `connection:Connecting` and its kernel log says a packet was received " +
			"\"for volume N, which is not configured locally\". Gateways created on a " +
			"cluster with a tiebreaker before this was fixed are in that state.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceRepair: running command", zap.Strings("args", args))
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceRepair: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)
			if err := haifyClient.RepairResource(ctx, args[0]); err != nil {
				log.Debug("resourceRepair: RepairResource failed", zap.Error(err))
				return fmt.Errorf("failed to repair %s: %w", args[0], err)
			}
			fmt.Printf("Config for '%s' reconciled on every node and adjusted\n", args[0])
			log.Debug("resourceRepair: done")
			return nil
		},
	}
}

func resourceGet() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Get resource details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceGet: running command", zap.Strings("args", args))
			name := args[0]

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceGet: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			resource, err := haifyClient.GetResource(ctx, name)
			if err != nil {
				log.Debug("resourceGet: GetResource failed", zap.Error(err))
				return fmt.Errorf("failed to get resource: %w", err)
			}

			log.Debug("resourceGet: resource", zap.String("resource", resource.Name), zap.Uint32("port", resource.Port),
				zap.Strings("nodes", resource.Nodes), zap.Strings("tiebreakers", resource.DisklessNodes),
				zap.Strings("diskless_clients", resource.DisklessClients), zap.Int("volumes", len(resource.Volumes)),
				zap.Bool("encrypted", resource.Encrypted), zap.Bool("quorum_risk", resource.QuorumRisk))
			fmt.Printf("Resource: %s\n", resource.Name)
			fmt.Printf("  Port:     %d\n", resource.Port)
			fmt.Printf("  Protocol: %s\n", resource.Protocol)
			if resource.Encrypted {
				fmt.Printf("  Encrypted: LUKS2 at rest (replication traffic is NOT encrypted)\n")
			}
			fmt.Printf("  Profile:  %s\n", displayValue(resource.Profile))
			fmt.Printf("  Labels:   %s\n", formatLabels(resource.Labels))
			fmt.Printf("  Nodes:\n")
			for _, node := range resource.Nodes {
				state := "Unknown"
				diskState := ""
				if ns, ok := resource.NodeStates[node]; ok {
					state = ns.Role
					if ns.DiskState != "" {
						diskState = fmt.Sprintf(", disk: %s", ns.DiskState)
					}
				}
				log.Debug("resourceGet: node", zap.String("node", node), zap.String("role", state), zap.String("disk_state", diskState))
				fmt.Printf("    %s: %s%s\n", node, state, diskState)
			}
			for _, node := range resource.DisklessNodes {
				fmt.Printf("    %s: diskless (quorum tiebreaker)\n", node)
			}
			for _, node := range resource.DisklessClients {
				fmt.Printf("    %s: diskless (data client)\n", node)
			}
			if resource.QuorumRisk {
				fmt.Printf("  Quorum:   ⚠ 2-node, no tiebreaker — a single node failure suspends I/O\n")
			}
			if len(resource.Volumes) > 0 {
				fmt.Printf("  Volumes:\n")
				for _, vol := range resource.Volumes {
					fmt.Printf("    Volume %d: %s (%s)\n", vol.VolumeId, vol.Device, formatSize(vol.SizeGb))
				}
			}

			log.Debug("resourceGet: done")
			return nil
		},
	}

	return cmd
}

func resourceDelete() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a resource",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceDelete: running command", zap.Strings("args", args))
			name := args[0]

			ctx, cancel := context.WithTimeout(context.Background(), nodeOpTimeout)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceDelete: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.DeleteResource(ctx, name)
			if err != nil {
				log.Debug("resourceDelete: DeleteResource failed", zap.Error(err))
				return fmt.Errorf("failed to delete resource: %w", err)
			}

			fmt.Printf("Resource '%s' deleted successfully\n", name)
			log.Debug("resourceDelete: done")
			return nil
		},
	}

	return cmd
}

func resourceList() *cobra.Command {
	var profile string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all resources",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceList: running command", zap.Strings("args", args), zap.String("profile", profile))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceList: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			var resources []*haifypb.ResourceInfo
			if profile != "" {
				log.Debug("resourceList: listing the profile's members", zap.String("profile", profile))
				resources, err = haifyClient.ListProfileMembers(ctx, profile)
			} else {
				log.Debug("resourceList: listing every resource")
				resources, err = haifyClient.ListResources(ctx)
			}
			if err != nil {
				log.Debug("resourceList: listing failed", zap.Error(err))
				return fmt.Errorf("failed to list resources: %w", err)
			}

			if len(resources) == 0 {
				log.Debug("resourceList: no resources")
				fmt.Println("No resources found")
				return nil
			}

			for _, r := range resources {
				log.Debug("resourceList: resource", zap.String("resource", r.Name), zap.Strings("nodes", r.Nodes),
					zap.Bool("quorum_risk", r.QuorumRisk), zap.String("fault_domain_risk", r.FaultDomainRisk))
				line := fmt.Sprintf("%s (port=%d, protocol=%s, nodes=%v)", r.Name, r.Port, r.Protocol, r.Nodes)
				line += fmt.Sprintf(" profile=%s labels=%s", displayValue(r.Profile), formatLabels(r.Labels))
				if len(r.DisklessNodes) > 0 {
					line += fmt.Sprintf(" tiebreaker=%v", r.DisklessNodes)
				}
				if len(r.DisklessClients) > 0 {
					line += fmt.Sprintf(" clients=%v", r.DisklessClients)
				}
				if r.QuorumRisk {
					line += " ⚠quorum-risk"
				}
				if r.FaultDomainRisk != "" {
					line += " ⚠one-failure-domain(" + r.FaultDomainRisk + ")"
				}
				fmt.Println(line)
			}

			log.Debug("resourceList: done", zap.Int("count", len(resources)))
			return nil
		},
	}

	cmd.Flags().StringVar(&profile, "profile", "", "Only the resources in this profile")
	return cmd
}

// newResourceGRPCClient dials the raw generated client for the commands that
// need request fields pkg/client does not expose. It must honour the same
// token and TLS settings as newHaifyClient.
func newResourceGRPCClient() (haifypb.HaifyControllerClient, *grpc.ClientConn, error) {
	log := zap.L()
	log.Debug("newResourceGRPCClient: dialing controller", zap.String("controller", controllerAddr))
	transport := insecure.NewCredentials()
	if tlsOpts := client.ResolveTLS(tlsFlags); tlsOpts.Active() {
		log.Debug("newResourceGRPCClient: TLS active", zap.String("ca_cert", tlsOpts.CACert), zap.String("server_name", tlsOpts.ServerName), zap.Bool("insecure", tlsOpts.Insecure))
		creds, err := tlsOpts.Credentials()
		if err != nil {
			log.Debug("newResourceGRPCClient: Credentials failed", zap.Error(err))
			return nil, nil, err
		}
		transport = creds
	} else {
		log.Debug("newResourceGRPCClient: TLS not active, plaintext")
	}
	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(transport),
		grpc.WithChainUnaryInterceptor(client.TraceUnaryInterceptor()),
		grpc.WithChainStreamInterceptor(client.TraceStreamInterceptor()),
	}
	if token := client.ResolveToken(tokenFlag); token != "" {
		log.Debug("newResourceGRPCClient: sending a bearer token")
		dialOpts = append(dialOpts, grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
			return invoker(ctx, method, req, reply, cc, opts...)
		}))
	}
	conn, err := grpc.NewClient(controllerAddr, dialOpts...)
	if err != nil {
		log.Debug("newResourceGRPCClient: NewClient failed", zap.Error(err))
		return nil, nil, err
	}
	log.Debug("newResourceGRPCClient: client ready", zap.String("controller", controllerAddr))
	return haifypb.NewHaifyControllerClient(conn), conn, nil
}

func displayValue(value string) string {
	if value == "" {
		return "(none)"
	}
	return value
}
