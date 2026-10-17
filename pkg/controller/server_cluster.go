package controller

import (
	"context"

	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// GetClusterSummary returns the cluster in numbers for the dashboard.
func (s *Server) GetClusterSummary(ctx context.Context, _ *haifypb.GetClusterSummaryRequest) (*haifypb.GetClusterSummaryResponse, error) {
	log := s.serverLog()
	log.Debug("GetClusterSummary: building")
	return s.ctrl.clusterSummary(ctx), nil
}

// GetClusterHistory returns the cluster's totals over one range.
func (s *Server) GetClusterHistory(ctx context.Context, req *haifypb.GetClusterHistoryRequest) (*haifypb.GetClusterHistoryResponse, error) {
	log := s.serverLog()
	log.Debug("GetClusterHistory: reading", zap.String("range", req.Range))
	if s.ctrl.history == nil {
		return &haifypb.GetClusterHistoryResponse{Success: true}, nil
	}
	samples, err := s.ctrl.history.query(req.Range)
	if err != nil {
		log.Debug("GetClusterHistory: failed", zap.Error(err))
		return &haifypb.GetClusterHistoryResponse{Success: false, Message: err.Error()}, nil
	}
	out := make([]*haifypb.ClusterSample, 0, len(samples))
	for _, x := range samples {
		out = append(out, &haifypb.ClusterSample{Unix: x.T, WriteBytesPerSec: x.W, ReadBytesPerSec: x.R,
			OutOfSyncBytes: x.OOS, CapacityUsedBytes: x.Used, CapacityTotalBytes: x.Total,
			ApiRequestsPerSec: x.Req, ApiErrorsPerSec: x.Err})
	}
	log.Debug("GetClusterHistory: read", zap.String("range", req.Range), zap.Int("samples", len(out)))
	return &haifypb.GetClusterHistoryResponse{Success: true, Samples: out}, nil
}

// GetClusterStats returns the nodes' statistics from Prometheus.
func (s *Server) GetClusterStats(ctx context.Context, req *haifypb.GetClusterStatsRequest) (*haifypb.GetClusterStatsResponse, error) {
	log := s.serverLog()
	log.Debug("GetClusterStats: reading", zap.String("range", req.Range))
	out, err := s.ctrl.clusterStats(ctx, req.Range)
	if err != nil {
		log.Debug("GetClusterStats: failed", zap.Error(err))
		return &haifypb.GetClusterStatsResponse{Success: false, Message: err.Error()}, nil
	}
	return out, nil
}
