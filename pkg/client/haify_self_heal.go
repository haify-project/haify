package client

import (
	"context"
	"fmt"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// GetSelfHealStatus returns what self-healing is doing and waiting for: the
// offline nodes auto-evict counts down, the replica moves under way, and for
// auto-diskful the clients it watches, why one is passed over, the swaps in
// progress and the resources in their cooldown.
func (c *HaifyClient) GetSelfHealStatus(ctx context.Context) (*haifypb.GetSelfHealStatusResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetSelfHealStatus: sending request")
	start := time.Now()
	resp, err := c.client.GetSelfHealStatus(ctx, &haifypb.GetSelfHealStatusRequest{})
	if err != nil {
		log.Debug("HaifyClient.GetSelfHealStatus: GetSelfHealStatus failed", zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, err
	}
	if !resp.GetSuccess() {
		log.Debug("HaifyClient.GetSelfHealStatus: controller reported failure", zap.String("message", resp.GetMessage()))
		return nil, fmt.Errorf("%s", resp.GetMessage())
	}
	log.Debug("HaifyClient.GetSelfHealStatus: done", zap.Int("clients", len(resp.GetAutoDiskful().GetClients())),
		zap.Int("swaps", len(resp.GetAutoDiskful().GetSwaps())), zap.Int("moves", len(resp.GetMoves())),
		zap.Duration("took", time.Since(start)))
	return resp, nil
}
