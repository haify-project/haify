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
	_, err := c.CreateResourceSnapshotNoted(ctx, resource, name)
	return err
}

// CreateResourceSnapshotNoted is CreateResourceSnapshot, and returns what the
// controller said about the snapshot: for a resource that belongs to a guest,
// whether it is filesystem or crash consistent and why.
func (c *HaifyClient) CreateResourceSnapshotNoted(ctx context.Context, resource, name string) (string, error) {
	log := zap.L()
	log.Debug("HaifyClient.CreateResourceSnapshot: sending request", zap.String("resource", resource), zap.String("name", name))
	resp, err := c.client.CreateResourceSnapshot(ctx, &haifypb.CreateResourceSnapshotRequest{Resource: resource, Name: name})
	if err != nil {
		log.Debug("HaifyClient.CreateResourceSnapshot: CreateResourceSnapshot failed", zap.Error(err))
		return "", err
	}
	if !resp.Success {
		log.Debug("HaifyClient.CreateResourceSnapshot: controller reported failure", zap.String("message", resp.Message))
		return "", fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.CreateResourceSnapshot: done", zap.String("message", resp.Message))
	return resp.Message, nil
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
	_, err := c.ResizeVolumeWith(ctx, &haifypb.ResizeVolumeRequest{Resource: resource, VolumeId: volumeID, SizeBytes: sizeBytes})
	return err
}
