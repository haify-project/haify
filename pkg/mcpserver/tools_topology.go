package mcpserver

import (
	"context"
	"fmt"
	"strings"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Topology and node-lifecycle tools.
//
// These change the shape of a cluster rather than its contents: how many copies
// a resource has and where, which nodes are eligible to hold them, and how a
// node is taken out of service. They were all reachable over gRPC and the CLI
// but had no MCP tool, so an assistant could describe a topology problem and
// not act on it.

type replicaIn struct {
	Resource string `json:"resource" jsonschema:"DRBD resource name"`
	Node     string `json:"node" jsonschema:"node to add or remove the replica on"`
}

type tiebreakerIn struct {
	Resource string `json:"resource"`
	Node     string `json:"node" jsonschema:"node to hold the diskless quorum tiebreaker; empty removes the tiebreaker"`
}

type addDRIn struct {
	Resource      string `json:"resource"`
	DRNode        string `json:"dr_node" jsonschema:"registered node at the off-site location"`
	DREndpoint    string `json:"dr_endpoint" jsonschema:"public address the primary site dials to reach the DR"`
	WANPort       uint32 `json:"wan_port,omitempty" jsonschema:"base mTLS WAN port; 0 auto-allocates"`
	EgressAddress string `json:"egress_address,omitempty" jsonschema:"source address the primary site egresses from, when it matters"`
}

type addDROut struct {
	Status  string `json:"status"`
	WANPort uint32 `json:"wan_port" jsonschema:"the base WAN port actually used"`
}

type wanRepairIn struct {
	Resource string `json:"resource"`
	DryRun   bool   `json:"dry_run,omitempty" jsonschema:"report the plan without touching anything; do this first, repair restarts tunnels"`
}

type wanRepairOut struct {
	Status            string   `json:"status"`
	Message           string   `json:"message"`
	ExpectedLegs      []string `json:"expected_legs,omitempty"`
	RemovedLegs       []string `json:"removed_legs,omitempty" jsonschema:"stale instances removed, or under dry_run would be removed"`
	AlreadyConsistent bool     `json:"already_consistent"`
}

type nodeNameIn struct {
	Node string `json:"node" jsonschema:"node name"`
}

type drainOut struct {
	Status    string   `json:"status"`
	Evacuated []string `json:"evacuated,omitempty" jsonschema:"resources moved off the node"`
}

type nodeLabelsIn struct {
	Node    string            `json:"node"`
	Labels  map[string]string `json:"labels" jsonschema:"key/value labels used for placement decisions"`
	Replace bool              `json:"replace,omitempty" jsonschema:"replace all labels instead of merging"`
}

type resourceLabelsIn struct {
	Resource string            `json:"resource"`
	Labels   map[string]string `json:"labels" jsonschema:"key/value labels to set; a key with an empty value is removed"`
}

type convertThinIn struct {
	Node string `json:"node"`
	Pool string `json:"pool" jsonschema:"LVM volume group to rebuild as a thin pool"`
}

type haTomlIn struct {
	Resource string `json:"resource"`
}

type haTomlOut struct {
	Resource string `json:"resource"`
	Content  string `json:"content" jsonschema:"the drbd-reactor promoter TOML currently on disk"`
	Path     string `json:"path,omitempty"`
}

type haTomlSyncIn struct {
	Resource string `json:"resource"`
	Content  string `json:"content" jsonschema:"full promoter TOML to write to every node"`
}

type agentOut struct {
	Provider  string `json:"provider"`
	Name      string `json:"name"`
	ShortDesc string `json:"shortdesc,omitempty"`
}

type agentListOut struct {
	Agents []agentOut `json:"agents"`
}

type agentMetaIn struct {
	Provider string `json:"provider" jsonschema:"OCF provider, e.g. heartbeat"`
	Name     string `json:"name" jsonschema:"agent name, e.g. IPaddr2"`
}

type agentParamOut struct {
	Name      string `json:"name"`
	Required  bool   `json:"required"`
	Type      string `json:"type,omitempty"`
	Default   string `json:"default,omitempty"`
	ShortDesc string `json:"shortdesc,omitempty"`
}

type agentMetaOut struct {
	Provider   string          `json:"provider,omitempty"`
	Name       string          `json:"name"`
	Version    string          `json:"version,omitempty"`
	ShortDesc  string          `json:"shortdesc,omitempty"`
	LongDesc   string          `json:"longdesc,omitempty"`
	Parameters []agentParamOut `json:"parameters" jsonschema:"every parameter the agent accepts; required ones must be supplied"`
}

// registerTopologyTools adds replica, DR, drain, label and HA-config tools.
func (s *Server) registerTopologyTools(srv *mcp.Server) {
	addWrite(s, srv, writeTool("haify_resource_add_replica", "Add a replica",
		"Add a full (diskful) copy of a running resource on another node. The new copy syncs in the background; "+
			"the resource stays usable throughout, and it is not a redundant copy until haify_resource_status shows "+
			"it UpToDate. The node's pool needs room for the whole volume. Refused if the node already holds a "+
			"replica, is the resource's quorum tiebreaker (remove it with haify_resource_set_tiebreaker first) or "+
			"is a diskless client (detach it first)."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in replicaIn) (*mcp.CallToolResult, opResult, error) {
			if err := s.client.AddReplica(ctx, in.Resource, in.Node); err != nil {
				return nil, opResult{}, err
			}
			return nil, ok("replica of " + in.Resource + " added on " + in.Node), nil
		})

	addWrite(s, srv, destructiveTool("haify_resource_remove_replica", "Remove a replica",
		"Take a full copy of a resource out permanently. Refused when the node is Primary, when it is the quorum "+
			"tiebreaker or the off-site DR, or when fewer than two diskful copies would remain. Unlike a conversion, "+
			"whose reduced-redundancy window closes when the resync finishes, this does not close — confirm before use."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in replicaIn) (*mcp.CallToolResult, opResult, error) {
			if err := s.client.RemoveReplica(ctx, in.Resource, in.Node); err != nil {
				return nil, opResult{}, err
			}
			return nil, ok("replica of " + in.Resource + " removed from " + in.Node), nil
		})

	addWrite(s, srv, writeTool("haify_resource_attach_diskless", "Attach a diskless client",
		"Let a node use a resource over the DRBD network without storing a local copy. This is how a workload runs "+
			"on a node that holds no replica."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in replicaIn) (*mcp.CallToolResult, opResult, error) {
			if err := s.client.AttachDisklessClient(ctx, in.Resource, in.Node); err != nil {
				return nil, opResult{}, err
			}
			return nil, ok(in.Node + " attached as a diskless client of " + in.Resource), nil
		})

	addWrite(s, srv, writeTool("haify_resource_detach_diskless", "Detach a diskless client",
		"Stop a node using a resource over the network. Does not touch any stored copy."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in replicaIn) (*mcp.CallToolResult, opResult, error) {
			if err := s.client.DetachDisklessClient(ctx, in.Resource, in.Node); err != nil {
				return nil, opResult{}, err
			}
			return nil, ok(in.Node + " detached from " + in.Resource), nil
		})

	addWrite(s, srv, writeTool("haify_resource_set_tiebreaker", "Set a quorum tiebreaker",
		"Add a diskless node that only votes in quorum and stores no data. A two-replica resource cannot keep "+
			"serving I/O when either node fails, because the survivor has no majority; a tiebreaker fixes that. "+
			"Note that a node attached as a diskless CLIENT does not vote — only a tiebreaker does. Naming a node "+
			"moves the tiebreaker there; an empty node removes it, which a two-replica resource has no majority "+
			"without — do that only to turn the node into a replica."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in tiebreakerIn) (*mcp.CallToolResult, opResult, error) {
			// The client returns the previous tiebreaker first and the
			// controller's message second.
			_, msg, err := s.client.SetTiebreaker(ctx, in.Resource, in.Node)
			if err != nil {
				return nil, opResult{}, err
			}
			if msg == "" {
				if in.Node == "" {
					msg = "tiebreaker removed from " + in.Resource
				} else {
					msg = in.Node + " is now the tiebreaker for " + in.Resource
				}
			}
			return nil, ok(msg), nil
		})

	addWrite(s, srv, writeTool("haify_resource_add_dr", "Add off-site DR replication",
		"Add an asynchronous off-site replica reached over a WAN tunnel. The DR copy replicates under protocol A "+
			"and never takes over automatically."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in addDRIn) (*mcp.CallToolResult, addDROut, error) {
			port, err := s.client.AddDR(ctx, in.Resource, in.DRNode, in.DREndpoint, in.WANPort, in.EgressAddress)
			if err != nil {
				return nil, addDROut{}, err
			}
			return nil, addDROut{Status: "ok", WANPort: port}, nil
		})

	addWrite(s, srv, writeTool("haify_wan_repair", "Repair WAN replication tunnels",
		"Reconcile a WAN resource's tunnels with the controller's current node list: re-provision the legs that "+
			"should exist, remove instances left behind by a node that was renumbered or removed. It converges, so "+
			"running it on a healthy resource does nothing. Always run with dry_run first — a repair restarts tunnels."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in wanRepairIn) (*mcp.CallToolResult, wanRepairOut, error) {
			resp, err := s.client.RepairWanProxy(ctx, in.Resource, in.DryRun)
			if err != nil {
				return nil, wanRepairOut{}, err
			}
			return nil, wanRepairOut{
				Status:            "ok",
				Message:           resp.Message,
				ExpectedLegs:      resp.ExpectedLegs,
				RemovedLegs:       resp.RemovedLegs,
				AlreadyConsistent: resp.AlreadyConsistent,
			}, nil
		})

	if c, supported := s.client.(interface {
		DRFailback(context.Context, string, string, uint32) (*haifypb.DRFailbackResponse, error)
	}); supported {
		addWrite(s, srv, destructiveTool("haify_resource_dr_failback", "Fail a WAN resource back to the primary site",
			"After a DR failover, move a WAN resource back to its primary site. Run it again until phase is done: it "+
				"rejoins the primary-site nodes, DISCARDING what they wrote after the failover, waits for them to resync "+
				"from the DR, then makes the primary site Primary. Needs each primary-site node, then the DR, unmounted."),
			func(ctx context.Context, _ *mcp.CallToolRequest, in drFailbackIn) (*mcp.CallToolResult, drFailbackOut, error) {
				resp, err := c.DRFailback(ctx, in.Resource, in.Node, in.WaitSeconds)
				if err != nil {
					return nil, drFailbackOut{}, err
				}
				return nil, drFailbackOut{OK: resp.Success, Phase: resp.Phase, Message: resp.Message, Steps: resp.Steps}, nil
			})
	}

	if c, supported := s.client.(interface {
		RepairResource(context.Context, string) error
	}); supported {
		addWrite(s, srv, writeTool("haify_resource_repair", "Repair a resource's DRBD config",
			"Bring every participant's copy of a resource's DRBD config back into agreement and apply it: a diskless "+
				"tiebreaker missing a volume, a node's address left over from a renumbering. It converges, so running it on a "+
				"healthy resource changes nothing. It rewrites /etc/drbd.d/<resource>.res on the nodes and runs drbdadm adjust."),
			func(ctx context.Context, _ *mcp.CallToolRequest, in resourceNameIn) (*mcp.CallToolResult, opResult, error) {
				if err := c.RepairResource(ctx, in.Name); err != nil {
					return nil, opResult{}, err
				}
				return nil, ok("config of " + in.Name + " reconciled"), nil
			})
	}

	if c, supported := s.client.(interface {
		VerifyResource(context.Context, *haifypb.VerifyResourceRequest) (*haifypb.VerifyResourceResponse, error)
	}); supported {
		addWrite(s, srv, writeTool("haify_resource_verify", "Verify a resource's replicas hold the same data",
			"Compare a resource's replicas block by block (DRBD online verify) from node, default the Primary. It reads "+
				"every replica in full, so it loads the disks while it runs. Call again while phase is running to follow "+
				"it. Marks left by earlier verifies, interrupted resyncs or reconnects count in out_of_sync_kib even when the copies "+
				"are identical; found_kib is what this verify itself found. Call with resync to copy node's data over the marked "+
				"blocks: harmless when the copies are identical, and it clears the marks."),
			func(ctx context.Context, _ *mcp.CallToolRequest, in verifyIn) (*mcp.CallToolResult, verifyOut, error) {
				resp, err := c.VerifyResource(ctx, &haifypb.VerifyResourceRequest{
					Name: in.Resource, Node: in.Node, WaitSeconds: in.WaitSeconds, Resync: in.Resync,
				})
				if err != nil {
					return nil, verifyOut{}, err
				}
				out := verifyOut{OK: resp.Success, Phase: resp.Phase, Source: resp.Source, Message: resp.Message, Steps: resp.Steps}
				for _, p := range resp.Peers {
					out.Peers = append(out.Peers, verifyPeerOut{Node: p.Node, State: p.State, OutOfSyncKiB: p.OutOfSyncKib, FoundKiB: p.FoundKib, BaselineKnown: p.BaselineKnown, PercentDone: p.PercentDone})
				}
				return nil, out, nil
			})
	}

	if c, supported := s.client.(interface {
		SetWanEndpoint(context.Context, *haifypb.SetWanEndpointRequest) (*haifypb.SetWanEndpointResponse, error)
	}); supported {
		addWrite(s, srv, writeTool("haify_wan_set_endpoint", "Change a WAN resource's DR endpoint",
			"Change the DR site's address a WAN resource's primary dials (IP or host name, no port) and/or the source "+
				"address it dials from, and rebuild the tunnels on it. A new DR endpoint that does not answer is refused "+
				"and the old one kept, unless skip_check is set for a DR site not reachable yet."),
			func(ctx context.Context, _ *mcp.CallToolRequest, in wanEndpointIn) (*mcp.CallToolResult, opResult, error) {
				resp, err := c.SetWanEndpoint(ctx, &haifypb.SetWanEndpointRequest{
					Name: in.Resource, DrEndpoint: in.DREndpoint, EgressAddress: in.EgressAddress,
					ClearEgress: in.ClearEgress, SkipReachabilityCheck: in.SkipCheck,
				})
				if err != nil {
					return nil, opResult{}, err
				}
				return nil, ok(resp.Message), nil
			})
	}

	addWrite(s, srv, destructiveTool("haify_node_drain", "Drain a node",
		"Mark the node maintenance (no new replicas or tiebreakers land on it until undrain) and move every "+
			"resource that is Primary on it. HA resources, gateways and haify-meta are evicted through drbd-reactor, as "+
			"haify_ha_evict does. Others are demoted and promoted on a replica that is diskful, UpToDate, connected, "+
			"and not drained, offline or a WAN DR node; if that promote fails the node is promoted back. A resource "+
			"that cannot move (no such replica, held open, eviction failed) stays Primary on the node and is named "+
			"in the error with the reason; the rest still move. Returns the resources it moved; undrain does not "+
			"move them back."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in nodeNameIn) (*mcp.CallToolResult, drainOut, error) {
			moved, err := s.client.DrainNode(ctx, in.Node)
			if err != nil {
				return nil, drainOut{}, err
			}
			return nil, drainOut{Status: "ok", Evacuated: moved}, nil
		})

	addWrite(s, srv, writeTool("haify_node_undrain", "Undrain a node",
		"Mark a drained node eligible to hold resources again. Does not move anything back on its own."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in nodeNameIn) (*mcp.CallToolResult, opResult, error) {
			if err := s.client.UndrainNode(ctx, in.Node); err != nil {
				return nil, opResult{}, err
			}
			return nil, ok("node " + in.Node + " is schedulable again"), nil
		})

	addWrite(s, srv, writeTool("haify_node_set_labels", "Set node labels",
		"Set key/value labels on a node. Labels drive placement — which nodes a resource's replicas may land on."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in nodeLabelsIn) (*mcp.CallToolResult, opResult, error) {
			if _, err := s.client.SetNodeLabels(ctx, in.Node, in.Labels, in.Replace); err != nil {
				return nil, opResult{}, err
			}
			verb := "merged into"
			if in.Replace {
				verb = "replaced on"
			}
			return nil, ok(fmt.Sprintf("%d label(s) %s %s", len(in.Labels), verb, in.Node)), nil
		})

	addWrite(s, srv, writeTool("haify_resource_set_labels", "Set resource labels",
		"Set key/value labels on a resource, merged into the ones it has; a key with an empty value is "+
			"removed. Integrations mark what runs on a resource this way (haify.libvirt/domain, "+
			"haify.pve/managed-by, haify.csi/managed-by, haify.openstack/managed-by)."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in resourceLabelsIn) (*mcp.CallToolResult, opResult, error) {
			after, err := s.client.SetResourceLabels(ctx, in.Resource, in.Labels, nil)
			if err != nil {
				return nil, opResult{}, err
			}
			return nil, ok(fmt.Sprintf("%s now has %d label(s)", in.Resource, len(after))), nil
		})

	if c, supported := s.client.(interface {
		SetNodeAddress(context.Context, string, string, string) (*haifypb.SetNodeAddressResponse, error)
		SetNodeAddresses(context.Context, []*haifypb.NodeAddressMove) (*haifypb.SetNodeAddressResponse, error)
	}); supported {
		addWrite(s, srv, destructiveTool("haify_node_set_address", "Renumber a node",
			"Move a registered node to a new IP address everywhere Haify records it: the node registry, /etc/hosts on the nodes, "+
				"and the DRBD config of every resource it takes part in, each of which reconnects on the new address. "+
				"The node must already answer on the new address as the same machine. When several nodes changed "+
				"address at once, pass them all in moves: one at a time cannot work then."),
			func(ctx context.Context, _ *mcp.CallToolRequest, in nodeAddressIn) (*mcp.CallToolResult, nodeAddressOut, error) {
				var resp *haifypb.SetNodeAddressResponse
				var err error
				if len(in.Moves) > 0 {
					moves := make([]*haifypb.NodeAddressMove, 0, len(in.Moves))
					for _, m := range in.Moves {
						moves = append(moves, &haifypb.NodeAddressMove{Node: m.Node, Address: m.Address, ReplicationAddress: m.ReplicationAddress})
					}
					resp, err = c.SetNodeAddresses(ctx, moves)
				} else {
					resp, err = c.SetNodeAddress(ctx, in.Node, in.Address, in.ReplicationAddress)
				}
				if err != nil {
					return nil, nodeAddressOut{}, err
				}
				return nil, nodeAddressOut{OK: resp.Success, Message: resp.Message, Resources: resp.Resources, Failed: resp.Failed}, nil
			})
	}

	addWrite(s, srv, destructiveTool("haify_pool_convert_thin", "Convert a pool to thin",
		"Rebuild an LVM volume group as a thin pool in place. A thick pool reserves a fixed copy-on-write area per "+
			"snapshot and so cannot hold a snapshot history. Every resource on the pool is rebuilt one copy at a "+
			"time, which reduces redundancy while it runs; it is refused when fewer than two diskful copies remain."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in convertThinIn) (*mcp.CallToolResult, opResult, error) {
			if err := s.client.ConvertPoolToThin(ctx, in.Node, in.Pool); err != nil {
				return nil, opResult{}, err
			}
			return nil, ok("pool " + in.Pool + " on " + in.Node + " converted to thin"), nil
		})

	addRead(s, srv, readOnlyTool("haify_ha_get_toml", "Read an HA promoter config",
		"Read the drbd-reactor promoter TOML for an HA resource — the file that decides what starts, in what order, "+
			"on whichever node holds the resource."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in haTomlIn) (*mcp.CallToolResult, haTomlOut, error) {
			resp, err := s.client.GetHaToml(ctx, in.Resource)
			if err != nil {
				return nil, haTomlOut{}, err
			}
			return nil, haTomlOut{Resource: in.Resource, Content: resp.Content, Path: resp.Path}, nil
		})

	addWrite(s, srv, destructiveTool("haify_ha_sync_toml", "Write an HA promoter config",
		"Write a promoter TOML to every node and reload drbd-reactor. A malformed file can stop the resource from "+
			"starting anywhere, so read the current one first and change only what is needed."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in haTomlSyncIn) (*mcp.CallToolResult, opResult, error) {
			msg, err := s.client.SyncHaToml(ctx, in.Resource, in.Content)
			if err != nil {
				return nil, opResult{}, err
			}
			if msg == "" {
				msg = "promoter config for " + in.Resource + " synced"
			}
			return nil, ok(msg), nil
		})

	addRead(s, srv, readOnlyTool("haify_ha_promoter_status", "Show promoter status",
		"Show drbd-reactor promoter state per HA resource (or of one gateway or app resource): the node running "+
			"its service chain, what each unit of the chain is and whether it is up, whether each standby could "+
			"take the chain over and why not, and which services outside the chain use its mounts — those hold "+
			"the unmount up on a failover."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in haStatusIn) (*mcp.CallToolResult, haStatusOut, error) {
			sts, err := s.client.GetHaStatus(ctx, in.Resource)
			if err != nil {
				return nil, haStatusOut{}, err
			}
			out := haStatusOut{Promoters: make([]haPromoterOut, 0, len(sts))}
			for _, p := range sts {
				out.Promoters = append(out.Promoters, haPromoterOutFrom(p))
			}
			return nil, out, nil
		})

	addRead(s, srv, readOnlyTool("haify_ocf_agent_list", "List OCF resource agents",
		"List the OCF resource agents installed on the cluster. These are what an HA promoter can start — "+
			"Filesystem, IPaddr2, nfsserver and so on."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, agentListOut, error) {
			agents, err := s.client.ListResourceAgents(ctx)
			if err != nil {
				return nil, agentListOut{}, err
			}
			out := agentListOut{Agents: make([]agentOut, 0, len(agents))}
			for _, a := range agents {
				out.Agents = append(out.Agents, agentOut{Provider: a.Provider, Name: a.Name, ShortDesc: a.Shortdesc})
			}
			return nil, out, nil
		})

	addRead(s, srv, readOnlyTool("haify_ocf_agent_metadata", "Describe an OCF agent",
		"Fetch an OCF agent's metadata, which lists every parameter it accepts. Read this before composing a "+
			"promoter start list, rather than guessing parameter names."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in agentMetaIn) (*mcp.CallToolResult, agentMetaOut, error) {
			if strings.TrimSpace(in.Name) == "" {
				return nil, agentMetaOut{}, fmt.Errorf("agent name is required")
			}
			resp, err := s.client.GetResourceAgentMetadata(ctx, in.Provider, in.Name)
			if err != nil {
				return nil, agentMetaOut{}, err
			}
			out := agentMetaOut{
				Provider:   resp.Provider,
				Name:       resp.Name,
				Version:    resp.Version,
				ShortDesc:  resp.Shortdesc,
				LongDesc:   resp.Longdesc,
				Parameters: make([]agentParamOut, 0, len(resp.Parameters)),
			}
			for _, p := range resp.Parameters {
				out.Parameters = append(out.Parameters, agentParamOut{
					Name:      p.Name,
					Required:  p.Required,
					Type:      p.Type,
					Default:   p.Default,
					ShortDesc: p.Shortdesc,
				})
			}
			return nil, out, nil
		})
}

type nodeAddressMoveIn struct {
	Node               string `json:"node" jsonschema:"registered node name"`
	Address            string `json:"address" jsonschema:"the node's new IP address"`
	ReplicationAddress string `json:"replication_address,omitempty" jsonschema:"move DRBD traffic to this address too"`
}

type nodeAddressIn struct {
	Moves              []nodeAddressMoveIn `json:"moves,omitempty" jsonschema:"several nodes to renumber together; instead of node/address"`
	Node               string              `json:"node,omitempty" jsonschema:"registered node name"`
	Address            string              `json:"address,omitempty" jsonschema:"the node's new IP address"`
	ReplicationAddress string              `json:"replication_address,omitempty" jsonschema:"move DRBD traffic to this address too; empty keeps the node's current arrangement"`
}

type nodeAddressOut struct {
	OK        bool     `json:"ok"`
	Message   string   `json:"message"`
	Resources []string `json:"resources"`
	Failed    []string `json:"failed,omitempty"`
}

type wanEndpointIn struct {
	Resource      string `json:"resource" jsonschema:"WAN resource name"`
	DREndpoint    string `json:"dr_endpoint,omitempty" jsonschema:"DR site's address the primary dials: IP or host name, no port; empty keeps it"`
	EgressAddress string `json:"egress_address,omitempty" jsonschema:"source address the primary dials from; empty keeps it"`
	ClearEgress   bool   `json:"clear_egress,omitempty" jsonschema:"let the routing table choose the source address again"`
	SkipCheck     bool   `json:"skip_check,omitempty" jsonschema:"save and provision even if the DR endpoint does not answer yet"`
}

type drFailbackIn struct {
	Resource    string `json:"resource" jsonschema:"WAN resource name"`
	Node        string `json:"node,omitempty" jsonschema:"primary-site node to make Primary; empty takes the first"`
	WaitSeconds uint32 `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the resync before returning"`
}

type drFailbackOut struct {
	OK      bool     `json:"ok"`
	Phase   string   `json:"phase"`
	Message string   `json:"message"`
	Steps   []string `json:"steps,omitempty"`
}

type verifyIn struct {
	Resource    string `json:"resource" jsonschema:"resource name"`
	Node        string `json:"node,omitempty" jsonschema:"node to verify from and whose data resync keeps; empty takes the Primary"`
	WaitSeconds uint32 `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the verify to finish"`
	Resync      bool   `json:"resync,omitempty" jsonschema:"copy node's data over the blocks a finished verify found different"`
}

type verifyPeerOut struct {
	Node          string  `json:"node"`
	State         string  `json:"state"`
	OutOfSyncKiB  uint64  `json:"out_of_sync_kib" jsonschema:"everything DRBD has marked out of sync with this peer: what this verify found plus marks that were already there"`
	FoundKiB      uint64  `json:"found_kib,omitempty" jsonschema:"the part of out_of_sync_kib this verify added; meaningful only when baseline_known"`
	BaselineKnown bool    `json:"baseline_known,omitempty"`
	PercentDone   float64 `json:"percent_done,omitempty"`
}

type verifyOut struct {
	OK      bool            `json:"ok"`
	Phase   string          `json:"phase"`
	Source  string          `json:"source"`
	Message string          `json:"message"`
	Peers   []verifyPeerOut `json:"peers,omitempty"`
	Steps   []string        `json:"steps,omitempty"`
}
