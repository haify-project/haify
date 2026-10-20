package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// CreateResourceSnapshot snapshots every volume of resource on every replica,
// at one instant, as name.
func (c *HaifyClient) CreateResourceSnapshot(ctx context.Context, resource, name string) error {
	log := zap.L()
	log.Debug("HaifyClient.CreateResourceSnapshot: sending request", zap.String("resource", resource), zap.String("name", name))
	resp, err := c.client.CreateResourceSnapshot(ctx, &haifypb.CreateResourceSnapshotRequest{Resource: resource, Name: name})
	if err != nil {
		log.Debug("HaifyClient.CreateResourceSnapshot: CreateResourceSnapshot failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.CreateResourceSnapshot: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.CreateResourceSnapshot: done")
	return nil
}

// ListResourceSnapshots names the snapshots CreateResourceSnapshot took of
// resource.
func (c *HaifyClient) ListResourceSnapshots(ctx context.Context, resource string) ([]string, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListResourceSnapshots: sending request", zap.String("resource", resource))
	resp, err := c.client.ListResourceSnapshots(ctx, &haifypb.ListResourceSnapshotsRequest{Resource: resource})
	if err != nil {
		log.Debug("HaifyClient.ListResourceSnapshots: ListResourceSnapshots failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListResourceSnapshots: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListResourceSnapshots: done", zap.Int("count", len(resp.Names)))
	return resp.Names, nil
}

// RollbackResourceSnapshot rolls every replica of resource back to snapshot
// name. The resource must not be Primary anywhere.
func (c *HaifyClient) RollbackResourceSnapshot(ctx context.Context, resource, name string) error {
	log := zap.L()
	log.Debug("HaifyClient.RollbackResourceSnapshot: sending request", zap.String("resource", resource), zap.String("name", name))
	resp, err := c.client.RollbackResourceSnapshot(ctx, &haifypb.RollbackResourceSnapshotRequest{Resource: resource, Name: name})
	if err != nil {
		log.Debug("HaifyClient.RollbackResourceSnapshot: RollbackResourceSnapshot failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.RollbackResourceSnapshot: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.RollbackResourceSnapshot: done")
	return nil
}

// DeleteResourceSnapshot deletes snapshot name of resource from every replica.
func (c *HaifyClient) DeleteResourceSnapshot(ctx context.Context, resource, name string) error {
	log := zap.L()
	log.Debug("HaifyClient.DeleteResourceSnapshot: sending request", zap.String("resource", resource), zap.String("name", name))
	resp, err := c.client.DeleteResourceSnapshot(ctx, &haifypb.DeleteResourceSnapshotRequest{Resource: resource, Name: name})
	if err != nil {
		log.Debug("HaifyClient.DeleteResourceSnapshot: DeleteResourceSnapshot failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.DeleteResourceSnapshot: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.DeleteResourceSnapshot: done")
	return nil
}

// ResizeVolumeBytes grows a volume to exactly sizeBytes.
func (c *HaifyClient) ResizeVolumeBytes(ctx context.Context, resource string, volumeID uint32, sizeBytes uint64) error {
	log := zap.L()
	log.Debug("HaifyClient.ResizeVolumeBytes: sending request", zap.String("resource", resource), zap.Uint32("volume_id", volumeID), zap.Uint64("size_bytes", sizeBytes))
	resp, err := c.client.ResizeVolume(ctx, &haifypb.ResizeVolumeRequest{Resource: resource, VolumeId: volumeID, SizeBytes: sizeBytes})
	if err != nil {
		log.Debug("HaifyClient.ResizeVolumeBytes: ResizeVolume failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ResizeVolumeBytes: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ResizeVolumeBytes: done")
	return nil
}
