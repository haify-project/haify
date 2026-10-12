package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// AttachDisklessClient adds a node to a resource as a diskless data client: it
// carries no local replica but connects over DRBD and can be promoted Primary
// to serve the volume over the network. Idempotent.
func (c *HaifyClient) AttachDisklessClient(ctx context.Context, resource, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.AttachDisklessClient: sending request", zap.String("resource", resource), zap.String("node", node))
	resp, err := c.client.AttachDisklessClient(ctx, &haifypb.AttachDisklessClientRequest{
		Resource: resource,
		Node:     node,
	})
	if err != nil {
		log.Debug("HaifyClient.AttachDisklessClient: AttachDisklessClient failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.AttachDisklessClient: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.AttachDisklessClient: done")
	return nil
}

// DetachDisklessClient removes a diskless client added via AttachDisklessClient.
// Idempotent.
func (c *HaifyClient) DetachDisklessClient(ctx context.Context, resource, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.DetachDisklessClient: sending request", zap.String("resource", resource), zap.String("node", node))
	resp, err := c.client.DetachDisklessClient(ctx, &haifypb.DetachDisklessClientRequest{
		Resource: resource,
		Node:     node,
	})
	if err != nil {
		log.Debug("HaifyClient.DetachDisklessClient: DetachDisklessClient failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.DetachDisklessClient: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.DetachDisklessClient: done")
	return nil
}

// EvictHa evicts an HA resource from the active node
// SetTiebreaker moves a resource's diskless quorum tiebreaker to node (empty
// removes it). It returns the node that previously held it and the controller's
// description of the result — which for a removal says whether the remaining
// replicas still make a quorum majority, something only the controller knows.
func (c *HaifyClient) SetTiebreaker(ctx context.Context, resource, node string) (string, string, error) {
	log := zap.L()
	log.Debug("HaifyClient.SetTiebreaker: sending request", zap.String("resource", resource), zap.String("node", node))
	resp, err := c.client.SetTiebreaker(ctx, &haifypb.SetTiebreakerRequest{
		Resource: resource,
		Node:     node,
	})
	if err != nil {
		log.Debug("HaifyClient.SetTiebreaker: SetTiebreaker failed", zap.Error(err))
		return "", "", err
	}
	if !resp.Success {
		log.Debug("HaifyClient.SetTiebreaker: controller reported failure", zap.String("message", resp.Message))
		return "", "", fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.SetTiebreaker: done")
	return resp.PreviousNode, resp.Message, nil
}

// AddDR attaches an off-site asynchronous replica to a running resource,
// returning the base WAN port in use (the controller allocates one when the
// caller passes 0).
func (c *HaifyClient) AddDR(ctx context.Context, resource, drNode, drEndpoint string, wanPort uint32, egressAddress string) (uint32, error) {
	log := zap.L()
	log.Debug("HaifyClient.AddDR: sending request", zap.String("resource", resource), zap.String("dr_node", drNode), zap.String("dr_endpoint", drEndpoint), zap.Uint32("wan_port", wanPort), zap.String("egress_address", egressAddress))
	resp, err := c.client.AddDR(ctx, &haifypb.AddDRRequest{
		Resource:      resource,
		DrNode:        drNode,
		DrEndpoint:    drEndpoint,
		WanPort:       wanPort,
		EgressAddress: egressAddress,
	})
	if err != nil {
		log.Debug("HaifyClient.AddDR: AddDR failed", zap.Error(err))
		return 0, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.AddDR: controller reported failure", zap.String("message", resp.Message))
		return 0, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.AddDR: done")
	return resp.WanPort, nil
}

// AddReplica adds a diskful local replica to a running resource.
func (c *HaifyClient) AddReplica(ctx context.Context, resource, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.AddReplica: sending request", zap.String("resource", resource), zap.String("node", node))
	resp, err := c.client.AddReplica(ctx, &haifypb.AddReplicaRequest{Resource: resource, Node: node})
	if err != nil {
		log.Debug("HaifyClient.AddReplica: AddReplica failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.AddReplica: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.AddReplica: done")
	return nil
}

// AddReplicaWith adds a replica with AddReplicaRequest's options.
func (c *HaifyClient) AddReplicaWith(ctx context.Context, req *haifypb.AddReplicaRequest) error {
	log := zap.L()
	log.Debug("HaifyClient.AddReplicaWith: sending request", zap.String("resource", req.GetResource()), zap.String("node", req.GetNode()),
		zap.Bool("ignore_free_space", req.GetIgnoreFreeSpace()), zap.Bool("allow_unreachable", req.GetAllowUnreachable()))
	resp, err := c.client.AddReplica(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.AddReplicaWith: AddReplica failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.AddReplicaWith: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.AddReplicaWith: done")
	return nil
}

// AddReplicaIgnoringFreeSpace adds a replica although the node's pool has less
// free space than the volume; see AddReplicaRequest.ignore_free_space.
func (c *HaifyClient) AddReplicaIgnoringFreeSpace(ctx context.Context, resource, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.AddReplicaIgnoringFreeSpace: sending request", zap.String("resource", resource), zap.String("node", node))
	resp, err := c.client.AddReplica(ctx, &haifypb.AddReplicaRequest{Resource: resource, Node: node, IgnoreFreeSpace: true})
	if err != nil {
		log.Debug("HaifyClient.AddReplicaIgnoringFreeSpace: AddReplica failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.AddReplicaIgnoringFreeSpace: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.AddReplicaIgnoringFreeSpace: done")
	return nil
}

// RemoveReplica takes a diskful replica out of a running resource.
func (c *HaifyClient) RemoveReplica(ctx context.Context, resource, node string) error {
	log := zap.L()
	log.Debug("HaifyClient.RemoveReplica: sending request", zap.String("resource", resource), zap.String("node", node))
	_, err := c.RemoveReplicaOptions(ctx, resource, node, false)
	log.Debug("HaifyClient.RemoveReplica: done")
	return err
}

// RemoveReplicaOptions is RemoveReplica; lost removes the replica of a node
// that is gone for good, without reaching it. It returns the controller's
// message, which for a lost node says what to clean up on it.
func (c *HaifyClient) RemoveReplicaOptions(ctx context.Context, resource, node string, lost bool) (string, error) {
	log := zap.L()
	log.Debug("HaifyClient.RemoveReplicaOptions: sending request", zap.String("resource", resource), zap.String("node", node), zap.Bool("lost", lost))
	resp, err := c.client.RemoveReplica(ctx, &haifypb.RemoveReplicaRequest{Resource: resource, Node: node, Lost: lost})
	if err != nil {
		log.Debug("HaifyClient.RemoveReplicaOptions: RemoveReplica failed", zap.Error(err))
		return "", err
	}
	if !resp.Success {
		log.Debug("HaifyClient.RemoveReplicaOptions: controller reported failure", zap.String("message", resp.Message))
		return "", fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.RemoveReplicaOptions: done")
	return resp.Message, nil
}

// RepairWanProxy reconciles a WAN resource's replication tunnels with the
// controller's current node list.
func (c *HaifyClient) RepairWanProxy(ctx context.Context, name string, dryRun bool) (*haifypb.RepairWanProxyResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.RepairWanProxy: sending request", zap.String("name", name), zap.Bool("dry_run", dryRun))
	resp, err := c.client.RepairWanProxy(ctx, &haifypb.RepairWanProxyRequest{Name: name, DryRun: dryRun})
	if err != nil {
		log.Debug("HaifyClient.RepairWanProxy: RepairWanProxy failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.RepairWanProxy: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.RepairWanProxy: done")
	return resp, nil
}

// SetWanEndpoint changes where a WAN resource's primary site reaches its DR
// site; see SetWanEndpointRequest for the fields.
func (c *HaifyClient) SetWanEndpoint(ctx context.Context, req *haifypb.SetWanEndpointRequest) (*haifypb.SetWanEndpointResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.SetWanEndpoint: sending request", zap.String("name", req.GetName()), zap.String("dr_endpoint", req.GetDrEndpoint()),
		zap.String("egress_address", req.GetEgressAddress()), zap.Bool("clear_egress", req.GetClearEgress()), zap.Bool("skip_reachability_check", req.GetSkipReachabilityCheck()))
	resp, err := c.client.SetWanEndpoint(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.SetWanEndpoint: SetWanEndpoint failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.SetWanEndpoint: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.SetWanEndpoint: done")
	return resp, nil
}

func (c *HaifyClient) DRFailback(ctx context.Context, name, node string, waitSeconds uint32) (*haifypb.DRFailbackResponse, error) {
	zap.L().Debug("HaifyClient.DRFailback: sending request", zap.String("name", name), zap.String("node", node), zap.Uint32("wait_seconds", waitSeconds))
	return c.client.DRFailback(ctx, &haifypb.DRFailbackRequest{Name: name, Node: node, WaitSeconds: waitSeconds})
}
