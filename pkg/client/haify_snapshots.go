package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// CreateSnapshot creates a snapshot
func (c *HaifyClient) CreateSnapshot(ctx context.Context, volume, snapshotName, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.CreateSnapshot: sending request", zap.String("volume", volume), zap.String("snapshot_name", snapshotName), zap.String("node", node))
	req := &haifypb.CreateSnapshotRequest{
		Volume:       volume,
		SnapshotName: snapshotName,
		Node:         node,
	}

	resp, err := c.client.CreateSnapshot(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.CreateSnapshot: CreateSnapshot failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.CreateSnapshot: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.CreateSnapshot: done")
	return nil
}

// DeleteSnapshot deletes a snapshot
func (c *HaifyClient) DeleteSnapshot(ctx context.Context, volume, snapshotName, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.DeleteSnapshot: sending request", zap.String("volume", volume), zap.String("snapshot_name", snapshotName), zap.String("node", node))
	req := &haifypb.DeleteSnapshotRequest{
		Volume:       volume,
		SnapshotName: snapshotName,
		Node:         node,
	}

	resp, err := c.client.DeleteSnapshot(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.DeleteSnapshot: DeleteSnapshot failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.DeleteSnapshot: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.DeleteSnapshot: done")
	return nil
}

// ListSnapshots lists snapshots for a volume
func (c *HaifyClient) ListSnapshots(ctx context.Context, volume, node string) ([]*haifypb.SnapshotInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListSnapshots: sending request", zap.String("volume", volume), zap.String("node", node))
	req := &haifypb.ListSnapshotsRequest{
		Volume: volume,
		Node:   node,
	}

	resp, err := c.client.ListSnapshots(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ListSnapshots: ListSnapshots failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.ListSnapshots: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.ListSnapshots: done", zap.Int("count", len(resp.Snapshots)))
	return resp.Snapshots, nil
}

// CreateSnapshotSchedule creates a cron-driven snapshot schedule with GFS
// retention, keeping the lock of any schedule it replaces.
func (c *HaifyClient) CreateSnapshotSchedule(ctx context.Context, resource, cron string, keep *haifypb.GFSRetention, enabled bool) error {
	return c.CreateSnapshotScheduleLocked(ctx, resource, cron, keep, enabled, nil)
}

// CreateSnapshotScheduleLocked is CreateSnapshotSchedule setting how many days
// the schedule locks its snapshots; nil keeps the current lock.
func (c *HaifyClient) CreateSnapshotScheduleLocked(ctx context.Context, resource, cron string, keep *haifypb.GFSRetention, enabled bool, lockDays *uint32) error {
	log := zap.L()
	log.Debug("HaifyClient.CreateSnapshotScheduleLocked: sending request", zap.String("resource", resource), zap.String("cron", cron), zap.Bool("enabled", enabled),
		zap.Bool("has_keep", keep != nil), zap.Bool("has_lock_days", lockDays != nil))
	resp, err := c.client.CreateSnapshotSchedule(ctx, &haifypb.CreateSnapshotScheduleRequest{
		Resource: resource,
		Cron:     cron,
		Keep:     keep,
		Enabled:  enabled,
		LockDays: lockDays,
	})
	if err != nil {
		log.Debug("HaifyClient.CreateSnapshotScheduleLocked: CreateSnapshotSchedule failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.CreateSnapshotScheduleLocked: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.CreateSnapshotScheduleLocked: done")
	return nil
}

// ListSnapshotSchedules returns all snapshot schedules.
func (c *HaifyClient) ListSnapshotSchedules(ctx context.Context) ([]*haifypb.SnapshotScheduleInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListSnapshotSchedules: sending request")
	resp, err := c.client.ListSnapshotSchedules(ctx, &haifypb.ListSnapshotSchedulesRequest{})
	if err != nil {
		log.Debug("HaifyClient.ListSnapshotSchedules: ListSnapshotSchedules failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListSnapshotSchedules: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListSnapshotSchedules: done", zap.Int("count", len(resp.Schedules)))
	return resp.Schedules, nil
}

// DeleteSnapshotSchedule removes a snapshot schedule by name.
func (c *HaifyClient) DeleteSnapshotSchedule(ctx context.Context, name string) error {
	log := zap.L()
	log.Debug("HaifyClient.DeleteSnapshotSchedule: sending request", zap.String("name", name))
	resp, err := c.client.DeleteSnapshotSchedule(ctx, &haifypb.DeleteSnapshotScheduleRequest{Name: name})
	if err != nil {
		log.Debug("HaifyClient.DeleteSnapshotSchedule: DeleteSnapshotSchedule failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.DeleteSnapshotSchedule: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.DeleteSnapshotSchedule: done")
	return nil
}

// FreezeSnapshotSchedule freezes resource's schedule for hours (0: a week)
// and returns when the freeze ends.
func (c *HaifyClient) FreezeSnapshotSchedule(ctx context.Context, resource string, hours uint32, reason string) (string, error) {
	log := zap.L()
	log.Debug("HaifyClient.FreezeSnapshotSchedule: sending request", zap.String("resource", resource), zap.Uint32("hours", hours), zap.String("reason", reason))
	resp, err := c.client.FreezeSnapshotSchedule(ctx, &haifypb.FreezeSnapshotScheduleRequest{
		Resource: resource, Hours: hours, Reason: reason})
	if err != nil {
		log.Debug("HaifyClient.FreezeSnapshotSchedule: FreezeSnapshotSchedule failed", zap.Error(err))
		return "", err
	}
	if !resp.Success {
		log.Debug("HaifyClient.FreezeSnapshotSchedule: controller reported failure", zap.String("message", resp.Message))
		return "", fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.FreezeSnapshotSchedule: done")
	return resp.FrozenUntil, nil
}

// UnfreezeSnapshotSchedule ends a freeze early.
func (c *HaifyClient) UnfreezeSnapshotSchedule(ctx context.Context, resource string) error {
	log := zap.L()
	log.Debug("HaifyClient.UnfreezeSnapshotSchedule: sending request", zap.String("resource", resource))
	resp, err := c.client.UnfreezeSnapshotSchedule(ctx, &haifypb.UnfreezeSnapshotScheduleRequest{Resource: resource})
	if err != nil {
		log.Debug("HaifyClient.UnfreezeSnapshotSchedule: UnfreezeSnapshotSchedule failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.UnfreezeSnapshotSchedule: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.UnfreezeSnapshotSchedule: done")
	return nil
}

// PopulateVolume copies sourceDevice (another volume's backing store, or a
// snapshot's device) into an already-created, still-empty resource. Unlike
// RestoreSnapshot it fills a different, new volume rather than merging into
// the source's own origin. For a snapshot, PopulateVolumeFromSnapshot names it
// instead and works on ZFS pools too.
func (c *HaifyClient) PopulateVolume(ctx context.Context, resource string, volumeID uint32, sourceDevice, node string) (uint64, error) {
	log := zap.L()
	log.Debug("HaifyClient.PopulateVolume: sending request", zap.String("resource", resource), zap.Uint32("volume_id", volumeID), zap.String("source_device", sourceDevice), zap.String("node", node))
	resp, err := c.client.PopulateVolume(ctx, &haifypb.PopulateVolumeRequest{
		Resource:     resource,
		VolumeId:     volumeID,
		SourceDevice: sourceDevice,
		Node:         node,
	})
	if err != nil {
		log.Debug("HaifyClient.PopulateVolume: PopulateVolume failed", zap.Error(err))
		return 0, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.PopulateVolume: controller reported failure", zap.String("message", resp.Message))
		return 0, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.PopulateVolume: done")
	return resp.BytesCopied, nil
}

// PopulateVolumeFromSnapshot copies a snapshot into an already-created,
// still-empty resource, on node, which holds both. The controller finds the
// snapshot's device itself, on LVM and ZFS pools alike.
func (c *HaifyClient) PopulateVolumeFromSnapshot(ctx context.Context, resource string, volumeID uint32, src *haifypb.PopulateSnapshotSource, node string) (uint64, error) {
	log := zap.L()
	log.Debug("HaifyClient.PopulateVolumeFromSnapshot: sending request", zap.String("resource", resource), zap.Uint32("volume_id", volumeID), zap.String("node", node),
		zap.String("src_resource", src.GetResource()), zap.Uint32("src_volume_id", src.GetVolumeId()), zap.String("src_snapshot", src.GetName()), zap.Bool("src_replicated", src.GetReplicated()))
	resp, err := c.client.PopulateVolume(ctx, &haifypb.PopulateVolumeRequest{
		Resource:       resource,
		VolumeId:       volumeID,
		SourceSnapshot: src,
		Node:           node,
	})
	if err != nil {
		log.Debug("HaifyClient.PopulateVolumeFromSnapshot: PopulateVolume failed", zap.Error(err))
		return 0, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.PopulateVolumeFromSnapshot: controller reported failure", zap.String("message", resp.Message))
		return 0, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.PopulateVolumeFromSnapshot: done")
	return resp.BytesCopied, nil
}

// RestoreSnapshot restores a snapshot to its source volume
func (c *HaifyClient) RestoreSnapshot(ctx context.Context, volume, snapshotName, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.RestoreSnapshot: sending request", zap.String("volume", volume), zap.String("snapshot_name", snapshotName), zap.String("node", node))
	req := &haifypb.RestoreSnapshotRequest{
		Volume:       volume,
		SnapshotName: snapshotName,
		Node:         node,
	}

	resp, err := c.client.RestoreSnapshot(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.RestoreSnapshot: RestoreSnapshot failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.RestoreSnapshot: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.RestoreSnapshot: done")
	return nil
}

// CreateLvmSnapshot creates an LVM snapshot
func (c *HaifyClient) CreateLvmSnapshot(ctx context.Context, pool, lvName, snapshotName, node, size string) error {
	log := zap.L()
	log.Debug("HaifyClient.CreateLvmSnapshot: sending request", zap.String("pool", pool), zap.String("lv_name", lvName), zap.String("snapshot_name", snapshotName), zap.String("node", node), zap.String("size", size))
	req := &haifypb.CreateLvmSnapshotRequest{
		Resource:     pool, // Mapped to VG Name
		LvName:       lvName,
		SnapshotName: snapshotName,
		Node:         node,
		Size:         size,
	}

	resp, err := c.client.CreateLvmSnapshot(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.CreateLvmSnapshot: CreateLvmSnapshot failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.CreateLvmSnapshot: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.CreateLvmSnapshot: done")
	return nil
}

// DeleteLvmSnapshot deletes an LVM snapshot
func (c *HaifyClient) DeleteLvmSnapshot(ctx context.Context, pool, snapshotName, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.DeleteLvmSnapshot: sending request", zap.String("pool", pool), zap.String("snapshot_name", snapshotName), zap.String("node", node))
	req := &haifypb.DeleteLvmSnapshotRequest{
		LvName:       pool, // Mapped to VG Name
		SnapshotName: snapshotName,
		Node:         node,
	}

	resp, err := c.client.DeleteLvmSnapshot(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.DeleteLvmSnapshot: DeleteLvmSnapshot failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.DeleteLvmSnapshot: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.DeleteLvmSnapshot: done")
	return nil
}

// ListLvmSnapshots lists LVM snapshots in a pool (VG). resource narrows the
// result to that DRBD resource's volumes; empty lists the whole pool.
func (c *HaifyClient) ListLvmSnapshots(ctx context.Context, pool, node, resource string) ([]*haifypb.SnapshotInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListLvmSnapshots: sending request", zap.String("pool", pool), zap.String("node", node), zap.String("resource", resource))
	req := &haifypb.ListLvmSnapshotsRequest{
		LvName:   pool, // the pool is a volume group at the LVM level
		Node:     node,
		Resource: resource,
	}

	resp, err := c.client.ListLvmSnapshots(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ListLvmSnapshots: ListLvmSnapshots failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.ListLvmSnapshots: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.ListLvmSnapshots: done", zap.Int("count", len(resp.Snapshots)))
	return resp.Snapshots, nil
}

// RestoreLvmSnapshot restores an LVM snapshot
func (c *HaifyClient) RestoreLvmSnapshot(ctx context.Context, pool, snapshotName, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.RestoreLvmSnapshot: sending request", zap.String("pool", pool), zap.String("snapshot_name", snapshotName), zap.String("node", node))
	req := &haifypb.RestoreLvmSnapshotRequest{
		LvName:       pool, // Mapped to VG Name
		SnapshotName: snapshotName,
		Node:         node,
	}

	resp, err := c.client.RestoreLvmSnapshot(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.RestoreLvmSnapshot: RestoreLvmSnapshot failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.RestoreLvmSnapshot: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.RestoreLvmSnapshot: done")
	return nil
}
