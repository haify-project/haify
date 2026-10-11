package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// CreateZFSPool creates a ZFS pool with the OpenZFS default compression.
func (c *HaifyClient) CreateZFSPool(ctx context.Context, name, node string, vdevs []string) error {
	return c.CreateZFSPoolOptions(ctx, name, node, vdevs, "", false, false)
}

// CreateZFSPoolOptions creates a ZFS pool with the given compression algorithm
// (empty: the OpenZFS default) and deduplication; wipe lets disks holding
// old data be erased.
func (c *HaifyClient) CreateZFSPoolOptions(ctx context.Context, name, node string, vdevs []string, compression string, dedup, wipe bool) error {
	req := &haifypb.CreateZFSPoolRequest{
		Name:        name,
		Node:        node,
		Vdevs:       vdevs,
		Compression: compression,
		Dedup:       dedup,
		Wipe:        wipe,
	}

	resp, err := c.client.CreateZFSPool(ctx, req)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}

	return nil
}

// DeleteZFSPool deletes a ZFS pool
func (c *HaifyClient) DeleteZFSPool(ctx context.Context, name, node string) error {
	req := &haifypb.DeleteZFSPoolRequest{
		Name: name,
		Node: node,
	}

	resp, err := c.client.DeleteZFSPool(ctx, req)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}

	return nil
}

// ListZFSpools lists all ZFS pools
func (c *HaifyClient) ListZFSpools(ctx context.Context) ([]*haifypb.PoolInfo, error) {
	req := &haifypb.ListZFSPoolsRequest{}

	resp, err := c.client.ListZFSpools(ctx, req)
	if err != nil {
		return nil, err
	}

	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	return resp.Pools, nil
}

// CreateZFSDataset creates a ZFS dataset
func (c *HaifyClient) CreateZFSDataset(ctx context.Context, datasetPath, node string) error {
	req := &haifypb.CreateZFSDatasetRequest{
		DatasetPath: datasetPath,
		Node:        node,
	}

	resp, err := c.client.CreateZFSDataset(ctx, req)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}

	return nil
}

// DeleteZFSDataset deletes a ZFS dataset or volume
func (c *HaifyClient) DeleteZFSDataset(ctx context.Context, datasetPath, node string) error {
	req := &haifypb.DeleteZFSDatasetRequest{
		DatasetPath: datasetPath,
		Node:        node,
	}

	resp, err := c.client.DeleteZFSDataset(ctx, req)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}

	return nil
}

// CreateZFSVolume creates a ZFS volume
func (c *HaifyClient) CreateZFSVolume(ctx context.Context, poolName, volumeName, size, node string) error {
	req := &haifypb.CreateZFSVolumeRequest{
		PoolName:   poolName,
		VolumeName: volumeName,
		Size:       size,
		Node:       node,
	}

	resp, err := c.client.CreateZFSVolume(ctx, req)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}

	return nil
}

// ResizeZFSVolume resizes a ZFS volume
func (c *HaifyClient) ResizeZFSVolume(ctx context.Context, volumePath, newSize, node string) error {
	req := &haifypb.ResizeZFSVolumeRequest{
		VolumePath: volumePath,
		NewSize:    newSize,
		Node:       node,
	}

	resp, err := c.client.ResizeZFSVolume(ctx, req)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}

	return nil
}

// CreateZFSSnapshot creates a ZFS snapshot
func (c *HaifyClient) CreateZFSSnapshot(ctx context.Context, dataset, snapshotName, node string) error {
	req := &haifypb.CreateZFSSnapshotRequest{
		Dataset:      dataset,
		SnapshotName: snapshotName,
		Node:         node,
	}

	resp, err := c.client.CreateZFSSnapshot(ctx, req)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}

	return nil
}

// DeleteZFSSnapshot deletes a ZFS snapshot
func (c *HaifyClient) DeleteZFSSnapshot(ctx context.Context, snapshot, node string) error {
	req := &haifypb.DeleteZFSSnapshotRequest{
		Snapshot: snapshot,
		Node:     node,
	}

	resp, err := c.client.DeleteZFSSnapshot(ctx, req)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}

	return nil
}

// ListZFSSnapshots lists ZFS snapshots
func (c *HaifyClient) ListZFSSnapshots(ctx context.Context, dataset, node string) ([]*haifypb.SnapshotInfo, error) {
	req := &haifypb.ListZFSSnapshotsRequest{
		Dataset: dataset,
		Node:    node,
	}

	resp, err := c.client.ListZFSSnapshots(ctx, req)
	if err != nil {
		return nil, err
	}

	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}

	return resp.Snapshots, nil
}

// RestoreZFSSnapshot restores a ZFS snapshot
func (c *HaifyClient) RestoreZFSSnapshot(ctx context.Context, dataset, snapshotName, node string) error {
	req := &haifypb.RestoreZFSSnapshotRequest{
		Dataset:      dataset,
		SnapshotName: snapshotName,
		Node:         node,
	}

	resp, err := c.client.RestoreZFSSnapshot(ctx, req)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}

	return nil
}

// CloneZFSSnapshot clones a ZFS snapshot
func (c *HaifyClient) CloneZFSSnapshot(ctx context.Context, snapshot, clonePath, node string) error {
	req := &haifypb.CloneZFSSnapshotRequest{
		Snapshot:  snapshot,
		ClonePath: clonePath,
		Node:      node,
	}

	resp, err := c.client.CloneZFSSnapshot(ctx, req)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("%s", resp.Message)
	}

	return nil
}
