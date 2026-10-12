package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// CreateZFSPool creates a ZFS pool with the OpenZFS default compression.
func (c *HaifyClient) CreateZFSPool(ctx context.Context, name, node string, vdevs []string) error {
	return c.CreateZFSPoolOptions(ctx, name, node, vdevs, "", false, false)
}

// CreateZFSPoolOptions creates a ZFS pool with the given compression algorithm
// (empty: the OpenZFS default) and deduplication; wipe lets disks holding
// old data be erased.
func (c *HaifyClient) CreateZFSPoolOptions(ctx context.Context, name, node string, vdevs []string, compression string, dedup, wipe bool) error {
	log := zap.L()
	log.Debug("HaifyClient.CreateZFSPoolOptions: sending request", zap.String("name", name), zap.String("node", node), zap.Strings("vdevs", vdevs), zap.String("compression", compression), zap.Bool("dedup", dedup), zap.Bool("wipe", wipe))
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
		log.Debug("HaifyClient.CreateZFSPoolOptions: CreateZFSPool failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.CreateZFSPoolOptions: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.CreateZFSPoolOptions: done")
	return nil
}

// DeleteZFSPool deletes a ZFS pool
func (c *HaifyClient) DeleteZFSPool(ctx context.Context, name, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.DeleteZFSPool: sending request", zap.String("name", name), zap.String("node", node))
	req := &haifypb.DeleteZFSPoolRequest{
		Name: name,
		Node: node,
	}

	resp, err := c.client.DeleteZFSPool(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.DeleteZFSPool: DeleteZFSPool failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.DeleteZFSPool: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.DeleteZFSPool: done")
	return nil
}

// ListZFSpools lists all ZFS pools
func (c *HaifyClient) ListZFSpools(ctx context.Context) ([]*haifypb.PoolInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListZFSpools: sending request")
	req := &haifypb.ListZFSPoolsRequest{}

	resp, err := c.client.ListZFSpools(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ListZFSpools: ListZFSpools failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.ListZFSpools: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.ListZFSpools: done", zap.Int("count", len(resp.Pools)))
	return resp.Pools, nil
}

// CreateZFSDataset creates a ZFS dataset
func (c *HaifyClient) CreateZFSDataset(ctx context.Context, datasetPath, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.CreateZFSDataset: sending request", zap.String("dataset_path", datasetPath), zap.String("node", node))
	req := &haifypb.CreateZFSDatasetRequest{
		DatasetPath: datasetPath,
		Node:        node,
	}

	resp, err := c.client.CreateZFSDataset(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.CreateZFSDataset: CreateZFSDataset failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.CreateZFSDataset: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.CreateZFSDataset: done")
	return nil
}

// DeleteZFSDataset deletes a ZFS dataset or volume
func (c *HaifyClient) DeleteZFSDataset(ctx context.Context, datasetPath, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.DeleteZFSDataset: sending request", zap.String("dataset_path", datasetPath), zap.String("node", node))
	req := &haifypb.DeleteZFSDatasetRequest{
		DatasetPath: datasetPath,
		Node:        node,
	}

	resp, err := c.client.DeleteZFSDataset(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.DeleteZFSDataset: DeleteZFSDataset failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.DeleteZFSDataset: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.DeleteZFSDataset: done")
	return nil
}

// CreateZFSVolume creates a ZFS volume
func (c *HaifyClient) CreateZFSVolume(ctx context.Context, poolName, volumeName, size, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.CreateZFSVolume: sending request", zap.String("pool_name", poolName), zap.String("volume_name", volumeName), zap.String("size", size), zap.String("node", node))
	req := &haifypb.CreateZFSVolumeRequest{
		PoolName:   poolName,
		VolumeName: volumeName,
		Size:       size,
		Node:       node,
	}

	resp, err := c.client.CreateZFSVolume(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.CreateZFSVolume: CreateZFSVolume failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.CreateZFSVolume: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.CreateZFSVolume: done")
	return nil
}

// ResizeZFSVolume resizes a ZFS volume
func (c *HaifyClient) ResizeZFSVolume(ctx context.Context, volumePath, newSize, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.ResizeZFSVolume: sending request", zap.String("volume_path", volumePath), zap.String("new_size", newSize), zap.String("node", node))
	req := &haifypb.ResizeZFSVolumeRequest{
		VolumePath: volumePath,
		NewSize:    newSize,
		Node:       node,
	}

	resp, err := c.client.ResizeZFSVolume(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ResizeZFSVolume: ResizeZFSVolume failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.ResizeZFSVolume: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.ResizeZFSVolume: done")
	return nil
}

// CreateZFSSnapshot creates a ZFS snapshot
func (c *HaifyClient) CreateZFSSnapshot(ctx context.Context, dataset, snapshotName, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.CreateZFSSnapshot: sending request", zap.String("dataset", dataset), zap.String("snapshot_name", snapshotName), zap.String("node", node))
	req := &haifypb.CreateZFSSnapshotRequest{
		Dataset:      dataset,
		SnapshotName: snapshotName,
		Node:         node,
	}

	resp, err := c.client.CreateZFSSnapshot(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.CreateZFSSnapshot: CreateZFSSnapshot failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.CreateZFSSnapshot: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.CreateZFSSnapshot: done")
	return nil
}

// DeleteZFSSnapshot deletes a ZFS snapshot
func (c *HaifyClient) DeleteZFSSnapshot(ctx context.Context, snapshot, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.DeleteZFSSnapshot: sending request", zap.String("snapshot", snapshot), zap.String("node", node))
	req := &haifypb.DeleteZFSSnapshotRequest{
		Snapshot: snapshot,
		Node:     node,
	}

	resp, err := c.client.DeleteZFSSnapshot(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.DeleteZFSSnapshot: DeleteZFSSnapshot failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.DeleteZFSSnapshot: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.DeleteZFSSnapshot: done")
	return nil
}

// ListZFSSnapshots lists ZFS snapshots
func (c *HaifyClient) ListZFSSnapshots(ctx context.Context, dataset, node string) ([]*haifypb.SnapshotInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListZFSSnapshots: sending request", zap.String("dataset", dataset), zap.String("node", node))
	req := &haifypb.ListZFSSnapshotsRequest{
		Dataset: dataset,
		Node:    node,
	}

	resp, err := c.client.ListZFSSnapshots(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ListZFSSnapshots: ListZFSSnapshots failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.ListZFSSnapshots: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.ListZFSSnapshots: done", zap.Int("count", len(resp.Snapshots)))
	return resp.Snapshots, nil
}

// RestoreZFSSnapshot restores a ZFS snapshot
func (c *HaifyClient) RestoreZFSSnapshot(ctx context.Context, dataset, snapshotName, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.RestoreZFSSnapshot: sending request", zap.String("dataset", dataset), zap.String("snapshot_name", snapshotName), zap.String("node", node))
	req := &haifypb.RestoreZFSSnapshotRequest{
		Dataset:      dataset,
		SnapshotName: snapshotName,
		Node:         node,
	}

	resp, err := c.client.RestoreZFSSnapshot(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.RestoreZFSSnapshot: RestoreZFSSnapshot failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.RestoreZFSSnapshot: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.RestoreZFSSnapshot: done")
	return nil
}

// CloneZFSSnapshot clones a ZFS snapshot
func (c *HaifyClient) CloneZFSSnapshot(ctx context.Context, snapshot, clonePath, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.CloneZFSSnapshot: sending request", zap.String("snapshot", snapshot), zap.String("clone_path", clonePath), zap.String("node", node))
	req := &haifypb.CloneZFSSnapshotRequest{
		Snapshot:  snapshot,
		ClonePath: clonePath,
		Node:      node,
	}

	resp, err := c.client.CloneZFSSnapshot(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.CloneZFSSnapshot: CloneZFSSnapshot failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.CloneZFSSnapshot: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.CloneZFSSnapshot: done")
	return nil
}
