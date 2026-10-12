package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// CreateSMBGateway creates an SMB gateway.
func (c *HaifyClient) CreateSMBGateway(ctx context.Context, req *haifypb.CreateSMBGatewayRequest) (*haifypb.CreateSMBGatewayResponse, error) {
	zap.L().Debug("HaifyClient.CreateSMBGateway: sending request", zap.String("resource", req.GetResource()), zap.String("service_ip", req.GetServiceIp()), zap.String("workgroup", req.GetWorkgroup()), zap.String("share_name", req.GetShareName()), zap.Bool("read_only", req.GetReadOnly()), zap.Strings("valid_users", req.GetValidUsers()))
	return c.client.CreateSMBGateway(ctx, req)
}

// AddSMBShare adds a share to a running SMB gateway.
func (c *HaifyClient) AddSMBShare(ctx context.Context, resource string, share *haifypb.SMBShareInfo) error {
	log := zap.L()
	log.Debug("HaifyClient.AddSMBShare: sending request", zap.String("resource", resource), zap.String("share", share.GetName()),
		zap.String("path", share.GetPath()), zap.Bool("read_only", share.GetReadOnly()), zap.Strings("valid_users", share.GetValidUsers()))
	resp, err := c.client.AddSMBShare(ctx, &haifypb.AddSMBShareRequest{Resource: resource, Share: share})
	return smbResult(resp, err)
}

// RemoveSMBShare removes a share; its data stays.
func (c *HaifyClient) RemoveSMBShare(ctx context.Context, resource, name string) error {
	log := zap.L()
	log.Debug("HaifyClient.RemoveSMBShare: sending request", zap.String("resource", resource), zap.String("name", name))
	resp, err := c.client.RemoveSMBShare(ctx, &haifypb.RemoveSMBShareRequest{Resource: resource, Name: name})
	return smbResult(resp, err)
}

// ListSMBShares lists the shares of a running SMB gateway.
func (c *HaifyClient) ListSMBShares(ctx context.Context, resource string) ([]*haifypb.SMBShareInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListSMBShares: sending request", zap.String("resource", resource))
	resp, err := c.client.ListSMBShares(ctx, &haifypb.ListSMBSharesRequest{Resource: resource})
	if err := smbResult(resp, err); err != nil {
		log.Debug("HaifyClient.ListSMBShares: call failed", zap.Error(err))
		return nil, err
	}
	log.Debug("HaifyClient.ListSMBShares: done", zap.Int("count", len(resp.Shares)))
	return resp.Shares, nil
}

// SetSMBUser adds an SMB user or changes its password.
func (c *HaifyClient) SetSMBUser(ctx context.Context, resource, user, password string) error {
	log := zap.L()
	log.Debug("HaifyClient.SetSMBUser: sending request", zap.String("resource", resource), zap.String("user", user), zap.Bool("has_password", password != ""))
	resp, err := c.client.SetSMBUser(ctx, &haifypb.SetSMBUserRequest{Resource: resource, User: user, Password: password})
	return smbResult(resp, err)
}

// RemoveSMBUser removes an SMB user.
func (c *HaifyClient) RemoveSMBUser(ctx context.Context, resource, user string) error {
	log := zap.L()
	log.Debug("HaifyClient.RemoveSMBUser: sending request", zap.String("resource", resource), zap.String("user", user))
	resp, err := c.client.RemoveSMBUser(ctx, &haifypb.RemoveSMBUserRequest{Resource: resource, User: user})
	return smbResult(resp, err)
}

// ListSMBUsers lists the SMB users of a running gateway.
func (c *HaifyClient) ListSMBUsers(ctx context.Context, resource string) ([]string, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListSMBUsers: sending request", zap.String("resource", resource))
	resp, err := c.client.ListSMBUsers(ctx, &haifypb.ListSMBUsersRequest{Resource: resource})
	if err := smbResult(resp, err); err != nil {
		log.Debug("HaifyClient.ListSMBUsers: call failed", zap.Error(err))
		return nil, err
	}
	log.Debug("HaifyClient.ListSMBUsers: done", zap.Int("count", len(resp.Users)))
	return resp.Users, nil
}

// smbResult folds a response's success flag into its error.
func smbResult(resp interface {
	GetSuccess() bool
	GetMessage() string
}, err error) error {
	log := zap.L()
	if err != nil {
		log.Debug("smbResult: call failed", zap.Error(err))
		return err
	}
	if !resp.GetSuccess() {
		log.Debug("smbResult: controller reported failure", zap.String("message", resp.GetMessage()))
		return fmt.Errorf("%s", resp.GetMessage())
	}
	log.Debug("smbResult: done")
	return nil
}
