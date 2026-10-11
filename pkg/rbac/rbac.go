// Package rbac provides a configurable, Casbin-backed authorization engine for
// the Haify controller. The model is embedded and identities are resolved from
// per-user bearer tokens. Runtime changes (admins adding users or changing
// roles) are persisted through a Store, which the controller backs with the
// same BBolt database as the rest of Haify — so RBAC needs no extra service or
// datastore and the system still ships as a handful of static binaries.
package rbac

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
)

// modelText is the Casbin model: role-based access control over (object,
// action) pairs. g maps a user to one or more roles (role inheritance is
// supported), and a policy grants a role an action on an object, with "*"
// acting as a wildcard for either field.
const modelText = `
[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = g(r.sub, p.sub) && (p.obj == "*" || r.obj == p.obj) && (p.act == "*" || r.act == p.act)
`

// Built-in object and action vocabulary. Custom policies in configuration may
// reference any object string; these are simply the ones the method classifier
// produces and the defaults grant.
const (
	ActRead  = "read"
	ActWrite = "write"
	// ActApprove is the right to approve another user's held-back call.
	ActApprove = "approve"
)

// User is an identity that authenticates with a bearer token and carries a role.
type User struct {
	Name  string
	Token string
	Role  string
}

// Policy grants Role the Action on Object. Object/Action may be "*".
type Policy struct {
	Role   string `json:"role"`
	Object string `json:"object"`
	Action string `json:"action"`
}

// defaultPolicies seed the three built-in roles so a minimal config (just
// users with role = admin/operator/viewer) works out of the box. Config
// policies are applied on top and can extend these or define new roles.
func defaultPolicies() []Policy {
	return []Policy{
		{"admin", "*", "*"},
		{"operator", "pool", "*"},
		{"operator", "resource", "*"},
		{"operator", "gateway", "*"},
		{"operator", "snapshot", "*"},
		{"operator", "backup", "*"},
		{"operator", "ha", "*"},
		{"operator", "app", "*"},
		{"operator", "node", ActRead},
		{"operator", "system", ActRead},
		{"viewer", "*", ActRead},
		// security-officer approves what [rbac.approval] holds back, and
		// reads everything; it changes nothing itself.
		{"security-officer", "approval", ActApprove},
		{"security-officer", "*", ActRead},
	}
}

// Snapshot is the persisted RBAC state: the full policy and role-assignment
// rows plus the token→user digest map. It is what an admin's runtime changes
// are saved as, so they survive restarts.
type Snapshot struct {
	Policies  [][]string        `json:"policies"`
	Groupings [][]string        `json:"groupings"`
	Tokens    map[string]string `json:"tokens"` // sha256(token) -> user name
}

// Store persists and restores the RBAC Snapshot. The controller backs it with
// the same BBolt database the rest of Haify uses, so RBAC needs no extra store.
type Store interface {
	Load() (*Snapshot, error)
	Save(*Snapshot) error
}

// Engine enforces authorization decisions, resolves token identities, and —
// for administrators — mutates users and roles, persisting every change.
type Engine struct {
	mu       sync.RWMutex
	enforcer *casbin.SyncedEnforcer
	// tokens maps a sha256 hex of a bearer token to the user name. Hashing
	// avoids keeping raw tokens in a long-lived map and makes lookups uniform.
	tokens map[string]string
	// pinned users are declared in config: they are re-applied on every boot
	// and cannot be deleted via the API, so there is always a break-glass admin.
	pinned map[string]bool
	store  Store
}

// New builds an Engine. Default role policies are always seeded. If a persisted
// snapshot exists it is loaded (runtime state is authoritative); config users
// and policies are then applied on top as a pinned baseline, so the operators
// declared in controller.toml always exist with the role stated there.
func New(store Store, users []User, policies []Policy) (*Engine, error) {
	m, err := model.NewModelFromString(modelText)
	if err != nil {
		return nil, fmt.Errorf("rbac: load model: %w", err)
	}
	e, err := casbin.NewSyncedEnforcer(m)
	if err != nil {
		return nil, fmt.Errorf("rbac: new enforcer: %w", err)
	}
	e.EnableAutoSave(false)

	eng := &Engine{
		enforcer: e,
		tokens:   make(map[string]string),
		pinned:   make(map[string]bool),
		store:    store,
	}

	for _, p := range defaultPolicies() {
		if _, err := e.AddPolicy(p.Role, p.Object, p.Action); err != nil {
			return nil, fmt.Errorf("rbac: seed default policy: %w", err)
		}
	}

	// Restore runtime-managed state (users an admin added, etc.).
	if store != nil {
		snap, err := store.Load()
		if err != nil {
			return nil, fmt.Errorf("rbac: load persisted state: %w", err)
		}
		if snap != nil {
			for _, p := range snap.Policies {
				if len(p) >= 3 {
					_, _ = e.AddPolicy(p[0], p[1], p[2])
				}
			}
			for _, g := range snap.Groupings {
				if len(g) >= 2 {
					_, _ = e.AddGroupingPolicy(g[0], g[1])
				}
			}
			for h, n := range snap.Tokens {
				eng.tokens[h] = n
			}
		}
	}

	// Config policies (additive on top of defaults).
	for _, p := range policies {
		if p.Role == "" || p.Object == "" || p.Action == "" {
			return nil, fmt.Errorf("rbac: policy must set role, object and action")
		}
		if _, err := e.AddPolicy(p.Role, p.Object, p.Action); err != nil {
			return nil, fmt.Errorf("rbac: add policy: %w", err)
		}
	}

	// Config users: pinned baseline, config wins for their token and role.
	for _, u := range users {
		if u.Name == "" || u.Token == "" || u.Role == "" {
			return nil, fmt.Errorf("rbac: user must set name, token and role")
		}
		eng.pinned[u.Name] = true
		if err := eng.setRoleLocked(u.Name, u.Role); err != nil {
			return nil, fmt.Errorf("rbac: assign role %q to %q: %w", u.Role, u.Name, err)
		}
		eng.setTokenLocked(u.Name, u.Token)
	}

	if err := eng.persistLocked(); err != nil {
		return nil, fmt.Errorf("rbac: persist initial state: %w", err)
	}
	return eng, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// GenerateToken returns a fresh, random bearer token suitable for a new user.
func GenerateToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// --- internal helpers (caller holds e.mu for write helpers) ---

func (e *Engine) knownRolesLocked() map[string]bool {
	roles := map[string]bool{}
	if p, err := e.enforcer.GetPolicy(); err == nil {
		for _, row := range p {
			if len(row) >= 1 {
				roles[row[0]] = true
			}
		}
	}
	return roles
}

func (e *Engine) userExistsLocked(name string) bool {
	roles, _ := e.enforcer.GetRolesForUser(name)
	return len(roles) > 0
}

// setRoleLocked replaces all of a user's role assignments with the single role.
func (e *Engine) setRoleLocked(name, role string) error {
	roles, _ := e.enforcer.GetRolesForUser(name)
	for _, r := range roles {
		if _, err := e.enforcer.RemoveGroupingPolicy(name, r); err != nil {
			return err
		}
	}
	if _, err := e.enforcer.AddGroupingPolicy(name, role); err != nil {
		return err
	}
	return nil
}

func (e *Engine) setTokenLocked(name, token string) {
	for h, n := range e.tokens {
		if n == name {
			delete(e.tokens, h)
		}
	}
	e.tokens[hashToken(token)] = name
}

func (e *Engine) persistLocked() error {
	if e.store == nil {
		return nil
	}
	pol, _ := e.enforcer.GetPolicy()
	grp, _ := e.enforcer.GetGroupingPolicy()
	toks := make(map[string]string, len(e.tokens))
	for h, n := range e.tokens {
		toks[h] = n
	}
	return e.store.Save(&Snapshot{Policies: pol, Groupings: grp, Tokens: toks})
}

// --- read API ---

// ResolveUser returns the user name for a bearer token, if known.
func (e *Engine) ResolveUser(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	name, ok := e.tokens[hashToken(token)]
	return name, ok
}

// Enforce reports whether user may perform action on object.
func (e *Engine) Enforce(user, object, action string) (bool, error) {
	return e.enforcer.Enforce(user, object, action)
}

// RoleOf returns the (first) role assigned to a user, or "".
func (e *Engine) RoleOf(user string) string {
	roles, err := e.enforcer.GetRolesForUser(user)
	if err != nil || len(roles) == 0 {
		return ""
	}
	return roles[0]
}

// Whoami resolves a bearer token to its user and role.
func (e *Engine) Whoami(token string) (user, role string, ok bool) {
	name, ok := e.ResolveUser(token)
	if !ok {
		return "", "", false
	}
	return name, e.RoleOf(name), true
}

// CanAdmin reports whether a user holds full administrative rights. It is the
// gate for viewing and changing role and user assignments.
func (e *Engine) CanAdmin(user string) bool {
	ok, _ := e.Enforce(user, "system", "write")
	return ok
}

// UserRole pairs a user with their role for read-only introspection. Pinned
// users are declared in config and cannot be removed via the API.
type UserRole struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Pinned bool   `json:"pinned"`
}

// Users returns the current user→role assignments, sorted by name.
func (e *Engine) Users() []UserRole {
	e.mu.RLock()
	defer e.mu.RUnlock()
	g, err := e.enforcer.GetGroupingPolicy()
	if err != nil {
		return nil
	}
	out := make([]UserRole, 0, len(g))
	for _, row := range g {
		if len(row) >= 2 {
			out = append(out, UserRole{Name: row[0], Role: row[1], Pinned: e.pinned[row[0]]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// EffectivePolicies returns the full set of (role, object, action) grants,
// including the built-in defaults, for read-only display.
func (e *Engine) EffectivePolicies() []Policy {
	p, err := e.enforcer.GetPolicy()
	if err != nil {
		return nil
	}
	out := make([]Policy, 0, len(p))
	for _, row := range p {
		if len(row) >= 3 {
			out = append(out, Policy{Role: row[0], Object: row[1], Action: row[2]})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Role != out[j].Role {
			return out[i].Role < out[j].Role
		}
		if out[i].Object != out[j].Object {
			return out[i].Object < out[j].Object
		}
		return out[i].Action < out[j].Action
	})
	return out
}

// --- write API (admin-gated at the transport layer) ---

// CreateUser adds a new user with the given role. If token is empty a secure
// random token is generated. The (possibly generated) token is returned and is
// the only time it is ever shown in clear text.
func (e *Engine) CreateUser(name, token, role string) (string, error) {
	if name == "" || role == "" {
		return "", fmt.Errorf("name and role are required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.userExistsLocked(name) {
		return "", fmt.Errorf("user %q already exists", name)
	}
	if !e.knownRolesLocked()[role] {
		return "", fmt.Errorf("unknown role %q", role)
	}
	if token == "" {
		var err error
		if token, err = GenerateToken(); err != nil {
			return "", err
		}
	}
	if len(token) < 16 {
		return "", fmt.Errorf("token must be at least 16 characters")
	}
	if _, dup := e.tokens[hashToken(token)]; dup {
		return "", fmt.Errorf("token is already in use")
	}
	if _, err := e.enforcer.AddGroupingPolicy(name, role); err != nil {
		return "", err
	}
	e.tokens[hashToken(token)] = name
	if err := e.persistLocked(); err != nil {
		return "", err
	}
	return token, nil
}

// SetRole changes a user's role.
func (e *Engine) SetRole(name, role string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.userExistsLocked(name) {
		return fmt.Errorf("no such user %q", name)
	}
	if !e.knownRolesLocked()[role] {
		return fmt.Errorf("unknown role %q", role)
	}
	if err := e.setRoleLocked(name, role); err != nil {
		return err
	}
	return e.persistLocked()
}

// DeleteUser removes a runtime-managed user. Config-pinned users are protected.
func (e *Engine) DeleteUser(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pinned[name] {
		return fmt.Errorf("user %q is declared in config and cannot be removed here", name)
	}
	if !e.userExistsLocked(name) {
		return fmt.Errorf("no such user %q", name)
	}
	roles, _ := e.enforcer.GetRolesForUser(name)
	for _, r := range roles {
		if _, err := e.enforcer.RemoveGroupingPolicy(name, r); err != nil {
			return err
		}
	}
	for h, n := range e.tokens {
		if n == name {
			delete(e.tokens, h)
		}
	}
	return e.persistLocked()
}

// Roles returns the known role names (those that have at least one policy).
func (e *Engine) Roles() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	set := e.knownRolesLocked()
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// appMethodRe matches the database application methods (CreateApp,
// ListApps, GetAppStatus, ...) and nothing that merely starts with "App",
// such as the approval methods.
var appMethodRe = regexp.MustCompile(`^[A-Z][a-z]+(App|Apps|AppStatus)$`)

// readVerbs are method-name prefixes that denote a read-only operation.
var readVerbs = []string{"List", "Get", "Describe", "Watch", "Stream", "Check"}

// Classify maps a gRPC full method (e.g. "/v1.HaifyController/CreateNFSGateway")
// to the (object, action) pair used for authorization. Unknown methods map to
// the "system" object and, unless clearly read-only, the "write" action — so a
// newly added RPC is locked down by default rather than silently permitted.
func Classify(fullMethod string) (object, action string) {
	method := fullMethod
	if i := strings.LastIndex(fullMethod, "/"); i >= 0 {
		method = fullMethod[i+1:]
	}
	return classifyObject(method), classifyAction(method)
}

func classifyAction(method string) string {
	if method == "ApproveRequest" || method == "RejectRequest" {
		return ActApprove
	}
	// An inspection reads the cluster and stores a report; it changes nothing
	// on it, so whoever may read the cluster may run one.
	if strings.Contains(method, "Status") || method == "RunInspection" {
		return ActRead
	}
	for _, v := range readVerbs {
		if strings.HasPrefix(method, v) {
			return ActRead
		}
	}
	return ActWrite
}

func classifyObject(method string) string {
	switch {
	case method == "ListApprovals" || method == "ApproveRequest" || method == "RejectRequest":
		return "approval"
	// Before the snapshot case: SnapshotApp freezes a database and is part of
	// running it, so whoever may run apps may snapshot them.
	case appMethodRe.MatchString(method):
		return "app"
	// Before the snapshot case: off-cluster backups are their own object, so an
	// operator who may ship data off-site can be distinguished from one who may
	// only snapshot in place. Unclassified would land it in "system", where an
	// operator has read only — a working feature nobody but admin could use.
	// Shipping a resource to another cluster sends its data off-site just as
	// a backup does, on both the sending and the receiving controller.
	case strings.Contains(method, "Backup"), strings.Contains(method, "Ship"):
		return "backup"
	case strings.Contains(method, "Snapshot"):
		return "snapshot"
	case containsAny(method, "Gateway", "NFS", "ISCSI", "NVMe", "SMB", "Chap", "Export", "Initiator", "LUN", "Host", "Namespace"):
		return "gateway"
	case containsAny(method, "Pool", "Disk", "ZFSDataset", "ZFSVolume", "ZFSpool"):
		return "pool"
	case containsAny(method, "SelfHa", "Ha"):
		return "ha"
	case containsAny(method, "Node", "Register"):
		return "node"
	case containsAny(method, "Resource", "Volume", "Filesystem", "Primary", "Secondary", "Mount"):
		return "resource"
	default:
		return "system"
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
