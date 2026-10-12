package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// HA config tools: the promoter TOML of an HA resource, its live status, and
// the OCF agents a promoter can start.

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

// registerHAConfigTools adds the promoter config, promoter status and OCF
// agent tools.
func (s *Server) registerHAConfigTools(srv *mcp.Server) {
	addRead(s, srv, readOnlyTool("haify_ha_get_toml", "Read an HA promoter config",
		"Read the drbd-reactor promoter TOML for an HA resource — the file that decides what starts, in what order, "+
			"on whichever node holds the resource."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in haTomlIn) (*mcp.CallToolResult, haTomlOut, error) {
			s.logger.Debug("haify_ha_get_toml: reading promoter config", zap.String("resource", in.Resource))
			resp, err := s.client.GetHaToml(ctx, in.Resource)
			if err != nil {
				s.logger.Debug("haify_ha_get_toml: read failed", zap.String("resource", in.Resource), zap.Error(err))
				return nil, haTomlOut{}, err
			}
			// The TOML can carry agent parameters such as CHAP credentials: only
			// its size is traced.
			s.logger.Debug("haify_ha_get_toml: read", zap.String("resource", in.Resource), zap.String("path", resp.Path),
				zap.Int("content_bytes", len(resp.Content)))
			return nil, haTomlOut{Resource: in.Resource, Content: resp.Content, Path: resp.Path}, nil
		})

	addWrite(s, srv, destructiveTool("haify_ha_sync_toml", "Write an HA promoter config",
		"Write a promoter TOML to every node and reload drbd-reactor. A malformed file can stop the resource from "+
			"starting anywhere, so read the current one first and change only what is needed."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in haTomlSyncIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_ha_sync_toml: writing promoter config", zap.String("resource", in.Resource),
				zap.Int("content_bytes", len(in.Content)))
			msg, err := s.client.SyncHaToml(ctx, in.Resource, in.Content)
			if err != nil {
				s.logger.Debug("haify_ha_sync_toml: write failed", zap.String("resource", in.Resource), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_ha_sync_toml: written", zap.String("resource", in.Resource))
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
			s.logger.Debug("haify_ha_promoter_status: reading promoter status", zap.String("resource", in.Resource))
			sts, err := s.client.GetHaStatus(ctx, in.Resource)
			if err != nil {
				s.logger.Debug("haify_ha_promoter_status: read failed", zap.String("resource", in.Resource), zap.Error(err))
				return nil, haStatusOut{}, err
			}
			out := haStatusOut{Promoters: make([]haPromoterOut, 0, len(sts))}
			for _, p := range sts {
				out.Promoters = append(out.Promoters, haPromoterOutFrom(p))
			}
			s.logger.Debug("haify_ha_promoter_status: read", zap.String("resource", in.Resource), zap.Int("count", len(out.Promoters)))
			return nil, out, nil
		})

	addRead(s, srv, readOnlyTool("haify_ocf_agent_list", "List OCF resource agents",
		"List the OCF resource agents installed on the cluster. These are what an HA promoter can start — "+
			"Filesystem, IPaddr2, nfsserver and so on."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, agentListOut, error) {
			s.logger.Debug("haify_ocf_agent_list: listing agents")
			agents, err := s.client.ListResourceAgents(ctx)
			if err != nil {
				s.logger.Debug("haify_ocf_agent_list: list failed", zap.Error(err))
				return nil, agentListOut{}, err
			}
			out := agentListOut{Agents: make([]agentOut, 0, len(agents))}
			for _, a := range agents {
				s.logger.Debug("haify_ocf_agent_list: agent", zap.String("provider", a.Provider), zap.String("name", a.Name))
				out.Agents = append(out.Agents, agentOut{Provider: a.Provider, Name: a.Name, ShortDesc: a.Shortdesc})
			}
			s.logger.Debug("haify_ocf_agent_list: listed", zap.Int("count", len(out.Agents)))
			return nil, out, nil
		})

	addRead(s, srv, readOnlyTool("haify_ocf_agent_metadata", "Describe an OCF agent",
		"Fetch an OCF agent's metadata, which lists every parameter it accepts. Read this before composing a "+
			"promoter start list, rather than guessing parameter names."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in agentMetaIn) (*mcp.CallToolResult, agentMetaOut, error) {
			s.logger.Debug("haify_ocf_agent_metadata: reading agent metadata", zap.String("provider", in.Provider),
				zap.String("name", in.Name))
			if strings.TrimSpace(in.Name) == "" {
				s.logger.Debug("haify_ocf_agent_metadata: refusing, no agent name given")
				return nil, agentMetaOut{}, fmt.Errorf("agent name is required")
			}
			resp, err := s.client.GetResourceAgentMetadata(ctx, in.Provider, in.Name)
			if err != nil {
				s.logger.Debug("haify_ocf_agent_metadata: read failed", zap.String("provider", in.Provider),
					zap.String("name", in.Name), zap.Error(err))
				return nil, agentMetaOut{}, err
			}
			s.logger.Debug("haify_ocf_agent_metadata: read", zap.String("provider", resp.Provider), zap.String("name", resp.Name),
				zap.String("version", resp.Version), zap.Int("parameters", len(resp.Parameters)))
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
