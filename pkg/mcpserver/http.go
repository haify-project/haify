package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/mcpauth"
)

// HTTPOptions configures the remote server.
type HTTPOptions struct {
	// Listen is the address to bind, e.g. ":43871".
	Listen string
	// PublicURL is where clients reach the server, e.g. https://mcp.example.com.
	// With it the server also runs the OAuth flow ChatGPT and claude.ai need;
	// without it only bearer tokens work, which is what Claude Code uses.
	PublicURL string
	// Tokens is the credential store.
	Tokens *mcpauth.Store
	// MaxRole caps every token: a deployment can decide that nothing reaching
	// it over the network may delete, whatever the token says. Empty is admin.
	MaxRole mcpauth.Role
	// TLSCert and TLSKey serve HTTPS directly. Leave both empty behind a
	// reverse proxy that terminates TLS.
	TLSCert, TLSKey string
	// TrustProxy takes the client address from X-Forwarded-For, for the
	// failure limiter. Only correct behind a proxy that sets it.
	TrustProxy bool
	// AdminListen, when set, opens a second listener for the local network
	// that is not capped by MaxRole: a token there gets the tools its own
	// role allows, admin included. It accepts only the tokens an operator
	// made with `haify-mcp token create` — not the access tokens the OAuth
	// flow hands to ChatGPT or claude.ai, and it runs no OAuth endpoints and
	// reads no proxy headers — so the internet-facing listener can stay
	// capped while an operator on the LAN keeps delete and evict. Never point
	// a reverse proxy at it.
	AdminListen string
}

// ServeHTTP serves MCP over streamable HTTP until ctx ends.
//
// Every request must carry a valid token. What a token may call is decided by
// which tool set the connection gets: a "read" token is given a server on
// which no mutating tool exists, an "operate" token one without the
// destructive ones, an "admin" token all of them. The tools are not filtered
// per call — an absent tool cannot be called, whatever the client sends.
func ServeHTTP(ctx context.Context, c ControllerClient, logger *zap.Logger, base Options, h HTTPOptions) error {
	logger.Debug("ServeHTTP: starting", zap.String("listen", h.Listen), zap.String("public_url", h.PublicURL),
		zap.String("max_role", string(h.MaxRole)), zap.Bool("has_token_store", h.Tokens != nil),
		zap.Bool("tls", h.TLSCert != ""), zap.Bool("trust_proxy", h.TrustProxy), zap.String("admin_listen", h.AdminListen))
	if h.Tokens == nil {
		logger.Debug("ServeHTTP: refusing to start without a token store")
		return errors.New("a token store is required")
	}
	if h.MaxRole == "" {
		logger.Debug("ServeHTTP: no max role given, capping at admin")
		h.MaxRole = mcpauth.RoleAdmin
	}
	servers := map[mcpauth.Role]*mcp.Server{}
	for role, opts := range map[mcpauth.Role]Options{
		mcpauth.RoleRead:    {ReadOnly: true},
		mcpauth.RoleOperate: {NoDestructive: true},
		mcpauth.RoleAdmin:   {},
	} {
		logger.Debug("ServeHTTP: building tool set for role", zap.String("role", string(role)),
			zap.Bool("read_only", opts.ReadOnly), zap.Bool("no_destructive", opts.NoDestructive))
		opts.Version = base.Version
		servers[role] = New(c, logger, opts).MCPServer()
	}

	limiter := mcpauth.NewLimiter(10, 5*time.Minute)
	primary := listener{addr: h.Listen, maxRole: h.MaxRole, publicURL: strings.TrimRight(h.PublicURL, "/"), trustProxy: h.TrustProxy}
	handler := newHandler(primary, h.Tokens, servers, limiter)

	primarySrv := newHTTPServer(h.Listen, handler)
	errc := make(chan error, 2)
	httpServers := []*http.Server{primarySrv}
	go func() {
		var err error
		if h.TLSCert != "" {
			err = primarySrv.ListenAndServeTLS(h.TLSCert, h.TLSKey)
		} else {
			err = primarySrv.ListenAndServe()
		}
		logger.Debug("ServeHTTP: primary listener stopped", zap.String("addr", h.Listen), zap.Error(err))
		errc <- err
	}()
	logger.Info("Haify MCP server listening",
		zap.String("addr", h.Listen), zap.Bool("tls", h.TLSCert != ""),
		zap.Bool("oauth", primary.publicURL != ""), zap.String("max_role", string(h.MaxRole)),
		zap.String("version", base.Version))

	if h.AdminListen != "" {
		admin := listener{addr: h.AdminListen, maxRole: mcpauth.RoleAdmin, operatorTokensOnly: true}
		adminSrv := newHTTPServer(h.AdminListen, newHandler(admin, h.Tokens, servers, limiter))
		httpServers = append(httpServers, adminSrv)
		go func() {
			var err error
			if h.TLSCert != "" {
				err = adminSrv.ListenAndServeTLS(h.TLSCert, h.TLSKey)
			} else {
				err = adminSrv.ListenAndServe()
			}
			logger.Debug("ServeHTTP: admin listener stopped", zap.String("addr", h.AdminListen), zap.Error(err))
			errc <- err
		}()
		logger.Info("Haify MCP admin listener (local network, not capped, operator tokens only)",
			zap.String("addr", h.AdminListen), zap.Bool("tls", h.TLSCert != ""))
	}

	select {
	case err := <-errc:
		logger.Debug("ServeHTTP: a listener ended, stopping", zap.Error(err))
		return err
	case <-ctx.Done():
		logger.Debug("ServeHTTP: context done, shutting listeners down", zap.Int("listeners", len(httpServers)))
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var first error
		for _, srv := range httpServers {
			start := time.Now()
			err := srv.Shutdown(sctx)
			logger.Debug("ServeHTTP: listener shut down", zap.String("addr", srv.Addr),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			if err != nil && first == nil {
				first = err
			}
		}
		return first
	}
}

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

// listener is one bound address and the ceiling of what it may do.
type listener struct {
	addr       string
	maxRole    mcpauth.Role
	publicURL  string
	trustProxy bool
	// operatorTokensOnly refuses OAuth access tokens. The admin listener is
	// not capped by --max-role, so it takes only tokens an operator created
	// and holds; a token a third-party client got through the OAuth flow on
	// the public listener is no credential here.
	operatorTokensOnly bool
}

// newHandler builds the routes for one listener: the token check, the tool
// set picked by the token's role (never above the listener's cap), and, when
// the listener has a public URL, the OAuth endpoints.
func newHandler(l listener, tokens *mcpauth.Store, servers map[mcpauth.Role]*mcp.Server, limiter *mcpauth.Limiter) http.Handler {
	log := zap.L()
	log.Debug("newHandler: building routes", zap.String("addr", l.addr), zap.String("max_role", string(l.maxRole)),
		zap.String("public_url", l.publicURL), zap.Bool("trust_proxy", l.trustProxy),
		zap.Bool("operator_tokens_only", l.operatorTokensOnly))
	verify := func(_ context.Context, secret string, r *http.Request) (*auth.TokenInfo, error) {
		ip := mcpauth.ClientIP(r, l.trustProxy)
		log.Debug("verifyBearer: checking token", zap.String("addr", l.addr), zap.String("client_ip", ip),
			zap.Bool("has_token", secret != ""), zap.String("path", r.URL.Path))
		if limiter.Blocked("bearer:" + ip) {
			log.Debug("verifyBearer: client is rate limited after failures", zap.String("client_ip", ip))
			return nil, auth.ErrInvalidToken
		}
		t, err := tokens.Verify(secret)
		if err == nil && l.operatorTokensOnly && t.Kind != mcpauth.KindStatic {
			log.Debug("verifyBearer: listener takes operator tokens only, refusing", zap.String("kind", t.Kind),
				zap.String("token_id", t.ID))
			err = mcpauth.ErrInvalid
		}
		if err != nil {
			log.Debug("verifyBearer: token refused, counting failure", zap.String("client_ip", ip), zap.Error(err))
			limiter.Fail("bearer:" + ip)
			return nil, auth.ErrInvalidToken
		}
		role := mcpauth.Min(t.Role, l.maxRole)
		log.Debug("verifyBearer: token accepted", zap.String("token_id", t.ID), zap.String("name", t.Name),
			zap.String("kind", t.Kind), zap.String("token_role", string(t.Role)), zap.String("role", string(role)),
			zap.Time("expires", t.Expires))
		return &auth.TokenInfo{
			Scopes:     role.Scopes(),
			Expiration: t.Expires,
			UserID:     t.ID,
			Extra:      map[string]any{"name": t.Name, "role": string(role)},
		}, nil
	}

	public := l.publicURL
	bearer := auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL:    publicOr(public, "/.well-known/oauth-protected-resource"),
		AllowMissingExpiration: true,
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		info := auth.TokenInfoFromContext(r.Context())
		if info == nil {
			log.Debug("newHandler: request has no token info, no tool set")
			return nil
		}
		role := mcpauth.Min(mcpauth.RoleFromScopes(info.Scopes), l.maxRole)
		log.Debug("newHandler: picked tool set", zap.String("role", string(role)), zap.String("token_id", info.UserID))
		return servers[role]
	}, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		log.Debug("healthz: answering", zap.String("addr", l.addr))
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/mcp", withChallenge(bearer, mcpHandler))
	if public != "" {
		log.Debug("newHandler: public URL set, mounting OAuth endpoints", zap.String("public_url", public))
		meta := auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
			Resource:               public + "/mcp",
			AuthorizationServers:   []string{public},
			ScopesSupported:        l.maxRole.Scopes(),
			BearerMethodsSupported: []string{"header"},
		})
		mux.Handle("/.well-known/oauth-protected-resource", meta)
		mux.Handle("/.well-known/oauth-protected-resource/mcp", meta)
		mcpauth.NewOAuth(mcpauth.OAuthConfig{
			Store: tokens, PublicURL: public, Limiter: limiter, TrustProxy: l.trustProxy, MaxRole: l.maxRole,
		}).Mount(mux)
	}
	log.Debug("newHandler: routes built", zap.String("addr", l.addr), zap.Bool("oauth", public != ""))
	return mux
}

// challengeRealm is the WWW-Authenticate challenge for a refused request on
// a listener that has no OAuth metadata to point at.
const challengeRealm = `Bearer realm="haify-mcp"`

// withChallenge puts the bearer check in front of next and makes every 401 it
// answers carry a WWW-Authenticate challenge, as RFC 6750 asks. The SDK writes
// one only when there is resource metadata to name, which a listener without
// a public URL — the admin listener among them — does not have. next gets the
// original ResponseWriter, so streaming is untouched.
func withChallenge(bearer func(http.Handler) http.Handler, next http.Handler) http.Handler {
	checked := bearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cw, ok := w.(*challengeWriter); ok {
			w = cw.ResponseWriter
		}
		next.ServeHTTP(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checked.ServeHTTP(&challengeWriter{ResponseWriter: w}, r)
	})
}

// challengeWriter adds the challenge to a 401 that goes out without one.
type challengeWriter struct {
	http.ResponseWriter
}

func (c *challengeWriter) WriteHeader(code int) {
	if code == http.StatusUnauthorized && c.Header().Get("WWW-Authenticate") == "" {
		zap.L().Debug("challengeWriter.WriteHeader: 401 without a challenge, adding the realm")
		c.Header().Set("WWW-Authenticate", challengeRealm)
	}
	c.ResponseWriter.WriteHeader(code)
}

func publicOr(public, path string) string {
	if public == "" {
		return ""
	}
	return fmt.Sprintf("%s%s", public, path)
}
