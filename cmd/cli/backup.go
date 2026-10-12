package main

import (
	"context"
	"fmt"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"

	"github.com/spf13/cobra"
)

// backupLimits is repeated in every long help below, because the single most
// expensive way to learn it is from a saturated uplink at 03:00.
const backupLimits = `LIMITATIONS (read before relying on this):
  * The first backup of a resource to a target is a full image. Later ones are
    incremental on thin pools: only blocks changed since the last backup are
    sent. A thick (non-thin) volume is backed up in full every time.
  * An incremental restores only together with every backup down to its full
    one, so none of those can be deleted while a later one exists. After 30
    incrementals the next backup is full again.
  * One snapshot per resource and target stays on the node as the base for the
    next incremental; it holds whatever the volume overwrote since.
  * LVM-backed volumes only. A ZFS zvol has no snapshot block device to read,
    so those resources are refused rather than half-supported.`

func backupCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Ship point-in-time copies off the cluster (S3 / SMB / WebDAV)",
		Long: `Back up DRBD resources to storage that is not part of the cluster.

This is the layer snapshots and WAN DR are not. A snapshot lives in the same
pool as its origin, so losing the machine loses both. WAN DR is a replica, so a
deletion replicates. A backup is a copy nothing in the cluster can reach.

Backups are taken from a storage-native snapshot, never from the live volume,
so the image is crash-consistent.

` + backupLimits,
	}
	cmd.AddCommand(backupTargetCommand())
	cmd.AddCommand(backupCreateCommand())
	cmd.AddCommand(backupListCommand())
	cmd.AddCommand(backupRestoreCommand())
	cmd.AddCommand(backupDeleteCommand())
	cmd.AddCommand(backupScheduleCommand())
	cmd.AddCommand(backupImportCommand())
	return cmd
}

func backupCreateCommand() *cobra.Command {
	var resource, target, node string
	var full bool
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Take a crash-consistent backup of a resource (incremental after the first)",
		Long: `Snapshot every volume of a resource and ship the images to a target.

The backup is only recorded as completed once every image has been uploaded and
the target has confirmed it holds exactly as many bytes as were sent. A run that
fails part-way is recorded as failed and its objects are removed; it is never
offered for restore.

` + backupLimits,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("backupCreateCommand: running command", zap.Strings("args", args), zap.String("resource", resource),
				zap.String("target", target), zap.String("node", node), zap.Bool("full", full), zap.Duration("timeout", timeout))
			if resource == "" {
				log.Debug("backupCreateCommand: --resource missing")
				return fmt.Errorf("--resource is required")
			}
			if target == "" {
				log.Debug("backupCreateCommand: --target missing")
				return fmt.Errorf("--target is required (see `haify backup target list`)")
			}

			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				log.Debug("backupCreateCommand: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(c)

			info, err := c.CreateBackup(ctx, resource, target, node, full)
			if err != nil {
				log.Debug("backupCreateCommand: CreateBackup failed", zap.Error(err))
				return fmt.Errorf("backup failed: %w", err)
			}
			log.Debug("backupCreateCommand: backup completed", zap.String("backup_id", info.Id), zap.String("node", info.Node),
				zap.String("kind", info.Kind), zap.String("parent", info.Parent), zap.Uint64("total_bytes", info.TotalBytes), zap.Int("volumes", len(info.Volumes)))
			out := cmd.OutOrStdout()
			// Writes to the command's own output stream are best-effort. The only ways
			// they fail are a closed pipe (`haify ... | head`) or a full disk, neither of
			// which this command can report anywhere the operator is still looking, and
			// treating them as errors would report a successful operation as failed.
			_, _ = fmt.Fprintf(out, "Backup %s completed\n", info.Id)
			_, _ = fmt.Fprintf(out, "  resource: %s (from node %s)\n", info.Resource, info.Node)
			_, _ = fmt.Fprintf(out, "  target:   %s/%s\n", info.Target, info.Prefix)
			_, _ = fmt.Fprintf(out, "  size:     %s across %d volume(s)\n", humanBytes(info.TotalBytes), len(info.Volumes))
			if info.Kind == "incremental" {
				_, _ = fmt.Fprintf(out, "  kind:     incremental on %s, %s changed\n", info.Parent, humanBytes(changedBytes(info)))
			} else {
				_, _ = fmt.Fprintf(out, "  kind:     full\n")
			}
			log.Debug("backupCreateCommand: done")
			return nil
		},
	}
	cmd.Flags().StringVar(&resource, "resource", "", "Resource to back up (required)")
	cmd.Flags().StringVar(&target, "target", "", "Backup target name (required)")
	cmd.Flags().StringVar(&node, "node", "", "Read from this replica (default: the one holding the last backup's base, else an UpToDate Secondary)")
	cmd.Flags().BoolVar(&full, "full", false, "Take a full backup even when an incremental is possible")
	// A full image of a real volume takes hours, not seconds; the default RPC
	// timeout used elsewhere in this CLI would abort the transfer.
	cmd.Flags().DurationVar(&timeout, "timeout", 24*time.Hour, "Give up if the backup takes longer than this")
	return cmd
}

func backupListCommand() *cobra.Command {
	var resource, target string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List backups, newest first",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("backupListCommand: running command", zap.Strings("args", args), zap.String("resource", resource), zap.String("target", target))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				log.Debug("backupListCommand: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(c)

			backups, err := c.ListBackups(ctx, resource, target)
			if err != nil {
				log.Debug("backupListCommand: ListBackups failed", zap.Error(err))
				return fmt.Errorf("failed to list backups: %w", err)
			}
			out := cmd.OutOrStdout()
			if len(backups) == 0 {
				log.Debug("backupListCommand: no backups")
				_, _ = fmt.Fprintln(out, "No backups found")
				return nil
			}
			for _, b := range backups {
				log.Debug("backupListCommand: backup", zap.String("backup_id", b.Id), zap.String("state", b.State),
					zap.String("resource", b.Resource), zap.String("target", b.Target), zap.String("kind", b.Kind))
				_, _ = fmt.Fprintf(out, "%s  [%s]\n", b.Id, b.State)
				_, _ = fmt.Fprintf(out, "  resource=%s target=%s node=%s size=%s\n",
					b.Resource, b.Target, b.Node, humanBytes(b.TotalBytes))
				if b.Kind == "incremental" {
					_, _ = fmt.Fprintf(out, "  incremental on %s, %s changed\n", b.Parent, humanBytes(changedBytes(b)))
				}
				if b.Schedule != "" {
					_, _ = fmt.Fprintf(out, "  schedule=%s\n", b.Schedule)
				}
				if b.LockMode != "" {
					_, _ = fmt.Fprintf(out, "  locked (%s) until %s\n", b.LockMode, b.RetainUntil)
				}
				if b.ReadAt != "" {
					_, _ = fmt.Fprintf(out, "  read as of %s\n", b.ReadAt)
				}
				if b.StartedAt != "" {
					_, _ = fmt.Fprintf(out, "  started=%s finished=%s\n", b.StartedAt, orDash(b.FinishedAt))
				}
				if b.Error != "" {
					_, _ = fmt.Fprintf(out, "  error: %s\n", b.Error)
				}
			}
			log.Debug("backupListCommand: done", zap.Int("count", len(backups)))
			return nil
		},
	}
	cmd.Flags().StringVar(&resource, "resource", "", "Only backups of this resource")
	cmd.Flags().StringVar(&target, "target", "", "Only backups on this target")
	return cmd
}

func backupRestoreCommand() *cobra.Command {
	var resource, node string
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "restore <backup-id>",
		Short: "Restore a completed backup into a resource",
		Long: `Write a backup's images back onto a resource, in place.

The destination resource must already exist, must be at least as large as the
backup, and must NOT be in use: the restore is refused while the resource is
Primary anywhere, or while a gateway exports it (a gateway can promote it in the
middle of the write). Stop the workload first.

Only a backup in the "completed" state can be restored. A run that was
interrupted or that failed verification is never restorable, by design.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("backupRestoreCommand: running command", zap.Strings("args", args), zap.String("resource", resource),
				zap.String("node", node), zap.Duration("timeout", timeout))
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				log.Debug("backupRestoreCommand: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(c)

			info, err := c.RestoreBackup(ctx, args[0], resource, node)
			if err != nil {
				log.Debug("backupRestoreCommand: RestoreBackup failed", zap.Error(err))
				return fmt.Errorf("restore failed: %w", err)
			}
			dest := resource
			if dest == "" {
				log.Debug("backupRestoreCommand: restored into the source resource", zap.String("resource", info.Resource))
				dest = info.Resource
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Backup %s restored into %q (%s across %d volume(s))\n",
				info.Id, dest, humanBytes(info.TotalBytes), len(info.Volumes))
			log.Debug("backupRestoreCommand: done", zap.String("backup_id", info.Id), zap.String("resource", dest), zap.Uint64("total_bytes", info.TotalBytes))
			return nil
		},
	}
	cmd.Flags().StringVar(&resource, "resource", "", "Restore into this resource (default: the one it came from)")
	cmd.Flags().StringVar(&node, "node", "", "Write through this replica (default: any UpToDate one)")
	cmd.Flags().DurationVar(&timeout, "timeout", 24*time.Hour, "Give up if the restore takes longer than this")
	return cmd
}

func backupDeleteCommand() *cobra.Command {
	var node string
	var force bool

	cmd := &cobra.Command{
		Use:   "delete <backup-id>",
		Short: "Delete a backup's objects and its record",
		Long: `Remove a backup from its target.

The record is only dropped once the objects are gone, so a failure leaves the
backup listed and the deletion retryable rather than turning it into storage
nobody knows about. --force drops the record regardless, which is what to use
when the target itself no longer exists.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("backupDeleteCommand: running command", zap.Strings("args", args), zap.String("node", node), zap.Bool("force", force))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				log.Debug("backupDeleteCommand: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(c)

			if err := c.DeleteBackup(ctx, args[0], node, force); err != nil {
				log.Debug("backupDeleteCommand: DeleteBackup failed", zap.Error(err))
				return fmt.Errorf("failed to delete backup: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Backup %s deleted\n", args[0])
			log.Debug("backupDeleteCommand: done")
			return nil
		},
	}
	cmd.Flags().StringVar(&node, "node", "", "Reach the target from this node (default: the node that wrote it)")
	cmd.Flags().BoolVar(&force, "force", false, "Drop the record even if the objects could not be removed")
	return cmd
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// changedBytes is how much an incremental backup carries across its volumes.
func changedBytes(b *haifypb.BackupInfo) uint64 {
	var n uint64
	for _, v := range b.Volumes {
		n += v.ChangedBytes
	}
	zap.L().Debug("changedBytes: summed", zap.String("backup_id", b.Id), zap.Int("volumes", len(b.Volumes)), zap.Uint64("changed_bytes", n))
	return n
}
