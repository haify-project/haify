package main

import (
	"context"
	"fmt"

	"github.com/haify-project/haify/pkg/util"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func resourceAddVolume() *cobra.Command {
	var name string
	var volume string
	var pool string
	var size string

	cmd := &cobra.Command{
		Use:   "add-volume <resource>",
		Short: "Add a volume to resource",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceAddVolume: running command", zap.Strings("args", args), zap.String("volume", volume),
				zap.String("pool", pool), zap.String("size", size))
			resource := args[0]

			if volume == "" {
				log.Debug("resourceAddVolume: --volume missing")
				return fmt.Errorf("volume name is required (--volume)")
			}
			if size == "" {
				log.Debug("resourceAddVolume: --size missing")
				return fmt.Errorf("size is required (--size)")
			}
			if pool == "" {
				log.Debug("resourceAddVolume: --pool missing")
				return fmt.Errorf("pool is required (--pool)")
			}

			ctx, cancel := context.WithTimeout(context.Background(), nodeOpTimeout)
			defer cancel()

			sizeBytes, err := util.ParseSize(size)
			if err != nil {
				log.Debug("resourceAddVolume: size not parsable", zap.String("size", size), zap.Error(err))
				return fmt.Errorf("invalid size format: %s: %w", size, err)
			}
			sizeGiB := util.BytesToGiB(sizeBytes)
			if sizeGiB == 0 {
				log.Debug("resourceAddVolume: size below 1 GiB", zap.Uint64("size_bytes", sizeBytes))
				return fmt.Errorf("size too small (minimum 1 GiB)")
			}
			log.Debug("resourceAddVolume: size", zap.Uint64("size_bytes", sizeBytes), zap.Uint64("size_gib", sizeGiB))

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceAddVolume: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.AddVolume(ctx, resource, volume, pool, uint32(sizeGiB))
			if err != nil {
				log.Debug("resourceAddVolume: AddVolume failed", zap.Error(err))
				return fmt.Errorf("failed to add volume: %w", err)
			}

			fmt.Printf("Volume '%s' added to '%s' (size: %s)\n", volume, resource, util.FormatBytes(sizeBytes))
			log.Debug("resourceAddVolume: done", zap.String("resource", resource), zap.String("volume", volume))
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Volume name (alias of --volume)")
	cmd.Flags().StringVar(&volume, "volume", "", "Volume name (required)")
	// Kept so existing scripts work, hidden so the help does not list two
	// required flags that mean the same thing.
	_ = cmd.Flags().MarkHidden("name")
	cmd.Flags().StringVar(&pool, "pool", "", "Storage pool (required)")
	cmd.Flags().StringVar(&size, "size", "", "Volume size (e.g., 1G, 10GB, 1TB, required)")

	// For compatibility, map --name to --volume
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		if name != "" && volume == "" {
			zap.L().Debug("resourceAddVolume: --name given as an alias of --volume", zap.String("volume", name))
			volume = name
		}
		return nil
	}

	return cmd
}

func resourceRemoveVolume() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove-volume <resource> <volume-id>",
		Short: "Remove a volume from resource",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceRemoveVolume: running command", zap.Strings("args", args))
			resource := args[0]
			var volumeID uint32
			_, err := fmt.Sscanf(args[1], "%d", &volumeID)
			if err != nil {
				log.Debug("resourceRemoveVolume: volume id not a number", zap.String("volume_id", args[1]), zap.Error(err))
				return fmt.Errorf("invalid volume ID: %s", args[1])
			}
			log.Debug("resourceRemoveVolume: removing volume", zap.String("resource", resource), zap.Uint32("volume_id", volumeID))

			ctx, cancel := context.WithTimeout(context.Background(), nodeOpTimeout)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceRemoveVolume: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.RemoveVolume(ctx, resource, volumeID)
			if err != nil {
				log.Debug("resourceRemoveVolume: RemoveVolume failed", zap.Error(err))
				return fmt.Errorf("failed to remove volume: %w", err)
			}

			fmt.Printf("Volume %d removed from '%s'\n", volumeID, resource)
			log.Debug("resourceRemoveVolume: done")
			return nil
		},
	}

	return cmd
}

func resourceResizeVolume() *cobra.Command {
	var size string
	var ignoreFreeSpace bool

	cmd := &cobra.Command{
		Use:   "resize-volume <resource> <volume-id> <size>",
		Short: "Resize a volume",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceResizeVolume: running command", zap.Strings("args", args), zap.Bool("ignore_free_space", ignoreFreeSpace))
			resource := args[0]
			var volumeID uint32
			_, err := fmt.Sscanf(args[1], "%d", &volumeID)
			if err != nil {
				log.Debug("resourceResizeVolume: volume id not a number", zap.String("volume_id", args[1]), zap.Error(err))
				return fmt.Errorf("invalid volume ID: %s", args[1])
			}
			size = args[2]

			ctx, cancel := context.WithTimeout(context.Background(), nodeOpTimeout)
			defer cancel()

			sizeBytes, err := util.ParseSize(size)
			if err != nil {
				log.Debug("resourceResizeVolume: size not parsable", zap.String("size", size), zap.Error(err))
				return fmt.Errorf("invalid size format: %s: %w", size, err)
			}
			sizeGiB := util.BytesToGiB(sizeBytes)
			if sizeGiB == 0 {
				log.Debug("resourceResizeVolume: size below 1 GiB", zap.Uint64("size_bytes", sizeBytes))
				return fmt.Errorf("size too small (minimum 1 GiB)")
			}
			log.Debug("resourceResizeVolume: resizing", zap.String("resource", resource), zap.Uint32("volume_id", volumeID), zap.Uint64("size_gib", sizeGiB))

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceResizeVolume: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			resize := haifyClient.ResizeVolume
			if ignoreFreeSpace {
				log.Debug("resourceResizeVolume: ignoring free space on the replicas")
				resize = haifyClient.ResizeVolumeIgnoringFreeSpace
			}
			err = resize(ctx, resource, volumeID, uint32(sizeGiB))
			if err != nil {
				log.Debug("resourceResizeVolume: resize failed", zap.Error(err))
				return fmt.Errorf("failed to resize volume: %w", err)
			}

			fmt.Printf("Volume %d resized to %s\n", volumeID, util.FormatBytes(sizeBytes))
			log.Debug("resourceResizeVolume: done", zap.String("resource", resource), zap.Uint32("volume_id", volumeID), zap.Uint64("size_bytes", sizeBytes))
			return nil
		},
	}
	cmd.Flags().BoolVar(&ignoreFreeSpace, "ignore-free-space", false,
		"Grow it although a replica's pool has less free space than the growth (the new area is written to every replica; a full pool drops the disk)")

	return cmd
}
