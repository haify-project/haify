package mcpserver

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// Node lifecycle tools: draining a node, its labels and a resource's, moving
// a node to a new address, and rebuilding a pool as thin.

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

// registerNodeLifecycleTools adds drain, label, renumber and pool-conversion
// tools.
func (s *Server) registerNodeLifecycleTools(srv *mcp.Server) {
	addWrite(s, srv, destructiveTool("haify_node_drain", "Drain a node",
		"Mark the node maintenance (no new replicas or tiebreakers land on it until undrain) and move every "+
			"resource that is Primary on it. HA resources, gateways and haify-meta are evicted through drbd-reactor, as "+
			"haify_ha_evict does. Others are demoted and promoted on a replica that is diskful, UpToDate, connected, "+
			"and not drained, offline or a WAN DR node; if that promote fails the node is promoted back. A resource "+
			"that cannot move (no such replica, held open, eviction failed) stays Primary on the node and is named "+
			"in the error with the reason; the rest still move. Returns the resources it moved; undrain does not "+
			"move them back."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in nodeNameIn) (*mcp.CallToolResult, drainOut, error) {
			s.logger.Debug("haify_node_drain: draining node", zap.String("node", in.Node))
			moved, err := s.client.DrainNode(ctx, in.Node)
			if err != nil {
				s.logger.Debug("haify_node_drain: failed", zap.String("node", in.Node), zap.Error(err))
				return nil, drainOut{}, err
			}
			s.logger.Debug("haify_node_drain: drained", zap.String("node", in.Node), zap.Strings("evacuated", moved))
			return nil, drainOut{Status: "ok", Evacuated: moved}, nil
		})

	addWrite(s, srv, writeTool("haify_node_undrain", "Undrain a node",
		"Mark a drained node eligible to hold resources again. Does not move anything back on its own."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in nodeNameIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_node_undrain: undraining node", zap.String("node", in.Node))
			if err := s.client.UndrainNode(ctx, in.Node); err != nil {
				s.logger.Debug("haify_node_undrain: failed", zap.String("node", in.Node), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_node_undrain: done", zap.String("node", in.Node))
			return nil, ok("node " + in.Node + " is schedulable again"), nil
		})

	addWrite(s, srv, writeTool("haify_node_set_labels", "Set node labels",
		"Set key/value labels on a node. Labels drive placement — which nodes a resource's replicas may land on."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in nodeLabelsIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_node_set_labels: setting labels", zap.String("node", in.Node),
				zap.Array("label_keys", keysOf(in.Labels)), zap.Bool("replace", in.Replace))
			if _, err := s.client.SetNodeLabels(ctx, in.Node, in.Labels, in.Replace); err != nil {
				s.logger.Debug("haify_node_set_labels: failed", zap.String("node", in.Node), zap.Error(err))
				return nil, opResult{}, err
			}
			verb := "merged into"
			if in.Replace {
				s.logger.Debug("haify_node_set_labels: labels replaced", zap.String("node", in.Node))
				verb = "replaced on"
			} else {
				s.logger.Debug("haify_node_set_labels: labels merged", zap.String("node", in.Node))
			}
			return nil, ok(fmt.Sprintf("%d label(s) %s %s", len(in.Labels), verb, in.Node)), nil
		})

	addWrite(s, srv, writeTool("haify_resource_set_labels", "Set resource labels",
		"Set key/value labels on a resource, merged into the ones it has; a key with an empty value is "+
			"removed. Integrations mark what runs on a resource this way (haify.libvirt/domain, "+
			"haify.pve/managed-by, haify.csi/managed-by, haify.openstack/managed-by)."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in resourceLabelsIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_resource_set_labels: setting labels", zap.String("resource", in.Resource),
				zap.Array("label_keys", keysOf(in.Labels)))
			after, err := s.client.SetResourceLabels(ctx, in.Resource, in.Labels, nil)
			if err != nil {
				s.logger.Debug("haify_resource_set_labels: failed", zap.String("resource", in.Resource), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_resource_set_labels: done", zap.String("resource", in.Resource), zap.Array("label_keys", keysOf(after)))
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
				s.logger.Debug("haify_node_set_address: renumbering", zap.String("node", in.Node), zap.String("address", in.Address),
					zap.String("replication_address", in.ReplicationAddress), zap.Int("moves", len(in.Moves)))
				var resp *haifypb.SetNodeAddressResponse
				var err error
				if len(in.Moves) > 0 {
					s.logger.Debug("haify_node_set_address: moving several nodes together", zap.Int("moves", len(in.Moves)))
					moves := make([]*haifypb.NodeAddressMove, 0, len(in.Moves))
					for _, m := range in.Moves {
						s.logger.Debug("haify_node_set_address: move", zap.String("node", m.Node), zap.String("address", m.Address),
							zap.String("replication_address", m.ReplicationAddress))
						moves = append(moves, &haifypb.NodeAddressMove{Node: m.Node, Address: m.Address, ReplicationAddress: m.ReplicationAddress})
					}
					resp, err = c.SetNodeAddresses(ctx, moves)
				} else {
					s.logger.Debug("haify_node_set_address: moving one node", zap.String("node", in.Node))
					resp, err = c.SetNodeAddress(ctx, in.Node, in.Address, in.ReplicationAddress)
				}
				if err != nil {
					s.logger.Debug("haify_node_set_address: failed", zap.String("node", in.Node), zap.Error(err))
					return nil, nodeAddressOut{}, err
				}
				s.logger.Debug("haify_node_set_address: done", zap.Bool("success", resp.Success),
					zap.Strings("resources", resp.Resources), zap.Strings("failed", resp.Failed))
				return nil, nodeAddressOut{OK: resp.Success, Message: resp.Message, Resources: resp.Resources, Failed: resp.Failed}, nil
			})
	} else {
		s.logger.Debug("Server.registerNodeLifecycleTools: client cannot renumber nodes, skipping")
	}

	addWrite(s, srv, destructiveTool("haify_pool_convert_thin", "Convert a pool to thin",
		"Rebuild an LVM volume group as a thin pool in place. A thick pool reserves a fixed copy-on-write area per "+
			"snapshot and so cannot hold a snapshot history. Every resource on the pool is rebuilt one copy at a "+
			"time, which reduces redundancy while it runs; it is refused when fewer than two diskful copies remain."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in convertThinIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_pool_convert_thin: converting pool to thin", zap.String("node", in.Node), zap.String("pool", in.Pool))
			if err := s.client.ConvertPoolToThin(ctx, in.Node, in.Pool); err != nil {
				s.logger.Debug("haify_pool_convert_thin: failed", zap.String("node", in.Node), zap.String("pool", in.Pool), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_pool_convert_thin: done", zap.String("node", in.Node), zap.String("pool", in.Pool))
			return nil, ok("pool " + in.Pool + " on " + in.Node + " converted to thin"), nil
		})
}
