package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// ShipResource starts copying a resource to another cluster; it returns the
// storage job that carries it out.
func (c *HaifyClient) ShipResource(ctx context.Context, req *haifypb.ShipResourceRequest) (*haifypb.StorageJobResponse, error) {
	return jobResult(c.client.ShipResource(ctx, req))
}

// PrepareShipReceive opens a receiver for one volume, on the receiving cluster.
func (c *HaifyClient) PrepareShipReceive(ctx context.Context, resource string, vol uint32, token string) (*haifypb.PrepareShipReceiveResponse, error) {
	resp, err := c.client.PrepareShipReceive(ctx, &haifypb.PrepareShipReceiveRequest{Resource: resource, VolumeId: vol, Token: token})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Message)
	}
	return resp, nil
}

// FinishShipReceive closes the receiver for one volume and reports what
// arrived.
func (c *HaifyClient) FinishShipReceive(ctx context.Context, resource string, vol uint32, last bool) (*haifypb.FinishShipReceiveResponse, error) {
	return c.client.FinishShipReceive(ctx, &haifypb.FinishShipReceiveRequest{Resource: resource, VolumeId: vol, Last: last})
}
