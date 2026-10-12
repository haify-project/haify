package mcpserver

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// ---- output types ----

type volumeOut struct {
	VolumeID      uint32 `json:"volume_id"`
	Device        string `json:"device,omitempty" jsonschema:"DRBD device path, e.g. /dev/drbd0"`
	SizeGB        uint64 `json:"size_gb"`
	Pool          string `json:"pool,omitempty"`
	BackingVolume string `json:"backing_volume,omitempty" jsonschema:"backing LV/dataset name inside the pool"`
}

type nodeStateOut struct {
	Node             string  `json:"node"`
	Role             string  `json:"role,omitempty" jsonschema:"DRBD role: Primary or Secondary"`
	DiskState        string  `json:"disk_state,omitempty" jsonschema:"DRBD disk state, e.g. UpToDate"`
	ReplicationState string  `json:"replication_state,omitempty" jsonschema:"DRBD replication state, e.g. Established, SyncSource, SyncTarget"`
	SyncPercent      float64 `json:"sync_percent,omitempty" jsonschema:"resync completion 0..100; 100 in steady state, lower while a resync is in progress"`
}

type resourceOut struct {
	Name          string            `json:"name"`
	Port          uint32            `json:"port"`
	Protocol      string            `json:"protocol,omitempty"`
	Nodes         []string          `json:"nodes"`
	Role          string            `json:"role,omitempty"`
	Volumes       []volumeOut       `json:"volumes,omitempty"`
	NodeStates    []nodeStateOut    `json:"node_states,omitempty"`
	DisklessNodes []string          `json:"diskless_nodes,omitempty"`
	QuorumRisk    bool              `json:"quorum_risk,omitempty"`
	Profile       string            `json:"profile,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
}

type resourceListOut struct {
	Resources []resourceOut `json:"resources"`
}

type resourceProfileOut struct {
	Name                string            `json:"name"`
	Protocol            string            `json:"protocol,omitempty"`
	StorageType         string            `json:"storage_type,omitempty"`
	Pool                string            `json:"pool,omitempty"`
	Replicas            uint32            `json:"replicas,omitempty"`
	ReplicasOnDifferent []string          `json:"replicas_on_different,omitempty"`
	ReplicasOnSame      []string          `json:"replicas_on_same,omitempty"`
	DrbdOptions         map[string]string `json:"drbd_options,omitempty"`
	Labels              map[string]string `json:"labels,omitempty"`
}

type resourceProfileListOut struct {
	Profiles []resourceProfileOut `json:"profiles"`
}

func volumesOut(vols []*haifypb.VolumeInfo) []volumeOut {
	out := make([]volumeOut, 0, len(vols))
	log := zap.L()
	for _, v := range vols {
		log.Debug("volumesOut: volume", zap.Uint32("volume", v.VolumeId), zap.String("device", v.Device),
			zap.Uint64("size_gb", v.SizeGb), zap.String("pool", v.Pool))
		out = append(out, volumeOut{
			VolumeID:      v.VolumeId,
			Device:        v.Device,
			SizeGB:        v.SizeGb,
			Pool:          v.Pool,
			BackingVolume: v.BackingVolume,
		})
	}
	return out
}

func nodeStatesOut(states map[string]*haifypb.NodeResourceState) []nodeStateOut {
	out := make([]nodeStateOut, 0, len(states))
	log := zap.L()
	for node, st := range states {
		log.Debug("nodeStatesOut: node state", zap.String("node", node), zap.String("role", st.Role),
			zap.String("disk_state", st.DiskState), zap.String("replication_state", st.ReplicationState))
		out = append(out, nodeStateOut{
			Node:             node,
			Role:             st.Role,
			DiskState:        st.DiskState,
			ReplicationState: st.ReplicationState,
			SyncPercent:      st.SyncPercent,
		})
	}
	return out
}

// ---- input types ----

type resourceNameIn struct {
	Name string `json:"name" jsonschema:"DRBD resource name"`
}

type volumeSpecIn struct {
	SizeGB uint32 `json:"size_gb" jsonschema:"volume size in GiB"`
	Pool   string `json:"pool,omitempty" jsonschema:"backing storage pool for this volume; auto-selected when empty"`
}

type resourceCreateIn struct {
	Name        string            `json:"name" jsonschema:"DRBD resource name"`
	Port        uint32            `json:"port" jsonschema:"DRBD replication TCP port, e.g. 7001; must be unique per resource"`
	Nodes       []string          `json:"nodes" jsonschema:"nodes to replicate across, e.g. [\"node1\",\"node2\"]"`
	Pool        string            `json:"pool,omitempty" jsonschema:"backing storage pool name (single-volume shorthand; ignored when volumes is set)"`
	SizeGB      uint32            `json:"size_gb,omitempty" jsonschema:"volume size in GiB (single-volume shorthand; ignored when volumes is set)"`
	Volumes     []volumeSpecIn    `json:"volumes,omitempty" jsonschema:"optional list of volumes for a multi-volume resource; each element is {size_gb, pool}. When set, volume 0..N are created atomically and the top-level size_gb/pool are ignored. Leave empty for a single-volume resource."`
	StorageType string            `json:"storage_type,omitempty" jsonschema:"backing storage type: lvm (default), lvm-thin, or zfs (applies to all volumes)"`
	Protocol    string            `json:"protocol,omitempty" jsonschema:"DRBD protocol A, B, or C (default C)"`
	DrbdOptions map[string]string `json:"drbd_options,omitempty" jsonschema:"extra DRBD options, e.g. {\"options/on-no-quorum\":\"suspend-io\"}"`
	Profile     string            `json:"profile,omitempty" jsonschema:"optional resource profile whose defaults are applied at creation"`
	Labels      map[string]string `json:"labels,omitempty" jsonschema:"resource metadata labels"`
}

type resourceSetRoleIn struct {
	Resource      string `json:"resource" jsonschema:"DRBD resource name"`
	Node          string `json:"node" jsonschema:"target node"`
	Role          string `json:"role" jsonschema:"desired role: primary or secondary"`
	Force         bool   `json:"force,omitempty" jsonschema:"force promotion (needed for the first promotion of a new resource)"`
	QuorumGuarded bool   `json:"quorum_guarded,omitempty" jsonschema:"safe hard-failover promote (primary only): the controller force-promotes only if the node holds DRBD quorum and refuses otherwise, preventing split-brain. Use for taking over after a hard node failure. Ignored when role is secondary."`
}

type resourceDualPrimaryIn struct {
	Resource string `json:"resource" jsonschema:"DRBD resource name"`
	Enable   bool   `json:"enable" jsonschema:"true opens the dual-primary window, false closes it"`
}

type resourceAdoptIn struct {
	Resource string   `json:"resource" jsonschema:"DRBD resource name to adopt (must already exist on the nodes)"`
	Nodes    []string `json:"nodes,omitempty" jsonschema:"nodes the resource lives on; auto-discovered from the .res when omitted"`
	Port     uint32   `json:"port,omitempty" jsonschema:"DRBD replication port; auto-discovered from the .res when omitted"`
	Protocol string   `json:"protocol,omitempty" jsonschema:"DRBD protocol A, B, or C; auto-discovered from the .res when omitted"`
}

type resourceAdoptOut struct {
	Resource string   `json:"resource"`
	Nodes    []string `json:"nodes,omitempty" jsonschema:"nodes recorded for the resource"`
	Port     uint32   `json:"port,omitempty"`
	Protocol string   `json:"protocol,omitempty"`
	Volumes  uint32   `json:"volumes" jsonschema:"number of volumes discovered and recorded"`
	Detail   string   `json:"detail,omitempty"`
}

// registerResourceTools adds DRBD resource and volume tools.
func (s *Server) registerResourceTools(srv *mcp.Server) {
	s.registerResourceProfileTools(srv)
	s.registerProfileGroupTools(srv)

	addRead(s, srv, readOnlyTool("haify_resource_list", "List resources",
		"List all DRBD resources with their nodes, volumes, and replication state."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, resourceListOut, error) {
			s.logger.Debug("haify_resource_list: listing resources")
			resources, err := s.client.ListResources(ctx)
			if err != nil {
				s.logger.Debug("haify_resource_list: list failed", zap.Error(err))
				return nil, resourceListOut{}, err
			}
			out := resourceListOut{Resources: make([]resourceOut, 0, len(resources))}
			for _, r := range resources {
				s.logger.Debug("haify_resource_list: resource", zap.String("resource", r.Name), zap.Uint32("port", r.Port),
					zap.Strings("nodes", r.Nodes), zap.String("role", r.Role), zap.Int("volumes", len(r.Volumes)),
					zap.Strings("diskless_nodes", r.DisklessNodes), zap.Bool("quorum_risk", r.QuorumRisk),
					zap.String("profile", r.Profile))
				out.Resources = append(out.Resources, resourceOut{
					Name:          r.Name,
					Port:          r.Port,
					Protocol:      r.Protocol,
					Nodes:         r.Nodes,
					Role:          r.Role,
					Volumes:       volumesOut(r.Volumes),
					NodeStates:    nodeStatesOut(r.NodeStates),
					DisklessNodes: r.DisklessNodes,
					QuorumRisk:    r.QuorumRisk,
					Profile:       r.Profile,
					Labels:        r.Labels,
				})
			}
			s.logger.Debug("haify_resource_list: listed", zap.Int("count", len(out.Resources)))
			return nil, out, nil
		})

	addRead(s, srv, readOnlyTool("haify_resource_status", "Resource status",
		"Show live DRBD status for one resource: per-node role and disk state (UpToDate, Inconsistent, ...)."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in resourceNameIn) (*mcp.CallToolResult, resourceOut, error) {
			s.logger.Debug("haify_resource_status: reading status", zap.String("resource", in.Name))
			st, err := s.client.ResourceStatus(ctx, in.Name)
			if err != nil {
				s.logger.Debug("haify_resource_status: read failed", zap.String("resource", in.Name), zap.Error(err))
				return nil, resourceOut{}, err
			}
			s.logger.Debug("haify_resource_status: read", zap.String("resource", st.Name), zap.String("role", st.Role),
				zap.Strings("nodes", st.Nodes), zap.Int("volumes", len(st.Volumes)), zap.Int("node_states", len(st.NodeStates)))
			return nil, resourceOut{
				Name:       st.Name,
				Role:       st.Role,
				Nodes:      st.Nodes,
				Volumes:    volumesOut(st.Volumes),
				NodeStates: nodeStatesOut(st.NodeStates),
			}, nil
		})

	addWrite(s, srv, writeTool("haify_resource_create", "Create resource",
		"Create a replicated DRBD resource backed by a storage pool. Allocates volumes on every node, "+
			"writes the DRBD config, and brings the resource up. Initial sync starts automatically. "+
			"For a single volume pass size_gb (and optionally pool). For a multi-volume resource pass "+
			"the volumes array instead — each element is {size_gb, pool} and becomes DRBD volume 0..N; "+
			"the top-level size_gb/pool are then ignored."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in resourceCreateIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_resource_create: creating resource", zap.String("resource", in.Name),
				zap.Uint32("port", in.Port), zap.Strings("nodes", in.Nodes), zap.String("pool", in.Pool),
				zap.Uint32("size_gb", in.SizeGB), zap.Int("volumes", len(in.Volumes)), zap.String("storage_type", in.StorageType),
				zap.String("protocol", in.Protocol), zap.Array("drbd_option_keys", keysOf(in.DrbdOptions)),
				zap.String("profile", in.Profile), zap.Array("label_keys", keysOf(in.Labels)))
			if len(in.Nodes) == 0 {
				s.logger.Debug("haify_resource_create: refusing, no nodes given")
				return nil, opResult{}, fmt.Errorf("nodes are required")
			}
			storageType := in.StorageType
			if storageType == "" {
				s.logger.Debug("haify_resource_create: no storage type given, using lvm")
				storageType = "lvm"
			}
			protocol := in.Protocol
			if protocol == "" {
				s.logger.Debug("haify_resource_create: no protocol given, using C")
				protocol = "C"
			}
			requestClient, metadataAware := s.client.(interface {
				CreateResourceRequest(context.Context, *haifypb.CreateResourceRequest) error
			})
			if in.Profile != "" || len(in.Labels) > 0 {
				s.logger.Debug("haify_resource_create: profile or labels given, creating through the full request",
					zap.String("profile", in.Profile), zap.Int("labels", len(in.Labels)))
				if !metadataAware {
					s.logger.Debug("haify_resource_create: refusing, client cannot send profiles or labels")
					return nil, opResult{}, fmt.Errorf("controller client does not support resource profiles or labels")
				}
				volumes := make([]*haifypb.VolumeSpec, 0, len(in.Volumes))
				for i, v := range in.Volumes {
					s.logger.Debug("haify_resource_create: volume spec", zap.Int("volume", i), zap.Uint32("size_gb", v.SizeGB),
						zap.String("pool", v.Pool))
					volumes = append(volumes, &haifypb.VolumeSpec{SizeGb: v.SizeGB, Pool: v.Pool})
				}
				if err := requestClient.CreateResourceRequest(ctx, &haifypb.CreateResourceRequest{
					Name: in.Name, Port: in.Port, Nodes: in.Nodes, Pool: in.Pool, SizeGb: in.SizeGB,
					Volumes: volumes, StorageType: storageType, Protocol: protocol,
					DrbdOptions: in.DrbdOptions, Profile: in.Profile, Labels: in.Labels,
				}); err != nil {
					s.logger.Debug("haify_resource_create: create failed", zap.String("resource", in.Name), zap.Error(err))
					return nil, opResult{}, err
				}
				s.logger.Debug("haify_resource_create: created with profile", zap.String("resource", in.Name),
					zap.String("profile", in.Profile))
				return nil, ok(fmt.Sprintf("resource %s created with profile %s", in.Name, in.Profile)), nil
			}
			if len(in.Volumes) > 0 {
				s.logger.Debug("haify_resource_create: creating a multi-volume resource", zap.Int("volumes", len(in.Volumes)))
				volumes := make([]*haifypb.VolumeSpec, 0, len(in.Volumes))
				for i, v := range in.Volumes {
					s.logger.Debug("haify_resource_create: volume spec", zap.Int("volume", i), zap.Uint32("size_gb", v.SizeGB),
						zap.String("pool", v.Pool))
					volumes = append(volumes, &haifypb.VolumeSpec{SizeGb: v.SizeGB, Pool: v.Pool})
				}
				if err := s.client.CreateResourceWithVolumes(ctx, in.Name, in.Port, in.Nodes,
					protocol, storageType, in.DrbdOptions, volumes); err != nil {
					s.logger.Debug("haify_resource_create: create failed", zap.String("resource", in.Name), zap.Error(err))
					return nil, opResult{}, err
				}
				s.logger.Debug("haify_resource_create: created", zap.String("resource", in.Name), zap.Int("volumes", len(volumes)))
				return nil, ok(fmt.Sprintf("resource %s created on %d node(s) with %d volume(s)",
					in.Name, len(in.Nodes), len(volumes))), nil
			}
			s.logger.Debug("haify_resource_create: creating a single-volume resource", zap.Uint32("size_gb", in.SizeGB),
				zap.String("pool", in.Pool))
			err := s.client.CreateResourceWithPoolAndType(ctx, in.Name, in.Port, in.Nodes,
				protocol, in.SizeGB, in.Pool, storageType, in.DrbdOptions)
			if err != nil {
				s.logger.Debug("haify_resource_create: create failed", zap.String("resource", in.Name), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_resource_create: created", zap.String("resource", in.Name), zap.Strings("nodes", in.Nodes))
			return nil, ok(fmt.Sprintf("resource %s created on %d node(s), %d GiB from pool %s",
				in.Name, len(in.Nodes), in.SizeGB, in.Pool)), nil
		})

	addWrite(s, srv, destructiveTool("haify_resource_delete", "Delete resource",
		"Delete a DRBD resource and its backing volumes on all nodes. All data on the resource is lost."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in resourceNameIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_resource_delete: deleting resource", zap.String("resource", in.Name))
			if err := s.client.DeleteResource(ctx, in.Name); err != nil {
				s.logger.Debug("haify_resource_delete: delete failed", zap.String("resource", in.Name), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_resource_delete: deleted", zap.String("resource", in.Name))
			return nil, ok(fmt.Sprintf("resource %s deleted", in.Name)), nil
		})

	addWrite(s, srv, destructiveTool("haify_resource_set_role", "Set resource role",
		"Promote a resource to Primary or demote it to Secondary on a node. "+
			"Only the Primary node can mount and write the volume. Demoting fails while the volume is "+
			"mounted or in use. Do not use it on a resource an HA promoter runs: the promoter puts the "+
			"role back within seconds — use haify_ha_evict. Set quorum_guarded=true "+
			"for a safe hard-failover promote: the controller force-promotes only if the node "+
			"holds DRBD quorum and refuses otherwise, avoiding split-brain."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in resourceSetRoleIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_resource_set_role: setting role", zap.String("resource", in.Resource),
				zap.String("node", in.Node), zap.String("role", in.Role), zap.Bool("force", in.Force),
				zap.Bool("quorum_guarded", in.QuorumGuarded))
			switch in.Role {
			case "primary":
				if in.QuorumGuarded {
					s.logger.Debug("haify_resource_set_role: quorum-guarded promote", zap.String("resource", in.Resource),
						zap.String("node", in.Node))
					if err := s.client.PromoteForNode(ctx, in.Resource, in.Node); err != nil {
						s.logger.Debug("haify_resource_set_role: guarded promote failed", zap.String("resource", in.Resource),
							zap.String("node", in.Node), zap.Error(err))
						return nil, opResult{}, err
					}
					s.logger.Debug("haify_resource_set_role: promoted under quorum guard", zap.String("resource", in.Resource),
						zap.String("node", in.Node))
					return nil, ok(fmt.Sprintf("resource %s safely promoted to primary on %s (quorum-guarded)", in.Resource, in.Node)), nil
				}
				s.logger.Debug("haify_resource_set_role: promoting", zap.String("resource", in.Resource),
					zap.String("node", in.Node), zap.Bool("force", in.Force))
				if err := s.client.SetPrimary(ctx, in.Resource, in.Node, in.Force); err != nil {
					s.logger.Debug("haify_resource_set_role: promote failed", zap.String("resource", in.Resource),
						zap.String("node", in.Node), zap.Error(err))
					return nil, opResult{}, err
				}
			case "secondary":
				s.logger.Debug("haify_resource_set_role: demoting", zap.String("resource", in.Resource), zap.String("node", in.Node))
				if err := s.client.SetSecondary(ctx, in.Resource, in.Node); err != nil {
					s.logger.Debug("haify_resource_set_role: demote failed", zap.String("resource", in.Resource),
						zap.String("node", in.Node), zap.Error(err))
					return nil, opResult{}, err
				}
			default:
				s.logger.Debug("haify_resource_set_role: refusing unknown role", zap.String("role", in.Role))
				return nil, opResult{}, fmt.Errorf("invalid role %q (use primary or secondary)", in.Role)
			}
			s.logger.Debug("haify_resource_set_role: done", zap.String("resource", in.Resource), zap.String("node", in.Node),
				zap.String("role", in.Role))
			return nil, ok(fmt.Sprintf("resource %s is now %s on %s", in.Resource, in.Role, in.Node)), nil
		})

	addWrite(s, srv, writeTool("haify_resource_dual_primary", "Toggle dual-primary",
		"Open or close a DRBD dual-primary (allow-two-primaries) window on a resource. "+
			"This exists for hypervisor LIVE MIGRATION, where the source and target host both "+
			"hold the disk open during hand-off — it is not a way to share a volume between two "+
			"machines (the filesystem on it would corrupt). WAN resources are refused because "+
			"their replication is asynchronous. Always close the window after the migration; "+
			"closing is idempotent and verifies no node is left dual-primary."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in resourceDualPrimaryIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_resource_dual_primary: toggling dual-primary", zap.String("resource", in.Resource),
				zap.Bool("enable", in.Enable))
			if in.Resource == "" {
				s.logger.Debug("haify_resource_dual_primary: refusing, no resource given")
				return nil, opResult{}, fmt.Errorf("resource is required")
			}
			if err := s.client.SetDualPrimary(ctx, in.Resource, in.Enable); err != nil {
				s.logger.Debug("haify_resource_dual_primary: failed", zap.String("resource", in.Resource),
					zap.Bool("enable", in.Enable), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_resource_dual_primary: done", zap.String("resource", in.Resource), zap.Bool("enable", in.Enable))
			if in.Enable {
				return nil, ok(fmt.Sprintf("dual-primary window OPEN on %s — close it as soon as the migration finishes", in.Resource)), nil
			}
			return nil, ok(fmt.Sprintf("dual-primary window closed on %s", in.Resource)), nil
		})

	addWrite(s, srv, writeTool("haify_resource_adopt", "Adopt resource",
		"Adopt a pre-existing/foreign DRBD resource into Haify management. Auto-discovers "+
			"nodes/port/volumes from the resource's .res on a node when omitted. Writes only "+
			"Haify metadata — never touches the DRBD device or data."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in resourceAdoptIn) (*mcp.CallToolResult, resourceAdoptOut, error) {
			s.logger.Debug("haify_resource_adopt: adopting resource", zap.String("resource", in.Resource),
				zap.Strings("nodes", in.Nodes), zap.Uint32("port", in.Port), zap.String("protocol", in.Protocol))
			if in.Resource == "" {
				s.logger.Debug("haify_resource_adopt: refusing, no resource given")
				return nil, resourceAdoptOut{}, fmt.Errorf("resource is required")
			}
			resp, err := s.client.AdoptResource(ctx, in.Resource, in.Nodes, in.Port, in.Protocol)
			if err != nil {
				s.logger.Debug("haify_resource_adopt: adopt failed", zap.String("resource", in.Resource), zap.Error(err))
				return nil, resourceAdoptOut{}, err
			}
			s.logger.Debug("haify_resource_adopt: adopted", zap.String("resource", in.Resource), zap.Strings("nodes", resp.Nodes),
				zap.Uint32("port", resp.Port), zap.String("protocol", resp.Protocol), zap.Uint32("volumes", resp.Volumes))
			return nil, resourceAdoptOut{
				Resource: in.Resource,
				Nodes:    resp.Nodes,
				Port:     resp.Port,
				Protocol: resp.Protocol,
				Volumes:  resp.Volumes,
				Detail: fmt.Sprintf("resource %s adopted (%d node(s), %d volume(s), port %d)",
					in.Resource, len(resp.Nodes), resp.Volumes, resp.Port),
			}, nil
		})

	s.registerResourceVolumeTools(srv)
}
