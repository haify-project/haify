// Package mcpserver exposes the Haify controller as a Model Context Protocol
// (MCP) server so AI assistants can inspect and manage storage through the
// same gRPC API used by haify.
//
// Tools are grouped by domain (cluster, resource, snapshot, gateway, HA) and
// carry MCP annotations: read-only tools are always registered, mutating
// tools are skipped entirely in read-only mode, and destructive tools are
// hinted so clients can require user confirmation.
//
// Read-only mode admits named exceptions via Options.AllowWrite, for the
// common shape of "let it see everything and do one thing". The exception is
// enforced by not registering the other tools at all, rather than by asking a
// client to hide them: a client-side filter constrains one client, while an
// unregistered tool cannot be called by anything that connects.
package mcpserver

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

const (
	// readTimeout bounds list/status calls.
	readTimeout = 30 * time.Second
	// writeTimeout bounds mutating calls; resource and gateway creation
	// run SSH commands on every node and can take minutes.
	writeTimeout = 5 * time.Minute
)

// Options configures the MCP server.
type Options struct {
	// ReadOnly registers only read-only tools (list/status/health).
	ReadOnly bool
	// AllowWrite names mutating tools to register despite ReadOnly, so a caller
	// can grant a few explicit exceptions — "everything you can look at, plus
	// evict" — without opening the whole write surface.
	//
	// Setting it implies ReadOnly. Granting exceptions to an open server would
	// mean nothing, and reading it as "additionally allow" on a server that
	// already allows everything is the kind of misconfiguration that is only
	// discovered by something being deleted.
	//
	// Enforcing this here rather than in the client matters: a client-side tool
	// filter hides a tool from the model, but the server still answers if
	// anything else connects to it. An unregistered tool cannot be called at
	// all.
	AllowWrite []string
	// NoDestructive registers the mutating tools that are not marked
	// destructive — create, grow, snapshot, start, mount — and leaves out
	// delete, restore, evict, drain and the like. It is the "operate" role of
	// the remote server: day-to-day work without the calls that lose data or
	// interrupt service.
	NoDestructive bool
	// Version reported in the MCP initialize handshake.
	Version string
}

// Server bridges MCP tool calls to the Haify controller client.
type Server struct {
	client        ControllerClient
	logger        *zap.Logger
	readOnly      bool
	noDestructive bool
	// allow names the write tools registered despite readOnly.
	allow map[string]bool
	// writeToolNames records every write tool the server knows how to offer,
	// registered or not, so an allowlist entry that matches nothing can be
	// reported instead of silently doing nothing.
	writeToolNames map[string]bool
	version        string
	// apps and k8s make this the haify-k8s server (see NewK8s).
	apps AppManager
	k8s  bool
}

// New creates a Server. The client is typically *client.HaifyClient; tests
// pass a mock. The logger must write to stderr only — stdout carries the
// MCP stdio protocol.
func New(c ControllerClient, logger *zap.Logger, opts Options) *Server {
	version := opts.Version
	if version == "" {
		version = "dev"
	}
	allow := make(map[string]bool, len(opts.AllowWrite))
	for _, name := range opts.AllowWrite {
		if name = strings.TrimSpace(name); name != "" {
			allow[name] = true
		}
	}
	return &Server{
		client:        c,
		logger:        logger,
		readOnly:      opts.ReadOnly || len(allow) > 0,
		noDestructive: opts.NoDestructive,
		allow:         allow,
		version:       version,
	}
}

// UnmatchedAllowed returns the AllowWrite names that no tool answers to.
//
// A typo in an allowlist is silent in the worst direction: the operator
// believes a tool is reachable, and finds out it is not at the moment they
// need it. Callers should treat a non-empty result as a configuration error
// rather than a warning.
//
// It must be called after MCPServer(), which is what records the names.
func (s *Server) UnmatchedAllowed() []string {
	var missing []string
	for name := range s.allow {
		if !s.writeToolNames[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// MCPServer builds the underlying MCP server with all tools registered.
func (s *Server) MCPServer() *mcp.Server {
	if s.k8s {
		return s.k8sMCPServer()
	}
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "haify",
		Title:   "Haify Software Defined Storage",
		Version: s.version,
	}, nil)
	s.registerClusterTools(srv)
	s.registerStorageTools(srv)
	s.registerNodeDiskTools(srv)
	s.registerResourceTools(srv)
	s.registerSnapshotTools(srv)
	s.registerGatewayTools(srv)
	s.registerHATools(srv)
	s.registerDBAppTools(srv)
	s.registerObservabilityTools(srv)
	s.registerDiagnoseTools(srv)
	s.registerInspectTools(srv)
	s.registerNotifyTools(srv)
	s.registerZFSTools(srv)
	s.registerTopologyTools(srv)
	s.registerDataLifecycleTools(srv)
	s.registerBackupScheduleTools(srv)
	s.registerReplicationTLSTools(srv)
	s.registerRunbooks(srv)
	return srv
}

// Run serves MCP over stdio until the client disconnects or ctx is done.
//
// It refuses to start on an allowlist entry that matches no tool. Starting
// anyway would serve a smaller tool set than the operator asked for and say
// nothing about it, and the gap would surface at the moment the tool was
// needed.
func (s *Server) Run(ctx context.Context) error {
	srv := s.MCPServer()
	if missing := s.UnmatchedAllowed(); len(missing) > 0 {
		return fmt.Errorf("allow: no such tool: %s", strings.Join(missing, ", "))
	}
	allowed := make([]string, 0, len(s.allow))
	for name := range s.allow {
		allowed = append(allowed, name)
	}
	sort.Strings(allowed)
	s.logger.Info("starting Haify MCP server on stdio",
		zap.Bool("read_only", s.readOnly),
		zap.Strings("allowed_write_tools", allowed),
		zap.String("version", s.version),
	)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// opResult is the structured output for mutating tools that return no data.
type opResult struct {
	Status string `json:"status" jsonschema:"operation outcome, always \"ok\" on success"`
	Detail string `json:"detail,omitempty" jsonschema:"human-readable summary of what was done"`
}

func ok(detail string) opResult {
	return opResult{Status: "ok", Detail: detail}
}

// Annotation helpers. Hints follow the MCP spec defaults: DestructiveHint
// defaults to true, so additive tools must set it to false explicitly.

func boolPtr(b bool) *bool { return &b }

func readOnlyTool(name, title, description string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Description: description,
		Annotations: &mcp.ToolAnnotations{
			Title:         title,
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}
}

// writeTool marks additive/idempotent mutations (create, start, mount).
func writeTool(name, title, description string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Description: description,
		Annotations: &mcp.ToolAnnotations{
			Title:           title,
			DestructiveHint: boolPtr(false),
			OpenWorldHint:   boolPtr(false),
		},
	}
}

// destructiveTool marks mutations that delete data or interrupt service
// (delete, restore, evict). Clients should ask the user before calling.
func destructiveTool(name, title, description string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Description: description,
		Annotations: &mcp.ToolAnnotations{
			Title:           title,
			DestructiveHint: boolPtr(true),
			OpenWorldHint:   boolPtr(false),
		},
	}
}

// addRead registers a read-only tool (available in every mode).
func addRead[In, Out any](s *Server, srv *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	mcp.AddTool(srv, t, instrument(s, t.Name, readTimeout, h))
}

// addReadWithin registers a read that is not a query.
//
// readTimeout is sized for a controller answering from memory. A tool that
// fans SSH out to every node is still read-only, and still cannot finish in
// thirty seconds on a cluster with a node that is down — where a timeout would
// throw away the answers from every node that did reply, which is exactly the
// case the tool exists for.
func addReadWithin[In, Out any](s *Server, srv *mcp.Server, t *mcp.Tool, d time.Duration, h mcp.ToolHandlerFor[In, Out]) {
	mcp.AddTool(srv, t, instrument(s, t.Name, d, h))
}

// addWrite registers a mutating tool unless the server is read-only and the
// tool is not named in the allowlist.
func addWrite[In, Out any](s *Server, srv *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	if s.writeToolNames == nil {
		s.writeToolNames = make(map[string]bool)
	}
	s.writeToolNames[t.Name] = true

	if s.readOnly && !s.allow[t.Name] {
		return
	}
	if s.noDestructive && t.Annotations != nil && t.Annotations.DestructiveHint != nil && *t.Annotations.DestructiveHint {
		return
	}
	mcp.AddTool(srv, t, instrument(s, t.Name, writeTimeout, h))
}

// instrument wraps a handler with a timeout and structured logging.
func instrument[In, Out any](s *Server, name string, timeout time.Duration, h mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		start := time.Now()
		res, out, err := h(ctx, req, in)
		fields := []zap.Field{zap.String("tool", name), zap.Duration("duration", time.Since(start))}
		// Over HTTP the caller is known: log who, so a change to the cluster
		// can be traced to a token. Arguments are not logged; some tools take
		// secrets (CHAP passwords), and the name of the tool says what was done.
		if req != nil && req.Extra != nil && req.Extra.TokenInfo != nil {
			extra := req.Extra.TokenInfo.Extra
			fields = append(fields, zap.Any("caller", extra["name"]), zap.Any("role", extra["role"]))
		}
		if err != nil {
			s.logger.Warn("tool call failed", append(fields, zap.Error(err))...)
		} else {
			s.logger.Info("tool call ok", fields...)
		}
		return res, out, err
	}
}
