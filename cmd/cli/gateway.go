package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func gatewayCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Gateway management (iSCSI, NFS, NVMe-oF)",
	}

	cmd.AddCommand(gatewayISCSI())
	cmd.AddCommand(gatewayNFS())
	cmd.AddCommand(gatewayNVMe())
	cmd.AddCommand(gatewaySMB())
	cmd.AddCommand(gatewayList())
	cmd.AddCommand(gatewayGet())
	cmd.AddCommand(gatewayStatus())
	cmd.AddCommand(gatewayDelete())
	cmd.AddCommand(gatewayStart())
	cmd.AddCommand(gatewayStop())

	return cmd
}

func gatewayList() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all gateways",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("gatewayList: running command", zap.Strings("args", args))
			ctx := cmd.Context()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("gatewayList: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			gateways, err := haifyClient.ListGateways(ctx)
			if err != nil {
				log.Debug("gatewayList: ListGateways failed", zap.Error(err))
				return fmt.Errorf("failed to list gateways: %w", err)
			}

			if len(gateways) == 0 {
				log.Debug("gatewayList: returning early", zap.String("condition", "len(gateways) == 0"))
				fmt.Println("No gateways configured")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tTYPE\tRESOURCE\tSTATE")

			for _, gw := range gateways {
				log.Debug("gatewayList: gateway", zap.String("id", gw.Id), zap.String("type", gw.Type), zap.String("resource", gw.Resource), zap.String("state", gw.State))
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
					gw.Id, gw.Type, gw.Resource, gw.State)
			}

			_ = w.Flush()

			log.Debug("gatewayList: done", zap.Int("count", len(gateways)))
			return nil
		},
	}

	return cmd
}

func gatewayDelete() *cobra.Command {
	var resource string

	cmd := &cobra.Command{
		Use:   "delete --resource <name>",
		Short: "Delete a gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("gatewayDelete: running command", zap.Strings("args", args), zap.String("resource", resource))
			ctx := cmd.Context()

			if resource == "" {
				log.Debug("gatewayDelete: returning early", zap.String("condition", "resource == \"\""))
				return fmt.Errorf("--resource is required")
			}

			// Create Haify client
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("gatewayDelete: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			// Delete gateway
			err = haifyClient.DeleteGateway(ctx, resource)
			if err != nil {
				log.Debug("gatewayDelete: DeleteGateway failed", zap.Error(err))
				return fmt.Errorf("failed to delete gateway: %w", err)
			}

			fmt.Printf("✓ Gateway deleted successfully\n")
			fmt.Printf("  Resource: %s\n", resource)

			log.Debug("gatewayDelete: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	_ = cmd.MarkFlagRequired("resource")

	return cmd
}

func gatewayStart() *cobra.Command {
	var resource string

	cmd := &cobra.Command{
		Use:   "start --resource <name>",
		Short: "Start a gateway (activate resources and services)",
		Long: `Start a gateway by promoting the DRBD resource and starting all services.
This is typically handled automatically by drbd-reactor.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("gatewayStart: running command", zap.Strings("args", args), zap.String("resource", resource))
			ctx := cmd.Context()

			if resource == "" {
				log.Debug("gatewayStart: returning early", zap.String("condition", "resource == \"\""))
				return fmt.Errorf("--resource is required")
			}

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("gatewayStart: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.StartGateway(ctx, resource)
			if err != nil {
				log.Debug("gatewayStart: StartGateway failed", zap.Error(err))
				return fmt.Errorf("failed to start gateway: %w", err)
			}

			fmt.Printf("✓ Gateway started successfully\n")
			fmt.Printf("  Resource: %s\n", resource)
			log.Debug("gatewayStart: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	_ = cmd.MarkFlagRequired("resource")

	return cmd
}

func gatewayStop() *cobra.Command {
	var resource string

	cmd := &cobra.Command{
		Use:   "stop --resource <name>",
		Short: "Stop a gateway (demote resources and stop services)",
		Long: `Stop a gateway by demoting the DRBD resource and stopping all services.
This is typically handled automatically by drbd-reactor.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("gatewayStop: running command", zap.Strings("args", args), zap.String("resource", resource))
			ctx := cmd.Context()

			if resource == "" {
				log.Debug("gatewayStop: returning early", zap.String("condition", "resource == \"\""))
				return fmt.Errorf("--resource is required")
			}

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("gatewayStop: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.StopGateway(ctx, resource)
			if err != nil {
				log.Debug("gatewayStop: StopGateway failed", zap.Error(err))
				return fmt.Errorf("failed to stop gateway: %w", err)
			}

			fmt.Printf("✓ Gateway stopped successfully\n")
			fmt.Printf("  Resource: %s\n", resource)
			log.Debug("gatewayStop: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	_ = cmd.MarkFlagRequired("resource")

	return cmd
}
