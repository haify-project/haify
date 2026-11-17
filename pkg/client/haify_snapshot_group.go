package client

import (
	"context"
	"fmt"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// CreateSnapshotGroup takes one-node snapshots of several volumes at the same
// instant, with I/O suspended on all of their resources while it does. It is
// all or nothing.
func (c *HaifyClient) CreateSnapshotGroup(ctx context.Context, members []*haifypb.SnapshotGroupMember) error {
	_, err := c.CreateSnapshotGroupNoted(ctx, members)
	return err
}

// CreateSnapshotGroupNoted is CreateSnapshotGroup, and returns what the
// controller said about the snapshots: for volumes of guests, their
// consistency and why.
func (c *HaifyClient) CreateSnapshotGroupNoted(ctx context.Context, members []*haifypb.SnapshotGroupMember) (string, error) {
	log := zap.L()
	log.Debug("HaifyClient.CreateSnapshotGroup: sending request", zap.Int("members", len(members)))
	start := time.Now()
	resp, err := c.client.CreateSnapshotGroup(ctx, &haifypb.CreateSnapshotGroupRequest{Members: members})
	if err != nil {
		log.Debug("HaifyClient.CreateSnapshotGroup: CreateSnapshotGroup failed", zap.Duration("took", time.Since(start)), zap.Error(err))
		return "", err
	}
	if !resp.GetSuccess() {
		log.Debug("HaifyClient.CreateSnapshotGroup: controller reported failure", zap.String("message", resp.GetMessage()),
			zap.Duration("took", time.Since(start)))
		return "", fmt.Errorf("%s", resp.GetMessage())
	}
	log.Debug("HaifyClient.CreateSnapshotGroup: done", zap.Duration("took", time.Since(start)))
	return resp.GetMessage(), nil
}
