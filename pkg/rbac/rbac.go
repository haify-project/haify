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
	"sort"
	"sync"
	"time"

	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
	"go.uber.org/zap"
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
	log := zap.L()
	log.Debug("New: start", zap.Bool("has_store", store != nil), zap.Int("users", len(users)), zap.Int("policies", len(policies)))
	m, err := model.NewModelFromString(modelText)
	if err != nil {
		log.Debug("New: model does not load", zap.Error(err))
		return nil, fmt.Errorf("rbac: load model: %w", err)
	}
	e, err := casbin.NewSyncedEnforcer(m)
	if err != nil {
		log.Debug("New: enforcer creation failed", zap.Error(err))
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
		log.Debug("New: seeding default policy", zap.String("role", p.Role), zap.String("object", p.Object), zap.String("action", p.Action))
		if _, err := e.AddPolicy(p.Role, p.Object, p.Action); err != nil {
			log.Debug("New: seeding default policy failed", zap.String("role", p.Role), zap.Error(err))
			return nil, fmt.Errorf("rbac: seed default policy: %w", err)
		}
	}

	// Restore runtime-managed state (users an admin added, etc.).
	if store != nil {
		start := time.Now()
		snap, err := store.Load()
		log.Debug("New: store.Load returned", zap.Bool("found", snap != nil), zap.Error(err), zap.Duration("took", time.Since(start)))
		if err != nil {
			return nil, fmt.Errorf("rbac: load persisted state: %w", err)
		}
		if snap != nil {
			log.Debug("New: restoring persisted state", zap.Int("policies", len(snap.Policies)),
				zap.Int("groupings", len(snap.Groupings)), zap.Int("tokens", len(snap.Tokens)))
			for _, p := range snap.Policies {
				if len(p) >= 3 {
					log.Debug("New: restoring policy", zap.Strings("policy", p))
					_, _ = e.AddPolicy(p[0], p[1], p[2])
				} else {
					log.Debug("New: skipping short policy row", zap.Strings("policy", p))
				}
			}
			for _, g := range snap.Groupings {
				if len(g) >= 2 {
					log.Debug("New: restoring role assignment", zap.String("user", g[0]), zap.String("role", g[1]))
					_, _ = e.AddGroupingPolicy(g[0], g[1])
				} else {
					log.Debug("New: skipping short grouping row", zap.Strings("grouping", g))
				}
			}
			for h, n := range snap.Tokens {
				log.Debug("New: restoring token for user", zap.String("user", n))
				eng.tokens[h] = n
			}
		}
	} else {
		log.Debug("New: no store, nothing persisted to restore")
	}

	// Config policies (additive on top of defaults).
	for _, p := range policies {
		log.Debug("New: adding config policy", zap.String("role", p.Role), zap.String("object", p.Object), zap.String("action", p.Action))
		if p.Role == "" || p.Object == "" || p.Action == "" {
			log.Debug("New: config policy incomplete")
			return nil, fmt.Errorf("rbac: policy must set role, object and action")
		}
		if _, err := e.AddPolicy(p.Role, p.Object, p.Action); err != nil {
			log.Debug("New: adding config policy failed", zap.String("role", p.Role), zap.Error(err))
			return nil, fmt.Errorf("rbac: add policy: %w", err)
		}
	}

	// Config users: pinned baseline, config wins for their token and role.
	for _, u := range users {
		log.Debug("New: pinning config user", zap.String("user", u.Name), zap.String("role", u.Role), zap.Bool("has_token", u.Token != ""))
		if u.Name == "" || u.Token == "" || u.Role == "" {
			log.Debug("New: config user incomplete", zap.String("user", u.Name))
			return nil, fmt.Errorf("rbac: user must set name, token and role")
		}
		eng.pinned[u.Name] = true
		if err := eng.setRoleLocked(u.Name, u.Role); err != nil {
			log.Debug("New: assigning config role failed", zap.String("user", u.Name), zap.String("role", u.Role), zap.Error(err))
			return nil, fmt.Errorf("rbac: assign role %q to %q: %w", u.Role, u.Name, err)
		}
		eng.setTokenLocked(u.Name, u.Token)
	}

	if err := eng.persistLocked(); err != nil {
		log.Debug("New: persisting initial state failed", zap.Error(err))
		return nil, fmt.Errorf("rbac: persist initial state: %w", err)
	}
	log.Debug("New: engine ready", zap.Int("tokens", len(eng.tokens)), zap.Int("pinned", len(eng.pinned)))
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
		zap.L().Debug("GenerateToken: reading random bytes failed", zap.Error(err))
		return "", err
	}
	zap.L().Debug("GenerateToken: token generated", zap.Int("bytes", len(b)))
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
	} else {
		zap.L().Debug("Engine.knownRolesLocked: reading policies failed", zap.Error(err))
	}
	zap.L().Debug("Engine.knownRolesLocked: roles found", zap.Int("count", len(roles)))
	return roles
}

func (e *Engine) userExistsLocked(name string) bool {
	roles, _ := e.enforcer.GetRolesForUser(name)
	zap.L().Debug("Engine.userExistsLocked: checked", zap.String("user", name), zap.Strings("roles", roles))
	return len(roles) > 0
}

// setRoleLocked replaces all of a user's role assignments with the single role.
func (e *Engine) setRoleLocked(name, role string) error {
	log := zap.L()
	roles, _ := e.enforcer.GetRolesForUser(name)
	log.Debug("Engine.setRoleLocked: start", zap.String("user", name), zap.String("role", role), zap.Strings("old_roles", roles))
	for _, r := range roles {
		if _, err := e.enforcer.RemoveGroupingPolicy(name, r); err != nil {
			log.Debug("Engine.setRoleLocked: removing old role failed", zap.String("user", name), zap.String("role", r), zap.Error(err))
			return err
		}
	}
	if _, err := e.enforcer.AddGroupingPolicy(name, role); err != nil {
		log.Debug("Engine.setRoleLocked: adding role failed", zap.String("user", name), zap.String("role", role), zap.Error(err))
		return err
	}
	return nil
}

func (e *Engine) setTokenLocked(name, token string) {
	removed := 0
	for h, n := range e.tokens {
		if n == name {
			delete(e.tokens, h)
			removed++
		}
	}
	e.tokens[hashToken(token)] = name
	zap.L().Debug("Engine.setTokenLocked: token replaced", zap.String("user", name), zap.Int("removed", removed))
}

func (e *Engine) persistLocked() error {
	if e.store == nil {
		zap.L().Debug("Engine.persistLocked: no store, nothing to save")
		return nil
	}
	pol, _ := e.enforcer.GetPolicy()
	grp, _ := e.enforcer.GetGroupingPolicy()
	toks := make(map[string]string, len(e.tokens))
	for h, n := range e.tokens {
		toks[h] = n
	}
	start := time.Now()
	err := e.store.Save(&Snapshot{Policies: pol, Groupings: grp, Tokens: toks})
	zap.L().Debug("Engine.persistLocked: store.Save returned", zap.Int("policies", len(pol)), zap.Int("groupings", len(grp)),
		zap.Int("tokens", len(toks)), zap.Error(err), zap.Duration("took", time.Since(start)))
	return err
}

// --- read API ---

// ResolveUser returns the user name for a bearer token, if known.
func (e *Engine) ResolveUser(token string) (string, bool) {
	if token == "" {
		zap.L().Debug("Engine.ResolveUser: no token")
		return "", false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	name, ok := e.tokens[hashToken(token)]
	zap.L().Debug("Engine.ResolveUser: looked up token", zap.Bool("found", ok), zap.String("user", name))
	return name, ok
}

// Enforce reports whether user may perform action on object.
func (e *Engine) Enforce(user, object, action string) (bool, error) {
	ok, err := e.enforcer.Enforce(user, object, action)
	zap.L().Debug("Engine.Enforce: decided", zap.String("user", user), zap.String("object", object),
		zap.String("action", action), zap.Bool("allowed", ok), zap.Error(err))
	return ok, err
}

// RoleOf returns the (first) role assigned to a user, or "".
func (e *Engine) RoleOf(user string) string {
	roles, err := e.enforcer.GetRolesForUser(user)
	if err != nil || len(roles) == 0 {
		zap.L().Debug("Engine.RoleOf: no role", zap.String("user", user), zap.Error(err))
		return ""
	}
	zap.L().Debug("Engine.RoleOf: found", zap.String("user", user), zap.String("role", roles[0]), zap.Int("roles", len(roles)))
	return roles[0]
}

// Whoami resolves a bearer token to its user and role.
func (e *Engine) Whoami(token string) (user, role string, ok bool) {
	name, ok := e.ResolveUser(token)
	if !ok {
		zap.L().Debug("Engine.Whoami: token unknown")
		return "", "", false
	}
	role = e.RoleOf(name)
	zap.L().Debug("Engine.Whoami: resolved", zap.String("user", name), zap.String("role", role))
	return name, role, true
}

// CanAdmin reports whether a user holds full administrative rights. It is the
// gate for viewing and changing role and user assignments.
func (e *Engine) CanAdmin(user string) bool {
	ok, _ := e.Enforce(user, "system", "write")
	zap.L().Debug("Engine.CanAdmin: decided", zap.String("user", user), zap.Bool("admin", ok))
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
		zap.L().Debug("Engine.Users: reading role assignments failed", zap.Error(err))
		return nil
	}
	out := make([]UserRole, 0, len(g))
	for _, row := range g {
		if len(row) >= 2 {
			out = append(out, UserRole{Name: row[0], Role: row[1], Pinned: e.pinned[row[0]]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	zap.L().Debug("Engine.Users: listed", zap.Int("count", len(out)))
	return out
}

// EffectivePolicies returns the full set of (role, object, action) grants,
// including the built-in defaults, for read-only display.
func (e *Engine) EffectivePolicies() []Policy {
	p, err := e.enforcer.GetPolicy()
	if err != nil {
		zap.L().Debug("Engine.EffectivePolicies: reading policies failed", zap.Error(err))
		return nil
	}
	zap.L().Debug("Engine.EffectivePolicies: listing", zap.Int("count", len(p)))
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
