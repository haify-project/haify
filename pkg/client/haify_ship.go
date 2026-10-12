package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// ShipResource starts copying a resource to another cluster; it returns the
// storage job that carries it out.
func (c *HaifyClient) ShipResource(ctx context.Context, req *haifypb.ShipResourceRequest) (*haifypb.StorageJobResponse, error) {
	zap.L().Debug("HaifyClient.ShipResource: sending request", zap.String("resource", req.GetResource()), zap.String("target", req.GetTarget()), zap.Bool("has_target_token", req.GetTargetToken() != ""), zap.String("name", req.GetName()), zap.String("pool", req.GetPool()), zap.Uint32("replicas", req.GetReplicas()))
	return jobResult(c.client.ShipResource(ctx, req))
}

// PrepareShipReceive opens a receiver for one volume, on the receiving cluster.
func (c *HaifyClient) PrepareShipReceive(ctx context.Context, resource string, vol uint32, token string) (*haifypb.PrepareShipReceiveResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.PrepareShipReceive: sending request", zap.String("resource", resource), zap.Uint32("volume", vol), zap.Bool("has_token", token != ""))
	resp, err := c.client.PrepareShipReceive(ctx, &haifypb.PrepareShipReceiveRequest{Resource: resource, VolumeId: vol, Token: token})
	if err != nil {
		log.Debug("HaifyClient.PrepareShipReceive: PrepareShipReceive failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.PrepareShipReceive: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.PrepareShipReceive: done")
	return resp, nil
}

// FinishShipReceive closes the receiver for one volume and reports what
// arrived.
func (c *HaifyClient) FinishShipReceive(ctx context.Context, resource string, vol uint32, last bool) (*haifypb.FinishShipReceiveResponse, error) {
	zap.L().Debug("HaifyClient.FinishShipReceive: sending request", zap.String("resource", resource), zap.Uint32("volume", vol), zap.Bool("last", last))
	return c.client.FinishShipReceive(ctx, &haifypb.FinishShipReceiveRequest{Resource: resource, VolumeId: vol, Last: last})
}
