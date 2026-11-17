package client

import (
	"context"
	"fmt"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// IncompleteError is a request the controller carried out in part: what it
// did stands, and sending the same request again finishes the rest. A resize
// returns it when the volume is grown and the filesystem a gateway has
// mounted from it could not be, because the gateway was failing over.
type IncompleteError struct {
	Message string
}

func (e *IncompleteError) Error() string { return e.Message }

// Retryable marks the error for callers that do not import this package.
func (e *IncompleteError) Retryable() bool { return true }

// ResizeVolumeWith grows a volume as req says and returns what the
// controller reported about it: whether a running guest was shown the new
// size, and whether a filesystem mounted on a storage node was grown. An
// *IncompleteError means the volume is grown and the request should be
// repeated.
func (c *HaifyClient) ResizeVolumeWith(ctx context.Context, req *haifypb.ResizeVolumeRequest) (string, error) {
	log := zap.L()
	log.Debug("HaifyClient.ResizeVolumeWith: sending request", zap.String("resource", req.GetResource()),
		zap.Uint32("volume_id", req.GetVolumeId()), zap.Uint32("size_gb", req.GetSizeGb()), zap.Uint64("size_bytes", req.GetSizeBytes()),
		zap.Bool("ignore_free_space", req.GetIgnoreFreeSpace()), zap.Bool("grow_filesystem", req.GetGrowFilesystem()))
	start := time.Now()
	resp, err := c.client.ResizeVolume(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ResizeVolumeWith: ResizeVolume failed", zap.Duration("took", time.Since(start)), zap.Error(err))
		return "", err
	}
	if !resp.GetSuccess() {
		log.Debug("HaifyClient.ResizeVolumeWith: controller reported failure", zap.String("message", resp.GetMessage()),
			zap.Bool("retryable", resp.GetRetryable()), zap.Duration("took", time.Since(start)))
		if resp.GetRetryable() {
			return "", &IncompleteError{Message: resp.GetMessage()}
		}
		return "", fmt.Errorf("%s", resp.GetMessage())
	}
	log.Debug("HaifyClient.ResizeVolumeWith: done", zap.String("message", resp.GetMessage()), zap.Duration("took", time.Since(start)))
	return resp.GetMessage(), nil
}
