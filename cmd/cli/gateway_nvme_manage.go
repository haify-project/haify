package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func nvmeNamespaceCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "namespace",
		Short: "Manage NVMe namespaces",
	}

	cmd.AddCommand(nvmeNamespaceAdd())
	cmd.AddCommand(nvmeNamespaceRemove())
	cmd.AddCommand(nvmeNamespaceList())

	return cmd
}

func nvmeNamespaceAdd() *cobra.Command {
	var resource, device string

	cmd := &cobra.Command{
		Use:   "add --resource <name> --device <path>",
		Short: "Add a namespace to an NVMe gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nvmeNamespaceAdd: running command", zap.Strings("args", args), zap.String("resource", resource), zap.String("device", device))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nvmeNamespaceAdd: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.AddNVMeNamespace(cmd.Context(), resource, device)
			if err != nil {
				log.Debug("nvmeNamespaceAdd: AddNVMeNamespace failed", zap.Error(err))
				return fmt.Errorf("failed to add NVMe namespace: %w", err)
			}

			fmt.Printf("Added namespace to '%s' using %s\n", resource, device)
			log.Debug("nvmeNamespaceAdd: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	cmd.Flags().StringVar(&device, "device", "", "Backing device path")
	_ = cmd.MarkFlagRequired("resource")
	_ = cmd.MarkFlagRequired("device")

	return cmd
}

func nvmeNamespaceRemove() *cobra.Command {
	var resource string
	var namespaceID int32

	cmd := &cobra.Command{
		Use:   "remove --resource <name> --id <nsid>",
		Short: "Remove a namespace from an NVMe gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nvmeNamespaceRemove: running command", zap.Strings("args", args), zap.String("resource", resource), zap.Int32("namespace_id", namespaceID))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nvmeNamespaceRemove: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.RemoveNVMeNamespace(cmd.Context(), resource, namespaceID)
			if err != nil {
				log.Debug("nvmeNamespaceRemove: RemoveNVMeNamespace failed", zap.Error(err))
				return fmt.Errorf("failed to remove NVMe namespace: %w", err)
			}

			fmt.Printf("Removed namespace %d from '%s'\n", namespaceID, resource)
			log.Debug("nvmeNamespaceRemove: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	cmd.Flags().Int32Var(&namespaceID, "id", 0, "Namespace ID")
	_ = cmd.MarkFlagRequired("resource")
	_ = cmd.MarkFlagRequired("id")

	return cmd
}

func nvmeNamespaceList() *cobra.Command {
	var resource string

	cmd := &cobra.Command{
		Use:   "list --resource <name>",
		Short: "List namespaces on an NVMe gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nvmeNamespaceList: running command", zap.Strings("args", args), zap.String("resource", resource))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nvmeNamespaceList: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			namespaces, err := haifyClient.ListNVMeNamespaces(cmd.Context(), resource)
			if err != nil {
				log.Debug("nvmeNamespaceList: ListNVMeNamespaces failed", zap.Error(err))
				return fmt.Errorf("failed to list NVMe namespaces: %w", err)
			}

			if len(namespaces) == 0 {
				log.Debug("nvmeNamespaceList: returning early", zap.String("condition", "len(namespaces) == 0"))
				fmt.Printf("No NVMe namespaces configured on '%s'\n", resource)
				return nil
			}

			fmt.Printf("NVMe namespaces for '%s':\n", resource)
			for _, namespace := range namespaces {
				log.Debug("nvmeNamespaceList: namespace", zap.Int32("nsid", namespace.NamespaceId), zap.String("device", namespace.BackingPath))
				fmt.Printf("  - nsid=%d device=%s nqn=%s\n", namespace.NamespaceId, namespace.BackingPath, namespace.Nqn)
			}
			log.Debug("nvmeNamespaceList: done", zap.Int("count", len(namespaces)))
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	_ = cmd.MarkFlagRequired("resource")

	return cmd
}

func nvmeHostCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host",
		Short: "Manage NVMe host allow-list entries",
	}

	cmd.AddCommand(nvmeHostAdd())
	cmd.AddCommand(nvmeHostRemove())
	cmd.AddCommand(nvmeHostList())

	return cmd
}

func nvmeHostAdd() *cobra.Command {
	var resource, hostNQN string

	cmd := &cobra.Command{
		Use:   "add --resource <name> --nqn <host-nqn>",
		Short: "Allow a host on an NVMe gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nvmeHostAdd: running command", zap.Strings("args", args), zap.String("resource", resource), zap.String("host_nqn", hostNQN))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nvmeHostAdd: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.AddNVMeHost(cmd.Context(), resource, hostNQN)
			if err != nil {
				log.Debug("nvmeHostAdd: AddNVMeHost failed", zap.Error(err))
				return fmt.Errorf("failed to add NVMe host: %w", err)
			}

			fmt.Printf("Added NVMe host to '%s': %s\n", resource, hostNQN)
			log.Debug("nvmeHostAdd: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	cmd.Flags().StringVar(&hostNQN, "nqn", "", "Host NQN")
	_ = cmd.MarkFlagRequired("resource")
	_ = cmd.MarkFlagRequired("nqn")

	return cmd
}

func nvmeHostRemove() *cobra.Command {
	var resource, hostNQN string

	cmd := &cobra.Command{
		Use:   "remove --resource <name> --nqn <host-nqn>",
		Short: "Remove a host from an NVMe gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nvmeHostRemove: running command", zap.Strings("args", args), zap.String("resource", resource), zap.String("host_nqn", hostNQN))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nvmeHostRemove: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.RemoveNVMeHost(cmd.Context(), resource, hostNQN)
			if err != nil {
				log.Debug("nvmeHostRemove: RemoveNVMeHost failed", zap.Error(err))
				return fmt.Errorf("failed to remove NVMe host: %w", err)
			}

			fmt.Printf("Removed NVMe host from '%s': %s\n", resource, hostNQN)
			log.Debug("nvmeHostRemove: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	cmd.Flags().StringVar(&hostNQN, "nqn", "", "Host NQN")
	_ = cmd.MarkFlagRequired("resource")
	_ = cmd.MarkFlagRequired("nqn")

	return cmd
}

func nvmeHostList() *cobra.Command {
	var resource string

	cmd := &cobra.Command{
		Use:   "list --resource <name>",
		Short: "List hosts allowed on an NVMe gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nvmeHostList: running command", zap.Strings("args", args), zap.String("resource", resource))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("nvmeHostList: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			hosts, err := haifyClient.ListNVMeHosts(cmd.Context(), resource)
			if err != nil {
				log.Debug("nvmeHostList: ListNVMeHosts failed", zap.Error(err))
				return fmt.Errorf("failed to list NVMe hosts: %w", err)
			}

			if len(hosts) == 0 {
				log.Debug("nvmeHostList: returning early", zap.String("condition", "len(hosts) == 0"))
				fmt.Printf("No NVMe hosts configured on '%s'\n", resource)
				return nil
			}

			fmt.Printf("NVMe hosts for '%s':\n", resource)
			for _, host := range hosts {
				fmt.Printf("  - %s\n", host)
			}
			log.Debug("nvmeHostList: done", zap.Int("count", len(hosts)))
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	_ = cmd.MarkFlagRequired("resource")

	return cmd
}
