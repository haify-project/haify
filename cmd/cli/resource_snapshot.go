package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// resourceSnapshot manages snapshots for DRBD resources
func resourceSnapshot() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Snapshot management for DRBD resources",
	}

	cmd.AddCommand(resourceSnapshotCreate())
	cmd.AddCommand(resourceSnapshotList())
	cmd.AddCommand(resourceSnapshotRestore())
	cmd.AddCommand(resourceSnapshotDelete())
	cmd.AddCommand(resourceSnapshotSchedule())
	cmd.AddCommand(resourceSnapshotReplicated())

	return cmd
}

func resourceSnapshotDelete() *cobra.Command {
	var resource string
	var snapshotName string
	var node string
	var storageType string
	var pool string

	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a snapshot",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceSnapshotDelete: running command", zap.Strings("args", args), zap.String("resource", resource),
				zap.String("snapshot", snapshotName), zap.String("node", node), zap.String("storage_type", storageType), zap.String("pool", pool))
			if resource == "" {
				log.Debug("resourceSnapshotDelete: --resource missing")
				return fmt.Errorf("resource name is required")
			}
			if snapshotName == "" {
				log.Debug("resourceSnapshotDelete: --name missing")
				return fmt.Errorf("snapshot name is required")
			}
			if pool == "" || node == "" {
				log.Debug("resourceSnapshotDelete: pool or node not given, asking the resource")
				var err error
				if pool, node, err = snapshotTarget(resource, pool, node); err != nil {
					log.Debug("resourceSnapshotDelete: snapshotTarget failed", zap.Error(err))
					return err
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), nodeOpTimeout)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceSnapshotDelete: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			if storageType == "zfs" {
				// ZFS snapshot
				// ZFS snapshot names are unique per dataset, usually passed as snapshot name only to destroy?
				// But DeleteZFSSnapshot in backend takes 'snapshot' arg.
				// Is it "snap1" or "pool/dataset@snap1"?
				// pkg/deployment/deployment.go ZFSDestroySnapshot: "sudo zfs destroy %s"
				// So it needs FULL path.
				snapshotPath := fmt.Sprintf("%s/%s_data@%s", pool, resource, snapshotName)
				log.Debug("resourceSnapshotDelete: deleting ZFS snapshot", zap.String("snapshot", snapshotPath), zap.String("node", node))
				err = haifyClient.DeleteZFSSnapshot(ctx, snapshotPath, node)
				if err != nil {
					log.Debug("resourceSnapshotDelete: DeleteZFSSnapshot failed", zap.Error(err))
					return fmt.Errorf("failed to delete ZFS snapshot: %w", err)
				}
				fmt.Printf("ZFS snapshot '%s' deleted on node '%s'\n", snapshotName, node)
			} else {
				// LVM snapshot
				// Pass pool as VG name
				log.Debug("resourceSnapshotDelete: deleting LVM snapshot", zap.String("pool", pool), zap.String("snapshot", snapshotName), zap.String("node", node))
				err = haifyClient.DeleteLvmSnapshot(ctx, pool, snapshotName, node)
				if err != nil {
					log.Debug("resourceSnapshotDelete: DeleteLvmSnapshot failed", zap.Error(err))
					return fmt.Errorf("failed to delete LVM snapshot: %w", err)
				}
				fmt.Printf("LVM snapshot '%s' deleted on node '%s'\n", snapshotName, node)
			}

			log.Debug("resourceSnapshotDelete: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	cmd.Flags().StringVar(&snapshotName, "name", "", "Snapshot name")
	cmd.Flags().StringVar(&node, "node", "", "Node where resource exists")
	cmd.Flags().StringVar(&storageType, "storage-type", "lvm", "Storage type: lvm or zfs")
	cmd.Flags().StringVar(&pool, "pool", "", "Storage pool name (default: the resource's own pool)")

	_ = cmd.MarkFlagRequired("resource")
	_ = cmd.MarkFlagRequired("name")

	return cmd
}

func resourceSnapshotCreate() *cobra.Command {
	var resource string
	var snapshotName string
	var node string
	var size string
	var storageType string
	var pool string

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a snapshot of DRBD resource",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceSnapshotCreate: running command", zap.Strings("args", args), zap.String("resource", resource),
				zap.String("snapshot", snapshotName), zap.String("node", node), zap.String("size", size),
				zap.String("storage_type", storageType), zap.String("pool", pool))
			if resource == "" {
				log.Debug("resourceSnapshotCreate: --resource missing")
				return fmt.Errorf("resource name is required")
			}
			if snapshotName == "" {
				log.Debug("resourceSnapshotCreate: --name missing")
				return fmt.Errorf("snapshot name is required")
			}
			if pool == "" || node == "" {
				log.Debug("resourceSnapshotCreate: pool or node not given, asking the resource")
				var err error
				if pool, node, err = snapshotTarget(resource, pool, node); err != nil {
					log.Debug("resourceSnapshotCreate: snapshotTarget failed", zap.Error(err))
					return err
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), nodeOpTimeout)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceSnapshotCreate: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			if storageType == "zfs" {
				// ZFS snapshot: pool/resource_data@snapshot
				dataset := fmt.Sprintf("%s/%s_data", pool, resource)
				log.Debug("resourceSnapshotCreate: creating ZFS snapshot", zap.String("dataset", dataset), zap.String("snapshot", snapshotName), zap.String("node", node))
				err = haifyClient.CreateZFSSnapshot(ctx, dataset, snapshotName, node)
				if err != nil {
					log.Debug("resourceSnapshotCreate: CreateZFSSnapshot failed", zap.Error(err))
					return fmt.Errorf("failed to create ZFS snapshot: %w", err)
				}
				fmt.Printf("ZFS snapshot '%s' created for resource '%s' on node '%s'\n", snapshotName, resource, node)
			} else {
				// LVM snapshot (default)
				if size == "" {
					log.Debug("resourceSnapshotCreate: no --size, using 1G")
					size = "1G"
				}
				lvName := fmt.Sprintf("%s_data", resource)
				log.Debug("resourceSnapshotCreate: creating LVM snapshot", zap.String("pool", pool), zap.String("lv", lvName),
					zap.String("snapshot", snapshotName), zap.String("node", node), zap.String("size", size))
				// Pass pool as the VG name (first argument)
				err = haifyClient.CreateLvmSnapshot(ctx, pool, lvName, snapshotName, node, size)
				if err != nil {
					log.Debug("resourceSnapshotCreate: CreateLvmSnapshot failed", zap.Error(err))
					return fmt.Errorf("failed to create LVM snapshot: %w", err)
				}
				fmt.Printf("LVM snapshot '%s' created for resource '%s' on node '%s'\n", snapshotName, resource, node)
			}

			log.Debug("resourceSnapshotCreate: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	cmd.Flags().StringVar(&snapshotName, "name", "", "Snapshot name")
	cmd.Flags().StringVar(&node, "node", "", "Node where resource exists")
	cmd.Flags().StringVar(&size, "size", "1G", "Snapshot size for LVM (e.g., 1G)")
	cmd.Flags().StringVar(&storageType, "storage-type", "lvm", "Storage type: lvm or zfs")
	cmd.Flags().StringVar(&pool, "pool", "", "Storage pool name (default: the resource's own pool)")

	_ = cmd.MarkFlagRequired("resource")
	_ = cmd.MarkFlagRequired("name")

	return cmd
}

func resourceSnapshotList() *cobra.Command {
	var resource string
	var node string
	var storageType string
	var pool string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List snapshots for DRBD resource",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceSnapshotList: running command", zap.Strings("args", args), zap.String("resource", resource),
				zap.String("node", node), zap.String("storage_type", storageType), zap.String("pool", pool))
			if resource == "" {
				log.Debug("resourceSnapshotList: --resource missing")
				return fmt.Errorf("resource name is required")
			}
			if pool == "" || node == "" {
				log.Debug("resourceSnapshotList: pool or node not given, asking the resource")
				var err error
				if pool, node, err = snapshotTarget(resource, pool, node); err != nil {
					log.Debug("resourceSnapshotList: snapshotTarget failed", zap.Error(err))
					return err
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceSnapshotList: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			if storageType == "zfs" {
				// ZFS snapshots
				dataset := fmt.Sprintf("%s/%s_data", pool, resource)
				snapshots, err := haifyClient.ListZFSSnapshots(ctx, dataset, node)
				if err != nil {
					log.Debug("resourceSnapshotList: ListZFSSnapshots failed", zap.Error(err))
					return fmt.Errorf("failed to list ZFS snapshots: %w", err)
				}

				if len(snapshots) == 0 {
					log.Debug("resourceSnapshotList: no ZFS snapshots", zap.String("dataset", dataset))
					fmt.Printf("No ZFS snapshots found for resource '%s'\n", resource)
					return nil
				}
				log.Debug("resourceSnapshotList: ZFS snapshots", zap.String("dataset", dataset), zap.Int("count", len(snapshots)))

				fmt.Printf("ZFS snapshots for resource '%s':\n", resource)
				for _, snap := range snapshots {
					fmt.Printf("  - %s (created: %s)\n", snap.Name, snap.CreatedAt)
				}
			} else {
				// LVM snapshots. The pool is the volume group; resource is what
				// narrows the group's snapshots down to this one's volumes.
				snapshots, err := haifyClient.ListLvmSnapshots(ctx, pool, node, resource)
				if err != nil {
					log.Debug("resourceSnapshotList: ListLvmSnapshots failed", zap.Error(err))
					return fmt.Errorf("failed to list LVM snapshots: %w", err)
				}

				if len(snapshots) == 0 {
					log.Debug("resourceSnapshotList: no LVM snapshots", zap.String("pool", pool), zap.String("node", node))
					fmt.Printf("No LVM snapshots found for resource '%s'\n", resource)
					return nil
				}
				log.Debug("resourceSnapshotList: LVM snapshots", zap.String("pool", pool), zap.String("node", node), zap.Int("count", len(snapshots)))

				fmt.Printf("LVM snapshots for resource '%s':\n", resource)
				fmt.Printf("  %-45s %-8s %-14s %s\n", "Name", "Size", "Origin", "Created")
				fmt.Printf("  %-45s %-8s %-14s %s\n",
					strings.Repeat("-", 45), strings.Repeat("-", 8),
					strings.Repeat("-", 14), strings.Repeat("-", 25))
				for _, snap := range snapshots {
					fmt.Printf("  %-45s %-8s %-14s %s\n",
						snap.Name,
						fmt.Sprintf("%d GB", snap.SizeGb),
						snap.Origin,
						snap.CreatedAt)
				}
			}

			log.Debug("resourceSnapshotList: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	cmd.Flags().StringVar(&node, "node", "", "Node where resource exists")
	cmd.Flags().StringVar(&storageType, "storage-type", "lvm", "Storage type: lvm or zfs")
	cmd.Flags().StringVar(&pool, "pool", "", "Storage pool name (default: the resource's own pool)")

	_ = cmd.MarkFlagRequired("resource")

	return cmd
}

func resourceSnapshotRestore() *cobra.Command {
	var resource string
	var snapshotName string
	var node string
	var storageType string
	var pool string

	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore DRBD resource from snapshot",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceSnapshotRestore: running command", zap.Strings("args", args), zap.String("resource", resource),
				zap.String("snapshot", snapshotName), zap.String("node", node), zap.String("storage_type", storageType), zap.String("pool", pool))
			if resource == "" {
				log.Debug("resourceSnapshotRestore: --resource missing")
				return fmt.Errorf("resource name is required")
			}
			if snapshotName == "" {
				log.Debug("resourceSnapshotRestore: --name missing")
				return fmt.Errorf("snapshot name is required")
			}
			if pool == "" || node == "" {
				log.Debug("resourceSnapshotRestore: pool or node not given, asking the resource")
				var err error
				if pool, node, err = snapshotTarget(resource, pool, node); err != nil {
					log.Debug("resourceSnapshotRestore: snapshotTarget failed", zap.Error(err))
					return err
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceSnapshotRestore: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			if storageType == "zfs" {
				// ZFS rollback
				dataset := fmt.Sprintf("%s/%s_data", pool, resource)
				log.Debug("resourceSnapshotRestore: rolling back ZFS dataset", zap.String("dataset", dataset), zap.String("snapshot", snapshotName), zap.String("node", node))
				err = haifyClient.RestoreZFSSnapshot(ctx, dataset, snapshotName, node)
				if err != nil {
					log.Debug("resourceSnapshotRestore: RestoreZFSSnapshot failed", zap.Error(err))
					return fmt.Errorf("failed to restore ZFS snapshot: %w", err)
				}
				fmt.Printf("ZFS snapshot '%s' restored for resource '%s' on node '%s'\n", snapshotName, resource, node)
			} else {
				// LVM snapshot restore (merge)
				// Pass pool as VG name
				log.Debug("resourceSnapshotRestore: merging LVM snapshot", zap.String("pool", pool), zap.String("snapshot", snapshotName), zap.String("node", node))
				err = haifyClient.RestoreLvmSnapshot(ctx, pool, snapshotName, node)
				if err != nil {
					log.Debug("resourceSnapshotRestore: RestoreLvmSnapshot failed", zap.Error(err))
					return fmt.Errorf("failed to restore LVM snapshot: %w", err)
				}
				fmt.Printf("LVM snapshot '%s' restored for resource '%s' on node '%s'\n", snapshotName, resource, node)
				fmt.Println("Note: The snapshot has been merged back into the original volume. The resource's")
				fmt.Println("other replicas are resyncing from this node; until that finishes it has one")
				fmt.Printf("complete copy. Follow it with: haify resource status %s\n", resource)
			}

			log.Debug("resourceSnapshotRestore: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	cmd.Flags().StringVar(&snapshotName, "name", "", "Snapshot name")
	cmd.Flags().StringVar(&node, "node", "", "Node where resource exists")
	cmd.Flags().StringVar(&storageType, "storage-type", "lvm", "Storage type: lvm or zfs")
	cmd.Flags().StringVar(&pool, "pool", "", "Storage pool name (default: the resource's own pool)")

	_ = cmd.MarkFlagRequired("resource")
	_ = cmd.MarkFlagRequired("name")

	return cmd
}

// snapshotTarget fills in what a snapshot command left out from the resource
// itself: the pool its volumes live in, and a node holding a replica. The pool
// used to default to "data-pool", a name no cluster is required to have, so
// every snapshot command failed unless --pool was given — although the
// controller has always known which pool a resource is in.
func snapshotTarget(resource, pool, node string) (string, string, error) {
	log := zap.L()
	log.Debug("snapshotTarget: looking up the resource's pool and node", zap.String("resource", resource), zap.String("pool", pool), zap.String("node", node))
	ctx, cancel := context.WithTimeout(context.Background(), nodeOpTimeout)
	defer cancel()
	c, err := newHaifyClient()
	if err != nil {
		log.Debug("snapshotTarget: newHaifyClient failed", zap.Error(err))
		return "", "", fmt.Errorf("failed to connect to controller: %w", err)
	}
	defer closeClient(c)
	info, err := c.GetResource(ctx, resource)
	if err != nil {
		log.Debug("snapshotTarget: GetResource failed", zap.Error(err))
		return "", "", fmt.Errorf("look up resource %s: %w", resource, err)
	}
	if pool == "" {
		for _, v := range info.GetVolumes() {
			if v.GetPool() != "" {
				pool = strings.TrimPrefix(v.GetPool(), "haify_")
				break
			}
		}
		if pool == "" {
			log.Debug("snapshotTarget: resource records no pool", zap.String("resource", resource))
			return "", "", fmt.Errorf("resource %s does not record its pool; pass --pool", resource)
		}
		log.Debug("snapshotTarget: pool from the resource", zap.String("pool", pool))
	}
	if node == "" {
		if nodes := info.GetNodes(); len(nodes) > 0 {
			node = nodes[0]
			log.Debug("snapshotTarget: node from the resource's first replica", zap.String("node", node))
		} else {
			log.Debug("snapshotTarget: resource has no replica node", zap.String("resource", resource))
			return "", "", fmt.Errorf("resource %s has no replica node; pass --node", resource)
		}
	}
	log.Debug("snapshotTarget: done", zap.String("pool", pool), zap.String("node", node))
	return pool, node, nil
}
