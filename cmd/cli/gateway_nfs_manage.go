package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/gateway"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func gatewayServiceHost(serviceIP string) string {
	log := zap.L()
	serviceIP = strings.TrimSpace(serviceIP)
	if serviceIP == "" {
		log.Debug("gatewayServiceHost: no service IP")
		return ""
	}

	ip, _, err := net.ParseCIDR(serviceIP)
	if err == nil {
		log.Debug("gatewayServiceHost: host from CIDR", zap.String("service_ip", serviceIP), zap.String("host", ip.String()))
		return ip.String()
	}

	host, _, found := strings.Cut(serviceIP, "/")
	if found {
		log.Debug("gatewayServiceHost: host cut at slash, not a valid CIDR", zap.String("service_ip", serviceIP), zap.String("host", host), zap.Error(err))
		return host
	}
	log.Debug("gatewayServiceHost: service IP used as host", zap.String("host", serviceIP))
	return serviceIP
}

func gatewayExportDirectory(resource, exportPath string) string {
	log := zap.L()
	// The same rule the gateway exports by. A copy of it here assumed every
	// export lived under the gateway base directory, so an absolute
	// --export-path got a mount hint for a directory that did not exist.
	dir, err := gateway.ResolveNFSExportPath(resource, exportPath)
	if err != nil {
		log.Debug("gatewayExportDirectory: export path not resolvable, using the gateway base directory",
			zap.String("resource", resource), zap.String("export_path", exportPath), zap.Error(err))
		return filepath.Join(gateway.DefaultExportBasePath, resource)
	}
	log.Debug("gatewayExportDirectory: resolved", zap.String("resource", resource), zap.String("export_path", exportPath), zap.String("dir", dir))
	return dir
}

func nfsExportCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Manage NFS exports",
	}

	cmd.AddCommand(nfsExportAdd())
	cmd.AddCommand(nfsExportRemove())
	cmd.AddCommand(nfsExportList())
	cmd.AddCommand(nfsExportQuota())

	return cmd
}

func nfsExportAdd() *cobra.Command {
	var resource, exportPath, clientSpec, exportOptions string
	var fsid int32

	cmd := &cobra.Command{
		Use:   "add --resource <name> --path <path>",
		Short: "Add an export to an NFS gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nfsExportAdd: running command", zap.Strings("args", args), zap.String("resource", resource), zap.String("export_path", exportPath), zap.Int32("fsid", fsid), zap.String("client", clientSpec), zap.String("options", exportOptions))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nfsExportAdd: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.AddNFSExport(cmd.Context(), resource, exportPath, fsid, clientSpec, exportOptions)
			if err != nil {
				log.Debug("nfsExportAdd: AddNFSExport failed", zap.Error(err))
				return fmt.Errorf("failed to add NFS export: %w", err)
			}

			fmt.Printf("NFS export added to '%s': %s\n", resource, gatewayExportDirectory(resource, exportPath))
			log.Debug("nfsExportAdd: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	cmd.Flags().StringVar(&exportPath, "path", "", "Export path")
	cmd.Flags().Int32Var(&fsid, "fsid", 0, "Filesystem ID (optional)")
	cmd.Flags().StringVar(&clientSpec, "client", "", "Client spec (e.g. 10.0.0.0/8)")
	cmd.Flags().StringVar(&exportOptions, "options", "", "Export options")
	_ = cmd.MarkFlagRequired("resource")
	_ = cmd.MarkFlagRequired("path")

	return cmd
}

func nfsExportRemove() *cobra.Command {
	var resource, exportPath string

	cmd := &cobra.Command{
		Use:   "remove --resource <name> --path <path>",
		Short: "Remove an export from an NFS gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nfsExportRemove: running command", zap.Strings("args", args), zap.String("resource", resource), zap.String("export_path", exportPath))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nfsExportRemove: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.RemoveNFSExport(cmd.Context(), resource, exportPath)
			if err != nil {
				log.Debug("nfsExportRemove: RemoveNFSExport failed", zap.Error(err))
				return fmt.Errorf("failed to remove NFS export: %w", err)
			}

			fmt.Printf("NFS export removed from '%s': %s\n", resource, gatewayExportDirectory(resource, exportPath))
			log.Debug("nfsExportRemove: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	cmd.Flags().StringVar(&exportPath, "path", "", "Export path")
	_ = cmd.MarkFlagRequired("resource")
	_ = cmd.MarkFlagRequired("path")

	return cmd
}

func nfsExportList() *cobra.Command {
	var resource string

	cmd := &cobra.Command{
		Use:   "list --resource <name>",
		Short: "List exports on an NFS gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nfsExportList: running command", zap.Strings("args", args), zap.String("resource", resource))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nfsExportList: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			exports, err := haifyClient.ListNFSExports(cmd.Context(), resource)
			if err != nil {
				log.Debug("nfsExportList: ListNFSExports failed", zap.Error(err))
				return fmt.Errorf("failed to list NFS exports: %w", err)
			}

			if len(exports) == 0 {
				log.Debug("nfsExportList: returning early", zap.String("condition", "len(exports) == 0"))
				fmt.Printf("No exports configured on '%s'\n", resource)
				return nil
			}

			fmt.Printf("NFS exports for '%s':\n", resource)
			for _, export := range exports {
				log.Debug("nfsExportList: export", zap.String("directory", export.Directory), zap.String("fsid", export.Fsid), zap.String("client", export.Clientspec))
				fmt.Printf("  - %s fsid=%s client=%s options=%s\n",
					export.Directory,
					export.Fsid,
					export.Clientspec,
					export.Options,
				)
			}
			log.Debug("nfsExportList: done", zap.Int("count", len(exports)))
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	_ = cmd.MarkFlagRequired("resource")

	return cmd
}

func nfsMount() *cobra.Command {
	var resource, target, server, exportPath, mountOptions string
	var useSudo, printOnly, mkdir bool

	cmd := &cobra.Command{
		Use:   "mount --resource <name> --target <path>",
		Short: "Mount an NFS gateway on the local machine",
		Long:  "Mount the selected NFS gateway on the local machine where this CLI command is executed.",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nfsMount: running command", zap.Strings("args", args), zap.String("resource", resource), zap.String("target", target), zap.String("server", server), zap.String("export_path", exportPath), zap.String("options", mountOptions), zap.Bool("sudo", useSudo), zap.Bool("print_only", printOnly), zap.Bool("mkdir", mkdir))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nfsMount: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			gw, err := haifyClient.GetGateway(cmd.Context(), resource)
			if err != nil {
				log.Debug("nfsMount: GetGateway failed", zap.Error(err))
				return fmt.Errorf("failed to get gateway: %w", err)
			}
			if gw.Type != "nfs" {
				log.Debug("nfsMount: returning early", zap.String("condition", "gw.Type != \"nfs\""))
				return fmt.Errorf("gateway '%s' is not an NFS gateway", resource)
			}

			if server == "" {
				server = gw.Options["service_host"]
				if server == "" {
					server = gatewayServiceHost(gw.Options["service_ip"])
				}
				log.Debug("nfsMount: server taken from the gateway", zap.String("server", server))
			}
			if exportPath == "" {
				exportPath = gw.Path
				if exportPath == "" {
					exportPath = gw.Options["export_directory"]
				}
				if exportPath == "" && gw.Options["export_path"] != "" {
					exportPath = gatewayExportDirectory(resource, gw.Options["export_path"])
				}
				log.Debug("nfsMount: export path taken from the gateway", zap.String("export_path", exportPath))
			}
			if server == "" {
				log.Debug("nfsMount: returning early", zap.String("condition", "server == \"\""))
				return fmt.Errorf("failed to determine NFS server address")
			}
			if exportPath == "" {
				log.Debug("nfsMount: returning early", zap.String("condition", "exportPath == \"\""))
				return fmt.Errorf("failed to determine NFS export path")
			}

			if mkdir {
				log.Debug("nfsMount: creating mount target", zap.String("target", target))
				if err := os.MkdirAll(target, 0755); err != nil {
					log.Debug("nfsMount: MkdirAll failed", zap.Error(err))
					return fmt.Errorf("failed to create mount target: %w", err)
				}
			}

			mountArgs := buildNFSMountArgs(server, exportPath, target, mountOptions)
			displayArgs := append([]string(nil), mountArgs...)
			if useSudo {
				displayArgs = append([]string{"sudo"}, displayArgs...)
			}

			fmt.Printf("Mount command: %s\n", strings.Join(displayArgs, " "))
			if printOnly {
				log.Debug("nfsMount: returning early", zap.String("condition", "printOnly"))
				return nil
			}

			commandName := mountArgs[0]
			commandArgs := mountArgs[1:]
			if useSudo {
				commandName = "sudo"
				commandArgs = append([]string{mountArgs[0]}, mountArgs[1:]...)
			}

			command := exec.CommandContext(cmd.Context(), commandName, commandArgs...)
			command.Stdout = os.Stdout
			command.Stderr = os.Stderr
			command.Stdin = os.Stdin

			log.Debug("nfsMount: running mount", zap.String("command", commandName), zap.Strings("command_args", commandArgs))
			start := time.Now()
			if err := command.Run(); err != nil {
				log.Debug("nfsMount: mount failed", zap.Duration("took", time.Since(start)), zap.Error(err))
				return fmt.Errorf("failed to mount NFS gateway: %w", err)
			}
			log.Debug("nfsMount: mount succeeded", zap.Duration("took", time.Since(start)))

			fmt.Printf("Mounted %s:%s on %s\n", server, exportPath, target)
			log.Debug("nfsMount: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	cmd.Flags().StringVar(&target, "target", "", "Local mount target")
	cmd.Flags().StringVar(&server, "server", "", "Override NFS server address")
	cmd.Flags().StringVar(&exportPath, "export-path", "", "Override exported directory")
	cmd.Flags().StringVar(&mountOptions, "options", "", "Mount options passed to mount -o")
	cmd.Flags().BoolVar(&useSudo, "sudo", false, "Run mount through sudo")
	cmd.Flags().BoolVar(&printOnly, "print-only", false, "Only print the mount command")
	cmd.Flags().BoolVar(&mkdir, "mkdir", false, "Create the mount target before mounting")
	_ = cmd.MarkFlagRequired("resource")
	_ = cmd.MarkFlagRequired("target")

	return cmd
}

func buildNFSMountArgs(server, exportPath, target, mountOptions string) []string {
	args := []string{"mount", "-t", "nfs"}
	if strings.TrimSpace(mountOptions) != "" {
		args = append(args, "-o", mountOptions)
	}
	args = append(args, fmt.Sprintf("%s:%s", server, exportPath), target)
	zap.L().Debug("buildNFSMountArgs: built mount command", zap.Strings("args", args), zap.Bool("has_options", strings.TrimSpace(mountOptions) != ""))
	return args
}
