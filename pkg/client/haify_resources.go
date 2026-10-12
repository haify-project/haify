package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// CreateResource creates a DRBD resource with LVM backend (default)
func (c *HaifyClient) CreateResource(ctx context.Context, name string, port uint32, nodes []string, protocol string, sizeGB uint32, drbdOptions map[string]string) error {
	return c.CreateResourceWithPool(ctx, name, port, nodes, protocol, sizeGB, "", drbdOptions)
}

// CreateResourceWithPool creates a DRBD resource with specified pool and LVM backend
func (c *HaifyClient) CreateResourceWithPool(ctx context.Context, name string, port uint32, nodes []string, protocol string, sizeGB uint32, pool string, drbdOptions map[string]string) error {
	return c.CreateResourceWithPoolAndType(ctx, name, port, nodes, protocol, sizeGB, pool, "lvm", drbdOptions)
}

// CreateResourceWithPoolAndType creates a DRBD resource with specified pool and storage type
func (c *HaifyClient) CreateResourceWithPoolAndType(ctx context.Context, name string, port uint32, nodes []string, protocol string, sizeGB uint32, pool string, storageType string, drbdOptions map[string]string) error {
	return c.CreateResourceRequest(ctx, &haifypb.CreateResourceRequest{
		Name:        name,
		Port:        port,
		Nodes:       nodes,
		Protocol:    protocol,
		SizeGb:      sizeGB,
		Pool:        pool,
		StorageType: storageType,
		DrbdOptions: drbdOptions,
	})
}

// CreateResourceRequest creates a resource from the full API request. It is the
// metadata-aware entry point used by CSI and MCP while legacy wrappers remain
// source-compatible.
func (c *HaifyClient) CreateResourceRequest(ctx context.Context, req *haifypb.CreateResourceRequest) error {
	log := zap.L()
	log.Debug("HaifyClient.CreateResourceRequest: sending request", zap.String("name", req.GetName()), zap.Uint32("port", req.GetPort()),
		zap.Strings("nodes", req.GetNodes()), zap.String("protocol", req.GetProtocol()), zap.Uint32("size_gb", req.GetSizeGb()),
		zap.Uint64("size_bytes", req.GetSizeBytes()), zap.String("pool", req.GetPool()), zap.String("storage_type", req.GetStorageType()),
		zap.Int("volumes_count", len(req.GetVolumes())), zap.Uint32("replicas", req.GetReplicas()), zap.String("profile", req.GetProfile()),
		zap.Bool("wan", req.GetWan()), zap.String("dr_node", req.GetDrNode()), zap.Bool("encrypt", req.GetEncrypt()),
		zap.Int("drbd_options_count", len(req.GetDrbdOptions())), zap.Int("labels_count", len(req.GetLabels())))
	resp, err := c.client.CreateResource(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.CreateResourceRequest: CreateResource failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.CreateResourceRequest: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.CreateResourceRequest: done")
	return nil
}

// CreateResourceAutoPlace creates a DRBD resource without naming nodes: the
// controller auto-places `replicas` copies on the nodes with the most free
// space in the target pool. LAN-only (WAN needs an explicit primary).
func (c *HaifyClient) CreateResourceAutoPlace(ctx context.Context, name string, port uint32, replicas uint32, replicasOnDifferent, replicasOnSame, doNotPlaceWith []string, protocol string, sizeGB uint32, pool, storageType string, drbdOptions map[string]string) error {
	log := zap.L()
	log.Debug("HaifyClient.CreateResourceAutoPlace: sending request", zap.String("name", name), zap.Uint32("port", port), zap.Uint32("replicas", replicas), zap.Strings("replicas_on_different", replicasOnDifferent), zap.Strings("replicas_on_same", replicasOnSame), zap.Strings("do_not_place_with", doNotPlaceWith), zap.String("protocol", protocol), zap.Uint32("size_gb", sizeGB), zap.String("pool", pool), zap.String("storage_type", storageType), zap.Int("drbd_options_count", len(drbdOptions)))
	resp, err := c.client.CreateResource(ctx, &haifypb.CreateResourceRequest{
		Name:                name,
		Port:                port,
		Replicas:            replicas,
		ReplicasOnDifferent: replicasOnDifferent,
		ReplicasOnSame:      replicasOnSame,
		DoNotPlaceWith:      doNotPlaceWith,
		Protocol:            protocol,
		SizeGb:              sizeGB,
		Pool:                pool,
		StorageType:         storageType,
		DrbdOptions:         drbdOptions,
	})
	if err != nil {
		log.Debug("HaifyClient.CreateResourceAutoPlace: CreateResource failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.CreateResourceAutoPlace: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.CreateResourceAutoPlace: done")
	return nil
}

// CreateResourceWithVolumes creates a DRBD resource with one or more volumes
// specified explicitly. The volumes slice maps to CreateResourceRequest.Volumes
// (volume 0..N); size_gb/pool on the request are left zero and ignored by the
// controller when volumes is non-empty. storageType applies to every volume.
func (c *HaifyClient) CreateResourceWithVolumes(ctx context.Context, name string, port uint32, nodes []string, protocol, storageType string, drbdOptions map[string]string, volumes []*haifypb.VolumeSpec) error {
	log := zap.L()
	log.Debug("HaifyClient.CreateResourceWithVolumes: sending request", zap.String("name", name), zap.Uint32("port", port), zap.Strings("nodes", nodes), zap.String("protocol", protocol), zap.String("storage_type", storageType), zap.Int("drbd_options_count", len(drbdOptions)), zap.Int("volumes_count", len(volumes)))
	req := &haifypb.CreateResourceRequest{
		Name:        name,
		Port:        port,
		Nodes:       nodes,
		Protocol:    protocol,
		StorageType: storageType,
		DrbdOptions: drbdOptions,
		Volumes:     volumes,
	}

	resp, err := c.client.CreateResource(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.CreateResourceWithVolumes: CreateResource failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.CreateResourceWithVolumes: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.CreateResourceWithVolumes: done")
	return nil
}

// CreateZFSResource creates a DRBD resource with ZFS backend
func (c *HaifyClient) CreateZFSResource(ctx context.Context, name string, port uint32, nodes []string, protocol string, sizeGB uint32, pool string, drbdOptions map[string]string) error {
	return c.CreateResourceWithPoolAndType(ctx, name, port, nodes, protocol, sizeGB, pool, "zfs", drbdOptions)
}

// CreateResourceWAN creates an opt-in WAN-replicated DRBD resource: it
// replicates between the single primary node and drNode across the internet via
// a per-resource haify-proxy pair. WAN forces protocol A; wanPort 0 lets the
// controller pick a random high port. Absent WAN flags, callers use the plain
// Create* methods above (which leave Wan=false ⇒ an unchanged LAN request).
func (c *HaifyClient) CreateResourceWAN(ctx context.Context, name string, port uint32, primaryNode string, sizeGB uint32, pool, storageType string, drbdOptions map[string]string, drNode, drEndpoint string, wanPort uint32) error {
	log := zap.L()
	log.Debug("HaifyClient.CreateResourceWAN: sending request", zap.String("name", name), zap.Uint32("port", port), zap.String("primary_node", primaryNode), zap.Uint32("size_gb", sizeGB), zap.String("pool", pool), zap.String("storage_type", storageType), zap.Int("drbd_options_count", len(drbdOptions)), zap.String("dr_node", drNode), zap.String("dr_endpoint", drEndpoint), zap.Uint32("wan_port", wanPort))
	req := &haifypb.CreateResourceRequest{
		Name:        name,
		Port:        port,
		Nodes:       []string{primaryNode},
		Protocol:    "A",
		SizeGb:      sizeGB,
		Pool:        pool,
		StorageType: storageType,
		DrbdOptions: drbdOptions,
		Wan:         true,
		DrNode:      drNode,
		DrEndpoint:  drEndpoint,
		WanPort:     wanPort,
	}

	resp, err := c.client.CreateResource(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.CreateResourceWAN: CreateResource failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.CreateResourceWAN: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.CreateResourceWAN: done")
	return nil
}

// AdoptResource imports an already-existing (foreign) DRBD resource into Haify
// management by recording its metadata. nodes/port/protocol may be left empty
// (nil/0/"") to auto-discover them from the live .res on a node. It never
// creates or modifies the DRBD resource or its data. Returns the metadata that
// was recorded.
func (c *HaifyClient) AdoptResource(ctx context.Context, name string, nodes []string, port uint32, protocol string) (*haifypb.AdoptResourceResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.AdoptResource: sending request", zap.String("name", name), zap.Strings("nodes", nodes), zap.Uint32("port", port), zap.String("protocol", protocol))
	req := &haifypb.AdoptResourceRequest{
		Name:     name,
		Nodes:    nodes,
		Port:     port,
		Protocol: protocol,
	}

	resp, err := c.client.AdoptResource(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.AdoptResource: AdoptResource failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.AdoptResource: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.AdoptResource: done")
	return resp, nil
}

// GetResource gets resource information
func (c *HaifyClient) GetResource(ctx context.Context, name string) (*haifypb.ResourceInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetResource: sending request", zap.String("name", name))
	req := &haifypb.GetResourceRequest{
		Name: name,
	}

	resp, err := c.client.GetResource(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.GetResource: GetResource failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.GetResource: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.GetResource: done")
	return resp.Resource, nil
}

// ListResources lists all resources
func (c *HaifyClient) ListResources(ctx context.Context) ([]*haifypb.ResourceInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListResources: sending request")
	req := &haifypb.ListResourcesRequest{}

	resp, err := c.client.ListResources(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ListResources: ListResources failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.ListResources: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.ListResources: done", zap.Int("count", len(resp.Resources)))
	return resp.Resources, nil
}

// DeleteResource deletes a DRBD resource
func (c *HaifyClient) DeleteResource(ctx context.Context, name string) error {
	log := zap.L()
	log.Debug("HaifyClient.DeleteResource: sending request", zap.String("name", name))
	req := &haifypb.DeleteResourceRequest{
		Name: name,
	}

	resp, err := c.client.DeleteResource(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.DeleteResource: DeleteResource failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.DeleteResource: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.DeleteResource: done")
	return nil
}

// AddVolume adds a volume to a resource
func (c *HaifyClient) AddVolume(ctx context.Context, resource, volume, pool string, sizeGB uint32) error {
	log := zap.L()
	log.Debug("HaifyClient.AddVolume: sending request", zap.String("resource", resource), zap.String("volume", volume), zap.String("pool", pool), zap.Uint32("size_gb", sizeGB))
	req := &haifypb.AddVolumeRequest{
		Resource: resource,
		Volume:   volume,
		Pool:     pool,
		SizeGb:   sizeGB,
	}

	resp, err := c.client.AddVolume(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.AddVolume: AddVolume failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.AddVolume: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.AddVolume: done")
	return nil
}

// UpdateResourceOptions applies DRBD options ("section/key" -> value) to an
// existing resource and runs drbdadm adjust across the cluster.
// RepairResource reconciles every participant's copy of a resource's config.
func (c *HaifyClient) RepairResource(ctx context.Context, resource string) error {
	log := zap.L()
	log.Debug("HaifyClient.RepairResource: sending request", zap.String("resource", resource))
	resp, err := c.client.RepairResource(ctx, &haifypb.RepairResourceRequest{Name: resource})
	if err != nil {
		log.Debug("HaifyClient.RepairResource: RepairResource failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.RepairResource: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.RepairResource: done")
	return nil
}

func (c *HaifyClient) UpdateResourceOptions(ctx context.Context, resource string, options map[string]string) error {
	log := zap.L()
	log.Debug("HaifyClient.UpdateResourceOptions: sending request", zap.String("resource", resource), zap.Int("options_count", len(options)))
	resp, err := c.client.UpdateResourceOptions(ctx, &haifypb.UpdateResourceOptionsRequest{
		Name:    resource,
		Options: options,
	})
	if err != nil {
		log.Debug("HaifyClient.UpdateResourceOptions: UpdateResourceOptions failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.UpdateResourceOptions: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.UpdateResourceOptions: done")
	return nil
}

// RemoveVolume removes a volume from a resource
func (c *HaifyClient) RemoveVolume(ctx context.Context, resource string, volumeID uint32) error {
	log := zap.L()
	log.Debug("HaifyClient.RemoveVolume: sending request", zap.String("resource", resource), zap.Uint32("volume_id", volumeID))
	req := &haifypb.RemoveVolumeRequest{
		Resource: resource,
		VolumeId: volumeID,
	}

	resp, err := c.client.RemoveVolume(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.RemoveVolume: RemoveVolume failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.RemoveVolume: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.RemoveVolume: done")
	return nil
}

// ResizeVolume resizes a volume
func (c *HaifyClient) ResizeVolume(ctx context.Context, resource string, volumeID uint32, sizeGB uint32) error {
	return c.resizeVolume(ctx, resource, volumeID, sizeGB, false)
}

// ResizeVolumeIgnoringFreeSpace is ResizeVolume without the check that every
// replica's pool has room for the growth.
func (c *HaifyClient) ResizeVolumeIgnoringFreeSpace(ctx context.Context, resource string, volumeID uint32, sizeGB uint32) error {
	return c.resizeVolume(ctx, resource, volumeID, sizeGB, true)
}

func (c *HaifyClient) resizeVolume(ctx context.Context, resource string, volumeID uint32, sizeGB uint32, ignoreFreeSpace bool) error {
	log := zap.L()
	log.Debug("HaifyClient.resizeVolume: sending request", zap.String("resource", resource), zap.Uint32("volume_id", volumeID), zap.Uint32("size_gb", sizeGB), zap.Bool("ignore_free_space", ignoreFreeSpace))
	req := &haifypb.ResizeVolumeRequest{
		Resource:        resource,
		VolumeId:        volumeID,
		SizeGb:          sizeGB,
		IgnoreFreeSpace: ignoreFreeSpace,
	}

	resp, err := c.client.ResizeVolume(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.resizeVolume: ResizeVolume failed", zap.Error(err))
		return err
	}

	if !resp.Success {
		log.Debug("HaifyClient.resizeVolume: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.resizeVolume: done")
	return nil
}

// ResourceStatus gets resource detailed status
func (c *HaifyClient) ResourceStatus(ctx context.Context, name string) (*haifypb.ResourceStatus, error) {
	log := zap.L()
	log.Debug("HaifyClient.ResourceStatus: sending request", zap.String("name", name))
	req := &haifypb.ResourceStatusRequest{
		Name: name,
	}

	resp, err := c.client.ResourceStatus(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.ResourceStatus: ResourceStatus failed", zap.Error(err))
		return nil, err
	}

	if !resp.Success {
		log.Debug("HaifyClient.ResourceStatus: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}

	log.Debug("HaifyClient.ResourceStatus: done")
	return resp.Status, nil
}

// DRFailback moves a WAN resource back from its DR node to the primary site,
// or advances a failback in progress; see DRFailbackRequest. The response's
// phase says whether to run it again.
// VerifyResource starts, follows or — with resync — repairs an online verify
// of a resource's replicas; see VerifyResourceRequest.
func (c *HaifyClient) VerifyResource(ctx context.Context, req *haifypb.VerifyResourceRequest) (*haifypb.VerifyResourceResponse, error) {
	zap.L().Debug("HaifyClient.VerifyResource: sending request", zap.String("name", req.GetName()), zap.String("node", req.GetNode()), zap.Uint32("wait_seconds", req.GetWaitSeconds()), zap.Bool("resync", req.GetResync()))
	return c.client.VerifyResource(ctx, req)
}

// SetResourceLabels sets the labels in set on resource, then removes the keys
// in remove, and returns the resource's labels afterwards.
func (c *HaifyClient) SetResourceLabels(ctx context.Context, resource string, set map[string]string, remove []string) (map[string]string, error) {
	log := zap.L()
	log.Debug("HaifyClient.SetResourceLabels: sending request", zap.String("resource", resource), zap.Int("set_count", len(set)), zap.Strings("remove", remove))
	resp, err := c.client.SetResourceLabels(ctx, &haifypb.SetResourceLabelsRequest{
		Resource: resource,
		Labels:   set,
		Remove:   remove,
	})
	if err != nil {
		log.Debug("HaifyClient.SetResourceLabels: SetResourceLabels failed", zap.Error(err))
		return nil, err
	}
	return resp.GetLabels(), nil
}
