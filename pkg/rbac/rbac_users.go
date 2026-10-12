package rbac

import (
	"fmt"
	"sort"

	"go.uber.org/zap"
)

// --- write API (admin-gated at the transport layer) ---

// CreateUser adds a new user with the given role. If token is empty a secure
// random token is generated. The (possibly generated) token is returned and is
// the only time it is ever shown in clear text.
func (e *Engine) CreateUser(name, token, role string) (string, error) {
	log := zap.L()
	log.Debug("Engine.CreateUser: start", zap.String("user", name), zap.String("role", role), zap.Bool("has_token", token != ""))
	if name == "" || role == "" {
		log.Debug("Engine.CreateUser: name or role missing")
		return "", fmt.Errorf("name and role are required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.userExistsLocked(name) {
		log.Debug("Engine.CreateUser: user exists", zap.String("user", name))
		return "", fmt.Errorf("user %q already exists", name)
	}
	if !e.knownRolesLocked()[role] {
		log.Debug("Engine.CreateUser: unknown role", zap.String("role", role))
		return "", fmt.Errorf("unknown role %q", role)
	}
	if token == "" {
		log.Debug("Engine.CreateUser: no token given, generating one", zap.String("user", name))
		var err error
		if token, err = GenerateToken(); err != nil {
			log.Debug("Engine.CreateUser: generating token failed", zap.Error(err))
			return "", err
		}
	}
	if len(token) < 16 {
		log.Debug("Engine.CreateUser: token too short", zap.String("user", name))
		return "", fmt.Errorf("token must be at least 16 characters")
	}
	if _, dup := e.tokens[hashToken(token)]; dup {
		log.Debug("Engine.CreateUser: token already in use", zap.String("user", name))
		return "", fmt.Errorf("token is already in use")
	}
	if _, err := e.enforcer.AddGroupingPolicy(name, role); err != nil {
		log.Debug("Engine.CreateUser: assigning role failed", zap.String("user", name), zap.Error(err))
		return "", err
	}
	e.tokens[hashToken(token)] = name
	if err := e.persistLocked(); err != nil {
		log.Debug("Engine.CreateUser: persisting failed", zap.String("user", name), zap.Error(err))
		return "", err
	}
	log.Debug("Engine.CreateUser: created", zap.String("user", name), zap.String("role", role))
	return token, nil
}

// SetRole changes a user's role.
func (e *Engine) SetRole(name, role string) error {
	log := zap.L()
	log.Debug("Engine.SetRole: start", zap.String("user", name), zap.String("role", role))
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.userExistsLocked(name) {
		log.Debug("Engine.SetRole: no such user", zap.String("user", name))
		return fmt.Errorf("no such user %q", name)
	}
	if !e.knownRolesLocked()[role] {
		log.Debug("Engine.SetRole: unknown role", zap.String("role", role))
		return fmt.Errorf("unknown role %q", role)
	}
	if err := e.setRoleLocked(name, role); err != nil {
		log.Debug("Engine.SetRole: setting role failed", zap.String("user", name), zap.Error(err))
		return err
	}
	err := e.persistLocked()
	log.Debug("Engine.SetRole: done", zap.String("user", name), zap.String("role", role), zap.Error(err))
	return err
}

// DeleteUser removes a runtime-managed user. Config-pinned users are protected.
func (e *Engine) DeleteUser(name string) error {
	log := zap.L()
	log.Debug("Engine.DeleteUser: start", zap.String("user", name))
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pinned[name] {
		log.Debug("Engine.DeleteUser: user pinned by config, refusing", zap.String("user", name))
		return fmt.Errorf("user %q is declared in config and cannot be removed here", name)
	}
	if !e.userExistsLocked(name) {
		log.Debug("Engine.DeleteUser: no such user", zap.String("user", name))
		return fmt.Errorf("no such user %q", name)
	}
	roles, _ := e.enforcer.GetRolesForUser(name)
	for _, r := range roles {
		log.Debug("Engine.DeleteUser: removing role", zap.String("user", name), zap.String("role", r))
		if _, err := e.enforcer.RemoveGroupingPolicy(name, r); err != nil {
			log.Debug("Engine.DeleteUser: removing role failed", zap.String("user", name), zap.String("role", r), zap.Error(err))
			return err
		}
	}
	removed := 0
	for h, n := range e.tokens {
		if n == name {
			delete(e.tokens, h)
			removed++
		}
	}
	err := e.persistLocked()
	log.Debug("Engine.DeleteUser: done", zap.String("user", name), zap.Int("tokens_removed", removed), zap.Error(err))
	return err
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
	zap.L().Debug("Engine.Roles: listed", zap.Strings("roles", out))
	return out
}
