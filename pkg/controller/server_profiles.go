package controller

import (
	"context"
	"fmt"
	"strings"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
)

// DRBD options can hold a shared-secret, so traces in this file log how
// many options a profile carries, never their values.

func profileToProto(profile *database.ResourceProfile) *haifypb.ResourceProfile {
	if profile == nil {
		zap.L().Debug("profileToProto: no profile")
		return nil
	}
	zap.L().Debug("profileToProto: rendering profile", zap.String("profile", profile.Name),
		zap.Int("replicas", profile.Replicas), zap.String("pool", profile.Pool), zap.Int("drbd_options", len(profile.DRBDOptions)))
	return &haifypb.ResourceProfile{
		Name:                profile.Name,
		Protocol:            profile.Protocol,
		StorageType:         profile.StorageType,
		Pool:                profile.Pool,
		Replicas:            uint32(profile.Replicas),
		ReplicasOnDifferent: append([]string(nil), profile.OnDifferent...),
		ReplicasOnSame:      append([]string(nil), profile.OnSame...),
		DrbdOptions:         cloneStringMap(profile.DRBDOptions),
		Labels:              cloneStringMap(profile.Labels),
	}
}

func profileFromProto(profile *haifypb.ResourceProfile) *database.ResourceProfile {
	if profile == nil {
		zap.L().Debug("profileFromProto: no profile in request")
		return nil
	}
	zap.L().Debug("profileFromProto: reading profile", zap.String("profile", profile.Name),
		zap.Uint32("replicas", profile.Replicas), zap.String("pool", profile.Pool), zap.Int("drbd_options", len(profile.DrbdOptions)))
	return &database.ResourceProfile{
		Name:        strings.TrimSpace(profile.Name),
		Protocol:    profile.Protocol,
		StorageType: profile.StorageType,
		Pool:        profile.Pool,
		Replicas:    int(profile.Replicas),
		OnDifferent: append([]string(nil), profile.ReplicasOnDifferent...),
		OnSame:      append([]string(nil), profile.ReplicasOnSame...),
		DRBDOptions: cloneStringMap(profile.DrbdOptions),
		Labels:      cloneStringMap(profile.Labels),
	}
}

func (s *Server) CreateResourceProfile(ctx context.Context, req *haifypb.CreateResourceProfileRequest) (*haifypb.CreateResourceProfileResponse, error) {
	log := s.serverLog()
	log.Debug("CreateResourceProfile: saving", zap.String("profile", req.GetProfile().GetName()))
	if s.ctrl == nil || s.ctrl.db == nil {
		log.Debug("CreateResourceProfile: no database")
		return &haifypb.CreateResourceProfileResponse{Success: false, Message: "database not available"}, nil
	}
	profile := profileFromProto(req.Profile)
	if profile == nil || profile.Name == "" {
		log.Debug("CreateResourceProfile: no profile name")
		return &haifypb.CreateResourceProfileResponse{Success: false, Message: "profile name is required"}, nil
	}
	if err := validateDrbdOptions(profile.DRBDOptions); err != nil {
		log.Debug("CreateResourceProfile: options rejected", zap.String("profile", profile.Name), zap.Error(err))
		return &haifypb.CreateResourceProfileResponse{Success: false, Message: err.Error()}, nil
	}
	if err := s.ctrl.db.SaveResourceProfile(ctx, profile); err != nil {
		log.Debug("CreateResourceProfile: database write failed", zap.String("profile", profile.Name), zap.Error(err))
		return &haifypb.CreateResourceProfileResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("CreateResourceProfile: saved", zap.String("profile", profile.Name))
	return &haifypb.CreateResourceProfileResponse{Success: true, Message: "Resource profile saved", Profile: profileToProto(profile)}, nil
}

func (s *Server) GetResourceProfile(ctx context.Context, req *haifypb.GetResourceProfileRequest) (*haifypb.GetResourceProfileResponse, error) {
	log := s.serverLog()
	log.Debug("GetResourceProfile: reading", zap.String("profile", req.Name))
	if s.ctrl == nil || s.ctrl.db == nil {
		log.Debug("GetResourceProfile: no database")
		return &haifypb.GetResourceProfileResponse{Success: false, Message: "database not available"}, nil
	}
	profile, err := s.ctrl.db.GetResourceProfile(ctx, req.Name)
	if err != nil {
		log.Debug("GetResourceProfile: not found", zap.String("profile", req.Name), zap.Error(err))
		return &haifypb.GetResourceProfileResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("GetResourceProfile: found", zap.String("profile", req.Name))
	return &haifypb.GetResourceProfileResponse{Success: true, Message: "Resource profile found", Profile: profileToProto(profile)}, nil
}

func (s *Server) ListResourceProfiles(ctx context.Context, _ *haifypb.ListResourceProfilesRequest) (*haifypb.ListResourceProfilesResponse, error) {
	log := s.serverLog()
	log.Debug("ListResourceProfiles: listing")
	if s.ctrl == nil || s.ctrl.db == nil {
		log.Debug("ListResourceProfiles: no database")
		return &haifypb.ListResourceProfilesResponse{Success: false, Message: "database not available"}, nil
	}
	profiles, err := s.ctrl.db.ListResourceProfiles(ctx)
	if err != nil {
		log.Debug("ListResourceProfiles: database read failed", zap.Error(err))
		return &haifypb.ListResourceProfilesResponse{Success: false, Message: err.Error()}, nil
	}
	result := make([]*haifypb.ResourceProfile, 0, len(profiles))
	for _, profile := range profiles {
		result = append(result, profileToProto(profile))
	}
	log.Debug("ListResourceProfiles: listed", zap.Int("count", len(result)))
	return &haifypb.ListResourceProfilesResponse{Success: true, Message: "Resource profiles listed", Profiles: result}, nil
}

func (s *Server) DeleteResourceProfile(ctx context.Context, req *haifypb.DeleteResourceProfileRequest) (*haifypb.DeleteResourceProfileResponse, error) {
	log := s.serverLog()
	log.Debug("DeleteResourceProfile: deleting", zap.String("profile", req.Name))
	if s.ctrl == nil || s.ctrl.db == nil {
		log.Debug("DeleteResourceProfile: no database")
		return &haifypb.DeleteResourceProfileResponse{Success: false, Message: "database not available"}, nil
	}
	// A profile with members is their group: deleting it would leave each
	// pointing at nothing, and the next adjust or option change would miss
	// them without a word.
	if members, err := s.resources.profileMembers(ctx, req.Name); err == nil && len(members) > 0 {
		names := make([]string, len(members))
		for i, m := range members {
			names[i] = m.Name
		}
		log.Debug("DeleteResourceProfile: profile still has members, refusing", zap.String("profile", req.Name),
			zap.Strings("members", names))
		return &haifypb.DeleteResourceProfileResponse{Success: false, Message: fmt.Sprintf(
			"profile %s still has %d member(s): %s; take them out first (resource set-profile <resource> --none)",
			req.Name, len(names), strings.Join(names, ", "))}, nil
	} else if err != nil {
		log.Debug("DeleteResourceProfile: could not read members, deleting anyway", zap.String("profile", req.Name), zap.Error(err))
	}
	if err := s.ctrl.db.DeleteResourceProfile(ctx, req.Name); err != nil {
		log.Debug("DeleteResourceProfile: database delete failed", zap.String("profile", req.Name), zap.Error(err))
		return &haifypb.DeleteResourceProfileResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("DeleteResourceProfile: deleted", zap.String("profile", req.Name))
	return &haifypb.DeleteResourceProfileResponse{Success: true, Message: "Resource profile deleted"}, nil
}

func memberResultsToProto(results []ProfileMemberResult) ([]*haifypb.ProfileMemberResult, bool) {
	out := make([]*haifypb.ProfileMemberResult, 0, len(results))
	ok := true
	for _, r := range results {
		zap.L().Debug("memberResultsToProto: member", zap.String("resource", r.Resource), zap.Bool("ok", r.OK))
		out = append(out, &haifypb.ProfileMemberResult{Resource: r.Resource, Success: r.OK, Message: r.Message})
		ok = ok && r.OK
	}
	return out, ok
}

func (s *Server) SetResourceProfileOptions(ctx context.Context, req *haifypb.SetResourceProfileOptionsRequest) (*haifypb.SetResourceProfileOptionsResponse, error) {
	log := s.serverLog()
	log.Debug("SetResourceProfileOptions: setting options", zap.String("profile", req.Name), zap.Int("options", len(req.Options)))
	profile, results, err := s.resources.SetProfileOptions(ctx, req.Name, req.Options)
	if err != nil {
		log.Debug("SetResourceProfileOptions: failed", zap.String("profile", req.Name), zap.Error(err))
		return &haifypb.SetResourceProfileOptionsResponse{Success: false, Message: err.Error(), Profile: profileToProto(profile)}, nil
	}
	members, ok := memberResultsToProto(results)
	msg := fmt.Sprintf("options saved on %s and applied to %d member(s)", req.Name, len(members))
	if !ok {
		msg = fmt.Sprintf("options saved on %s; some members failed", req.Name)
	}
	log.Debug("SetResourceProfileOptions: done", zap.String("profile", req.Name), zap.Int("members", len(members)), zap.Bool("all_ok", ok))
	return &haifypb.SetResourceProfileOptionsResponse{Success: ok, Message: msg, Profile: profileToProto(profile), Members: members}, nil
}

func (s *Server) AdjustResourceProfile(ctx context.Context, req *haifypb.AdjustResourceProfileRequest) (*haifypb.AdjustResourceProfileResponse, error) {
	log := s.serverLog()
	log.Debug("AdjustResourceProfile: adjusting members", zap.String("profile", req.Name), zap.Bool("dry_run", req.DryRun))
	results, err := s.resources.AdjustProfile(ctx, req.Name, req.DryRun)
	if err != nil {
		log.Debug("AdjustResourceProfile: failed", zap.String("profile", req.Name), zap.Error(err))
		return &haifypb.AdjustResourceProfileResponse{Success: false, Message: err.Error()}, nil
	}
	members, ok := memberResultsToProto(results)
	msg := fmt.Sprintf("%d member(s) adjusted", len(members))
	if req.DryRun {
		msg = fmt.Sprintf("dry run over %d member(s); nothing changed", len(members))
	}
	log.Debug("AdjustResourceProfile: done", zap.String("profile", req.Name), zap.Int("members", len(members)), zap.Bool("all_ok", ok))
	return &haifypb.AdjustResourceProfileResponse{Success: ok, Message: msg, Members: members}, nil
}

func (s *Server) GetResourceProfileMaxSize(ctx context.Context, req *haifypb.GetResourceProfileMaxSizeRequest) (*haifypb.GetResourceProfileMaxSizeResponse, error) {
	log := s.serverLog()
	log.Debug("GetResourceProfileMaxSize: computing", zap.String("profile", req.Name))
	size, nodes, thin, err := s.resources.ProfileMaxSize(ctx, req.Name)
	if err != nil {
		log.Debug("GetResourceProfileMaxSize: failed", zap.String("profile", req.Name), zap.Error(err))
		return &haifypb.GetResourceProfileMaxSizeResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("GetResourceProfileMaxSize: computed", zap.String("profile", req.Name), zap.Uint64("max_size_gb", size),
		zap.Strings("nodes", nodes), zap.Bool("thin", thin))
	return &haifypb.GetResourceProfileMaxSizeResponse{Success: true, MaxSizeGb: size, Nodes: nodes, Thin: thin}, nil
}

func (s *Server) SetResourceProfile(ctx context.Context, req *haifypb.SetResourceProfileRequest) (*haifypb.SetResourceProfileResponse, error) {
	log := s.serverLog()
	log.Debug("SetResourceProfile: assigning", zap.String("resource", req.Resource), zap.String("profile", req.Profile))
	if err := s.resources.AssignProfile(ctx, req.Resource, req.Profile); err != nil {
		log.Debug("SetResourceProfile: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.SetResourceProfileResponse{Success: false, Message: err.Error()}, nil
	}
	if req.Profile == "" {
		log.Debug("SetResourceProfile: removed from its profile", zap.String("resource", req.Resource))
		return &haifypb.SetResourceProfileResponse{Success: true, Message: req.Resource + " is in no profile"}, nil
	}
	log.Debug("SetResourceProfile: assigned", zap.String("resource", req.Resource), zap.String("profile", req.Profile))
	return &haifypb.SetResourceProfileResponse{Success: true, Message: fmt.Sprintf("%s is a member of %s", req.Resource, req.Profile)}, nil
}
