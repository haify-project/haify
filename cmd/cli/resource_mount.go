package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func resourceFs() *cobra.Command {
	var node string

	cmd := &cobra.Command{
		Use:   "fs <resource> <volume-id> <fstype>",
		Short: "Create filesystem on volume",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceFs: running command", zap.Strings("args", args), zap.String("node", node))
			resource := args[0]
			var volumeID uint32
			_, err := fmt.Sscanf(args[1], "%d", &volumeID)
			if err != nil {
				log.Debug("resourceFs: volume id not a number", zap.String("volume_id", args[1]), zap.Error(err))
				return fmt.Errorf("invalid volume ID: %s", args[1])
			}
			fstype := args[2]

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceFs: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.CreateFilesystem(ctx, resource, volumeID, node, fstype)
			if err != nil {
				log.Debug("resourceFs: CreateFilesystem failed", zap.Error(err))
				return fmt.Errorf("failed to create filesystem: %w", err)
			}

			fmt.Printf("Filesystem '%s' created on volume %d\n", fstype, volumeID)
			log.Debug("resourceFs: done", zap.String("resource", resource), zap.Uint32("volume_id", volumeID), zap.String("fs_type", fstype))
			return nil
		},
	}

	cmd.Flags().StringVar(&node, "node", "", "Node to run mkfs on (default: the resource's current Primary)")

	return cmd
}

func resourceMount() *cobra.Command {
	var node string
	var fstype string

	cmd := &cobra.Command{
		Use:   "mount <resource> <volume-id> <mount-path>",
		Short: "Mount a DRBD volume",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceMount: running command", zap.Strings("args", args), zap.String("node", node), zap.String("fs_type", fstype))
			resource := args[0]
			var volumeID uint32
			_, err := fmt.Sscanf(args[1], "%d", &volumeID)
			if err != nil {
				log.Debug("resourceMount: volume id not a number", zap.String("volume_id", args[1]), zap.Error(err))
				return fmt.Errorf("invalid volume ID: %s", args[1])
			}
			mountPath := args[2]

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceMount: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.MountResource(ctx, resource, volumeID, mountPath, node, fstype)
			if err != nil {
				log.Debug("resourceMount: MountResource failed", zap.Error(err))
				return fmt.Errorf("failed to mount resource: %w", err)
			}

			fmt.Printf("Resource '%s' volume %d mounted at %s\n", resource, volumeID, mountPath)
			log.Debug("resourceMount: done", zap.String("resource", resource), zap.Uint32("volume_id", volumeID), zap.String("path", mountPath))
			return nil
		},
	}

	cmd.Flags().StringVar(&node, "node", "", "Target node (required)")
	cmd.Flags().StringVar(&fstype, "fstype", "ext4", "Filesystem type")
	_ = cmd.MarkFlagRequired("node")

	return cmd
}

func resourceUnmount() *cobra.Command {
	var node string

	cmd := &cobra.Command{
		Use:   "unmount <resource> <volume-id>",
		Short: "Unmount a DRBD volume",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceUnmount: running command", zap.Strings("args", args), zap.String("node", node))
			resource := args[0]
			var volumeID uint32
			_, err := fmt.Sscanf(args[1], "%d", &volumeID)
			if err != nil {
				log.Debug("resourceUnmount: volume id not a number", zap.String("volume_id", args[1]), zap.Error(err))
				return fmt.Errorf("invalid volume ID: %s", args[1])
			}

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceUnmount: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.UnmountResource(ctx, resource, volumeID, node)
			if err != nil {
				log.Debug("resourceUnmount: UnmountResource failed", zap.Error(err))
				return fmt.Errorf("failed to unmount resource: %w", err)
			}

			fmt.Printf("Resource '%s' volume %d unmounted\n", resource, volumeID)
			log.Debug("resourceUnmount: done", zap.String("resource", resource), zap.Uint32("volume_id", volumeID))
			return nil
		},
	}

	cmd.Flags().StringVar(&node, "node", "", "Target node (required)")
	_ = cmd.MarkFlagRequired("node")

	return cmd
}
