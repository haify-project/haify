package controller

import (
	"context"
	"fmt"

	pb "github.com/haify-project/haify/api/proto/v1"
)

func (s *Server) ShipResource(ctx context.Context, req *pb.ShipResourceRequest) (*pb.StorageJobResponse, error) {
	id, err := s.ctrl.backups.StartShip(ctx, ShipRequest{Resource: req.Resource, Target: req.Target,
		TargetToken: req.TargetToken, Name: req.Name, Pool: req.Pool, Replicas: req.Replicas})
	if err != nil {
		return &pb.StorageJobResponse{Success: false, Message: err.Error()}, nil
	}
	return &pb.StorageJobResponse{Success: true, JobId: id,
		Message: fmt.Sprintf("copying %s to %s; follow it with haify pool jobs", req.Resource, req.Target)}, nil
}

func (s *Server) PrepareShipReceive(ctx context.Context, req *pb.PrepareShipReceiveRequest) (*pb.PrepareShipReceiveResponse, error) {
	resp, err := s.resources.PrepareShipReceive(ctx, req.Resource, req.VolumeId, req.Token)
	if err != nil {
		return &pb.PrepareShipReceiveResponse{Success: false, Message: err.Error()}, nil
	}
	return resp, nil
}

func (s *Server) FinishShipReceive(ctx context.Context, req *pb.FinishShipReceiveRequest) (*pb.FinishShipReceiveResponse, error) {
	resp, err := s.resources.FinishShipReceive(ctx, req.Resource, req.VolumeId, req.Last)
	if err != nil {
		return &pb.FinishShipReceiveResponse{Success: false, Message: err.Error()}, nil
	}
	return resp, nil
}
