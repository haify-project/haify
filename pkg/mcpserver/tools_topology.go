package mcpserver

import (
	"context"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
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

// registerTopologyTools adds replica, DR and WAN tools, then the node
// lifecycle and HA-config ones.
func (s *Server) registerTopologyTools(srv *mcp.Server) {
	addWrite(s, srv, writeTool("haify_resource_add_replica", "Add a replica",
		"Add a full (diskful) copy of a running resource on another node. The new copy syncs in the background; "+
			"the resource stays usable throughout, and it is not a redundant copy until haify_resource_status shows "+
			"it UpToDate. The node's pool needs room for the whole volume. Refused if the node already holds a "+
			"replica, is the resource's quorum tiebreaker (remove it with haify_resource_set_tiebreaker first) or "+
			"is a diskless client (detach it first)."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in replicaIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_resource_add_replica: adding replica", zap.String("resource", in.Resource), zap.String("node", in.Node))
			if err := s.client.AddReplica(ctx, in.Resource, in.Node); err != nil {
				s.logger.Debug("haify_resource_add_replica: failed", zap.String("resource", in.Resource), zap.String("node", in.Node), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_resource_add_replica: done", zap.String("resource", in.Resource), zap.String("node", in.Node))
			return nil, ok("replica of " + in.Resource + " added on " + in.Node), nil
		})

	addWrite(s, srv, destructiveTool("haify_resource_remove_replica", "Remove a replica",
		"Take a full copy of a resource out permanently. Refused when the node is Primary, when it is the quorum "+
			"tiebreaker or the off-site DR, or when fewer than two diskful copies would remain. Unlike a conversion, "+
			"whose reduced-redundancy window closes when the resync finishes, this does not close — confirm before use."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in replicaIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_resource_remove_replica: removing replica", zap.String("resource", in.Resource), zap.String("node", in.Node))
			if err := s.client.RemoveReplica(ctx, in.Resource, in.Node); err != nil {
				s.logger.Debug("haify_resource_remove_replica: failed", zap.String("resource", in.Resource), zap.String("node", in.Node), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_resource_remove_replica: done", zap.String("resource", in.Resource), zap.String("node", in.Node))
			return nil, ok("replica of " + in.Resource + " removed from " + in.Node), nil
		})

	addWrite(s, srv, writeTool("haify_resource_attach_diskless", "Attach a diskless client",
		"Let a node use a resource over the DRBD network without storing a local copy. This is how a workload runs "+
			"on a node that holds no replica."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in replicaIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_resource_attach_diskless: attaching diskless client", zap.String("resource", in.Resource), zap.String("node", in.Node))
			if err := s.client.AttachDisklessClient(ctx, in.Resource, in.Node); err != nil {
				s.logger.Debug("haify_resource_attach_diskless: failed", zap.String("resource", in.Resource), zap.String("node", in.Node), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_resource_attach_diskless: done", zap.String("resource", in.Resource), zap.String("node", in.Node))
			return nil, ok(in.Node + " attached as a diskless client of " + in.Resource), nil
		})

	addWrite(s, srv, writeTool("haify_resource_detach_diskless", "Detach a diskless client",
		"Stop a node using a resource over the network. Does not touch any stored copy."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in replicaIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_resource_detach_diskless: detaching diskless client", zap.String("resource", in.Resource), zap.String("node", in.Node))
			if err := s.client.DetachDisklessClient(ctx, in.Resource, in.Node); err != nil {
				s.logger.Debug("haify_resource_detach_diskless: failed", zap.String("resource", in.Resource), zap.String("node", in.Node), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_resource_detach_diskless: done", zap.String("resource", in.Resource), zap.String("node", in.Node))
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
			s.logger.Debug("haify_resource_set_tiebreaker: setting tiebreaker", zap.String("resource", in.Resource),
				zap.String("node", in.Node), zap.Bool("remove", in.Node == ""))
			previous, msg, err := s.client.SetTiebreaker(ctx, in.Resource, in.Node)
			if err != nil {
				s.logger.Debug("haify_resource_set_tiebreaker: failed", zap.String("resource", in.Resource),
					zap.String("node", in.Node), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_resource_set_tiebreaker: done", zap.String("resource", in.Resource),
				zap.String("previous", previous), zap.String("node", in.Node))
			if msg == "" {
				s.logger.Debug("haify_resource_set_tiebreaker: controller gave no message, composing one")
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
			s.logger.Debug("haify_resource_add_dr: adding DR replica", zap.String("resource", in.Resource),
				zap.String("dr_node", in.DRNode), zap.String("dr_endpoint", in.DREndpoint), zap.Uint32("wan_port", in.WANPort),
				zap.String("egress_address", in.EgressAddress))
			port, err := s.client.AddDR(ctx, in.Resource, in.DRNode, in.DREndpoint, in.WANPort, in.EgressAddress)
			if err != nil {
				s.logger.Debug("haify_resource_add_dr: failed", zap.String("resource", in.Resource), zap.Error(err))
				return nil, addDROut{}, err
			}
			s.logger.Debug("haify_resource_add_dr: added", zap.String("resource", in.Resource), zap.Uint32("wan_port", port))
			return nil, addDROut{Status: "ok", WANPort: port}, nil
		})

	addWrite(s, srv, writeTool("haify_wan_repair", "Repair WAN replication tunnels",
		"Reconcile a WAN resource's tunnels with the controller's current node list: re-provision the legs that "+
			"should exist, remove instances left behind by a node that was renumbered or removed. It converges, so "+
			"running it on a healthy resource does nothing. Always run with dry_run first — a repair restarts tunnels."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in wanRepairIn) (*mcp.CallToolResult, wanRepairOut, error) {
			s.logger.Debug("haify_wan_repair: repairing WAN tunnels", zap.String("resource", in.Resource),
				zap.Bool("dry_run", in.DryRun))
			resp, err := s.client.RepairWanProxy(ctx, in.Resource, in.DryRun)
			if err != nil {
				s.logger.Debug("haify_wan_repair: failed", zap.String("resource", in.Resource), zap.Error(err))
				return nil, wanRepairOut{}, err
			}
			s.logger.Debug("haify_wan_repair: done", zap.String("resource", in.Resource), zap.Bool("dry_run", in.DryRun),
				zap.Strings("expected_legs", resp.ExpectedLegs), zap.Strings("removed_legs", resp.RemovedLegs),
				zap.Bool("already_consistent", resp.AlreadyConsistent))
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
				s.logger.Debug("haify_resource_dr_failback: failing back", zap.String("resource", in.Resource),
					zap.String("node", in.Node), zap.Uint32("wait_seconds", in.WaitSeconds))
				resp, err := c.DRFailback(ctx, in.Resource, in.Node, in.WaitSeconds)
				if err != nil {
					s.logger.Debug("haify_resource_dr_failback: failed", zap.String("resource", in.Resource), zap.Error(err))
					return nil, drFailbackOut{}, err
				}
				s.logger.Debug("haify_resource_dr_failback: step done", zap.String("resource", in.Resource),
					zap.Bool("success", resp.Success), zap.String("phase", resp.Phase), zap.Int("steps", len(resp.Steps)))
				return nil, drFailbackOut{OK: resp.Success, Phase: resp.Phase, Message: resp.Message, Steps: resp.Steps}, nil
			})
	} else {
		s.logger.Debug("Server.registerTopologyTools: client has no DR failback call, skipping")
	}

	if c, supported := s.client.(interface {
		RepairResource(context.Context, string) error
	}); supported {
		addWrite(s, srv, writeTool("haify_resource_repair", "Repair a resource's DRBD config",
			"Bring every participant's copy of a resource's DRBD config back into agreement and apply it: a diskless "+
				"tiebreaker missing a volume, a node's address left over from a renumbering. It converges, so running it on a "+
				"healthy resource changes nothing. It rewrites /etc/drbd.d/<resource>.res on the nodes and runs drbdadm adjust."),
			func(ctx context.Context, _ *mcp.CallToolRequest, in resourceNameIn) (*mcp.CallToolResult, opResult, error) {
				s.logger.Debug("haify_resource_repair: reconciling resource config", zap.String("resource", in.Name))
				if err := c.RepairResource(ctx, in.Name); err != nil {
					s.logger.Debug("haify_resource_repair: failed", zap.String("resource", in.Name), zap.Error(err))
					return nil, opResult{}, err
				}
				s.logger.Debug("haify_resource_repair: done", zap.String("resource", in.Name))
				return nil, ok("config of " + in.Name + " reconciled"), nil
			})
	} else {
		s.logger.Debug("Server.registerTopologyTools: client has no resource repair call, skipping")
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
				s.logger.Debug("haify_resource_verify: verifying", zap.String("resource", in.Resource), zap.String("node", in.Node),
					zap.Uint32("wait_seconds", in.WaitSeconds), zap.Bool("resync", in.Resync))
				resp, err := c.VerifyResource(ctx, &haifypb.VerifyResourceRequest{
					Name: in.Resource, Node: in.Node, WaitSeconds: in.WaitSeconds, Resync: in.Resync,
				})
				if err != nil {
					s.logger.Debug("haify_resource_verify: failed", zap.String("resource", in.Resource), zap.Error(err))
					return nil, verifyOut{}, err
				}
				s.logger.Debug("haify_resource_verify: answered", zap.String("resource", in.Resource), zap.Bool("success", resp.Success),
					zap.String("phase", resp.Phase), zap.String("source", resp.Source), zap.Int("peers", len(resp.Peers)))
				out := verifyOut{OK: resp.Success, Phase: resp.Phase, Source: resp.Source, Message: resp.Message, Steps: resp.Steps}
				for _, p := range resp.Peers {
					s.logger.Debug("haify_resource_verify: peer", zap.String("resource", in.Resource), zap.String("node", p.Node),
						zap.String("state", p.State), zap.Uint64("out_of_sync_kib", p.OutOfSyncKib), zap.Uint64("found_kib", p.FoundKib),
						zap.Float64("percent_done", p.PercentDone))
					out.Peers = append(out.Peers, verifyPeerOut{Node: p.Node, State: p.State, OutOfSyncKiB: p.OutOfSyncKib, FoundKiB: p.FoundKib, BaselineKnown: p.BaselineKnown, PercentDone: p.PercentDone})
				}
				return nil, out, nil
			})
	} else {
		s.logger.Debug("Server.registerTopologyTools: client has no verify call, skipping")
	}

	if c, supported := s.client.(interface {
		SetWanEndpoint(context.Context, *haifypb.SetWanEndpointRequest) (*haifypb.SetWanEndpointResponse, error)
	}); supported {
		addWrite(s, srv, writeTool("haify_wan_set_endpoint", "Change a WAN resource's DR endpoint",
			"Change the DR site's address a WAN resource's primary dials (IP or host name, no port) and/or the source "+
				"address it dials from, and rebuild the tunnels on it. A new DR endpoint that does not answer is refused "+
				"and the old one kept, unless skip_check is set for a DR site not reachable yet."),
			func(ctx context.Context, _ *mcp.CallToolRequest, in wanEndpointIn) (*mcp.CallToolResult, opResult, error) {
				s.logger.Debug("haify_wan_set_endpoint: changing endpoint", zap.String("resource", in.Resource),
					zap.String("dr_endpoint", in.DREndpoint), zap.String("egress_address", in.EgressAddress),
					zap.Bool("clear_egress", in.ClearEgress), zap.Bool("skip_check", in.SkipCheck))
				resp, err := c.SetWanEndpoint(ctx, &haifypb.SetWanEndpointRequest{
					Name: in.Resource, DrEndpoint: in.DREndpoint, EgressAddress: in.EgressAddress,
					ClearEgress: in.ClearEgress, SkipReachabilityCheck: in.SkipCheck,
				})
				if err != nil {
					s.logger.Debug("haify_wan_set_endpoint: failed", zap.String("resource", in.Resource), zap.Error(err))
					return nil, opResult{}, err
				}
				s.logger.Debug("haify_wan_set_endpoint: done", zap.String("resource", in.Resource))
				return nil, ok(resp.Message), nil
			})
	} else {
		s.logger.Debug("Server.registerTopologyTools: client has no WAN endpoint call, skipping")
	}

	s.registerNodeLifecycleTools(srv)
	s.registerHAConfigTools(srv)
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
