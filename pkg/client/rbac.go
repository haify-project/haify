package client

import (
	"context"
	"errors"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// ErrRBACDisabled is returned by the RBAC user-management calls when the
// controller runs without [rbac]; there are no users to manage.
var ErrRBACDisabled = errors.New("RBAC is not enabled on the controller")

// RbacWhoami reports the identity and role of the token this client sends.
// Enabled is false when the controller runs without [rbac].
func (c *HaifyClient) RbacWhoami(ctx context.Context) (*haifypb.GetRbacWhoamiResponse, error) {
	zap.L().Debug("HaifyClient.RbacWhoami: sending request")
	return c.client.GetRbacWhoami(ctx, &haifypb.GetRbacWhoamiRequest{})
}

// ListRbacPolicies returns the effective policy and user assignments. Admin only.
func (c *HaifyClient) ListRbacPolicies(ctx context.Context) (*haifypb.ListRbacPoliciesResponse, error) {
	zap.L().Debug("HaifyClient.ListRbacPolicies: sending request")
	return c.client.ListRbacPolicies(ctx, &haifypb.ListRbacPoliciesRequest{})
}

// CreateRbacUser adds a user and returns its token. An empty token asks the
// controller to generate one.
func (c *HaifyClient) CreateRbacUser(ctx context.Context, name, role, token string) (string, error) {
	log := zap.L()
	log.Debug("HaifyClient.CreateRbacUser: sending request", zap.String("name", name), zap.String("role", role), zap.Bool("has_token", token != ""))
	resp, err := c.client.CreateRbacUser(ctx, &haifypb.CreateRbacUserRequest{Name: name, Role: role, Token: token})
	if err != nil {
		log.Debug("HaifyClient.CreateRbacUser: CreateRbacUser failed", zap.Error(err))
		return "", err
	}
	if err := rbacResult(resp.Enabled, resp.Success, resp.Message); err != nil {
		log.Debug("HaifyClient.CreateRbacUser: controller refused", zap.Error(err))
		return "", err
	}
	log.Debug("HaifyClient.CreateRbacUser: done", zap.Bool("token_issued", resp.Token != ""))
	return resp.Token, nil
}

// DeleteRbacUser removes a user. Users declared in controller.toml cannot be removed.
func (c *HaifyClient) DeleteRbacUser(ctx context.Context, name string) error {
	log := zap.L()
	log.Debug("HaifyClient.DeleteRbacUser: sending request", zap.String("name", name))
	resp, err := c.client.DeleteRbacUser(ctx, &haifypb.DeleteRbacUserRequest{Name: name})
	if err != nil {
		log.Debug("HaifyClient.DeleteRbacUser: DeleteRbacUser failed", zap.Error(err))
		return err
	}
	return rbacResult(resp.Enabled, resp.Success, resp.Message)
}

// SetRbacUserRole changes a user's role.
func (c *HaifyClient) SetRbacUserRole(ctx context.Context, name, role string) error {
	log := zap.L()
	log.Debug("HaifyClient.SetRbacUserRole: sending request", zap.String("name", name), zap.String("role", role))
	resp, err := c.client.SetRbacUserRole(ctx, &haifypb.SetRbacUserRoleRequest{Name: name, Role: role})
	if err != nil {
		log.Debug("HaifyClient.SetRbacUserRole: SetRbacUserRole failed", zap.Error(err))
		return err
	}
	return rbacResult(resp.Enabled, resp.Success, resp.Message)
}

func rbacResult(enabled, success bool, message string) error {
	log := zap.L()
	switch {
	case !enabled:
		log.Debug("rbacResult: RBAC not enabled on the controller")
		return ErrRBACDisabled
	case !success:
		log.Debug("rbacResult: controller reported failure", zap.String("message", message))
		return fmt.Errorf("%s", message)
	}
	log.Debug("rbacResult: done")
	return nil
}
