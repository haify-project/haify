package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// Storage upkeep: trimming, disks, and storage jobs.

// TrimPools trims the DRBD-backed filesystems on node, or on every node.
func (c *HaifyClient) TrimPools(ctx context.Context, node string) (*haifypb.TrimPoolsResponse, error) {
	zap.L().Debug("HaifyClient.TrimPools: sending request", zap.String("node", node))
	return c.client.TrimPools(ctx, &haifypb.TrimPoolsRequest{Node: node})
}

// ListPoolDisks lists the disks under pools, with their health.
func (c *HaifyClient) ListPoolDisks(ctx context.Context, pool, node string) ([]*haifypb.PoolDiskInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListPoolDisks: sending request", zap.String("pool", pool), zap.String("node", node))
	resp, err := c.client.ListPoolDisks(ctx, &haifypb.ListPoolDisksRequest{Pool: pool, Node: node})
	if err != nil {
		log.Debug("HaifyClient.ListPoolDisks: ListPoolDisks failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListPoolDisks: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListPoolDisks: done", zap.Int("count", len(resp.Disks)))
	return resp.Disks, nil
}

// ListNodeDisks lists the disks on node (every online node when empty) and
// whether a pool can use each.
func (c *HaifyClient) ListNodeDisks(ctx context.Context, node string) ([]*haifypb.NodeDiskInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListNodeDisks: sending request", zap.String("node", node))
	resp, err := c.client.ListNodeDisks(ctx, &haifypb.ListNodeDisksRequest{Node: node})
	if err != nil {
		log.Debug("HaifyClient.ListNodeDisks: ListNodeDisks failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListNodeDisks: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListNodeDisks: done", zap.Int("count", len(resp.Disks)))
	return resp.Disks, nil
}

// RemovePoolDisk starts moving a disk's data off it and taking it out of the pool.
func (c *HaifyClient) RemovePoolDisk(ctx context.Context, pool, node, disk string) (*haifypb.StorageJobResponse, error) {
	zap.L().Debug("HaifyClient.RemovePoolDisk: sending request", zap.String("pool", pool), zap.String("node", node), zap.String("disk", disk))
	return jobResult(c.client.RemovePoolDisk(ctx, &haifypb.RemovePoolDiskRequest{Pool: pool, Node: node, Disk: disk}))
}

// ReplacePoolDisk starts moving a disk's data to a new disk.
func (c *HaifyClient) ReplacePoolDisk(ctx context.Context, pool, node, oldDisk, newDisk string, wipe bool) (*haifypb.StorageJobResponse, error) {
	zap.L().Debug("HaifyClient.ReplacePoolDisk: sending request", zap.String("pool", pool), zap.String("node", node), zap.String("old_disk", oldDisk), zap.String("new_disk", newDisk), zap.Bool("wipe", wipe))
	return jobResult(c.client.ReplacePoolDisk(ctx, &haifypb.ReplacePoolDiskRequest{Pool: pool, Node: node, OldDisk: oldDisk, NewDisk: newDisk, Wipe: wipe}))
}

// MoveVolume starts moving a resource's volume to another pool.
func (c *HaifyClient) MoveVolume(ctx context.Context, resource string, volumeID int32, pool string) (*haifypb.StorageJobResponse, error) {
	zap.L().Debug("HaifyClient.MoveVolume: sending request", zap.String("resource", resource), zap.Int32("volume_id", volumeID), zap.String("pool", pool))
	return jobResult(c.client.MoveVolume(ctx, &haifypb.MoveVolumeRequest{Resource: resource, VolumeId: volumeID, Pool: pool}))
}

// ListStorageJobs lists running (and optionally finished) storage jobs.
func (c *HaifyClient) ListStorageJobs(ctx context.Context, includeFinished bool) ([]*haifypb.StorageJobInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListStorageJobs: sending request", zap.Bool("include_finished", includeFinished))
	resp, err := c.client.ListStorageJobs(ctx, &haifypb.ListStorageJobsRequest{IncludeFinished: includeFinished})
	if err != nil {
		log.Debug("HaifyClient.ListStorageJobs: ListStorageJobs failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListStorageJobs: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListStorageJobs: done", zap.Int("count", len(resp.Jobs)))
	return resp.Jobs, nil
}

func jobResult(resp *haifypb.StorageJobResponse, err error) (*haifypb.StorageJobResponse, error) {
	log := zap.L()
	if err != nil {
		log.Debug("jobResult: call failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("jobResult: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("jobResult: job accepted", zap.String("job_id", resp.JobId))
	return resp, nil
}
