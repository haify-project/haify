package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// CreateNFSGateway creates an NFS gateway
func (c *HaifyClient) CreateNFSGateway(ctx context.Context, req *haifypb.CreateNFSGatewayRequest) (*haifypb.CreateNFSGatewayResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.CreateNFSGateway: sending request", zap.String("resource", req.GetResource()), zap.String("service_ip", req.GetServiceIp()),
		zap.String("export_path", req.GetExportPath()), zap.Strings("allowed_ips", req.GetAllowedIps()), zap.String("fs_type", req.GetFsType()), zap.Int("options_count", len(req.GetOptions())))
	resp, err := c.client.CreateNFSGateway(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.CreateNFSGateway: CreateNFSGateway failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.CreateNFSGateway: controller reported failure", zap.String("message", resp.Message))
		return resp, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.CreateNFSGateway: done")
	return resp, nil
}

// CreateISCSIGateway creates an iSCSI gateway
func (c *HaifyClient) CreateISCSIGateway(ctx context.Context, req *haifypb.CreateISCSIGatewayRequest) (*haifypb.CreateISCSIGatewayResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.CreateISCSIGateway: sending request", zap.String("resource", req.GetResource()), zap.String("service_ip", req.GetServiceIp()),
		zap.String("iqn", req.GetIqn()), zap.Strings("allowed_initiators", req.GetAllowedInitiators()), zap.String("username", req.GetUsername()),
		zap.Bool("has_password", req.GetPassword() != ""), zap.String("implementation", req.GetImplementation()), zap.Int("options_count", len(req.GetOptions())))
	resp, err := c.client.CreateISCSIGateway(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.CreateISCSIGateway: CreateISCSIGateway failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.CreateISCSIGateway: controller reported failure", zap.String("message", resp.Message))
		return resp, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.CreateISCSIGateway: done")
	return resp, nil
}

// CreateNVMeGateway creates an NVMe gateway
func (c *HaifyClient) CreateNVMeGateway(ctx context.Context, req *haifypb.CreateNVMeGatewayRequest) (*haifypb.CreateNVMeGatewayResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.CreateNVMeGateway: sending request", zap.String("resource", req.GetResource()), zap.String("service_ip", req.GetServiceIp()),
		zap.String("nqn", req.GetNqn()), zap.String("transport_type", req.GetTransportType()), zap.Int("options_count", len(req.GetOptions())))
	resp, err := c.client.CreateNVMeGateway(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.CreateNVMeGateway: CreateNVMeGateway failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.CreateNVMeGateway: controller reported failure", zap.String("message", resp.Message))
		return resp, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.CreateNVMeGateway: done")
	return resp, nil
}

// ListGateways lists all gateways
func (c *HaifyClient) ListGateways(ctx context.Context) ([]*haifypb.GatewayInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListGateways: sending request")
	req := &haifypb.ListGatewaysRequest{}

	resp, err := c.client.ListGateways(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ListGateways: ListGateways failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.ListGateways: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.ListGateways: done", zap.Int("count", len(resp.Gateways)))
	return resp.Gateways, nil
}

// GetGateway gets a gateway by resource identifier.
func (c *HaifyClient) GetGateway(ctx context.Context, id string) (*haifypb.GatewayInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetGateway: sending request", zap.String("id", id))
	req := &haifypb.GetGatewayRequest{Id: id}

	resp, err := c.client.GetGateway(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.GetGateway: GetGateway failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.GetGateway: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.GetGateway: done")
	return resp.Gateway, nil
}

// StartGateway starts a gateway
func (c *HaifyClient) StartGateway(ctx context.Context, id string) error {
	log := zap.L()
	log.Debug("HaifyClient.StartGateway: sending request", zap.String("id", id))
	req := &haifypb.StartGatewayRequest{
		Id: id,
	}

	resp, err := c.client.StartGateway(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.StartGateway: StartGateway failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.StartGateway: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.StartGateway: done")
	return nil
}

// StopGateway stops a gateway
func (c *HaifyClient) StopGateway(ctx context.Context, id string) error {
	log := zap.L()
	log.Debug("HaifyClient.StopGateway: sending request", zap.String("id", id))
	req := &haifypb.StopGatewayRequest{
		Id: id,
	}

	resp, err := c.client.StopGateway(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.StopGateway: StopGateway failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.StopGateway: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.StopGateway: done")
	return nil
}

// DeleteGateway deletes a gateway
func (c *HaifyClient) DeleteGateway(ctx context.Context, id string) error {
	log := zap.L()
	log.Debug("HaifyClient.DeleteGateway: sending request", zap.String("id", id))
	req := &haifypb.DeleteGatewayRequest{
		Id: id,
	}

	resp, err := c.client.DeleteGateway(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.DeleteGateway: DeleteGateway failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.DeleteGateway: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.DeleteGateway: done")
	return nil
}

// AddNFSExport adds an export to an NFS gateway.
func (c *HaifyClient) AddNFSExport(ctx context.Context, resource, exportPath string, fsid int32, clientSpec, options string) error {
	log := zap.L()
	log.Debug("HaifyClient.AddNFSExport: sending request", zap.String("resource", resource), zap.String("export_path", exportPath), zap.Int32("fsid", fsid), zap.String("client_spec", clientSpec), zap.String("options", options))
	req := &haifypb.AddNFSExportRequest{
		Resource:   resource,
		ExportPath: exportPath,
		Fsid:       fsid,
		ClientSpec: clientSpec,
		Options:    options,
	}

	resp, err := c.client.AddNFSExport(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.AddNFSExport: AddNFSExport failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.AddNFSExport: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.AddNFSExport: done")
	return nil
}

// RemoveNFSExport removes an export from an NFS gateway.
func (c *HaifyClient) RemoveNFSExport(ctx context.Context, resource, exportPath string) error {
	log := zap.L()
	log.Debug("HaifyClient.RemoveNFSExport: sending request", zap.String("resource", resource), zap.String("export_path", exportPath))
	req := &haifypb.RemoveNFSExportRequest{
		Resource:   resource,
		ExportPath: exportPath,
	}

	resp, err := c.client.RemoveNFSExport(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.RemoveNFSExport: RemoveNFSExport failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.RemoveNFSExport: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.RemoveNFSExport: done")
	return nil
}

// ListNFSExports lists exports for an NFS gateway.
func (c *HaifyClient) ListNFSExports(ctx context.Context, resource string) ([]*haifypb.NFSExportInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListNFSExports: sending request", zap.String("resource", resource))
	req := &haifypb.ListNFSExportsRequest{Resource: resource}

	resp, err := c.client.ListNFSExports(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ListNFSExports: ListNFSExports failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListNFSExports: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListNFSExports: done", zap.Int("count", len(resp.Exports)))
	return resp.Exports, nil
}

// AddISCSILUN adds a LUN to an iSCSI gateway.
func (c *HaifyClient) AddISCSILUN(ctx context.Context, resource string, lun int32, device string) error {
	log := zap.L()
	log.Debug("HaifyClient.AddISCSILUN: sending request", zap.String("resource", resource), zap.Int32("lun", lun), zap.String("device", device))
	req := &haifypb.AddISCSILUNRequest{Resource: resource, Lun: lun, Device: device}

	resp, err := c.client.AddISCSILUN(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.AddISCSILUN: AddISCSILUN failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.AddISCSILUN: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.AddISCSILUN: done")
	return nil
}

// RemoveISCSILUN removes a LUN from an iSCSI gateway.
func (c *HaifyClient) RemoveISCSILUN(ctx context.Context, resource string, lun int32) error {
	log := zap.L()
	log.Debug("HaifyClient.RemoveISCSILUN: sending request", zap.String("resource", resource), zap.Int32("lun", lun))
	req := &haifypb.RemoveISCSILUNRequest{Resource: resource, Lun: lun}

	resp, err := c.client.RemoveISCSILUN(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.RemoveISCSILUN: RemoveISCSILUN failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.RemoveISCSILUN: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.RemoveISCSILUN: done")
	return nil
}

// ListISCSILUNs lists LUNs for an iSCSI gateway.
func (c *HaifyClient) ListISCSILUNs(ctx context.Context, resource string) ([]*haifypb.ISCSILUNInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListISCSILUNs: sending request", zap.String("resource", resource))
	req := &haifypb.ListISCSILUNsRequest{Resource: resource}

	resp, err := c.client.ListISCSILUNs(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ListISCSILUNs: ListISCSILUNs failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListISCSILUNs: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListISCSILUNs: done", zap.Int("count", len(resp.Luns)))
	return resp.Luns, nil
}

// AddISCSIInitiator adds an initiator ACL to an iSCSI gateway.
func (c *HaifyClient) AddISCSIInitiator(ctx context.Context, resource, initiator string) error {
	log := zap.L()
	log.Debug("HaifyClient.AddISCSIInitiator: sending request", zap.String("resource", resource), zap.String("initiator", initiator))
	req := &haifypb.AddISCSIInitiatorRequest{Resource: resource, Initiator: initiator}

	resp, err := c.client.AddISCSIInitiator(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.AddISCSIInitiator: AddISCSIInitiator failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.AddISCSIInitiator: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.AddISCSIInitiator: done")
	return nil
}

// RemoveISCSIInitiator removes an initiator ACL from an iSCSI gateway.
func (c *HaifyClient) RemoveISCSIInitiator(ctx context.Context, resource, initiator string) error {
	log := zap.L()
	log.Debug("HaifyClient.RemoveISCSIInitiator: sending request", zap.String("resource", resource), zap.String("initiator", initiator))
	req := &haifypb.RemoveISCSIInitiatorRequest{Resource: resource, Initiator: initiator}

	resp, err := c.client.RemoveISCSIInitiator(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.RemoveISCSIInitiator: RemoveISCSIInitiator failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.RemoveISCSIInitiator: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.RemoveISCSIInitiator: done")
	return nil
}

// ListISCSIInitiators lists initiator ACLs for an iSCSI gateway.
func (c *HaifyClient) ListISCSIInitiators(ctx context.Context, resource string) ([]string, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListISCSIInitiators: sending request", zap.String("resource", resource))
	req := &haifypb.ListISCSIInitiatorsRequest{Resource: resource}

	resp, err := c.client.ListISCSIInitiators(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ListISCSIInitiators: ListISCSIInitiators failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListISCSIInitiators: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListISCSIInitiators: done", zap.Int("count", len(resp.Initiators)))
	return resp.Initiators, nil
}

// SetISCSIChap updates CHAP settings on an iSCSI gateway.
func (c *HaifyClient) SetISCSIChap(ctx context.Context, resource, username, password string, mutual bool) error {
	log := zap.L()
	log.Debug("HaifyClient.SetISCSIChap: sending request", zap.String("resource", resource), zap.String("username", username), zap.Bool("has_password", password != ""), zap.Bool("mutual", mutual))
	req := &haifypb.SetISCSIChapRequest{
		Resource: resource,
		Username: username,
		Password: password,
		Mutual:   mutual,
	}

	resp, err := c.client.SetISCSIChap(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.SetISCSIChap: SetISCSIChap failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.SetISCSIChap: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.SetISCSIChap: done")
	return nil
}

// GetISCSIChap returns CHAP settings for an iSCSI gateway.
func (c *HaifyClient) GetISCSIChap(ctx context.Context, resource string) (*haifypb.GetISCSIChapResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetISCSIChap: sending request", zap.String("resource", resource))
	req := &haifypb.GetISCSIChapRequest{Resource: resource}

	resp, err := c.client.GetISCSIChap(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.GetISCSIChap: GetISCSIChap failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.GetISCSIChap: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.GetISCSIChap: done")
	return resp, nil
}

// AddNVMeNamespace adds a namespace to an NVMe gateway.
func (c *HaifyClient) AddNVMeNamespace(ctx context.Context, resource, device string) error {
	log := zap.L()
	log.Debug("HaifyClient.AddNVMeNamespace: sending request", zap.String("resource", resource), zap.String("device", device))
	req := &haifypb.AddNVMeNamespaceRequest{Resource: resource, Device: device}

	resp, err := c.client.AddNVMeNamespace(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.AddNVMeNamespace: AddNVMeNamespace failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.AddNVMeNamespace: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.AddNVMeNamespace: done")
	return nil
}

// RemoveNVMeNamespace removes a namespace from an NVMe gateway.
func (c *HaifyClient) RemoveNVMeNamespace(ctx context.Context, resource string, namespaceID int32) error {
	log := zap.L()
	log.Debug("HaifyClient.RemoveNVMeNamespace: sending request", zap.String("resource", resource), zap.Int32("namespace_id", namespaceID))
	req := &haifypb.RemoveNVMeNamespaceRequest{Resource: resource, NamespaceId: namespaceID}

	resp, err := c.client.RemoveNVMeNamespace(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.RemoveNVMeNamespace: RemoveNVMeNamespace failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.RemoveNVMeNamespace: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.RemoveNVMeNamespace: done")
	return nil
}

// ListNVMeNamespaces lists namespaces for an NVMe gateway.
func (c *HaifyClient) ListNVMeNamespaces(ctx context.Context, resource string) ([]*haifypb.NVMeNamespaceInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListNVMeNamespaces: sending request", zap.String("resource", resource))
	req := &haifypb.ListNVMeNamespacesRequest{Resource: resource}

	resp, err := c.client.ListNVMeNamespaces(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ListNVMeNamespaces: ListNVMeNamespaces failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListNVMeNamespaces: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListNVMeNamespaces: done", zap.Int("count", len(resp.Namespaces)))
	return resp.Namespaces, nil
}

// AddNVMeHost adds a host allow-list entry to an NVMe gateway.
func (c *HaifyClient) AddNVMeHost(ctx context.Context, resource, hostNQN string) error {
	log := zap.L()
	log.Debug("HaifyClient.AddNVMeHost: sending request", zap.String("resource", resource), zap.String("host_nqn", hostNQN))
	req := &haifypb.AddNVMeHostRequest{Resource: resource, HostNqn: hostNQN}

	resp, err := c.client.AddNVMeHost(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.AddNVMeHost: AddNVMeHost failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.AddNVMeHost: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.AddNVMeHost: done")
	return nil
}

// RemoveNVMeHost removes a host allow-list entry from an NVMe gateway.
func (c *HaifyClient) RemoveNVMeHost(ctx context.Context, resource, hostNQN string) error {
	log := zap.L()
	log.Debug("HaifyClient.RemoveNVMeHost: sending request", zap.String("resource", resource), zap.String("host_nqn", hostNQN))
	req := &haifypb.RemoveNVMeHostRequest{Resource: resource, HostNqn: hostNQN}

	resp, err := c.client.RemoveNVMeHost(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.RemoveNVMeHost: RemoveNVMeHost failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.RemoveNVMeHost: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.RemoveNVMeHost: done")
	return nil
}

// ListNVMeHosts lists hosts configured for an NVMe gateway.
func (c *HaifyClient) ListNVMeHosts(ctx context.Context, resource string) ([]string, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListNVMeHosts: sending request", zap.String("resource", resource))
	req := &haifypb.ListNVMeHostsRequest{Resource: resource}

	resp, err := c.client.ListNVMeHosts(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ListNVMeHosts: ListNVMeHosts failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListNVMeHosts: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListNVMeHosts: done", zap.Int("count", len(resp.Hosts)))
	return resp.Hosts, nil
}
