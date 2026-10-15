package csi

import (
	"context"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// CreateSnapshotGroup takes every member's snapshot. The fake has no I/O to
// suspend, and its CreateSnapshot does not fail, so the all-or-nothing part of
// the real controller has nothing to do here.
func (b *sanityBackend) CreateSnapshotGroup(ctx context.Context, members []*haifypb.SnapshotGroupMember) error {
	zap.L().Debug("sanityBackend.CreateSnapshotGroup: creating", zap.Int("members", len(members)))
	for _, m := range members {
		if err := b.CreateSnapshot(ctx, m.GetVolume(), m.GetSnapshotName(), m.GetNode()); err != nil {
			return err
		}
	}
	return nil
}
