package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func iscsiLUNCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lun",
		Short: "Manage iSCSI LUNs",
	}

	cmd.AddCommand(iscsiLUNAdd())
	cmd.AddCommand(iscsiLUNRemove())
	cmd.AddCommand(iscsiLUNList())

	return cmd
}

func iscsiLUNAdd() *cobra.Command {
	var resource, device string
	var lun int32

	cmd := &cobra.Command{
		Use:   "add --resource <name> --lun <id> --device <path>",
		Short: "Add a LUN to an iSCSI gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("iscsiLUNAdd: running command", zap.Strings("args", args), zap.String("resource", resource), zap.Int32("lun", lun), zap.String("device", device))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("iscsiLUNAdd: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.AddISCSILUN(cmd.Context(), resource, lun, device)
			if err != nil {
				log.Debug("iscsiLUNAdd: AddISCSILUN failed", zap.Error(err))
				return fmt.Errorf("failed to add iSCSI LUN: %w", err)
			}

			fmt.Printf("Added LUN %d to '%s' using %s\n", lun, resource, device)
			log.Debug("iscsiLUNAdd: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	cmd.Flags().Int32Var(&lun, "lun", 0, "LUN number")
	cmd.Flags().StringVar(&device, "device", "", "Backing device path")
	_ = cmd.MarkFlagRequired("resource")
	_ = cmd.MarkFlagRequired("lun")
	_ = cmd.MarkFlagRequired("device")

	return cmd
}

func iscsiLUNRemove() *cobra.Command {
	var resource string
	var lun int32

	cmd := &cobra.Command{
		Use:   "remove --resource <name> --lun <id>",
		Short: "Remove a LUN from an iSCSI gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("iscsiLUNRemove: running command", zap.Strings("args", args), zap.String("resource", resource), zap.Int32("lun", lun))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("iscsiLUNRemove: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.RemoveISCSILUN(cmd.Context(), resource, lun)
			if err != nil {
				log.Debug("iscsiLUNRemove: RemoveISCSILUN failed", zap.Error(err))
				return fmt.Errorf("failed to remove iSCSI LUN: %w", err)
			}

			fmt.Printf("Removed LUN %d from '%s'\n", lun, resource)
			log.Debug("iscsiLUNRemove: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	cmd.Flags().Int32Var(&lun, "lun", 0, "LUN number")
	_ = cmd.MarkFlagRequired("resource")
	_ = cmd.MarkFlagRequired("lun")

	return cmd
}

func iscsiLUNList() *cobra.Command {
	var resource string

	cmd := &cobra.Command{
		Use:   "list --resource <name>",
		Short: "List LUNs on an iSCSI gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("iscsiLUNList: running command", zap.Strings("args", args), zap.String("resource", resource))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("iscsiLUNList: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			luns, err := haifyClient.ListISCSILUNs(cmd.Context(), resource)
			if err != nil {
				log.Debug("iscsiLUNList: ListISCSILUNs failed", zap.Error(err))
				return fmt.Errorf("failed to list iSCSI LUNs: %w", err)
			}

			if len(luns) == 0 {
				log.Debug("iscsiLUNList: returning early", zap.String("condition", "len(luns) == 0"))
				fmt.Printf("No iSCSI LUNs configured on '%s'\n", resource)
				return nil
			}

			fmt.Printf("iSCSI LUNs for '%s':\n", resource)
			for _, lun := range luns {
				log.Debug("iscsiLUNList: lun", zap.Int32("lun", lun.Lun), zap.String("device", lun.Device), zap.String("target_iqn", lun.TargetIqn))
				fmt.Printf("  - lun=%d device=%s target=%s\n", lun.Lun, lun.Device, lun.TargetIqn)
			}
			log.Debug("iscsiLUNList: done", zap.Int("count", len(luns)))
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	_ = cmd.MarkFlagRequired("resource")

	return cmd
}

func iscsiInitiatorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "initiator",
		Short: "Manage iSCSI initiator ACLs",
	}

	cmd.AddCommand(iscsiInitiatorAdd())
	cmd.AddCommand(iscsiInitiatorRemove())
	cmd.AddCommand(iscsiInitiatorList())

	return cmd
}

func iscsiInitiatorAdd() *cobra.Command {
	var resource, initiator string

	cmd := &cobra.Command{
		Use:   "add --resource <name> --iqn <initiator>",
		Short: "Allow an initiator on an iSCSI gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("iscsiInitiatorAdd: running command", zap.Strings("args", args), zap.String("resource", resource), zap.String("initiator", initiator))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("iscsiInitiatorAdd: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.AddISCSIInitiator(cmd.Context(), resource, initiator)
			if err != nil {
				log.Debug("iscsiInitiatorAdd: AddISCSIInitiator failed", zap.Error(err))
				return fmt.Errorf("failed to add initiator: %w", err)
			}

			fmt.Printf("Added initiator to '%s': %s\n", resource, initiator)
			log.Debug("iscsiInitiatorAdd: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	cmd.Flags().StringVar(&initiator, "iqn", "", "Initiator IQN")
	_ = cmd.MarkFlagRequired("resource")
	_ = cmd.MarkFlagRequired("iqn")

	return cmd
}

func iscsiInitiatorRemove() *cobra.Command {
	var resource, initiator string

	cmd := &cobra.Command{
		Use:   "remove --resource <name> --iqn <initiator>",
		Short: "Remove an initiator from an iSCSI gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("iscsiInitiatorRemove: running command", zap.Strings("args", args), zap.String("resource", resource), zap.String("initiator", initiator))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("iscsiInitiatorRemove: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.RemoveISCSIInitiator(cmd.Context(), resource, initiator)
			if err != nil {
				log.Debug("iscsiInitiatorRemove: RemoveISCSIInitiator failed", zap.Error(err))
				return fmt.Errorf("failed to remove initiator: %w", err)
			}

			fmt.Printf("Removed initiator from '%s': %s\n", resource, initiator)
			log.Debug("iscsiInitiatorRemove: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	cmd.Flags().StringVar(&initiator, "iqn", "", "Initiator IQN")
	_ = cmd.MarkFlagRequired("resource")
	_ = cmd.MarkFlagRequired("iqn")

	return cmd
}

func iscsiInitiatorList() *cobra.Command {
	var resource string

	cmd := &cobra.Command{
		Use:   "list --resource <name>",
		Short: "List initiators on an iSCSI gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("iscsiInitiatorList: running command", zap.Strings("args", args), zap.String("resource", resource))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("iscsiInitiatorList: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			initiators, err := haifyClient.ListISCSIInitiators(cmd.Context(), resource)
			if err != nil {
				log.Debug("iscsiInitiatorList: ListISCSIInitiators failed", zap.Error(err))
				return fmt.Errorf("failed to list initiators: %w", err)
			}

			if len(initiators) == 0 {
				log.Debug("iscsiInitiatorList: returning early", zap.String("condition", "len(initiators) == 0"))
				fmt.Printf("No explicit initiators configured on '%s'\n", resource)
				return nil
			}

			fmt.Printf("Initiators for '%s':\n", resource)
			for _, initiator := range initiators {
				fmt.Printf("  - %s\n", initiator)
			}
			log.Debug("iscsiInitiatorList: done", zap.Int("count", len(initiators)))
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	_ = cmd.MarkFlagRequired("resource")

	return cmd
}

func iscsiCHAPCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "chap",
		Short: "Manage iSCSI CHAP settings",
	}

	cmd.AddCommand(iscsiCHAPSet())
	cmd.AddCommand(iscsiCHAPGet())

	return cmd
}

func iscsiCHAPSet() *cobra.Command {
	var resource, username, password string

	cmd := &cobra.Command{
		Use:   "set --resource <name> --username <user> --password <pass>",
		Short: "Set one-way CHAP credentials on an iSCSI gateway (mutual CHAP is not supported)",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("iscsiCHAPSet: running command", zap.Strings("args", args), zap.String("resource", resource), zap.String("username", username), zap.Bool("has_password", password != ""))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("iscsiCHAPSet: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.SetISCSIChap(cmd.Context(), resource, username, password, false)
			if err != nil {
				log.Debug("iscsiCHAPSet: SetISCSIChap failed", zap.Error(err))
				return fmt.Errorf("failed to set CHAP: %w", err)
			}

			fmt.Printf("Updated CHAP settings for '%s'\n", resource)
			log.Debug("iscsiCHAPSet: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	cmd.Flags().StringVar(&username, "username", "", "CHAP username")
	cmd.Flags().StringVar(&password, "password", "", "CHAP password")
	_ = cmd.MarkFlagRequired("resource")
	_ = cmd.MarkFlagRequired("username")
	_ = cmd.MarkFlagRequired("password")

	return cmd
}

func iscsiCHAPGet() *cobra.Command {
	var resource string

	cmd := &cobra.Command{
		Use:   "get --resource <name>",
		Short: "Get CHAP credentials for an iSCSI gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("iscsiCHAPGet: running command", zap.Strings("args", args), zap.String("resource", resource))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("iscsiCHAPGet: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			resp, err := haifyClient.GetISCSIChap(cmd.Context(), resource)
			if err != nil {
				log.Debug("iscsiCHAPGet: GetISCSIChap failed", zap.Error(err))
				return fmt.Errorf("failed to get CHAP: %w", err)
			}

			fmt.Printf("Resource: %s\n", resource)
			fmt.Printf("Username: %s\n", resp.Username)
			fmt.Printf("Password: %s\n", resp.Password)
			log.Debug("iscsiCHAPGet: done", zap.String("username", resp.Username), zap.Bool("has_password", resp.Password != ""))
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	_ = cmd.MarkFlagRequired("resource")

	return cmd
}
