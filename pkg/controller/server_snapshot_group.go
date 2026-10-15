package controller

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// CreateSnapshotGroup is the RPC over ResourceManager.CreateSnapshotGroup
// (snapshot_group.go).
func (s *Server) CreateSnapshotGroup(ctx context.Context, req *haifypb.CreateSnapshotGroupRequest) (*haifypb.CreateSnapshotGroupResponse, error) {
	log := s.serverLog()
	log.Debug("CreateSnapshotGroup: taking group", zap.Int("members", len(req.GetMembers())))
	members := make([]SnapshotGroupMember, 0, len(req.GetMembers()))
	for _, m := range req.GetMembers() {
		log.Debug("CreateSnapshotGroup: member", zap.String("resource", m.GetResource()), zap.String("volume", m.GetVolume()),
			zap.String("snapshot", m.GetSnapshotName()), zap.String("node", m.GetNode()))
		members = append(members, SnapshotGroupMember{Resource: m.GetResource(), Volume: m.GetVolume(),
			Name: m.GetSnapshotName(), Node: m.GetNode()})
	}
	if err := s.resources.CreateSnapshotGroup(ctx, members); err != nil {
		log.Debug("CreateSnapshotGroup: failed", zap.Error(err))
		return &haifypb.CreateSnapshotGroupResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("CreateSnapshotGroup: taken", zap.Int("members", len(members)))
	return &haifypb.CreateSnapshotGroupResponse{Success: true,
		Message: fmt.Sprintf("%d snapshots taken at one instant", len(members))}, nil
}
