package client

import (
	"context"
	"errors"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// ErrApprovalDisabled is returned when the controller runs without
// [rbac.approval].
var ErrApprovalDisabled = errors.New("two-person approval is not enabled on the controller ([rbac.approval])")

// ListApprovals returns the open approval requests, or every one with all.
func (c *HaifyClient) ListApprovals(ctx context.Context, all bool) ([]*haifypb.ApprovalInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListApprovals: sending request", zap.Bool("all", all))
	resp, err := c.client.ListApprovals(ctx, &haifypb.ListApprovalsRequest{IncludeClosed: all})
	if err != nil {
		log.Debug("HaifyClient.ListApprovals: ListApprovals failed", zap.Error(err))
		return nil, err
	}
	if !resp.Enabled {
		log.Debug("HaifyClient.ListApprovals: approval not enabled on the controller")
		return nil, ErrApprovalDisabled
	}
	log.Debug("HaifyClient.ListApprovals: done", zap.Int("count", len(resp.Approvals)))
	return resp.Approvals, nil
}

// ApproveRequest approves another user's pending request.
func (c *HaifyClient) ApproveRequest(ctx context.Context, id string) (*haifypb.ApprovalInfo, string, error) {
	log := zap.L()
	log.Debug("HaifyClient.ApproveRequest: sending request", zap.String("id", id))
	resp, err := c.client.ApproveRequest(ctx, &haifypb.ApproveRequestRequest{Id: id})
	if err != nil {
		log.Debug("HaifyClient.ApproveRequest: ApproveRequest failed", zap.Error(err))
		return nil, "", err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ApproveRequest: controller reported failure", zap.String("message", resp.Message))
		return resp.Approval, "", fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ApproveRequest: done")
	return resp.Approval, resp.Message, nil
}

// RejectRequest closes a pending request without running it.
func (c *HaifyClient) RejectRequest(ctx context.Context, id string) (*haifypb.ApprovalInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.RejectRequest: sending request", zap.String("id", id))
	resp, err := c.client.RejectRequest(ctx, &haifypb.RejectRequestRequest{Id: id})
	if err != nil {
		log.Debug("HaifyClient.RejectRequest: RejectRequest failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.RejectRequest: controller reported failure", zap.String("message", resp.Message))
		return resp.Approval, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.RejectRequest: done")
	return resp.Approval, nil
}
