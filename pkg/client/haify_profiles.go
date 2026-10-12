package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// CreateResourceProfile creates or updates a resource profile.
func (c *HaifyClient) CreateResourceProfile(ctx context.Context, profile *haifypb.ResourceProfile) (*haifypb.ResourceProfile, error) {
	log := zap.L()
	log.Debug("HaifyClient.CreateResourceProfile: sending request", zap.String("name", profile.GetName()), zap.String("protocol", profile.GetProtocol()),
		zap.String("storage_type", profile.GetStorageType()), zap.String("pool", profile.GetPool()), zap.Uint32("replicas", profile.GetReplicas()),
		zap.Int("drbd_options_count", len(profile.GetDrbdOptions())), zap.Int("labels_count", len(profile.GetLabels())))
	resp, err := c.client.CreateResourceProfile(ctx, &haifypb.CreateResourceProfileRequest{Profile: profile})
	if err != nil {
		log.Debug("HaifyClient.CreateResourceProfile: CreateResourceProfile failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.CreateResourceProfile: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.CreateResourceProfile: done")
	return resp.Profile, nil
}

// GetResourceProfile gets a resource profile by name.
func (c *HaifyClient) GetResourceProfile(ctx context.Context, name string) (*haifypb.ResourceProfile, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetResourceProfile: sending request", zap.String("name", name))
	resp, err := c.client.GetResourceProfile(ctx, &haifypb.GetResourceProfileRequest{Name: name})
	if err != nil {
		log.Debug("HaifyClient.GetResourceProfile: GetResourceProfile failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.GetResourceProfile: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.GetResourceProfile: done")
	return resp.Profile, nil
}

// ListResourceProfiles lists resource profiles.
func (c *HaifyClient) ListResourceProfiles(ctx context.Context) ([]*haifypb.ResourceProfile, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListResourceProfiles: sending request")
	resp, err := c.client.ListResourceProfiles(ctx, &haifypb.ListResourceProfilesRequest{})
	if err != nil {
		log.Debug("HaifyClient.ListResourceProfiles: ListResourceProfiles failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListResourceProfiles: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListResourceProfiles: done", zap.Int("count", len(resp.Profiles)))
	return resp.Profiles, nil
}

// DeleteResourceProfile deletes a profile that has no members.
func (c *HaifyClient) DeleteResourceProfile(ctx context.Context, name string) error {
	log := zap.L()
	log.Debug("HaifyClient.DeleteResourceProfile: sending request", zap.String("name", name))
	resp, err := c.client.DeleteResourceProfile(ctx, &haifypb.DeleteResourceProfileRequest{Name: name})
	if err != nil {
		log.Debug("HaifyClient.DeleteResourceProfile: DeleteResourceProfile failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.DeleteResourceProfile: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.DeleteResourceProfile: done")
	return nil
}

// ListProfileMembers lists the resources that belong to a profile.
func (c *HaifyClient) ListProfileMembers(ctx context.Context, profile string) ([]*haifypb.ResourceInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListProfileMembers: sending request", zap.String("profile", profile))
	resp, err := c.client.ListResources(ctx, &haifypb.ListResourcesRequest{Profile: profile})
	if err != nil {
		log.Debug("HaifyClient.ListProfileMembers: ListResources failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListProfileMembers: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListProfileMembers: done", zap.Int("count", len(resp.Resources)))
	return resp.Resources, nil
}

// SetResourceProfileOptions records DRBD options on a profile and applies
// them to every member; the response has each member's outcome.
func (c *HaifyClient) SetResourceProfileOptions(ctx context.Context, name string, options map[string]string) (*haifypb.SetResourceProfileOptionsResponse, error) {
	zap.L().Debug("HaifyClient.SetResourceProfileOptions: sending request", zap.String("name", name), zap.Int("options_count", len(options)))
	return c.client.SetResourceProfileOptions(ctx, &haifypb.SetResourceProfileOptionsRequest{Name: name, Options: options})
}

// AdjustResourceProfile brings every member into line with the profile, or
// with dryRun says what it would change.
func (c *HaifyClient) AdjustResourceProfile(ctx context.Context, name string, dryRun bool) (*haifypb.AdjustResourceProfileResponse, error) {
	zap.L().Debug("HaifyClient.AdjustResourceProfile: sending request", zap.String("name", name), zap.Bool("dry_run", dryRun))
	return c.client.AdjustResourceProfile(ctx, &haifypb.AdjustResourceProfileRequest{Name: name, DryRun: dryRun})
}

// GetResourceProfileMaxSize is the largest volume a new member could get now.
func (c *HaifyClient) GetResourceProfileMaxSize(ctx context.Context, name string) (*haifypb.GetResourceProfileMaxSizeResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetResourceProfileMaxSize: sending request", zap.String("name", name))
	resp, err := c.client.GetResourceProfileMaxSize(ctx, &haifypb.GetResourceProfileMaxSizeRequest{Name: name})
	if err != nil {
		log.Debug("HaifyClient.GetResourceProfileMaxSize: GetResourceProfileMaxSize failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.GetResourceProfileMaxSize: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.GetResourceProfileMaxSize: done")
	return resp, nil
}

// SetResourceProfile makes a resource a member of a profile, or takes it out
// of the one it is in when profile is empty.
func (c *HaifyClient) SetResourceProfile(ctx context.Context, resource, profile string) error {
	log := zap.L()
	log.Debug("HaifyClient.SetResourceProfile: sending request", zap.String("resource", resource), zap.String("profile", profile))
	resp, err := c.client.SetResourceProfile(ctx, &haifypb.SetResourceProfileRequest{Resource: resource, Profile: profile})
	if err != nil {
		log.Debug("HaifyClient.SetResourceProfile: SetResourceProfile failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.SetResourceProfile: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.SetResourceProfile: done")
	return nil
}
