package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// CreateSnapshot creates a snapshot
func (c *HaifyClient) CreateSnapshot(ctx context.Context, volume, snapshotName, node string) error {
	req := &haifypb.CreateSnapshotRequest{
		Volume:       volume,
		SnapshotName: snapshotName,
		Node:         node,
	}

	resp, err := c.client.CreateSnapshot(ctx, req)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}

	return nil
}

// DeleteSnapshot deletes a snapshot
func (c *HaifyClient) DeleteSnapshot(ctx context.Context, volume, snapshotName, node string) error {
	req := &haifypb.DeleteSnapshotRequest{
		Volume:       volume,
		SnapshotName: snapshotName,
		Node:         node,
	}

	resp, err := c.client.DeleteSnapshot(ctx, req)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}

	return nil
}

// ListSnapshots lists snapshots for a volume
func (c *HaifyClient) ListSnapshots(ctx context.Context, volume, node string) ([]*haifypb.SnapshotInfo, error) {
	req := &haifypb.ListSnapshotsRequest{
		Volume: volume,
		Node:   node,
	}

	resp, err := c.client.ListSnapshots(ctx, req)
	if err != nil {
		return nil, err
	}

	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

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
	resp, err := c.client.CreateSnapshotSchedule(ctx, &haifypb.CreateSnapshotScheduleRequest{
		Resource: resource,
		Cron:     cron,
		Keep:     keep,
		Enabled:  enabled,
		LockDays: lockDays,
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// ListSnapshotSchedules returns all snapshot schedules.
func (c *HaifyClient) ListSnapshotSchedules(ctx context.Context) ([]*haifypb.SnapshotScheduleInfo, error) {
	resp, err := c.client.ListSnapshotSchedules(ctx, &haifypb.ListSnapshotSchedulesRequest{})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}
	return resp.Schedules, nil
}

// DeleteSnapshotSchedule removes a snapshot schedule by name.
func (c *HaifyClient) DeleteSnapshotSchedule(ctx context.Context, name string) error {
	resp, err := c.client.DeleteSnapshotSchedule(ctx, &haifypb.DeleteSnapshotScheduleRequest{Name: name})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// FreezeSnapshotSchedule freezes resource's schedule for hours (0: a week)
// and returns when the freeze ends.
func (c *HaifyClient) FreezeSnapshotSchedule(ctx context.Context, resource string, hours uint32, reason string) (string, error) {
	resp, err := c.client.FreezeSnapshotSchedule(ctx, &haifypb.FreezeSnapshotScheduleRequest{
		Resource: resource, Hours: hours, Reason: reason})
	if err != nil {
		return "", err
	}
	if !resp.Success {
		return "", fmt.Errorf("%s", resp.Message)
	}
	return resp.FrozenUntil, nil
}

// UnfreezeSnapshotSchedule ends a freeze early.
func (c *HaifyClient) UnfreezeSnapshotSchedule(ctx context.Context, resource string) error {
	resp, err := c.client.UnfreezeSnapshotSchedule(ctx, &haifypb.UnfreezeSnapshotScheduleRequest{Resource: resource})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}
	return nil
}

// PopulateVolume copies sourceDevice (another volume's backing store, or a
// snapshot's device) into an already-created, still-empty resource. Unlike
// RestoreSnapshot it fills a different, new volume rather than merging into
// the source's own origin. For a snapshot, PopulateVolumeFromSnapshot names it
// instead and works on ZFS pools too.
func (c *HaifyClient) PopulateVolume(ctx context.Context, resource string, volumeID uint32, sourceDevice, node string) (uint64, error) {
	resp, err := c.client.PopulateVolume(ctx, &haifypb.PopulateVolumeRequest{
		Resource:     resource,
		VolumeId:     volumeID,
		SourceDevice: sourceDevice,
		Node:         node,
	})
	if err != nil {
		return 0, err
	}
	if !resp.Success {
		return 0, fmt.Errorf("%s", resp.Message)
	}
	return resp.BytesCopied, nil
}

// PopulateVolumeFromSnapshot copies a snapshot into an already-created,
// still-empty resource, on node, which holds both. The controller finds the
// snapshot's device itself, on LVM and ZFS pools alike.
func (c *HaifyClient) PopulateVolumeFromSnapshot(ctx context.Context, resource string, volumeID uint32, src *haifypb.PopulateSnapshotSource, node string) (uint64, error) {
	resp, err := c.client.PopulateVolume(ctx, &haifypb.PopulateVolumeRequest{
		Resource:       resource,
		VolumeId:       volumeID,
		SourceSnapshot: src,
		Node:           node,
	})
	if err != nil {
		return 0, err
	}
	if !resp.Success {
		return 0, fmt.Errorf("%s", resp.Message)
	}
	return resp.BytesCopied, nil
}

// RestoreSnapshot restores a snapshot to its source volume
func (c *HaifyClient) RestoreSnapshot(ctx context.Context, volume, snapshotName, node string) error {
	req := &haifypb.RestoreSnapshotRequest{
		Volume:       volume,
		SnapshotName: snapshotName,
		Node:         node,
	}

	resp, err := c.client.RestoreSnapshot(ctx, req)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}

	return nil
}

// CreateLvmSnapshot creates an LVM snapshot
func (c *HaifyClient) CreateLvmSnapshot(ctx context.Context, pool, lvName, snapshotName, node, size string) error {
	req := &haifypb.CreateLvmSnapshotRequest{
		Resource:     pool, // Mapped to VG Name
		LvName:       lvName,
		SnapshotName: snapshotName,
		Node:         node,
		Size:         size,
	}

	resp, err := c.client.CreateLvmSnapshot(ctx, req)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}

	return nil
}

// DeleteLvmSnapshot deletes an LVM snapshot
func (c *HaifyClient) DeleteLvmSnapshot(ctx context.Context, pool, snapshotName, node string) error {
	req := &haifypb.DeleteLvmSnapshotRequest{
		LvName:       pool, // Mapped to VG Name
		SnapshotName: snapshotName,
		Node:         node,
	}

	resp, err := c.client.DeleteLvmSnapshot(ctx, req)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}

	return nil
}

// ListLvmSnapshots lists LVM snapshots in a pool (VG). resource narrows the
// result to that DRBD resource's volumes; empty lists the whole pool.
func (c *HaifyClient) ListLvmSnapshots(ctx context.Context, pool, node, resource string) ([]*haifypb.SnapshotInfo, error) {
	req := &haifypb.ListLvmSnapshotsRequest{
		LvName:   pool, // the pool is a volume group at the LVM level
		Node:     node,
		Resource: resource,
	}

	resp, err := c.client.ListLvmSnapshots(ctx, req)
	if err != nil {
		return nil, err
	}

	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	return resp.Snapshots, nil
}

// RestoreLvmSnapshot restores an LVM snapshot
func (c *HaifyClient) RestoreLvmSnapshot(ctx context.Context, pool, snapshotName, node string) error {
	req := &haifypb.RestoreLvmSnapshotRequest{
		LvName:       pool, // Mapped to VG Name
		SnapshotName: snapshotName,
		Node:         node,
	}

	resp, err := c.client.RestoreLvmSnapshot(ctx, req)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}

	return nil
}
