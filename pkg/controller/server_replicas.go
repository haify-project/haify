package controller

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

func (s *Server) AttachDisklessClient(ctx context.Context, req *haifypb.AttachDisklessClientRequest) (*haifypb.AttachDisklessClientResponse, error) {
	log := s.serverLog()
	log.Debug("AttachDisklessClient: attaching", zap.String("resource", req.Resource), zap.String("node", req.Node))
	if err := s.resources.AttachDisklessClient(ctx, req.Resource, req.Node); err != nil {
		log.Debug("AttachDisklessClient: failed", zap.String("resource", req.Resource), zap.String("node", req.Node), zap.Error(err))
		return &haifypb.AttachDisklessClientResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("AttachDisklessClient: attached", zap.String("resource", req.Resource), zap.String("node", req.Node))
	return &haifypb.AttachDisklessClientResponse{
		Success: true,
		Message: "Diskless client attached successfully",
	}, nil
}

func (s *Server) DetachDisklessClient(ctx context.Context, req *haifypb.DetachDisklessClientRequest) (*haifypb.DetachDisklessClientResponse, error) {
	log := s.serverLog()
	log.Debug("DetachDisklessClient: detaching", zap.String("resource", req.Resource), zap.String("node", req.Node))
	if err := s.resources.DetachDisklessClient(ctx, req.Resource, req.Node); err != nil {
		log.Debug("DetachDisklessClient: failed", zap.String("resource", req.Resource), zap.String("node", req.Node), zap.Error(err))
		return &haifypb.DetachDisklessClientResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("DetachDisklessClient: detached", zap.String("resource", req.Resource), zap.String("node", req.Node))
	return &haifypb.DetachDisklessClientResponse{
		Success: true,
		Message: "Diskless client detached successfully",
	}, nil
}

// SetTiebreaker moves a resource's diskless quorum tiebreaker to another node.
// The previous holder is reported back so the caller can show what changed.
func (s *Server) SetTiebreaker(ctx context.Context, req *haifypb.SetTiebreakerRequest) (*haifypb.SetTiebreakerResponse, error) {
	log := s.serverLog()
	log.Debug("SetTiebreaker: moving tiebreaker", zap.String("resource", req.Resource), zap.String("node", req.Node))
	previous := ""
	if s.ctrl.db != nil {
		if dbRes, err := s.ctrl.db.GetResource(ctx, req.Resource); err == nil && dbRes != nil {
			previous = dbRes.DisklessNodes
		} else {
			log.Debug("SetTiebreaker: resource record not read", zap.String("resource", req.Resource), zap.Error(err))
		}
	}
	log.Debug("SetTiebreaker: previous tiebreaker", zap.String("resource", req.Resource), zap.String("previous", previous))

	if err := s.resources.SetTiebreaker(ctx, req.Resource, req.Node); err != nil {
		log.Debug("SetTiebreaker: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.SetTiebreakerResponse{Success: false, Message: err.Error()}, nil
	}

	msg := fmt.Sprintf("tiebreaker for %q is now %q", req.Resource, req.Node)
	if req.Node == "" {
		// Whether that is dangerous depends on how many diskful replicas are
		// left. Two need the tiebreaker to reach a majority; three already have
		// one without it, and an off-site DR counts — warning regardless would
		// train the operator to ignore the message.
		replicas := 0
		if s.ctrl.db != nil {
			if dbRes, err := s.ctrl.db.GetResource(ctx, req.Resource); err == nil && dbRes != nil {
				replicas = len(splitCSV(dbRes.Nodes))
			}
		}
		if replicas >= 3 {
			log.Debug("SetTiebreaker: removed, majority kept without it", zap.String("resource", req.Resource), zap.Int("replicas", replicas))
			msg = fmt.Sprintf("tiebreaker removed from %q; %d replicas still give a quorum majority",
				req.Resource, replicas)
		} else {
			log.Debug("SetTiebreaker: removed, one failure now suspends I/O", zap.String("resource", req.Resource), zap.Int("replicas", replicas))
			msg = fmt.Sprintf("tiebreaker removed from %q; with %d replicas a single node failure will now suspend I/O",
				req.Resource, replicas)
		}
	}
	log.Debug("SetTiebreaker: done", zap.String("resource", req.Resource), zap.String("previous", previous), zap.String("node", req.Node))
	return &haifypb.SetTiebreakerResponse{
		Success:      true,
		Message:      msg,
		PreviousNode: previous,
		Node:         req.Node,
	}, nil
}

// AddDR attaches an off-site asynchronous replica to a running resource. The
// port actually used is echoed back, since a zero request port is allocated by
// the controller and the caller needs it to open the firewall.
func (s *Server) AddDR(ctx context.Context, req *haifypb.AddDRRequest) (*haifypb.AddDRResponse, error) {
	log := s.serverLog()
	log.Debug("AddDR: attaching DR site", zap.String("resource", req.Resource), zap.String("dr_node", req.DrNode),
		zap.String("dr_endpoint", req.DrEndpoint), zap.Uint32("wan_port", req.WanPort), zap.String("egress_address", req.EgressAddress))
	if err := s.resources.AddDR(ctx, req.Resource, req.DrNode, req.DrEndpoint, req.WanPort, req.EgressAddress); err != nil {
		log.Debug("AddDR: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.AddDRResponse{Success: false, Message: err.Error()}, nil
	}

	port := req.WanPort
	if s.ctrl.db != nil {
		if dbRes, err := s.ctrl.db.GetResource(ctx, req.Resource); err == nil && dbRes != nil {
			port = uint32(dbRes.WANPort)
		}
	}
	log.Debug("AddDR: attached", zap.String("resource", req.Resource), zap.String("dr_node", req.DrNode), zap.Uint32("wan_port", port))
	return &haifypb.AddDRResponse{
		Success: true,
		Message: fmt.Sprintf("DR site %q attached to %q; initial sync runs in the background", req.DrNode, req.Resource),
		WanPort: port,
	}, nil
}

// AddReplica adds a diskful local replica to a running resource.
func (s *Server) AddReplica(ctx context.Context, req *haifypb.AddReplicaRequest) (*haifypb.AddReplicaResponse, error) {
	log := s.serverLog()
	log.Debug("AddReplica: adding", zap.String("resource", req.Resource), zap.String("node", req.Node),
		zap.Bool("ignore_free_space", req.IgnoreFreeSpace), zap.Bool("allow_unreachable", req.AllowUnreachable))
	if err := s.resources.AddReplicaWith(ctx, req.Resource, req.Node,
		AddReplicaOpts{IgnoreFreeSpace: req.IgnoreFreeSpace, AllowUnreachable: req.AllowUnreachable}); err != nil {
		log.Debug("AddReplica: failed", zap.String("resource", req.Resource), zap.String("node", req.Node), zap.Error(err))
		return &haifypb.AddReplicaResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("AddReplica: added", zap.String("resource", req.Resource), zap.String("node", req.Node))
	return &haifypb.AddReplicaResponse{
		Success: true,
		Message: fmt.Sprintf("replica added on %q; initial sync runs in the background", req.Node),
	}, nil
}

func (s *Server) RemoveReplica(ctx context.Context, req *haifypb.RemoveReplicaRequest) (*haifypb.RemoveReplicaResponse, error) {
	log := s.serverLog()
	log.Debug("RemoveReplica: removing", zap.String("resource", req.Resource), zap.String("node", req.Node), zap.Bool("lost", req.Lost))
	if err := s.resources.RemoveReplicaOptions(ctx, req.Resource, req.Node, req.Lost); err != nil {
		log.Debug("RemoveReplica: failed", zap.String("resource", req.Resource), zap.String("node", req.Node), zap.Error(err))
		return &haifypb.RemoveReplicaResponse{Success: false, Message: err.Error()}, nil
	}
	if req.Lost {
		log.Debug("RemoveReplica: lost node removed from survivors", zap.String("resource", req.Resource), zap.String("node", req.Node))
		return &haifypb.RemoveReplicaResponse{
			Success: true,
			Message: fmt.Sprintf("replica of the lost node %q removed from the survivors; %s",
				req.Node, LostReplicaCleanup(req.Resource, req.Node)),
		}, nil
	}
	log.Debug("RemoveReplica: removed", zap.String("resource", req.Resource), zap.String("node", req.Node))
	return &haifypb.RemoveReplicaResponse{
		Success: true,
		Message: fmt.Sprintf("replica removed from %q", req.Node),
	}, nil
}
