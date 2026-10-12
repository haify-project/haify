package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// RunInspection inspects the cluster now and returns the report. It blocks
// for the whole run: one SSH round to every node plus the database reads.
func (c *HaifyClient) RunInspection(ctx context.Context, areas []string) (*haifypb.InspectionReport, error) {
	log := zap.L()
	log.Debug("HaifyClient.RunInspection: sending request", zap.Strings("areas", areas))
	resp, err := c.client.RunInspection(ctx, &haifypb.RunInspectionRequest{Areas: areas})
	if err != nil {
		log.Debug("HaifyClient.RunInspection: RunInspection failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.RunInspection: controller reported failure", zap.String("message", resp.Message))
		return resp.Report, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.RunInspection: done")
	return resp.Report, nil
}

// ListInspections returns stored reports, newest first, without checks.
func (c *HaifyClient) ListInspections(ctx context.Context, limit int32) ([]*haifypb.InspectionReport, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListInspections: sending request", zap.Int32("limit", limit))
	resp, err := c.client.ListInspections(ctx, &haifypb.ListInspectionsRequest{Limit: limit})
	if err != nil {
		log.Debug("HaifyClient.ListInspections: ListInspections failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListInspections: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListInspections: done", zap.Int("count", len(resp.Reports)))
	return resp.Reports, nil
}

// GetInspection returns one stored report; id "latest" is the newest.
func (c *HaifyClient) GetInspection(ctx context.Context, id string) (*haifypb.InspectionReport, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetInspection: sending request", zap.String("id", id))
	resp, err := c.client.GetInspection(ctx, &haifypb.GetInspectionRequest{Id: id})
	if err != nil {
		log.Debug("HaifyClient.GetInspection: GetInspection failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.GetInspection: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.GetInspection: done")
	return resp.Report, nil
}
