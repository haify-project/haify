package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/client"
)

// ---- input/output types ----

type haCreateIn struct {
	Resource string   `json:"resource" jsonschema:"DRBD resource to make highly available"`
	VIP      string   `json:"vip,omitempty" jsonschema:"floating virtual IP in CIDR notation, e.g. 192.168.1.50/24; omit for failover without a VIP"`
	Mount    string   `json:"mount,omitempty" jsonschema:"mount point managed by the HA config, e.g. /mnt/data"`
	FsType   string   `json:"fs_type,omitempty" jsonschema:"filesystem type for the mount (default ext4)"`
	Services []string `json:"services,omitempty" jsonschema:"systemd services started on the active node, e.g. [\"mysql\"]"`
	VM       string   `json:"vm,omitempty" jsonschema:"libvirt guest to run on the active node instead; it must already be defined under this name on every diskful replica (virsh define, autostart off), with its disks on /dev/drbd/by-res/<resource>/<volume>"`
}

type haResourceIn struct {
	Resource string `json:"resource" jsonschema:"DRBD resource name"`
}

type haConfigOut struct {
	Resource   string   `json:"resource"`
	VIP        string   `json:"vip,omitempty"`
	MountPoint string   `json:"mount_point,omitempty"`
	FsType     string   `json:"fs_type,omitempty"`
	Services   []string `json:"services,omitempty"`
}

type haConfigStatusOut struct {
	haConfigOut
	Promoter *haPromoterOut `json:"promoter,omitempty" jsonschema:"live state of the resource's drbd-reactor chain"`
}

type haListOut struct {
	Configs []haConfigOut `json:"configs"`
}

type selfHaStatusOut struct {
	Enabled    bool     `json:"enabled"`
	Resource   string   `json:"resource,omitempty" jsonschema:"DRBD resource backing the controller database"`
	VIP        string   `json:"vip,omitempty"`
	Nodes      []string `json:"nodes,omitempty"`
	ActiveNode string   `json:"active_node,omitempty"`
}

type selfHaEnableIn struct {
	VIP    string   `json:"vip" jsonschema:"floating virtual IP for the controller in CIDR notation, e.g. 192.168.1.60/24"`
	Pool   string   `json:"pool" jsonschema:"storage pool for the controller metadata resource (must exist on all target nodes)"`
	SizeGB uint32   `json:"size_gb,omitempty" jsonschema:"metadata resource size in GiB (default 1)"`
	Port   uint32   `json:"port,omitempty" jsonschema:"DRBD port for the metadata resource (default 7999)"`
	Nodes  []string `json:"nodes,omitempty" jsonschema:"target nodes; defaults to all registered nodes"`
}

type selfHaDisableIn struct {
	Node string `json:"node" jsonschema:"node that keeps running the controller standalone"`
}

// registerHATools adds HA failover and controller self-HA tools.
func (s *Server) registerHATools(srv *mcp.Server) {
	addRead(s, srv, readOnlyTool("haify_ha_list", "List HA configs",
		"List all drbd-reactor HA configurations managed by Haify."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, haListOut, error) {
			configs, err := s.client.ListHa(ctx)
			if err != nil {
				return nil, haListOut{}, err
			}
			out := haListOut{Configs: make([]haConfigOut, 0, len(configs))}
			for _, c := range configs {
				out.Configs = append(out.Configs, haConfigOut{
					Resource:   c.Resource,
					VIP:        c.Vip,
					MountPoint: c.MountPoint,
					FsType:     c.FsType,
					Services:   c.Services,
				})
			}
			return nil, out, nil
		})

	addRead(s, srv, readOnlyTool("haify_ha_status", "HA config status",
		"Show the HA configuration of one resource — VIP, mount point, managed services — and, live, its "+
			"service chain, whether each standby could take it over, and what outside the chain uses its mounts."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in haResourceIn) (*mcp.CallToolResult, haConfigStatusOut, error) {
			c, err := s.client.GetHa(ctx, in.Resource)
			if err != nil {
				return nil, haConfigStatusOut{}, err
			}
			out := haConfigStatusOut{haConfigOut: haConfigOut{
				Resource:   c.Resource,
				VIP:        c.Vip,
				MountPoint: c.MountPoint,
				FsType:     c.FsType,
				Services:   c.Services,
			}}
			// The live part is best effort: the stored config stands on its own.
			if sts, err := s.client.GetHaStatus(ctx, in.Resource); err == nil && len(sts) > 0 {
				p := haPromoterOutFrom(sts[0])
				out.Promoter = &p
			}
			return nil, out, nil
		})

	addWrite(s, srv, writeTool("haify_ha_create", "Create HA config",
		"Make a DRBD resource highly available via drbd-reactor: on the active node the resource is promoted, "+
			"mounted, the VIP is brought up, and services are started. Failover is automatic. With vm, the "+
			"active node runs that libvirt guest (ocf:heartbeat:VirtualDomain), restarted on another replica "+
			"when its node fails."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in haCreateIn) (*mcp.CallToolResult, opResult, error) {
			fsType := in.FsType
			if fsType == "" && in.Mount != "" {
				fsType = "ext4"
			}
			var agents []*haifypb.OcfAgent
			if in.VM != "" {
				agent, err := client.VirtualDomainAgent(in.VM)
				if err != nil {
					return nil, opResult{}, err
				}
				agents = append(agents, agent)
			}
			configPath, err := s.client.MakeHa(ctx, in.Resource, in.Services, in.Mount, fsType, in.VIP, agents, nil)
			if err != nil {
				return nil, opResult{}, err
			}
			parts := []string{fmt.Sprintf("HA config for %s created (%s)", in.Resource, configPath)}
			if in.VM != "" {
				if _, err := s.client.SetResourceLabels(ctx, in.Resource,
					map[string]string{client.LibvirtDomainLabel: in.VM}, nil); err != nil {
					parts = append(parts, "the resource could not be labelled with the guest: "+err.Error())
				}
			}
			if in.VIP != "" {
				parts = append(parts, "VIP "+in.VIP)
			}
			if in.Mount != "" {
				parts = append(parts, "mount "+in.Mount)
			}
			return nil, ok(strings.Join(parts, ", ")), nil
		})

	addWrite(s, srv, destructiveTool("haify_ha_evict", "Evict HA resource",
		"Planned switchover of one HA resource: its active node stops the services and another node takes over. "+
			"It moves exactly the resource named and nothing else on the node — haify-meta is the controller with its "+
			"VIP and the AI Copilot, any other name is that resource's own service. For haify-meta the call returns as "+
			"soon as the switchover is launched, and when this server runs in the same chain (as Haify sets it up) it "+
			"moves too: expect this connection to drop for several seconds, which is not a failure; check "+
			"haify_ha_promoter_status for where it landed. For any other resource the call returns once another node "+
			"has taken over, and fails if none did. Never stop or restart a drbd-reactor-managed service by hand "+
			"instead: the promoter treats that as a fault and fails the resource over."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in haResourceIn) (*mcp.CallToolResult, opResult, error) {
			if err := s.client.EvictHa(ctx, in.Resource); err != nil {
				return nil, opResult{}, err
			}
			return nil, ok(fmt.Sprintf("resource %s evicted from its active node", in.Resource)), nil
		})

	addWrite(s, srv, destructiveTool("haify_ha_delete", "Delete HA config",
		"Remove the drbd-reactor HA configuration of a resource. Running services are stopped; "+
			"the DRBD resource and its data are kept."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in haResourceIn) (*mcp.CallToolResult, opResult, error) {
			if err := s.client.DeleteHa(ctx, in.Resource); err != nil {
				return nil, opResult{}, err
			}
			return nil, ok(fmt.Sprintf("HA config for %s deleted", in.Resource)), nil
		})

	addRead(s, srv, readOnlyTool("haify_self_ha_status", "Controller self-HA status",
		"Show whether the Haify controller itself runs highly available and which node is active."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, selfHaStatusOut, error) {
			st, err := s.client.GetSelfHaStatus(ctx)
			if err != nil {
				return nil, selfHaStatusOut{}, err
			}
			return nil, selfHaStatusOut{
				Enabled:    st.Enabled,
				Resource:   st.Resource,
				VIP:        st.VIP,
				Nodes:      st.Nodes,
				ActiveNode: st.ActiveNode,
			}, nil
		})

	addWrite(s, srv, destructiveTool("haify_self_ha_enable", "Enable controller self-HA",
		"Make the Haify controller itself highly available: creates a small DRBD resource for the controller "+
			"database, distributes the controller to all nodes, and hands management to drbd-reactor behind a VIP. "+
			"The controller restarts during the handoff — reconnect to the VIP afterwards."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in selfHaEnableIn) (*mcp.CallToolResult, opResult, error) {
			sizeGB := in.SizeGB
			if sizeGB == 0 {
				sizeGB = 1
			}
			port := in.Port
			if port == 0 {
				port = 7999
			}
			resource, vip, err := s.client.EnableSelfHa(ctx, in.VIP, in.Pool, sizeGB, port, in.Nodes)
			if err != nil {
				return nil, opResult{}, err
			}
			return nil, ok(fmt.Sprintf("controller self-HA enabled: resource %s, VIP %s; reconnect via the VIP",
				resource, vip)), nil
		})

	addWrite(s, srv, destructiveTool("haify_self_ha_disable", "Disable controller self-HA",
		"Revert the controller to standalone mode on one node. The controller restarts during the handoff."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in selfHaDisableIn) (*mcp.CallToolResult, opResult, error) {
			if err := s.client.DisableSelfHa(ctx, in.Node); err != nil {
				return nil, opResult{}, err
			}
			return nil, ok(fmt.Sprintf("controller self-HA disabled, standalone on %s", in.Node)), nil
		})
}
