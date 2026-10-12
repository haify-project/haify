package mcpserver

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// Snapshot naming follows the controller convention used by haify:
// the data volume of a resource is "<resource>_data" inside its pool, so
// LVM snapshots target LV "<resource>_data" in VG <pool>, and ZFS
// snapshots target dataset "<pool>/<resource>_data".

func zfsDataset(pool, resource string) string {
	return fmt.Sprintf("%s/%s_data", pool, resource)
}

func lvmDataLV(resource string) string {
	return fmt.Sprintf("%s_data", resource)
}

// ---- input/output types ----

type snapshotIn struct {
	Resource    string `json:"resource" jsonschema:"DRBD resource name"`
	Name        string `json:"name" jsonschema:"snapshot name"`
	Node        string `json:"node" jsonschema:"node where the resource volume exists"`
	Pool        string `json:"pool" jsonschema:"storage pool backing the resource"`
	StorageType string `json:"storage_type,omitempty" jsonschema:"lvm (default) or zfs"`
}

type snapshotCreateIn struct {
	Resource    string `json:"resource" jsonschema:"DRBD resource name"`
	Name        string `json:"name" jsonschema:"snapshot name"`
	Node        string `json:"node" jsonschema:"node where the resource volume exists"`
	Pool        string `json:"pool" jsonschema:"storage pool backing the resource"`
	StorageType string `json:"storage_type,omitempty" jsonschema:"lvm (default) or zfs"`
	SizeGiB     uint32 `json:"size_gib,omitempty" jsonschema:"copy-on-write space in GiB reserved for the LVM snapshot (default 1); ignored for ZFS"`
}

type snapshotListIn struct {
	Resource    string `json:"resource" jsonschema:"DRBD resource name"`
	Node        string `json:"node" jsonschema:"node to query"`
	Pool        string `json:"pool" jsonschema:"storage pool backing the resource"`
	StorageType string `json:"storage_type,omitempty" jsonschema:"lvm (default) or zfs"`
}

type snapshotOut struct {
	Name      string `json:"name"`
	Volume    string `json:"volume,omitempty"`
	SizeGB    uint64 `json:"size_gb,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	// Origin is the volume the snapshot was taken from, which is what says
	// which resource it belongs to when a pool holds several.
	Origin string `json:"origin,omitempty"`
}

type snapshotListOut struct {
	Snapshots []snapshotOut `json:"snapshots"`
}

// registerSnapshotTools adds storage-type-aware snapshot tools.
func (s *Server) registerSnapshotTools(srv *mcp.Server) {
	addWrite(s, srv, writeTool("haify_snapshot_create", "Create snapshot",
		"Create a point-in-time snapshot of a DRBD resource's data volume. "+
			"LVM uses copy-on-write snapshots (reserve enough COW space via size_gib); ZFS uses native snapshots."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in snapshotCreateIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_snapshot_create: creating snapshot", zap.String("resource", in.Resource),
				zap.String("name", in.Name), zap.String("node", in.Node), zap.String("pool", in.Pool),
				zap.String("storage_type", in.StorageType), zap.Uint32("size_gib", in.SizeGiB))
			if in.StorageType == "zfs" {
				dataset := zfsDataset(in.Pool, in.Resource)
				s.logger.Debug("haify_snapshot_create: zfs snapshot", zap.String("dataset", dataset))
				if err := s.client.CreateZFSSnapshot(ctx, dataset, in.Name, in.Node); err != nil {
					s.logger.Debug("haify_snapshot_create: zfs snapshot failed", zap.String("dataset", dataset), zap.Error(err))
					return nil, opResult{}, err
				}
			} else {
				size := in.SizeGiB
				if size == 0 {
					s.logger.Debug("haify_snapshot_create: no COW size given, reserving 1 GiB")
					size = 1
				}
				s.logger.Debug("haify_snapshot_create: lvm snapshot", zap.String("pool", in.Pool),
					zap.String("lv", lvmDataLV(in.Resource)), zap.Uint32("size_gib", size))
				err := s.client.CreateLvmSnapshot(ctx, in.Pool, lvmDataLV(in.Resource), in.Name,
					in.Node, fmt.Sprintf("%dG", size))
				if err != nil {
					s.logger.Debug("haify_snapshot_create: lvm snapshot failed", zap.String("pool", in.Pool), zap.Error(err))
					return nil, opResult{}, err
				}
			}
			s.logger.Debug("haify_snapshot_create: created", zap.String("resource", in.Resource), zap.String("name", in.Name))
			return nil, ok(fmt.Sprintf("snapshot %s created for resource %s on %s", in.Name, in.Resource, in.Node)), nil
		})

	addRead(s, srv, readOnlyTool("haify_snapshot_list", "List snapshots",
		"List snapshots of a DRBD resource's data volume on a node."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in snapshotListIn) (*mcp.CallToolResult, snapshotListOut, error) {
			var (
				infos []*haifypb.SnapshotInfo
				err   error
			)
			s.logger.Debug("haify_snapshot_list: listing snapshots", zap.String("resource", in.Resource),
				zap.String("node", in.Node), zap.String("pool", in.Pool), zap.String("storage_type", in.StorageType))
			if in.StorageType == "zfs" {
				s.logger.Debug("haify_snapshot_list: listing zfs snapshots", zap.String("dataset", zfsDataset(in.Pool, in.Resource)))
				infos, err = s.client.ListZFSSnapshots(ctx, zfsDataset(in.Pool, in.Resource), in.Node)
			} else {
				s.logger.Debug("haify_snapshot_list: listing lvm snapshots", zap.String("pool", in.Pool))
				infos, err = s.client.ListLvmSnapshots(ctx, in.Pool, in.Node, in.Resource)
			}
			if err != nil {
				s.logger.Debug("haify_snapshot_list: list failed", zap.String("resource", in.Resource), zap.Error(err))
				return nil, snapshotListOut{}, err
			}
			out := snapshotListOut{Snapshots: make([]snapshotOut, 0, len(infos))}
			for _, sn := range infos {
				s.logger.Debug("haify_snapshot_list: snapshot", zap.String("name", sn.Name), zap.String("volume", sn.Volume),
					zap.String("origin", sn.Origin), zap.Uint64("size_gb", sn.SizeGb))
				out.Snapshots = append(out.Snapshots, snapshotOut{
					Name:      sn.Name,
					Volume:    sn.Volume,
					SizeGB:    sn.SizeGb,
					CreatedAt: sn.CreatedAt,
					Origin:    sn.Origin,
				})
			}
			s.logger.Debug("haify_snapshot_list: listed", zap.Int("count", len(out.Snapshots)))
			return nil, out, nil
		})

	addWrite(s, srv, destructiveTool("haify_snapshot_delete", "Delete snapshot",
		"Delete a snapshot of a DRBD resource's data volume."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in snapshotIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_snapshot_delete: deleting snapshot", zap.String("resource", in.Resource),
				zap.String("name", in.Name), zap.String("node", in.Node), zap.String("pool", in.Pool),
				zap.String("storage_type", in.StorageType))
			if in.StorageType == "zfs" {
				snapshot := fmt.Sprintf("%s@%s", zfsDataset(in.Pool, in.Resource), in.Name)
				s.logger.Debug("haify_snapshot_delete: deleting zfs snapshot", zap.String("snapshot", snapshot))
				if err := s.client.DeleteZFSSnapshot(ctx, snapshot, in.Node); err != nil {
					s.logger.Debug("haify_snapshot_delete: zfs delete failed", zap.String("snapshot", snapshot), zap.Error(err))
					return nil, opResult{}, err
				}
			} else {
				s.logger.Debug("haify_snapshot_delete: deleting lvm snapshot", zap.String("pool", in.Pool))
				if err := s.client.DeleteLvmSnapshot(ctx, in.Pool, in.Name, in.Node); err != nil {
					s.logger.Debug("haify_snapshot_delete: lvm delete failed", zap.String("pool", in.Pool), zap.Error(err))
					return nil, opResult{}, err
				}
			}
			s.logger.Debug("haify_snapshot_delete: deleted", zap.String("resource", in.Resource), zap.String("name", in.Name))
			return nil, ok(fmt.Sprintf("snapshot %s of resource %s deleted on %s", in.Name, in.Resource, in.Node)), nil
		})

	addWrite(s, srv, destructiveTool("haify_snapshot_restore", "Restore snapshot",
		"Roll a DRBD resource's data volume back to a snapshot. All changes since the snapshot are lost. "+
			"The resource should be stopped (Secondary everywhere, unmounted) before restoring."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in snapshotIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_snapshot_restore: restoring snapshot", zap.String("resource", in.Resource),
				zap.String("name", in.Name), zap.String("node", in.Node), zap.String("pool", in.Pool),
				zap.String("storage_type", in.StorageType))
			if in.StorageType == "zfs" {
				s.logger.Debug("haify_snapshot_restore: rolling back zfs dataset", zap.String("dataset", zfsDataset(in.Pool, in.Resource)))
				if err := s.client.RestoreZFSSnapshot(ctx, zfsDataset(in.Pool, in.Resource), in.Name, in.Node); err != nil {
					s.logger.Debug("haify_snapshot_restore: zfs restore failed", zap.Error(err))
					return nil, opResult{}, err
				}
			} else {
				s.logger.Debug("haify_snapshot_restore: merging lvm snapshot", zap.String("pool", in.Pool))
				if err := s.client.RestoreLvmSnapshot(ctx, in.Pool, in.Name, in.Node); err != nil {
					s.logger.Debug("haify_snapshot_restore: lvm restore failed", zap.Error(err))
					return nil, opResult{}, err
				}
			}
			s.logger.Debug("haify_snapshot_restore: restored", zap.String("resource", in.Resource), zap.String("name", in.Name))
			return nil, ok(fmt.Sprintf("resource %s restored from snapshot %s on %s", in.Resource, in.Name, in.Node)), nil
		})

	s.registerSnapshotScheduleTools(srv)
}

type scheduleCreateIn struct {
	Resource string `json:"resource" jsonschema:"DRBD resource to snapshot on a schedule"`
	Cron     string `json:"cron" jsonschema:"standard 5-field cron expression, e.g. '0 * * * *' for hourly"`
	Hourly   int32  `json:"keep_hourly,omitempty" jsonschema:"hourly snapshots to retain"`
	Daily    int32  `json:"keep_daily,omitempty" jsonschema:"daily snapshots to retain"`
	Weekly   int32  `json:"keep_weekly,omitempty" jsonschema:"weekly snapshots to retain"`
	Monthly  int32  `json:"keep_monthly,omitempty" jsonschema:"monthly snapshots to retain"`
	Yearly   int32  `json:"keep_yearly,omitempty" jsonschema:"yearly snapshots to retain"`
	Disabled bool   `json:"disabled,omitempty" jsonschema:"create the schedule disabled"`
}

type scheduleNameIn struct {
	Name string `json:"name" jsonschema:"schedule name (equals the resource name)"`
}

type scheduleOut struct {
	Name     string `json:"name"`
	Resource string `json:"resource"`
	Cron     string `json:"cron"`
	Enabled  bool   `json:"enabled"`
	Keep     string `json:"keep"`
	LastRun  string `json:"last_run,omitempty"`
	NextRun  string `json:"next_run,omitempty"`
}

type scheduleListOut struct {
	Schedules []scheduleOut `json:"schedules"`
}

// registerSnapshotScheduleTools adds cron-driven snapshot schedule tools.
func (s *Server) registerSnapshotScheduleTools(srv *mcp.Server) {
	addWrite(s, srv, writeTool("haify_snapshot_schedule_create", "Create snapshot schedule",
		"Create (or replace) a cron-driven snapshot schedule for a resource. Snapshots are taken on every "+
			"diskful node and pruned by a grandfather-father-son retention policy. One schedule per resource. "+
			"Set at least one keep_* count."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in scheduleCreateIn) (*mcp.CallToolResult, opResult, error) {
			keep := &haifypb.GFSRetention{
				Hourly: in.Hourly, Daily: in.Daily, Weekly: in.Weekly, Monthly: in.Monthly, Yearly: in.Yearly,
			}
			s.logger.Debug("haify_snapshot_schedule_create: creating schedule", zap.String("resource", in.Resource),
				zap.String("cron", in.Cron), zap.Bool("disabled", in.Disabled), zap.Int32("keep_hourly", in.Hourly),
				zap.Int32("keep_daily", in.Daily), zap.Int32("keep_weekly", in.Weekly),
				zap.Int32("keep_monthly", in.Monthly), zap.Int32("keep_yearly", in.Yearly))
			if err := s.client.CreateSnapshotSchedule(ctx, in.Resource, in.Cron, keep, !in.Disabled); err != nil {
				s.logger.Debug("haify_snapshot_schedule_create: create failed", zap.String("resource", in.Resource), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_snapshot_schedule_create: created", zap.String("resource", in.Resource))
			return nil, ok(fmt.Sprintf("snapshot schedule for %s created (cron %q)", in.Resource, in.Cron)), nil
		})

	addRead(s, srv, readOnlyTool("haify_snapshot_schedule_list", "List snapshot schedules",
		"List all cron-driven snapshot schedules with their retention policy, last run and next run time."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, scheduleListOut, error) {
			s.logger.Debug("haify_snapshot_schedule_list: listing schedules")
			schedules, err := s.client.ListSnapshotSchedules(ctx)
			if err != nil {
				s.logger.Debug("haify_snapshot_schedule_list: list failed", zap.Error(err))
				return nil, scheduleListOut{}, err
			}
			out := scheduleListOut{Schedules: make([]scheduleOut, 0, len(schedules))}
			for _, sc := range schedules {
				s.logger.Debug("haify_snapshot_schedule_list: schedule", zap.String("name", sc.Name),
					zap.String("resource", sc.Resource), zap.String("cron", sc.Cron), zap.Bool("enabled", sc.Enabled),
					zap.String("next_run", sc.NextRun))
				k := sc.Keep
				out.Schedules = append(out.Schedules, scheduleOut{
					Name:     sc.Name,
					Resource: sc.Resource,
					Cron:     sc.Cron,
					Enabled:  sc.Enabled,
					Keep: fmt.Sprintf("hourly=%d daily=%d weekly=%d monthly=%d yearly=%d",
						k.GetHourly(), k.GetDaily(), k.GetWeekly(), k.GetMonthly(), k.GetYearly()),
					LastRun: sc.LastRun,
					NextRun: sc.NextRun,
				})
			}
			s.logger.Debug("haify_snapshot_schedule_list: listed", zap.Int("count", len(out.Schedules)))
			return nil, out, nil
		})

	addWrite(s, srv, destructiveTool("haify_snapshot_schedule_delete", "Delete snapshot schedule",
		"Delete a snapshot schedule by name. Existing snapshots are kept; only future scheduled snapshots stop."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in scheduleNameIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_snapshot_schedule_delete: deleting schedule", zap.String("name", in.Name))
			if err := s.client.DeleteSnapshotSchedule(ctx, in.Name); err != nil {
				s.logger.Debug("haify_snapshot_schedule_delete: delete failed", zap.String("name", in.Name), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_snapshot_schedule_delete: deleted", zap.String("name", in.Name))
			return nil, ok(fmt.Sprintf("snapshot schedule %s deleted", in.Name)), nil
		})
}
