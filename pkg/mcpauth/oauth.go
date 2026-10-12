package mcpauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// The authorization server for clients that cannot be given a token by hand.
//
// ChatGPT and claude.ai add an MCP server by URL and then run OAuth against
// it: they discover the endpoints below, register themselves, and send the
// user to /oauth/authorize. There the operator pastes a token made with
// `haify-mcp token create`, which is how "the human approves this client" is
// expressed without a second kind of credential to look after. The client is
// then given an access token of the same role, or lower if it asked for less,
// that lives an hour and is renewed with a refresh token for as long as the
// approving token stands.
//
// Public clients with PKCE (S256) only: there is no client secret to leak, and
// a stolen authorization code is useless without the verifier.

const (
	authCodeTTL     = 5 * time.Minute
	defaultAccess   = time.Hour
	defaultRefresh  = 30 * 24 * time.Hour
	maxAuthCodes    = 1000
	maxRedirectURIs = 10
)

// OAuthConfig configures the authorization server.
type OAuthConfig struct {
	Store *Store
	// PublicURL is where clients reach this server, without a trailing slash:
	// https://mcp.example.com. It is the issuer, and the base of every URL in
	// the metadata.
	PublicURL string
	// Limiter counts bad tokens pasted into the authorization page.
	Limiter    *Limiter
	TrustProxy bool
	// AccessTTL and RefreshTTL default to an hour and thirty days.
	AccessTTL, RefreshTTL time.Duration
	// MaxRole caps what any client can be granted, whatever the approving
	// token allows.
	MaxRole Role
}

// OAuth serves the endpoints.
type OAuth struct {
	cfg OAuthConfig

	mu    sync.Mutex
	codes map[string]authCode
}

type authCode struct {
	client    string
	redirect  string
	challenge string
	parent    Token
	role      Role
	expires   time.Time
}

// NewOAuth builds the server.
func NewOAuth(cfg OAuthConfig) *OAuth {
	log := zap.L()
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	if cfg.AccessTTL <= 0 {
		log.Debug("NewOAuth: no access TTL given, using default", zap.Duration("access_ttl", defaultAccess))
		cfg.AccessTTL = defaultAccess
	}
	if cfg.RefreshTTL <= 0 {
		log.Debug("NewOAuth: no refresh TTL given, using default", zap.Duration("refresh_ttl", defaultRefresh))
		cfg.RefreshTTL = defaultRefresh
	}
	if cfg.MaxRole == "" {
		log.Debug("NewOAuth: no role cap given, clients may reach admin")
		cfg.MaxRole = RoleAdmin
	}
	if cfg.Limiter == nil {
		log.Debug("NewOAuth: no limiter given, using the default one")
		cfg.Limiter = NewLimiter(10, 5*time.Minute)
	}
	log.Debug("NewOAuth: authorization server configured", zap.String("public_url", cfg.PublicURL),
		zap.Duration("access_ttl", cfg.AccessTTL), zap.Duration("refresh_ttl", cfg.RefreshTTL),
		zap.String("max_role", string(cfg.MaxRole)), zap.Bool("trust_proxy", cfg.TrustProxy))
	return &OAuth{cfg: cfg, codes: map[string]authCode{}}
}

// Mount registers the OAuth endpoints on mux.
func (o *OAuth) Mount(mux *http.ServeMux) {
	zap.L().Debug("OAuth.Mount: registering OAuth endpoints", zap.String("public_url", o.cfg.PublicURL))
	mux.HandleFunc("/.well-known/oauth-authorization-server", o.metadata)
	mux.HandleFunc("/oauth/register", o.register)
	mux.HandleFunc("/oauth/authorize", o.authorize)
	mux.HandleFunc("/oauth/token", o.token)
}

func (o *OAuth) scopes() []string { return o.cfg.MaxRole.Scopes() }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		zap.L().Debug("writeJSON: writing the response failed", zap.Int("status", status), zap.Error(err))
	}
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	zap.L().Debug("oauthError: answering with an OAuth error", zap.Int("status", status), zap.String("oauth_error", code),
		zap.String("description", desc))
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func (o *OAuth) metadata(w http.ResponseWriter, _ *http.Request) {
	zap.L().Debug("OAuth.metadata: serving authorization server metadata", zap.String("issuer", o.cfg.PublicURL))
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                o.cfg.PublicURL,
		"authorization_endpoint":                o.cfg.PublicURL + "/oauth/authorize",
		"token_endpoint":                        o.cfg.PublicURL + "/oauth/token",
		"registration_endpoint":                 o.cfg.PublicURL + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      o.scopes(),
	})
}

// validRedirect accepts https, and http only to a loopback address, which is
// where a desktop client listens for the redirect.
func validRedirect(raw string) bool {
	log := zap.L()
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" {
		log.Debug("validRedirect: rejected, unparseable, no host or has a fragment", zap.Bool("parse_error", err != nil))
		return false
	}
	switch u.Scheme {
	case "https":
		log.Debug("validRedirect: accepted https redirect", zap.String("redirect_host", u.Host))
		return true
	case "http":
		h := u.Hostname()
		loopback := h == "localhost" || h == "127.0.0.1" || h == "::1"
		log.Debug("validRedirect: http redirect, accepted only to loopback", zap.String("redirect_host", u.Host),
			zap.Bool("loopback", loopback))
		return loopback
	}
	log.Debug("validRedirect: rejected scheme", zap.String("scheme", u.Scheme), zap.String("redirect_host", u.Host))
	return false
}

func (o *OAuth) register(w http.ResponseWriter, r *http.Request) {
	log := zap.L()
	log.Debug("OAuth.register: client registration request", zap.String("method", r.Method),
		zap.String("remote_addr", r.RemoteAddr))
	if r.Method != http.MethodPost {
		log.Debug("OAuth.register: refused, not POST", zap.String("method", r.Method))
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST only")
		return
	}
	ip := ClientIP(r, o.cfg.TrustProxy)
	if o.cfg.Limiter.Blocked("register:" + ip) {
		log.Debug("OAuth.register: refused, address rate limited", zap.String("client_ip", ip))
		oauthError(w, http.StatusTooManyRequests, "slow_down", "too many registrations")
		return
	}
	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		log.Debug("OAuth.register: refused, body is not JSON", zap.String("client_ip", ip), zap.Error(err))
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "body is not JSON")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > maxRedirectURIs {
		log.Debug("OAuth.register: refused, redirect_uris count out of range",
			zap.Int("redirect_uris", len(req.RedirectURIs)), zap.Int("max", maxRedirectURIs))
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "give between 1 and 10 redirect_uris")
		return
	}
	for i, u := range req.RedirectURIs {
		if !validRedirect(u) {
			log.Debug("OAuth.register: refused, invalid redirect_uri", zap.Int("index", i))
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", fmt.Sprintf("%q must be https, or http to localhost", u))
			return
		}
	}
	name := strings.TrimSpace(req.ClientName)
	if name == "" {
		log.Debug("OAuth.register: client gave no name")
		name = "unnamed client"
	}
	if len(name) > 80 {
		log.Debug("OAuth.register: client name truncated", zap.Int("length", len(name)))
		name = name[:80]
	}
	// Every registration counts against the address, so one host cannot fill
	// the client list; a normal client registers once.
	o.cfg.Limiter.Fail("register:" + ip)
	start := time.Now()
	c, err := o.cfg.Store.RegisterClient(name, req.RedirectURIs)
	if err != nil {
		log.Debug("OAuth.register: storing the client failed", zap.Error(err), zap.Duration("took", time.Since(start)))
		oauthError(w, http.StatusInternalServerError, "server_error", "could not store the client")
		return
	}
	log.Debug("OAuth.register: client registered", zap.String("client_id", c.ID), zap.String("client_name", c.Name),
		zap.Int("redirect_uris", len(c.RedirectURIs)), zap.String("client_ip", ip), zap.Duration("took", time.Since(start)))
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  c.ID,
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

var authorizePage = template.Must(template.New("authorize").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Authorize {{.Client}}</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;max-width:32rem;margin:12vh auto;padding:0 1rem;color:#1c1c1e}
h1{font-size:1.25rem;margin:0 0 .5rem}p{margin:.5rem 0}
input[type=password]{width:100%;box-sizing:border-box;padding:.6rem;font:inherit;border:1px solid #888;border-radius:6px}
button{margin-top:.8rem;padding:.55rem 1.1rem;font:inherit;border:0;border-radius:6px;background:#0a5;color:#fff;cursor:pointer}
.err{color:#b00020}.dim{color:#666;font-size:.9rem}
@media(prefers-color-scheme:dark){body{background:#111;color:#eee}input{background:#222;color:#eee}.dim{color:#aaa}}
</style></head><body>
<h1>Authorize {{.Client}}</h1>
<p>{{.Client}} wants to use the Haify cluster at {{.Host}}.</p>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<form method="post">
{{range $k, $v := .Hidden}}<input type="hidden" name="{{$k}}" value="{{$v}}">{{end}}
<label for="t">Access token</label>
<input id="t" name="token" type="password" autocomplete="off" autofocus required>
<p class="dim">Paste a token made with <code>haify-mcp token create</code>. {{.Client}} gets the same role as that token{{if .Capped}}, at most <b>{{.Capped}}</b>{{end}}, and loses access when the token is revoked.</p>
<button type="submit">Authorize</button>
</form></body></html>`))

func (o *OAuth) page(w http.ResponseWriter, status int, client Client, hidden map[string]string, errMsg string) {
	zap.L().Debug("OAuth.page: rendering authorization page", zap.Int("status", status),
		zap.String("client_id", client.ID), zap.String("page_error", errMsg))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(status)
	host := strings.TrimPrefix(strings.TrimPrefix(o.cfg.PublicURL, "https://"), "http://")
	capped := ""
	if o.cfg.MaxRole != RoleAdmin {
		capped = string(o.cfg.MaxRole)
	}
	if err := authorizePage.Execute(w, map[string]any{
		"Client": client.Name, "Host": host, "Hidden": hidden, "Error": errMsg, "Capped": capped,
	}); err != nil {
		zap.L().Debug("OAuth.page: writing the page failed", zap.Error(err))
	}
}

func plainError(w http.ResponseWriter, status int, msg string) {
	zap.L().Debug("plainError: answering with a plain error", zap.Int("status", status), zap.String("message", msg))
	http.Error(w, msg, status)
}

func (o *OAuth) authorize(w http.ResponseWriter, r *http.Request) {
	log := zap.L()
	if err := r.ParseForm(); err != nil {
		log.Debug("OAuth.authorize: refused, form does not parse", zap.Error(err))
		plainError(w, http.StatusBadRequest, "bad request")
		return
	}
	get := func(k string) string { return r.Form.Get(k) }
	// The form carries the pasted token and the PKCE challenge: only their
	// presence is logged.
	log.Debug("OAuth.authorize: authorization request", zap.String("method", r.Method),
		zap.String("client_id", get("client_id")), zap.String("response_type", get("response_type")),
		zap.String("code_challenge_method", get("code_challenge_method")),
		zap.Bool("has_code_challenge", get("code_challenge") != ""), zap.Bool("has_state", get("state") != ""),
		zap.String("scope", get("scope")), zap.Bool("has_token", get("token") != ""))
	client, ok := o.cfg.Store.Client(get("client_id"))
	if !ok {
		log.Debug("OAuth.authorize: refused, unknown client", zap.String("client_id", get("client_id")))
		plainError(w, http.StatusBadRequest, "unknown client_id")
		return
	}
	redirect := get("redirect_uri")
	match := false
	for _, u := range client.RedirectURIs {
		if u == redirect {
			match = true
		}
	}
	if !match {
		log.Debug("OAuth.authorize: refused, redirect_uri not registered by the client", zap.String("client_id", client.ID),
			zap.Int("registered", len(client.RedirectURIs)))
		plainError(w, http.StatusBadRequest, "redirect_uri is not one the client registered")
		return
	}
	// From here an error goes back to the client through its redirect.
	fail := func(code, desc string) {
		log.Debug("OAuth.authorize: failing back to the client's redirect", zap.String("client_id", client.ID),
			zap.String("oauth_error", code), zap.String("description", desc))
		u, _ := url.Parse(redirect)
		q := u.Query()
		q.Set("error", code)
		q.Set("error_description", desc)
		if s := get("state"); s != "" {
			q.Set("state", s)
		}
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	}
	if get("response_type") != "code" {
		fail("unsupported_response_type", "only response_type=code")
		return
	}
	if get("code_challenge") == "" || get("code_challenge_method") != "S256" {
		fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	hidden := map[string]string{}
	for _, k := range []string{"client_id", "redirect_uri", "response_type", "code_challenge", "code_challenge_method", "state", "scope"} {
		hidden[k] = get(k)
	}
	if r.Method != http.MethodPost {
		log.Debug("OAuth.authorize: showing the approval page", zap.String("client_id", client.ID))
		o.page(w, http.StatusOK, client, hidden, "")
		return
	}

	ip := ClientIP(r, o.cfg.TrustProxy)
	if o.cfg.Limiter.Blocked("authorize:" + ip) {
		log.Debug("OAuth.authorize: refused, address rate limited", zap.String("client_ip", ip))
		o.page(w, http.StatusTooManyRequests, client, hidden, "Too many attempts. Wait a few minutes.")
		return
	}
	start := time.Now()
	parent, err := o.cfg.Store.Verify(strings.TrimSpace(get("token")))
	if err != nil || parent.Kind != KindStatic {
		log.Debug("OAuth.authorize: pasted token rejected", zap.String("client_id", client.ID), zap.String("client_ip", ip),
			zap.String("kind", parent.Kind), zap.Error(err), zap.Duration("took", time.Since(start)))
		o.cfg.Limiter.Fail("authorize:" + ip)
		o.page(w, http.StatusUnauthorized, client, hidden, "That is not a valid access token.")
		return
	}
	role := Min(parent.Role, o.cfg.MaxRole)
	log.Debug("OAuth.authorize: pasted token accepted", zap.String("parent_id", parent.ID),
		zap.String("parent_role", string(parent.Role)), zap.String("max_role", string(o.cfg.MaxRole)),
		zap.String("role", string(role)), zap.Duration("took", time.Since(start)))
	if scope := get("scope"); scope != "" {
		if asked := RoleFromScopes(strings.Fields(scope)); asked.Rank() > 0 {
			role = Min(role, asked)
			log.Debug("OAuth.authorize: client asked for a scope, role capped to it", zap.String("asked", string(asked)),
				zap.String("role", string(role)))
		} else {
			log.Debug("OAuth.authorize: scope names no known role, ignored", zap.String("scope", scope))
		}
	}
	code, err := randomCode()
	if err != nil {
		log.Debug("OAuth.authorize: could not make a code", zap.Error(err))
		fail("server_error", "could not make a code")
		return
	}
	o.mu.Lock()
	now := time.Now()
	expired := 0
	for k, c := range o.codes {
		if now.After(c.expires) {
			delete(o.codes, k)
			expired++
		}
	}
	if expired > 0 {
		log.Debug("OAuth.authorize: dropped expired authorization codes", zap.Int("count", expired))
	}
	if len(o.codes) >= maxAuthCodes {
		pending := len(o.codes)
		o.mu.Unlock()
		log.Debug("OAuth.authorize: too many pending codes", zap.Int("pending", pending), zap.Int("max", maxAuthCodes))
		fail("temporarily_unavailable", "too many pending authorizations")
		return
	}
	o.codes[code] = authCode{client: client.ID, redirect: redirect, challenge: get("code_challenge"),
		parent: parent, role: role, expires: now.Add(authCodeTTL)}
	pending := len(o.codes)
	o.mu.Unlock()
	log.Debug("OAuth.authorize: authorization code issued, redirecting", zap.String("client_id", client.ID),
		zap.String("parent_id", parent.ID), zap.String("role", string(role)), zap.Time("expires", now.Add(authCodeTTL)),
		zap.Int("pending", pending))

	u, _ := url.Parse(redirect)
	q := u.Query()
	q.Set("code", code)
	if s := get("state"); s != "" {
		q.Set("state", s)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func randomCode() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		zap.L().Debug("randomCode: reading random bytes failed", zap.Error(err))
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (o *OAuth) token(w http.ResponseWriter, r *http.Request) {
	log := zap.L()
	if r.Method != http.MethodPost {
		log.Debug("OAuth.token: refused, not POST", zap.String("method", r.Method))
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST only")
		return
	}
	if err := r.ParseForm(); err != nil {
		log.Debug("OAuth.token: refused, form does not parse", zap.Error(err))
		oauthError(w, http.StatusBadRequest, "invalid_request", "form body expected")
		return
	}
	get := func(k string) string { return r.PostForm.Get(k) }
	// The form carries the code, its verifier or a refresh token: only their
	// presence is logged.
	log.Debug("OAuth.token: token request", zap.String("client_id", get("client_id")),
		zap.String("grant_type", get("grant_type")), zap.Bool("has_code", get("code") != ""),
		zap.Bool("has_code_verifier", get("code_verifier") != ""), zap.Bool("has_refresh_token", get("refresh_token") != ""),
		zap.String("remote_addr", r.RemoteAddr))
	client, ok := o.cfg.Store.Client(get("client_id"))
	if !ok {
		log.Debug("OAuth.token: refused, unknown client", zap.String("client_id", get("client_id")))
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown client_id")
		return
	}
	switch get("grant_type") {
	case "authorization_code":
		o.mu.Lock()
		c, found := o.codes[get("code")]
		delete(o.codes, get("code")) // single use, whatever happens next
		o.mu.Unlock()
		if !found || time.Now().After(c.expires) || c.client != client.ID || c.redirect != get("redirect_uri") {
			log.Debug("OAuth.token: authorization code rejected", zap.String("client_id", client.ID), zap.Bool("found", found),
				zap.Bool("expired", found && time.Now().After(c.expires)), zap.Bool("client_matches", c.client == client.ID),
				zap.Bool("redirect_matches", c.redirect == get("redirect_uri")))
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code is unknown, used, expired, or for another client")
			return
		}
		sum := sha256.Sum256([]byte(get("code_verifier")))
		want := base64.RawURLEncoding.EncodeToString(sum[:])
		if subtle.ConstantTimeCompare([]byte(want), []byte(c.challenge)) != 1 {
			log.Debug("OAuth.token: PKCE verifier does not match the challenge", zap.String("client_id", client.ID),
				zap.Bool("has_code_verifier", get("code_verifier") != ""))
			oauthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match the code_challenge")
			return
		}
		start := time.Now()
		access, refresh, exp, err := o.cfg.Store.IssueOAuth(c.parent, client, c.role, o.cfg.AccessTTL, o.cfg.RefreshTTL)
		if err != nil {
			log.Debug("OAuth.token: issuing tokens failed", zap.String("client_id", client.ID), zap.Error(err),
				zap.Duration("took", time.Since(start)))
			oauthError(w, http.StatusInternalServerError, "server_error", "could not issue a token")
			return
		}
		log.Debug("OAuth.token: code exchanged for tokens", zap.String("client_id", client.ID),
			zap.String("parent_id", c.parent.ID), zap.String("role", string(c.role)), zap.Time("expires", exp),
			zap.Duration("took", time.Since(start)))
		o.respond(w, access, refresh, exp, c.role)
	case "refresh_token":
		start := time.Now()
		access, refresh, exp, role, err := o.cfg.Store.Refresh(get("refresh_token"), client.ID, o.cfg.AccessTTL, o.cfg.RefreshTTL)
		if errors.Is(err, ErrInvalid) {
			log.Debug("OAuth.token: refresh token rejected", zap.String("client_id", client.ID),
				zap.Duration("took", time.Since(start)))
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is unknown, used, or revoked")
			return
		}
		if err != nil {
			log.Debug("OAuth.token: refresh failed", zap.String("client_id", client.ID), zap.Error(err),
				zap.Duration("took", time.Since(start)))
			oauthError(w, http.StatusInternalServerError, "server_error", "could not issue a token")
			return
		}
		log.Debug("OAuth.token: tokens refreshed", zap.String("client_id", client.ID), zap.String("role", string(role)),
			zap.Time("expires", exp), zap.Duration("took", time.Since(start)))
		o.respond(w, access, refresh, exp, role)
	default:
		log.Debug("OAuth.token: refused, unsupported grant type", zap.String("grant_type", get("grant_type")))
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "authorization_code or refresh_token")
	}
}

func (o *OAuth) respond(w http.ResponseWriter, access, refresh string, exp time.Time, role Role) {
	zap.L().Debug("OAuth.respond: sending tokens", zap.Bool("has_access_token", access != ""),
		zap.Bool("has_refresh_token", refresh != ""), zap.Time("expires", exp), zap.String("role", string(role)))
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(time.Until(exp).Seconds()),
		"refresh_token": refresh,
		"scope":         strings.Join(role.Scopes(), " "),
	})
}
