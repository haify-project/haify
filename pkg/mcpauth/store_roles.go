package mcpauth

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// Roles and the OAuth scopes that carry them.

// Role is what a token may do.
type Role string

const (
	// RoleRead inspects the cluster and changes nothing.
	RoleRead Role = "read"
	// RoleOperate adds the day-to-day writes: create, grow, snapshot, start,
	// mount. Nothing that deletes data or interrupts service.
	RoleOperate Role = "operate"
	// RoleAdmin is everything, including delete, restore, evict and drain.
	RoleAdmin Role = "admin"
)

// ParseRole accepts the three role names.
func ParseRole(s string) (Role, error) {
	switch r := Role(strings.ToLower(strings.TrimSpace(s))); r {
	case RoleRead, RoleOperate, RoleAdmin:
		zap.L().Debug("ParseRole: parsed role", zap.String("role", string(r)))
		return r, nil
	}
	zap.L().Debug("ParseRole: unknown role", zap.String("role", s))
	return "", fmt.Errorf("role %q is not one of read, operate, admin", s)
}

// Rank orders roles: a higher rank may do everything a lower one may.
func (r Role) Rank() int {
	switch r {
	case RoleAdmin:
		return 3
	case RoleOperate:
		return 2
	case RoleRead:
		return 1
	}
	return 0
}

// Min is the lesser of two roles.
func Min(a, b Role) Role {
	if a.Rank() <= b.Rank() {
		return a
	}
	return b
}

// Scopes are the OAuth scopes a role is carried in. Each role includes the
// ones below it, so a client asking for less than a token allows gets less.
func (r Role) Scopes() []string {
	switch r {
	case RoleAdmin:
		return []string{ScopeRead, ScopeOperate, ScopeAdmin}
	case RoleOperate:
		return []string{ScopeRead, ScopeOperate}
	case RoleRead:
		return []string{ScopeRead}
	}
	return nil
}

// OAuth scope names.
const (
	ScopeRead    = "haify:read"
	ScopeOperate = "haify:operate"
	ScopeAdmin   = "haify:admin"
)

// RoleFromScopes is the highest role the scopes name.
func RoleFromScopes(scopes []string) Role {
	var best Role
	for _, s := range scopes {
		var r Role
		switch s {
		case ScopeAdmin:
			r = RoleAdmin
		case ScopeOperate:
			r = RoleOperate
		case ScopeRead:
			r = RoleRead
		}
		if r.Rank() > best.Rank() {
			best = r
		}
	}
	zap.L().Debug("RoleFromScopes: highest role named", zap.Strings("scopes", scopes), zap.String("role", string(best)))
	return best
}
