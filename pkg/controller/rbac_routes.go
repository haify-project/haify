package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.uber.org/zap"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/rbac"
)

// dbRBACStore persists the RBAC snapshot inside the controller's BBolt
// database, so runtime user/role changes survive restarts without any extra
// datastore.
//
// The snapshot holds every user's token, so traces give its size only.
type dbRBACStore struct{ db *database.DB }

func (s dbRBACStore) Load() (*rbac.Snapshot, error) {
	log := zap.L()
	start := time.Now()
	data, err := s.db.LoadRBAC(context.Background())
	log.Debug("dbRBACStore.Load: read RBAC snapshot", zap.Int("bytes", len(data)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		log.Debug("dbRBACStore.Load: no snapshot stored")
		return nil, nil
	}
	var snap rbac.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		log.Debug("dbRBACStore.Load: snapshot unreadable", zap.Error(err))
		return nil, err
	}
	return &snap, nil
}

func (s dbRBACStore) Save(snap *rbac.Snapshot) error {
	log := zap.L()
	data, err := json.Marshal(snap)
	if err != nil {
		log.Debug("dbRBACStore.Save: snapshot does not marshal", zap.Error(err))
		return err
	}
	start := time.Now()
	err = s.db.SaveRBAC(context.Background(), data)
	log.Debug("dbRBACStore.Save: wrote RBAC snapshot", zap.Int("bytes", len(data)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// registerRBACRoutes adds read-only RBAC introspection endpoints to the REST
// gateway so the web UI and CLI can show "who am I / what can I do" and, for
// admins, the effective policy. They live on the gateway mux (port 3375), so
// the UI reaches them via its existing /api proxy. engine is nil when RBAC is
// disabled, in which case the endpoints report enabled=false.
func (c *Controller) registerRBACRoutes(mux *runtime.ServeMux, engine *rbac.Engine) {
	log := c.traceLogger()
	log.Debug("Controller.registerRBACRoutes: registering", zap.Bool("rbac_enabled", engine != nil))
	routes := []struct {
		method, path string
		h            runtime.HandlerFunc
	}{
		{"GET", "/v1/rbac/whoami", rbacWhoamiHandler(engine, c.config != nil && c.config.Auth.Enabled)},
		{"GET", "/v1/rbac/policies", rbacPoliciesHandler(engine)},
		{"GET", "/v1/rbac/roles", rbacRolesHandler(engine)},
		{"POST", "/v1/rbac/users", rbacCreateUserHandler(engine, c.approvals)},
		{"DELETE", "/v1/rbac/users/{name}", rbacDeleteUserHandler(engine, c.approvals)},
		{"PUT", "/v1/rbac/users/{name}/role", rbacSetRoleHandler(engine, c.approvals)},
	}
	for _, rt := range routes {
		err := mux.HandlePath(rt.method, rt.path, rt.h)
		log.Debug("Controller.registerRBACRoutes: route", zap.String("method", rt.method),
			zap.String("path", rt.path), zap.Error(err))
		if err != nil {
			c.logger.Error("failed to register RBAC route",
				zap.String("path", rt.path), zap.Error(err))
		}
	}
}

// adminCaller resolves the caller and ensures administrative rights, writing
// the appropriate error response and returning false if not.
func adminCaller(engine *rbac.Engine, w http.ResponseWriter, r *http.Request) (string, bool) {
	log := zap.L()
	if engine == nil {
		log.Debug("adminCaller: RBAC not enabled", zap.String("path", r.URL.Path))
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "RBAC is not enabled"})
		return "", false
	}
	token := bearerFromHeader(r)
	user, _, ok := engine.Whoami(token)
	if !ok {
		log.Debug("adminCaller: token not recognised", zap.String("path", r.URL.Path), zap.Bool("has_token", token != ""))
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unknown or missing API token"})
		return "", false
	}
	if !engine.CanAdmin(user) {
		log.Debug("adminCaller: caller is not an administrator", zap.String("path", r.URL.Path), zap.String("user", user))
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "administrator role required"})
		return "", false
	}
	log.Debug("adminCaller: administrator", zap.String("path", r.URL.Path), zap.String("user", user))
	return user, true
}

// approvedHTTP holds back a user change that [rbac.approval] lists, as the
// gRPC interceptor does for the RPC of the same name: these routes call the
// RBAC engine directly, so without it they would be the way around approval.
func approvedHTTP(g *approvalGate, w http.ResponseWriter, r *http.Request, user, method string, req proto.Message) bool {
	if err := g.check(r.Context(), method, user, req); err != nil {
		zap.L().Debug("approvedHTTP: held for approval or refused", zap.String("method", method), zap.String("user", user),
			zap.Error(err))
		writeJSON(w, http.StatusPreconditionFailed, map[string]any{"error": status.Convert(err).Message()})
		return false
	}
	zap.L().Debug("approvedHTTP: approved", zap.String("method", method), zap.String("user", user))
	return true
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func bearerFromHeader(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("Authorization"))
	return strings.TrimSpace(strings.TrimPrefix(v, "Bearer"))
}

// rbacWhoamiHandler reports the caller's identity and role.
// tokenAuth says whether [auth] is on, so a page can tell "one shared token"
// apart from "no authentication at all" when RBAC is off.
func rbacWhoamiHandler(engine *rbac.Engine, tokenAuth bool) runtime.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, _ map[string]string) {
		log := zap.L()
		if engine == nil {
			log.Debug("rbacWhoamiHandler: RBAC off", zap.Bool("token_auth", tokenAuth))
			writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false, "token_auth": tokenAuth})
			return
		}
		token := bearerFromHeader(r)
		user, role, ok := engine.Whoami(token)
		if !ok {
			log.Debug("rbacWhoamiHandler: token not recognised", zap.Bool("has_token", token != ""))
			writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
				"enabled": true,
				"error":   "unknown or missing API token",
			})
			return
		}
		log.Debug("rbacWhoamiHandler: caller", zap.String("user", user), zap.String("role", role))
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"enabled":   true,
			"user":      user,
			"role":      role,
			"can_admin": engine.CanAdmin(user),
		})
	}
}

// rbacPoliciesHandler returns the effective policy and user assignments. This
// is sensitive, so it is restricted to administrators.
func rbacPoliciesHandler(engine *rbac.Engine) runtime.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, _ map[string]string) {
		log := zap.L()
		if engine == nil {
			log.Debug("rbacPoliciesHandler: RBAC off")
			writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
			return
		}
		token := bearerFromHeader(r)
		user, _, ok := engine.Whoami(token)
		if !ok {
			log.Debug("rbacPoliciesHandler: token not recognised", zap.Bool("has_token", token != ""))
			writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
				"error": "unknown or missing API token",
			})
			return
		}
		if !engine.CanAdmin(user) {
			log.Debug("rbacPoliciesHandler: caller is not an administrator", zap.String("user", user))
			writeJSON(w, http.StatusForbidden, map[string]interface{}{
				"error": "administrator role required",
			})
			return
		}
		log.Debug("rbacPoliciesHandler: returning policies", zap.String("user", user))
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"enabled":  true,
			"policies": engine.EffectivePolicies(),
			"users":    engine.Users(),
		})
	}
}

// rbacRolesHandler returns the known role names for selection UIs.
func rbacRolesHandler(engine *rbac.Engine) runtime.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, _ map[string]string) {
		log := zap.L()
		if engine == nil {
			log.Debug("rbacRolesHandler: RBAC off")
			writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
			return
		}
		token := bearerFromHeader(r)
		if _, _, ok := engine.Whoami(token); !ok {
			log.Debug("rbacRolesHandler: token not recognised", zap.Bool("has_token", token != ""))
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unknown or missing API token"})
			return
		}
		roles := engine.Roles()
		log.Debug("rbacRolesHandler: returning roles", zap.Int("count", len(roles)))
		writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "roles": roles})
	}
}

// rbacCreateUserHandler adds a user and returns the (possibly generated) token
// once. Admin only. The token is never traced, only whether one was supplied.
func rbacCreateUserHandler(engine *rbac.Engine, g *approvalGate) runtime.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, _ map[string]string) {
		log := zap.L()
		user, ok := adminCaller(engine, w, r)
		if !ok {
			return
		}
		var body struct {
			Name  string `json:"name"`
			Role  string `json:"role"`
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			log.Debug("rbacCreateUserHandler: body does not decode", zap.String("caller", user))
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
			return
		}
		log.Debug("rbacCreateUserHandler: creating user", zap.String("caller", user), zap.String("user", body.Name),
			zap.String("role", body.Role), zap.Bool("has_token", strings.TrimSpace(body.Token) != ""))
		if !approvedHTTP(g, w, r, user, "CreateRbacUser",
			&pb.CreateRbacUserRequest{Name: strings.TrimSpace(body.Name), Role: body.Role, Token: strings.TrimSpace(body.Token)}) {
			return
		}
		token, err := engine.CreateUser(strings.TrimSpace(body.Name), strings.TrimSpace(body.Token), body.Role)
		log.Debug("rbacCreateUserHandler: created", zap.String("user", body.Name), zap.Bool("ok", err == nil),
			zap.Bool("generated_token", err == nil && strings.TrimSpace(body.Token) == ""), zap.Error(err))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"name":  body.Name,
			"role":  body.Role,
			"token": token,
		})
	}
}

// rbacDeleteUserHandler removes a runtime-managed user. Admin only.
func rbacDeleteUserHandler(engine *rbac.Engine, g *approvalGate) runtime.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
		log := zap.L()
		user, ok := adminCaller(engine, w, r)
		if !ok {
			return
		}
		log.Debug("rbacDeleteUserHandler: deleting user", zap.String("caller", user), zap.String("user", pathParams["name"]))
		if !approvedHTTP(g, w, r, user, "DeleteRbacUser", &pb.DeleteRbacUserRequest{Name: pathParams["name"]}) {
			return
		}
		err := engine.DeleteUser(pathParams["name"])
		log.Debug("rbacDeleteUserHandler: deleted", zap.String("user", pathParams["name"]), zap.Error(err))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// rbacSetRoleHandler changes a user's role. Admin only.
func rbacSetRoleHandler(engine *rbac.Engine, g *approvalGate) runtime.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
		log := zap.L()
		user, ok := adminCaller(engine, w, r)
		if !ok {
			return
		}
		var body struct {
			Role string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			log.Debug("rbacSetRoleHandler: body does not decode", zap.String("caller", user))
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
			return
		}
		log.Debug("rbacSetRoleHandler: setting role", zap.String("caller", user), zap.String("user", pathParams["name"]),
			zap.String("role", body.Role))
		if !approvedHTTP(g, w, r, user, "SetRbacUserRole", &pb.SetRbacUserRoleRequest{Name: pathParams["name"], Role: body.Role}) {
			return
		}
		err := engine.SetRole(pathParams["name"], body.Role)
		log.Debug("rbacSetRoleHandler: set", zap.String("user", pathParams["name"]), zap.String("role", body.Role), zap.Error(err))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
