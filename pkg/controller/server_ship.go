package controller

import (
	"context"
	"fmt"

	pb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// The ship tokens authenticate the receiving controller: traced only as
// present or not.

func (s *Server) ShipResource(ctx context.Context, req *pb.ShipResourceRequest) (*pb.StorageJobResponse, error) {
	log := s.serverLog()
	log.Debug("ShipResource: starting ship", zap.String("resource", req.Resource), zap.String("target", req.Target),
		zap.String("name", req.Name), zap.String("pool", req.Pool), zap.Uint32("replicas", req.Replicas),
		zap.Bool("has_target_token", req.TargetToken != ""))
	id, err := s.ctrl.backups.StartShip(ctx, ShipRequest{Resource: req.Resource, Target: req.Target,
		TargetToken: req.TargetToken, Name: req.Name, Pool: req.Pool, Replicas: req.Replicas})
	if err != nil {
		log.Debug("ShipResource: failed to start", zap.String("resource", req.Resource), zap.Error(err))
		return &pb.StorageJobResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("ShipResource: started", zap.String("resource", req.Resource), zap.String("job", id))
	return &pb.StorageJobResponse{Success: true, JobId: id,
		Message: fmt.Sprintf("copying %s to %s; follow it with haify pool jobs", req.Resource, req.Target)}, nil
}

func (s *Server) PrepareShipReceive(ctx context.Context, req *pb.PrepareShipReceiveRequest) (*pb.PrepareShipReceiveResponse, error) {
	log := s.serverLog()
	log.Debug("PrepareShipReceive: preparing", zap.String("resource", req.Resource), zap.Uint32("volume", req.VolumeId),
		zap.Bool("has_token", req.Token != ""))
	resp, err := s.resources.PrepareShipReceive(ctx, req.Resource, req.VolumeId, req.Token)
	if err != nil {
		log.Debug("PrepareShipReceive: failed", zap.String("resource", req.Resource), zap.Uint32("volume", req.VolumeId), zap.Error(err))
		return &pb.PrepareShipReceiveResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("PrepareShipReceive: ready", zap.String("resource", req.Resource), zap.Uint32("volume", req.VolumeId),
		zap.Bool("success", resp.GetSuccess()))
	return resp, nil
}

func (s *Server) FinishShipReceive(ctx context.Context, req *pb.FinishShipReceiveRequest) (*pb.FinishShipReceiveResponse, error) {
	log := s.serverLog()
	log.Debug("FinishShipReceive: finishing", zap.String("resource", req.Resource), zap.Uint32("volume", req.VolumeId),
		zap.Bool("last", req.Last))
	resp, err := s.resources.FinishShipReceive(ctx, req.Resource, req.VolumeId, req.Last)
	if err != nil {
		log.Debug("FinishShipReceive: failed", zap.String("resource", req.Resource), zap.Uint32("volume", req.VolumeId), zap.Error(err))
		return &pb.FinishShipReceiveResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("FinishShipReceive: finished", zap.String("resource", req.Resource), zap.Uint32("volume", req.VolumeId),
		zap.Bool("success", resp.GetSuccess()))
	return resp, nil
}
