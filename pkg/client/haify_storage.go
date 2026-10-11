package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// Storage upkeep: trimming, disks, and storage jobs.

// TrimPools trims the DRBD-backed filesystems on node, or on every node.
func (c *HaifyClient) TrimPools(ctx context.Context, node string) (*haifypb.TrimPoolsResponse, error) {
	return c.client.TrimPools(ctx, &haifypb.TrimPoolsRequest{Node: node})
}

// ListPoolDisks lists the disks under pools, with their health.
func (c *HaifyClient) ListPoolDisks(ctx context.Context, pool, node string) ([]*haifypb.PoolDiskInfo, error) {
	resp, err := c.client.ListPoolDisks(ctx, &haifypb.ListPoolDisksRequest{Pool: pool, Node: node})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}
	return resp.Disks, nil
}

// ListNodeDisks lists the disks on node (every online node when empty) and
// whether a pool can use each.
func (c *HaifyClient) ListNodeDisks(ctx context.Context, node string) ([]*haifypb.NodeDiskInfo, error) {
	resp, err := c.client.ListNodeDisks(ctx, &haifypb.ListNodeDisksRequest{Node: node})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}
	return resp.Disks, nil
}

// RemovePoolDisk starts moving a disk's data off it and taking it out of the pool.
func (c *HaifyClient) RemovePoolDisk(ctx context.Context, pool, node, disk string) (*haifypb.StorageJobResponse, error) {
	return jobResult(c.client.RemovePoolDisk(ctx, &haifypb.RemovePoolDiskRequest{Pool: pool, Node: node, Disk: disk}))
}

// ReplacePoolDisk starts moving a disk's data to a new disk.
func (c *HaifyClient) ReplacePoolDisk(ctx context.Context, pool, node, oldDisk, newDisk string, wipe bool) (*haifypb.StorageJobResponse, error) {
	return jobResult(c.client.ReplacePoolDisk(ctx, &haifypb.ReplacePoolDiskRequest{Pool: pool, Node: node, OldDisk: oldDisk, NewDisk: newDisk, Wipe: wipe}))
}

// MoveVolume starts moving a resource's volume to another pool.
func (c *HaifyClient) MoveVolume(ctx context.Context, resource string, volumeID int32, pool string) (*haifypb.StorageJobResponse, error) {
	return jobResult(c.client.MoveVolume(ctx, &haifypb.MoveVolumeRequest{Resource: resource, VolumeId: volumeID, Pool: pool}))
}

// ListStorageJobs lists running (and optionally finished) storage jobs.
func (c *HaifyClient) ListStorageJobs(ctx context.Context, includeFinished bool) ([]*haifypb.StorageJobInfo, error) {
	resp, err := c.client.ListStorageJobs(ctx, &haifypb.ListStorageJobsRequest{IncludeFinished: includeFinished})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}
	return resp.Jobs, nil
}

func jobResult(resp *haifypb.StorageJobResponse, err error) (*haifypb.StorageJobResponse, error) {
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}
	return resp, nil
}
