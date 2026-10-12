package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

type tlsStatusIn struct {
	Nodes []string `json:"nodes,omitempty" jsonschema:"nodes to check; empty checks every registered node"`
}

type tlsNodeOut struct {
	Node    string `json:"node"`
	Ready   bool   `json:"ready"`
	Problem string `json:"problem,omitempty" jsonschema:"what stops the node from carrying an encrypted connection"`
	Expires string `json:"cert_expires,omitempty"`
}

type tlsStatusOut struct {
	Nodes []tlsNodeOut `json:"nodes"`
}

type resourceTLSIn struct {
	Resource string `json:"resource"`
	Enabled  bool   `json:"enabled" jsonschema:"true to encrypt every connection, false to go back to plain TCP"`
}

// registerReplicationTLSTools adds the encrypted-replication tools when the
// client supports them. Node setup is not a tool: it installs a CA into each
// node's system trust store, which is an operator's decision.
func (s *Server) registerReplicationTLSTools(srv *mcp.Server) {
	c, supported := s.client.(interface {
		ReplicationTLSStatus(context.Context, []string) ([]*haifypb.NodeTLSInfo, error)
		SetResourceTLS(context.Context, string, bool) (string, error)
	})
	if !supported {
		s.logger.Debug("Server.registerReplicationTLSTools: client has no replication TLS calls, skipping")
		return
	}
	addRead(s, srv, readOnlyTool("haify_replication_tls_status", "Which nodes can carry encrypted replication",
		"For each node: whether it can run a DRBD connection over TLS (tlshd running with a certificate from this "+
			"controller's replication CA, CA trusted, tls module loaded) and, if not, what is missing."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in tlsStatusIn) (*mcp.CallToolResult, tlsStatusOut, error) {
			s.logger.Debug("haify_replication_tls_status: checking nodes", zap.Strings("nodes", in.Nodes))
			states, err := c.ReplicationTLSStatus(ctx, in.Nodes)
			if err != nil {
				s.logger.Debug("haify_replication_tls_status: check failed", zap.Strings("nodes", in.Nodes), zap.Error(err))
				return nil, tlsStatusOut{}, err
			}
			out := tlsStatusOut{Nodes: make([]tlsNodeOut, 0, len(states))}
			for _, st := range states {
				s.logger.Debug("haify_replication_tls_status: node", zap.String("node", st.Node), zap.Bool("ready", st.Ready),
					zap.String("problem", st.Problem), zap.String("cert_expires", st.Expires))
				out.Nodes = append(out.Nodes, tlsNodeOut{Node: st.Node, Ready: st.Ready, Problem: st.Problem, Expires: st.Expires})
			}
			s.logger.Debug("haify_replication_tls_status: checked", zap.Int("count", len(out.Nodes)))
			return nil, out, nil
		})
	addWrite(s, srv, writeTool("haify_resource_tls", "Encrypt a resource's replication, or stop",
		"Switch every DRBD connection of a resource to TLS or back. Each link is taken down and brought back on its "+
			"own while the others keep quorum, so the Primary keeps serving; a resync may follow on that link. Refused "+
			"unless every node of the resource is ready (haify_replication_tls_status) and for WAN resources, whose "+
			"off-site leg already runs mutual TLS. A failed handshake leaves that link StandAlone and stops the switch."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in resourceTLSIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_resource_tls: switching replication TLS", zap.String("resource", in.Resource),
				zap.Bool("enabled", in.Enabled))
			msg, err := c.SetResourceTLS(ctx, in.Resource, in.Enabled)
			if err != nil {
				s.logger.Debug("haify_resource_tls: switch failed", zap.String("resource", in.Resource),
					zap.Bool("enabled", in.Enabled), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_resource_tls: switched", zap.String("resource", in.Resource), zap.String("message", msg))
			return nil, ok(msg), nil
		})
}
