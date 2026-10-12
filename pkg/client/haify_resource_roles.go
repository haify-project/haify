package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// SetPrimary sets a node as Primary for a resource
func (c *HaifyClient) SetPrimary(ctx context.Context, resource, node string, force bool) error {
	log := zap.L()
	log.Debug("HaifyClient.SetPrimary: sending request", zap.String("resource", resource), zap.String("node", node), zap.Bool("force", force))
	req := &haifypb.SetPrimaryRequest{
		Resource: resource,
		Node:     node,
		Force:    force,
	}

	resp, err := c.client.SetPrimary(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.SetPrimary: SetPrimary failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.SetPrimary: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.SetPrimary: done")
	return nil
}

// PromoteForNode requests a SAFE hard-failover promote of resource on node.
// The controller tries a normal promote first and only escalates to a forced
// promote if the node holds DRBD quorum; it refuses (returns an error) when the
// node lacks quorum, avoiding split-brain. This is what the CSI node plugin
// uses so a Pod rescheduled after a hard node failure can take over its RWO
// volume iff doing so is safe.
func (c *HaifyClient) PromoteForNode(ctx context.Context, resource, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.PromoteForNode: sending request", zap.String("resource", resource), zap.String("node", node))
	req := &haifypb.SetPrimaryRequest{
		Resource:      resource,
		Node:          node,
		QuorumGuarded: true,
	}

	resp, err := c.client.SetPrimary(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.PromoteForNode: SetPrimary failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.PromoteForNode: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.PromoteForNode: done")
	return nil
}

// SetSecondary sets a node as Secondary for a resource
func (c *HaifyClient) SetSecondary(ctx context.Context, resource, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.SetSecondary: sending request", zap.String("resource", resource), zap.String("node", node))
	req := &haifypb.SetSecondaryRequest{
		Resource: resource,
		Node:     node,
	}

	resp, err := c.client.SetSecondary(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.SetSecondary: SetSecondary failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.SetSecondary: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.SetSecondary: done")
	return nil
}

// SetDualPrimary toggles DRBD's allow-two-primaries for a resource, bracketing
// a hypervisor live migration (both hosts hold the disk open during hand-off).
//
// Enabling is refused for WAN resources; disabling is idempotent and verified,
// so it is safe to call unconditionally from a cleanup path.
func (c *HaifyClient) SetDualPrimary(ctx context.Context, resource string, enable bool) error {
	log := zap.L()
	log.Debug("HaifyClient.SetDualPrimary: sending request", zap.String("resource", resource), zap.Bool("enable", enable))
	resp, err := c.client.SetDualPrimary(ctx, &haifypb.SetDualPrimaryRequest{
		Resource: resource,
		Enable:   enable,
	})
	if err != nil {
		log.Debug("HaifyClient.SetDualPrimary: SetDualPrimary failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.SetDualPrimary: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.SetDualPrimary: done")
	return nil
}

// CreateFilesystem creates a filesystem on a DRBD device
func (c *HaifyClient) CreateFilesystem(ctx context.Context, resource string, volumeID uint32, node, fstype string) error {
	log := zap.L()
	log.Debug("HaifyClient.CreateFilesystem: sending request", zap.String("resource", resource), zap.Uint32("volume_id", volumeID), zap.String("node", node), zap.String("fstype", fstype))
	req := &haifypb.CreateFilesystemRequest{
		Resource: resource,
		VolumeId: volumeID,
		Fstype:   fstype,
		Node:     node,
	}

	resp, err := c.client.CreateFilesystem(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.CreateFilesystem: CreateFilesystem failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.CreateFilesystem: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.CreateFilesystem: done")
	return nil
}

// MountResource mounts a DRBD device
func (c *HaifyClient) MountResource(ctx context.Context, resource string, volumeID uint32, path, node, fstype string) error {
	log := zap.L()
	log.Debug("HaifyClient.MountResource: sending request", zap.String("resource", resource), zap.Uint32("volume_id", volumeID), zap.String("path", path), zap.String("node", node), zap.String("fstype", fstype))
	req := &haifypb.MountResourceRequest{
		Resource: resource,
		VolumeId: volumeID,
		Path:     path,
		Node:     node,
		Fstype:   fstype,
	}

	resp, err := c.client.MountResource(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.MountResource: MountResource failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.MountResource: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.MountResource: done")
	return nil
}

// UnmountResource unmounts a DRBD device
func (c *HaifyClient) UnmountResource(ctx context.Context, resource string, volumeID uint32, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.UnmountResource: sending request", zap.String("resource", resource), zap.Uint32("volume_id", volumeID), zap.String("node", node))
	req := &haifypb.UnmountResourceRequest{
		Resource: resource,
		VolumeId: volumeID,
		Node:     node,
	}

	resp, err := c.client.UnmountResource(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.UnmountResource: UnmountResource failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.UnmountResource: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.UnmountResource: done")
	return nil
}
