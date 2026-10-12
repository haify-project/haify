package mcpserver

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// Resource profiles: reusable settings a resource is created from.

type resourceProfileIn struct {
	Name                string            `json:"name" jsonschema:"resource profile name"`
	Protocol            string            `json:"protocol,omitempty" jsonschema:"default DRBD protocol A, B, or C"`
	StorageType         string            `json:"storage_type,omitempty" jsonschema:"default storage type: lvm, lvm-thin, or zfs"`
	Pool                string            `json:"pool,omitempty" jsonschema:"default storage pool"`
	Replicas            uint32            `json:"replicas,omitempty" jsonschema:"default replica count"`
	ReplicasOnDifferent []string          `json:"replicas_on_different,omitempty" jsonschema:"node-label keys used to spread replicas"`
	ReplicasOnSame      []string          `json:"replicas_on_same,omitempty" jsonschema:"node-label keys replicas must share"`
	DrbdOptions         map[string]string `json:"drbd_options,omitempty" jsonschema:"default DRBD options"`
	Labels              map[string]string `json:"labels,omitempty" jsonschema:"default resource labels"`
}

type resourceProfileNameIn struct {
	Name string `json:"name" jsonschema:"resource profile name"`
}

// registerResourceProfileTools adds the resource profile tools when the
// client supports them.
func (s *Server) registerResourceProfileTools(srv *mcp.Server) {
	profileClient, profilesSupported := s.client.(interface {
		CreateResourceProfile(context.Context, *haifypb.ResourceProfile) (*haifypb.ResourceProfile, error)
		GetResourceProfile(context.Context, string) (*haifypb.ResourceProfile, error)
		ListResourceProfiles(context.Context) ([]*haifypb.ResourceProfile, error)
		DeleteResourceProfile(context.Context, string) error
	})
	if !profilesSupported {
		s.logger.Debug("Server.registerResourceProfileTools: client has no profile calls, skipping")
		return
	}
	addRead(s, srv, readOnlyTool("haify_resource_profile_list", "List resource profiles",
		"List reusable resource creation profiles."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, resourceProfileListOut, error) {
			s.logger.Debug("haify_resource_profile_list: listing profiles")
			profiles, err := profileClient.ListResourceProfiles(ctx)
			if err != nil {
				s.logger.Debug("haify_resource_profile_list: list failed", zap.Error(err))
				return nil, resourceProfileListOut{}, err
			}
			out := resourceProfileListOut{Profiles: make([]resourceProfileOut, 0, len(profiles))}
			for _, profile := range profiles {
				s.logger.Debug("haify_resource_profile_list: profile", zap.String("profile", profile.GetName()),
					zap.String("pool", profile.GetPool()), zap.Uint32("replicas", profile.GetReplicas()))
				out.Profiles = append(out.Profiles, profileOut(profile))
			}
			s.logger.Debug("haify_resource_profile_list: listed", zap.Int("count", len(out.Profiles)))
			return nil, out, nil
		})
	addRead(s, srv, readOnlyTool("haify_resource_profile_get", "Get resource profile",
		"Get one reusable resource creation profile."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in resourceProfileNameIn) (*mcp.CallToolResult, resourceProfileOut, error) {
			s.logger.Debug("haify_resource_profile_get: reading profile", zap.String("profile", in.Name))
			profile, err := profileClient.GetResourceProfile(ctx, in.Name)
			if err != nil {
				s.logger.Debug("haify_resource_profile_get: read failed", zap.String("profile", in.Name), zap.Error(err))
				return nil, resourceProfileOut{}, err
			}
			s.logger.Debug("haify_resource_profile_get: read", zap.String("profile", profile.GetName()),
				zap.String("pool", profile.GetPool()), zap.Uint32("replicas", profile.GetReplicas()))
			return nil, profileOut(profile), nil
		})
	addWrite(s, srv, writeTool("haify_resource_profile_create", "Create resource profile",
		"Create or replace a reusable resource creation profile. Existing resources are not changed."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in resourceProfileIn) (*mcp.CallToolResult, resourceProfileOut, error) {
			s.logger.Debug("haify_resource_profile_create: creating profile", zap.String("profile", in.Name),
				zap.String("protocol", in.Protocol), zap.String("storage_type", in.StorageType), zap.String("pool", in.Pool),
				zap.Uint32("replicas", in.Replicas), zap.Strings("replicas_on_different", in.ReplicasOnDifferent),
				zap.Strings("replicas_on_same", in.ReplicasOnSame), zap.Array("drbd_option_keys", keysOf(in.DrbdOptions)),
				zap.Array("label_keys", keysOf(in.Labels)))
			profile, err := profileClient.CreateResourceProfile(ctx, &haifypb.ResourceProfile{
				Name: in.Name, Protocol: in.Protocol, StorageType: in.StorageType, Pool: in.Pool,
				Replicas: in.Replicas, ReplicasOnDifferent: in.ReplicasOnDifferent,
				ReplicasOnSame: in.ReplicasOnSame, DrbdOptions: in.DrbdOptions, Labels: in.Labels,
			})
			if err != nil {
				s.logger.Debug("haify_resource_profile_create: create failed", zap.String("profile", in.Name), zap.Error(err))
				return nil, resourceProfileOut{}, err
			}
			s.logger.Debug("haify_resource_profile_create: created", zap.String("profile", profile.GetName()))
			return nil, profileOut(profile), nil
		})
	addWrite(s, srv, destructiveTool("haify_resource_profile_delete", "Delete resource profile",
		"Delete a resource profile. Refused while any resource is still a member; take members out with haify_resource_set_profile first."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in resourceProfileNameIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_resource_profile_delete: deleting profile", zap.String("profile", in.Name))
			if err := profileClient.DeleteResourceProfile(ctx, in.Name); err != nil {
				s.logger.Debug("haify_resource_profile_delete: delete failed", zap.String("profile", in.Name), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_resource_profile_delete: deleted", zap.String("profile", in.Name))
			return nil, ok(fmt.Sprintf("resource profile %s deleted", in.Name)), nil
		})
}
