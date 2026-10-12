package mcpserver

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/mcpauth"
)

type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.next.RoundTrip(r)
}

type remoteEnv struct {
	url   string
	store *mcpauth.Store
}

func startRemote(t *testing.T, maxRole mcpauth.Role, public string) *remoteEnv {
	t.Helper()
	store, err := mcpauth.Open(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	mock := &mockClient{listNodesFn: func(context.Context) ([]*haifypb.NodeInfo, error) {
		return []*haifypb.NodeInfo{{Name: "n1"}}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServeHTTP(ctx, mock, zap.NewNop(), Options{Version: "test"},
			HTTPOptions{Listen: addr, Tokens: store, MaxRole: maxRole, PublicURL: public})
	}()
	t.Cleanup(func() { cancel(); <-done })
	for range 100 {
		if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
			_ = resp.Body.Close()
			return &remoteEnv{url: "http://" + addr, store: store}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server did not start")
	return nil
}

func (e *remoteEnv) session(t *testing.T, token string) (*mcp.ClientSession, error) {
	t.Helper()
	cli := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	return cli.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:             e.url + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerTransport{token: token, next: http.DefaultTransport}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
}

func (e *remoteEnv) tools(t *testing.T, token string) map[string]*mcp.Tool {
	t.Helper()
	s, err := e.session(t, token)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return listTools(t, s)
}

// What a token can call is decided by which tools exist on its connection.
func TestRemoteRolesGetDifferentToolSets(t *testing.T) {
	e := startRemote(t, mcpauth.RoleAdmin, "")
	mk := func(name string, role mcpauth.Role) string {
		secret, _, err := e.store.Create(name, role, 0)
		if err != nil {
			t.Fatal(err)
		}
		return secret
	}
	read, operate, admin := mk("r", mcpauth.RoleRead), mk("o", mcpauth.RoleOperate), mk("a", mcpauth.RoleAdmin)

	rt := e.tools(t, read)
	if _, ok := rt["haify_node_list"]; !ok {
		t.Fatal("a read token must see the read tools")
	}
	if _, ok := rt["haify_pool_create"]; ok {
		t.Fatal("a read token was given a write tool")
	}

	ot := e.tools(t, operate)
	if _, ok := ot["haify_pool_create"]; !ok {
		t.Fatal("an operate token must see the everyday write tools")
	}
	if _, ok := ot["haify_pool_delete"]; ok {
		t.Fatal("an operate token was given a destructive tool")
	}
	if _, ok := ot["haify_resource_delete"]; ok {
		t.Fatal("an operate token was given a destructive tool")
	}

	at := e.tools(t, admin)
	for _, name := range []string{"haify_node_list", "haify_pool_create", "haify_pool_delete", "haify_resource_delete"} {
		if _, ok := at[name]; !ok {
			t.Fatalf("an admin token is missing %s", name)
		}
	}
	if len(rt) >= len(ot) || len(ot) >= len(at) {
		t.Fatalf("tool counts must grow with the role: read %d, operate %d, admin %d", len(rt), len(ot), len(at))
	}

	// A tool that does not exist on this connection cannot be called by name.
	s, err := e.session(t, read)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "haify_pool_delete"}); err == nil && (res == nil || !res.IsError) {
		t.Fatal("a read token deleted a pool by naming the tool")
	}
	res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "haify_node_list"})
	if err != nil || res.IsError {
		t.Fatalf("a read token could not call a read tool: %v %+v", err, res)
	}
}

func TestRemoteRefusesMissingBadAndRevokedTokens(t *testing.T) {
	e := startRemote(t, mcpauth.RoleAdmin, "https://mcp.example.test")

	// No token: 401, and the response says where the OAuth metadata is.
	resp, err := http.Post(e.url+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	if h := resp.Header.Get("WWW-Authenticate"); !strings.Contains(h, "https://mcp.example.test/.well-known/oauth-protected-resource") {
		t.Fatalf("WWW-Authenticate must point at the resource metadata, got %q", h)
	}

	if _, err := e.session(t, "haifymcp_not_a_token"); err == nil {
		t.Fatal("a bad token connected")
	}

	secret, _, _ := e.store.Create("temp", mcpauth.RoleAdmin, 0)
	s, err := e.session(t, secret)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.ListTools(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Revoke("temp"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListTools(t.Context(), nil); err == nil {
		t.Fatal("an open session kept working after its token was revoked")
	}

	// Metadata is public; the health check too.
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-authorization-server", "/healthz"} {
		r, err := http.Get(e.url + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		if r.StatusCode != 200 {
			t.Fatalf("%s: %d", p, r.StatusCode)
		}
	}
}

// A deployment can cap what any token may do, whatever the token says.
func TestRemoteMaxRoleCapsTokens(t *testing.T) {
	e := startRemote(t, mcpauth.RoleRead, "")
	secret, _, _ := e.store.Create("admin-but-capped", mcpauth.RoleAdmin, 0)
	tools := e.tools(t, secret)
	if _, ok := tools["haify_pool_create"]; ok {
		t.Fatal("--max-role read let an admin token write")
	}
}

func TestRemoteWithoutPublicURLHasNoOAuth(t *testing.T) {
	e := startRemote(t, mcpauth.RoleAdmin, "")
	r, err := http.Get(e.url + "/oauth/register")
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("OAuth endpoints exist without a public URL: %d", r.StatusCode)
	}
}

type repairingClient struct {
	*mockClient
	repaired []string
}

func (r *repairingClient) RepairResource(_ context.Context, name string) error {
	r.repaired = append(r.repaired, name)
	return nil
}

func TestRepairToolIsWriteAndRegisteredWhenTheClientCanRepair(t *testing.T) {
	rc := &repairingClient{mockClient: &mockClient{}}
	read := listTools(t, connect(t, rc, true))
	if _, ok := read["haify_resource_repair"]; ok {
		t.Fatal("a read-only server offers a repair")
	}
	all := connect(t, rc, false)
	tools := listTools(t, all)
	tool, ok := tools["haify_resource_repair"]
	if !ok || tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
		t.Fatalf("repair must exist and be marked non-destructive: %+v", tool)
	}
	res, err := all.CallTool(t.Context(), &mcp.CallToolParams{Name: "haify_resource_repair", Arguments: map[string]any{"name": "db"}})
	if err != nil || res.IsError || len(rc.repaired) != 1 || rc.repaired[0] != "db" {
		t.Fatalf("call: %v %+v repaired=%v", err, res, rc.repaired)
	}
}
