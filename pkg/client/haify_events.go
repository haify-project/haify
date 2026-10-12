package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// ListEvents returns retained notifications, oldest first.
func (c *HaifyClient) ListEvents(ctx context.Context, req *haifypb.ListEventsRequest) (*haifypb.ListEventsResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListEvents: sending request", zap.Int32("limit", req.GetLimit()), zap.String("min_severity", req.GetMinSeverity()),
		zap.Strings("types", req.GetTypes()), zap.String("resource", req.GetResource()), zap.Uint64("since_id", req.GetSinceId()))
	resp, err := c.client.ListEvents(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ListEvents: ListEvents failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListEvents: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListEvents: done", zap.Int("count", len(resp.Events)), zap.Uint64("dropped", resp.Dropped))
	return resp, nil
}

// WatchEvents opens a notification stream. The caller reads until Recv returns
// an error; cancelling ctx closes the stream.
func (c *HaifyClient) WatchEvents(ctx context.Context, req *haifypb.WatchEventsRequest) (haifypb.HaifyController_WatchEventsClient, error) {
	zap.L().Debug("HaifyClient.WatchEvents: sending request", zap.String("min_severity", req.GetMinSeverity()), zap.Strings("types", req.GetTypes()), zap.String("resource", req.GetResource()), zap.Uint64("since_id", req.GetSinceId()))
	return c.client.WatchEvents(ctx, req)
}

// ListAuditEvents returns the persisted audit trail, newest first.
func (c *HaifyClient) ListAuditEvents(ctx context.Context, req *haifypb.ListAuditEventsRequest) (*haifypb.ListAuditEventsResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListAuditEvents: sending request", zap.Int32("limit", req.GetLimit()), zap.String("method", req.GetMethod()),
		zap.String("target", req.GetTarget()), zap.String("user", req.GetUser()), zap.Bool("failures_only", req.GetFailuresOnly()))
	resp, err := c.client.ListAuditEvents(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ListAuditEvents: ListAuditEvents failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListAuditEvents: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListAuditEvents: done", zap.Int("count", len(resp.Events)), zap.Int64("total", resp.Total))
	return resp, nil
}

func (c *HaifyClient) ListControllerLogs(ctx context.Context, req *haifypb.ListControllerLogsRequest) (*haifypb.ListControllerLogsResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListControllerLogs: sending request", zap.Int32("limit", req.GetLimit()), zap.String("level", req.GetLevel()),
		zap.String("contains", req.GetContains()))
	resp, err := c.client.ListControllerLogs(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ListControllerLogs: ListControllerLogs failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListControllerLogs: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListControllerLogs: done", zap.Int("count", len(resp.Entries)), zap.String("node", resp.Node), zap.Bool("truncated", resp.Truncated))
	return resp, nil
}
