package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	v1 "github.com/haify-project/haify/api/proto/v1"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func gatewayNFS() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nfs",
		Short: "NFS gateway management",
	}

	cmd.AddCommand(nfsCreate())
	cmd.AddCommand(nfsList())
	cmd.AddCommand(nfsExportCommand())
	cmd.AddCommand(nfsMount())

	return cmd
}

func nfsCreate() *cobra.Command {
	var resource, serviceIP, exportPath, fsType string
	var allowedIPs []string

	cmd := &cobra.Command{
		Use:   "create --resource <name> --service-ip <ip/cidr> --export-path <path>",
		Short: "Create NFS gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nfsCreate: running command", zap.Strings("args", args), zap.String("resource", resource), zap.String("service_ip", serviceIP), zap.String("export_path", exportPath), zap.Strings("allowed_ips", allowedIPs), zap.String("fs_type", fsType))
			ctx := cmd.Context()

			if resource == "" {
				log.Debug("nfsCreate: returning early", zap.String("condition", "resource == \"\""))
				return fmt.Errorf("--resource is required")
			}
			if serviceIP == "" {
				log.Debug("nfsCreate: returning early", zap.String("condition", "serviceIP == \"\""))
				return fmt.Errorf("--service-ip is required")
			}
			if exportPath == "" {
				log.Debug("nfsCreate: returning early", zap.String("condition", "exportPath == \"\""))
				return fmt.Errorf("--export-path is required")
			}

			// Create Haify client
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nfsCreate: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			// Create NFS gateway
			req := &v1.CreateNFSGatewayRequest{
				Resource:   resource,
				ServiceIp:  serviceIP,
				ExportPath: exportPath,
				AllowedIps: allowedIPs,
				FsType:     fsType,
			}

			if req.FsType == "" {
				log.Debug("nfsCreate: no filesystem type given, using ext4")
				req.FsType = "ext4"
			}

			resp, err := haifyClient.CreateNFSGateway(ctx, req)
			if err != nil {
				log.Debug("nfsCreate: CreateNFSGateway failed", zap.Error(err))
				return fmt.Errorf("failed to create NFS gateway: %w", err)
			}

			if !resp.Success {
				log.Debug("nfsCreate: controller reported failure", zap.String("message", resp.Message))
				return fmt.Errorf("failed to create NFS gateway: %s", resp.Message)
			}

			fmt.Printf("✓ NFS gateway created successfully\n")
			fmt.Printf("  Resource:     %s\n", resource)
			fmt.Printf("  Service IP:   %s\n", serviceIP)
			fmt.Printf("  Export Path:  %s\n", exportPath)
			fmt.Printf("  Config Path:  %s\n", resp.ConfigPath)
			fmt.Printf("\nNext steps:\n")
			fmt.Printf("  1. Check gateway status: haify gateway list\n")
			fmt.Printf("  2. Mount on client: sudo mount -t nfs %s:%s /mnt\n", gatewayServiceHost(serviceIP), gatewayExportDirectory(resource, exportPath))

			log.Debug("nfsCreate: done", zap.String("resource", resource), zap.String("config_path", resp.ConfigPath))
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	cmd.Flags().StringVar(&serviceIP, "service-ip", "", "Service IP (e.g., 192.168.1.200/24)")
	cmd.Flags().StringVar(&exportPath, "export-path", "", "Export path (e.g., /data)")
	cmd.Flags().StringSliceVar(&allowedIPs, "allowed-ips", []string{}, "Allowed client IPs (e.g., 192.168.1.0/24)")
	cmd.Flags().StringVar(&fsType, "fs-type", "ext4", "Filesystem type (ext4, xfs, btrfs)")

	_ = cmd.MarkFlagRequired("resource")
	_ = cmd.MarkFlagRequired("service-ip")
	_ = cmd.MarkFlagRequired("export-path")

	return cmd
}

func nfsList() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List NFS gateways",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nfsList: running command", zap.Strings("args", args))
			ctx := cmd.Context()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nfsList: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			gateways, err := haifyClient.ListGateways(ctx)
			if err != nil {
				log.Debug("nfsList: ListGateways failed", zap.Error(err))
				return fmt.Errorf("failed to list gateways: %w", err)
			}

			// Filter only NFS gateways
			var nfsGateways []*v1.GatewayInfo
			for _, gw := range gateways {
				if gw.Type == "nfs" {
					nfsGateways = append(nfsGateways, gw)
				}
			}
			log.Debug("nfsList: filtered NFS gateways", zap.Int("total", len(gateways)), zap.Int("nfs", len(nfsGateways)))

			if len(nfsGateways) == 0 {
				log.Debug("nfsList: returning early", zap.String("condition", "len(nfsGateways) == 0"))
				fmt.Println("No NFS gateways configured")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tTYPE\tRESOURCE\tSTATE")

			for _, gw := range nfsGateways {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
					gw.Id, gw.Type, gw.Resource, gw.State)
			}

			_ = w.Flush()

			log.Debug("nfsList: done")
			return nil
		},
	}

	return cmd
}
