package client

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// SetWanTuning changes a WAN resource's proxy buffer and compression; see
// SetWanTuningRequest for the fields.
func (c *HaifyClient) SetWanTuning(ctx context.Context, req *haifypb.SetWanTuningRequest) (*haifypb.SetWanTuningResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.SetWanTuning: sending request", zap.String("name", req.GetName()),
		zap.String("buffer", req.GetBuffer()), zap.String("compression", req.GetCompression()))
	resp, err := c.client.SetWanTuning(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.SetWanTuning: SetWanTuning failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.SetWanTuning: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.SetWanTuning: done", zap.Uint64("buffer_bytes", resp.BufferBytes), zap.String("compression", resp.Compression))
	return resp, nil
}

// GetWanSizing asks which WAN setup a resource's workload needs; see
// GetWanSizingRequest for the fields.
func (c *HaifyClient) GetWanSizing(ctx context.Context, req *haifypb.GetWanSizingRequest) (*haifypb.GetWanSizingResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetWanSizing: sending request", zap.String("name", req.GetName()),
		zap.String("bandwidth", req.GetBandwidth()), zap.Uint32("days", req.GetDays()), zap.Bool("sample", req.GetSampleCompression()))
	resp, err := c.client.GetWanSizing(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.GetWanSizing: GetWanSizing failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.GetWanSizing: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.GetWanSizing: done", zap.String("outcome", resp.Outcome))
	return resp, nil
}
