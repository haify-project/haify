package mcpserver

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/mcpauth"
)

// listenerPair is a server with both listeners up: the public one, capped by
// --max-role and running OAuth, and the admin one.
type listenerPair struct {
	public, admin *remoteEnv
	store         *mcpauth.Store
}

func startPair(t *testing.T, maxRole mcpauth.Role) *listenerPair {
	t.Helper()
	store, err := mcpauth.Open(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	free := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Close() }()
		return l.Addr().String()
	}
	capped, admin := free(), free()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServeHTTP(ctx, &mockClient{}, zap.NewNop(), Options{Version: "test"}, HTTPOptions{
			Listen: capped, AdminListen: admin, Tokens: store, MaxRole: maxRole, PublicURL: "https://mcp.example.com",
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	for _, a := range []string{capped, admin} {
		up := false
		for range 100 {
			if resp, err := http.Get("http://" + a + "/healthz"); err == nil {
				_ = resp.Body.Close()
				up = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !up {
			t.Fatalf("%s did not start", a)
		}
	}
	return &listenerPair{
		public: &remoteEnv{url: "http://" + capped, store: store},
		admin:  &remoteEnv{url: "http://" + admin, store: store},
		store:  store,
	}
}

// oauthAccess issues an access token the way the OAuth flow does, approved by
// a static token of the given role.
func (p *listenerPair) oauthAccess(t *testing.T, role mcpauth.Role) string {
	t.Helper()
	_, parent, err := p.store.Create("approver-"+string(role), role, 0)
	if err != nil {
		t.Fatal(err)
	}
	client, err := p.store.RegisterClient("chatgpt", []string{"https://chatgpt.com/cb"})
	if err != nil {
		t.Fatal(err)
	}
	access, _, _, err := p.store.IssueOAuth(parent, client, role, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return access
}

// post sends an MCP initialize with the token and returns the response.
func post(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"t"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

// The admin listener is for the local network: an admin token keeps every tool
// there while the capped listener still refuses to give it destructive ones,
// and the admin listener runs no OAuth.
func TestAdminListenerIsNotCappedAndHasNoOAuth(t *testing.T) {
	p := startPair(t, mcpauth.RoleOperate)
	secret, _, _ := p.store.Create("ops", mcpauth.RoleAdmin, 0)
	if _, ok := p.public.tools(t, secret)["haify_ha_evict"]; ok {
		t.Fatal("the capped listener exposed a destructive tool")
	}
	if _, ok := p.admin.tools(t, secret)["haify_ha_evict"]; !ok {
		t.Fatal("the admin listener did not give an admin token its destructive tools")
	}
	for _, path := range []string{
		"/oauth/register", "/oauth/authorize", "/oauth/token",
		"/.well-known/oauth-authorization-server",
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
	} {
		r, err := http.Get(p.admin.url + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("the admin listener serves %s: %d", path, r.StatusCode)
		}
	}
}

// A token a client got through OAuth works where OAuth runs and nowhere else:
// the admin listener takes only the tokens an operator made.
func TestAdminListenerRefusesOAuthTokens(t *testing.T) {
	p := startPair(t, mcpauth.RoleAdmin)
	access := p.oauthAccess(t, mcpauth.RoleAdmin)

	if _, ok := p.public.tools(t, access)["haify_ha_evict"]; !ok {
		t.Fatal("the public listener refused an OAuth access token")
	}
	if s, err := p.admin.session(t, access); err == nil {
		_ = s.Close()
		t.Fatal("the admin listener accepted an OAuth access token")
	}
	resp := post(t, p.admin.url, access)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("OAuth token on the admin listener: %d, want 401", resp.StatusCode)
	}
	if h := resp.Header.Get("WWW-Authenticate"); h != challengeRealm {
		t.Fatalf("WWW-Authenticate on the admin listener = %q, want %q", h, challengeRealm)
	}
	// No token at all gets the same answer there.
	resp = post(t, p.admin.url, "")
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != challengeRealm {
		t.Fatalf("no token on the admin listener: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	// The public listener still points a refused client at its OAuth metadata.
	resp = post(t, p.public.url, "")
	if h := resp.Header.Get("WWW-Authenticate"); resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(h, "https://mcp.example.com/.well-known/oauth-protected-resource") {
		t.Fatalf("no token on the public listener: %d %q", resp.StatusCode, h)
	}
	// An accepted request carries no challenge.
	if resp := post(t, p.public.url, access); resp.StatusCode != http.StatusOK || resp.Header.Get("WWW-Authenticate") != "" {
		t.Fatalf("OAuth token on the public listener: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
}

// An operator's token works on both listeners, with its own role: capped by
// --max-role on the public listener, not capped on the admin one.
func TestRoleTokensWorkOnBothListeners(t *testing.T) {
	p := startPair(t, mcpauth.RoleOperate)
	mk := func(name string, role mcpauth.Role) string {
		secret, _, err := p.store.Create(name, role, 0)
		if err != nil {
			t.Fatal(err)
		}
		return secret
	}
	read, operate, admin := mk("r", mcpauth.RoleRead), mk("o", mcpauth.RoleOperate), mk("a", mcpauth.RoleAdmin)

	for _, tc := range []struct {
		name   string
		env    *remoteEnv
		token  string
		has    []string
		hasNot []string
	}{
		{"read/public", p.public, read, []string{"haify_node_list"}, []string{"haify_pool_create", "haify_ha_evict"}},
		{"read/admin", p.admin, read, []string{"haify_node_list"}, []string{"haify_pool_create", "haify_ha_evict"}},
		{"operate/public", p.public, operate, []string{"haify_pool_create"}, []string{"haify_ha_evict"}},
		{"operate/admin", p.admin, operate, []string{"haify_pool_create"}, []string{"haify_ha_evict"}},
		{"admin/public", p.public, admin, []string{"haify_pool_create"}, []string{"haify_ha_evict", "haify_pool_delete"}},
		{"admin/admin", p.admin, admin, []string{"haify_pool_create", "haify_ha_evict", "haify_pool_delete"}, nil},
	} {
		tools := tc.env.tools(t, tc.token)
		for _, name := range tc.has {
			if _, ok := tools[name]; !ok {
				t.Errorf("%s: missing %s", tc.name, name)
			}
		}
		for _, name := range tc.hasNot {
			if _, ok := tools[name]; ok {
				t.Errorf("%s: was given %s", tc.name, name)
			}
		}
	}
}
