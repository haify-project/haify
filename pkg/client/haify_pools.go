package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// CreatePool creates a storage pool
func (c *HaifyClient) CreatePool(ctx context.Context, name, poolType, node string, disks []string, sizeGB uint64, wipe bool) error {
	log := zap.L()
	log.Debug("HaifyClient.CreatePool: sending request", zap.String("name", name), zap.String("pool_type", poolType), zap.String("node", node), zap.Strings("disks", disks), zap.Uint64("size_gb", sizeGB), zap.Bool("wipe", wipe))
	req := &haifypb.CreatePoolRequest{
		Name:   name,
		Type:   poolType,
		Node:   node,
		Disks:  disks,
		SizeGb: sizeGB,
		Wipe:   wipe,
	}

	resp, err := c.client.CreatePool(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.CreatePool: CreatePool failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.CreatePool: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.CreatePool: done")
	return nil
}

// GetPool gets pool information
func (c *HaifyClient) GetPool(ctx context.Context, name, node string) (*haifypb.PoolInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetPool: sending request", zap.String("name", name), zap.String("node", node))
	req := &haifypb.GetPoolRequest{
		Name: name,
		Node: node,
	}

	resp, err := c.client.GetPool(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.GetPool: GetPool failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.GetPool: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.GetPool: done")
	return resp.Pool, nil
}

// ListPools lists all pools
func (c *HaifyClient) ListPools(ctx context.Context) ([]*haifypb.PoolInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListPools: sending request")
	req := &haifypb.ListPoolsRequest{}

	resp, err := c.client.ListPools(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ListPools: ListPools failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.ListPools: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.ListPools: done", zap.Int("count", len(resp.Pools)))
	return resp.Pools, nil
}

// AddDiskToPool adds a disk to a pool
func (c *HaifyClient) AddDiskToPool(ctx context.Context, pool, disk, node string, wipe bool) error {
	log := zap.L()
	log.Debug("HaifyClient.AddDiskToPool: sending request", zap.String("pool", pool), zap.String("disk", disk), zap.String("node", node), zap.Bool("wipe", wipe))
	req := &haifypb.AddDiskToPoolRequest{
		Pool: pool,
		Disk: disk,
		Node: node,
		Wipe: wipe,
	}

	resp, err := c.client.AddDiskToPool(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.AddDiskToPool: AddDiskToPool failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.AddDiskToPool: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.AddDiskToPool: done")
	return nil
}

// DeletePool deletes a storage pool
func (c *HaifyClient) DeletePool(ctx context.Context, pool, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.DeletePool: sending request", zap.String("pool", pool), zap.String("node", node))
	req := &haifypb.DeletePoolRequest{
		Name: pool,
		Node: node,
	}

	resp, err := c.client.DeletePool(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.DeletePool: DeletePool failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.DeletePool: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.DeletePool: done")
	return nil
}

// ConvertPoolToThin rebuilds one node's LVM pool as a thin pool, in place.
func (c *HaifyClient) ConvertPoolToThin(ctx context.Context, node, pool string) error {
	log := zap.L()
	log.Debug("HaifyClient.ConvertPoolToThin: sending request", zap.String("node", node), zap.String("pool", pool))
	resp, err := c.client.ConvertPoolToThin(ctx, &haifypb.ConvertPoolToThinRequest{Node: node, Pool: pool})
	if err != nil {
		log.Debug("HaifyClient.ConvertPoolToThin: ConvertPoolToThin failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ConvertPoolToThin: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ConvertPoolToThin: done")
	return nil
}

// AddPoolCache puts a fast device in front of one node's LVM pool. mode may be
// empty, which the controller resolves to writethrough. It returns the mode
// actually applied and the size of the cache.
func (c *HaifyClient) AddPoolCache(ctx context.Context, node, pool, device, mode string) (string, uint64, error) {
	log := zap.L()
	log.Debug("HaifyClient.AddPoolCache: sending request", zap.String("node", node), zap.String("pool", pool), zap.String("device", device), zap.String("mode", mode))
	resp, err := c.client.AddPoolCache(ctx, &haifypb.AddPoolCacheRequest{
		Node: node, Pool: pool, Device: device, Mode: mode,
	})
	if err != nil {
		log.Debug("HaifyClient.AddPoolCache: AddPoolCache failed", zap.Error(err))
		return "", 0, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.AddPoolCache: controller reported failure", zap.String("message", resp.Message))
		return "", 0, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.AddPoolCache: done")
	return resp.Mode, resp.CacheSizeBytes, nil
}

// RemovePoolCache flushes and detaches a pool's cache.
func (c *HaifyClient) RemovePoolCache(ctx context.Context, node, pool string) error {
	log := zap.L()
	log.Debug("HaifyClient.RemovePoolCache: sending request", zap.String("node", node), zap.String("pool", pool))
	resp, err := c.client.RemovePoolCache(ctx, &haifypb.RemovePoolCacheRequest{Node: node, Pool: pool})
	if err != nil {
		log.Debug("HaifyClient.RemovePoolCache: RemovePoolCache failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.RemovePoolCache: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.RemovePoolCache: done")
	return nil
}
