package csi

import (
	"context"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// CreateSnapshotGroup records the call and takes every member, or, with
// groupErr set, takes none: the real controller is all or nothing.
func (f *fakeBackend) CreateSnapshotGroup(ctx context.Context, members []*haifypb.SnapshotGroupMember) error {
	f.groupCalls = append(f.groupCalls, members)
	if f.groupErr != nil {
		return f.groupErr
	}
	for _, m := range members {
		if err := f.CreateSnapshot(ctx, m.GetVolume(), m.GetSnapshotName(), m.GetNode()); err != nil {
			return err
		}
	}
	return nil
}
