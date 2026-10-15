package csi

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// identityServer implements the CSI Identity service.
type identityServer struct {
	csi.UnimplementedIdentityServer
}

// NewIdentityServer returns the Identity service implementation.
func NewIdentityServer() csi.IdentityServer { return &identityServer{} }

func (s *identityServer) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	zap.L().Debug("identityServer.GetPluginInfo: reporting plugin info",
		zap.String("driver", DriverName), zap.String("version", DriverVersion))
	return &csi.GetPluginInfoResponse{Name: DriverName, VendorVersion: DriverVersion}, nil
}

func (s *identityServer) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	zap.L().Debug("identityServer.GetPluginCapabilities: reporting controller, group controller and accessibility constraints")
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{Type: &csi.PluginCapability_Service_{Service: &csi.PluginCapability_Service{
				Type: csi.PluginCapability_Service_CONTROLLER_SERVICE}}},
			{Type: &csi.PluginCapability_Service_{Service: &csi.PluginCapability_Service{
				Type: csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS}}},
			// VolumeGroupSnapshot (group_snapshot.go).
			{Type: &csi.PluginCapability_Service_{Service: &csi.PluginCapability_Service{
				Type: csi.PluginCapability_Service_GROUP_CONTROLLER_SERVICE}}},
		},
	}, nil
}

func (s *identityServer) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	zap.L().Debug("identityServer.Probe: ready")
	return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: true}}, nil
}
