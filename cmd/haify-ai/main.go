// Command haify-ai hosts the Haify AI Copilot backend. It imports the steward
// library, wires it to the read-only haify-mcp cluster tools and the Haify
// knowledge base, and serves a single NDJSON streaming endpoint the haify web-ui
// Copilot sidebar talks to.
//
// It is deliberately a small, separate binary (its own Go module) so the
// AI/vector-DB dependency tree never bloats the lean haify-controller, and so the
// AI is optional and independently deployable. The controller never imports it.
//
// Config via environment (all optional except a knowledge DB + LLM key):
//
//	HAIFY_AI_ADDR           listen address (default "127.0.0.1:7634")
//	HAIFY_AI_KNOWLEDGE_DB   path to the cortexdb knowledge store (required)
//	HAIFY_AI_SHARED_KNOWLEDGE_DB  the Haify shared knowledge base (`make kb`), searched
//	                      alongside it read-only; built with the same embedder
//	HAIFY_AI_DOMAIN         path to ai/domain.toml (default "ai/domain.toml")
//	HAIFY_AI_CONTROLLER     haify controller addr for haify-mcp (default "127.0.0.1:3374")
//	HAIFY_AI_MCP_CMD        haify-mcp executable (default "haify-mcp")
//	HAIFY_AI_EMB_DIM        embedding dim of the knowledge index (default 768)
//	HAIFY_AI_KUBECONFIG     kubeconfig for the haify-k8s tools (`haify-mcp k8s`); unset = bare-metal tools only
//	HAIFY_AI_ALLOW_ORIGIN   CORS allow-origin (default "*")
//	STEWARD_LLM_API_KEY / _BASE_URL / _MODEL    LLM (steward also reads the old OPSPILOT_*, OPSDOCTOR_* and OSS_* names)
//	STEWARD_EMB_MODEL / _BASE_URL / _API_KEY    embedder (must match the index)
package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/liliang-cn/steward"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func atoiOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func main() {
	// Loopback by default. The web UI proxies /ai/* from its own port, so this
	// costs the UI nothing — and the previous ":7634" put an unauthenticated
	// knowledge-base writer on every interface.
	addr := envOr("HAIFY_AI_ADDR", "127.0.0.1:7634")
	knowledgeDB := os.Getenv("HAIFY_AI_KNOWLEDGE_DB")
	if knowledgeDB == "" {
		log.Fatal("HAIFY_AI_KNOWLEDGE_DB is required (path to the knowledge DB)")
	}

	// Settings saved from the UI, layered over the environment. The file lives
	// beside the knowledge DB — on the replicated volume — so a model chosen on
	// one node is already in place when the promoter starts the agent on
	// another.
	setPath := settingsPath(knowledgeDB)
	saved, err := loadSettings(setPath)
	if err != nil {
		log.Fatalf("read settings %s: %v", setPath, err)
	}
	if saved.LLMModel != "" || saved.LLMBaseURL != "" {
		log.Printf("haify-ai: settings from %s override the environment (model=%q base=%q)",
			setPath, saved.LLMModel, saved.LLMBaseURL)
	}

	embDim := atoiOr("HAIFY_AI_EMB_DIM", 768)
	embModel := envOr("STEWARD_EMB_MODEL", envOr("OPSPILOT_EMB_MODEL", envOr("OPSDOCTOR_EMB_MODEL", os.Getenv("OSS_EMB_MODEL"))))

	shared, err := sharedKnowledge(os.Getenv("HAIFY_AI_SHARED_KNOWLEDGE_DB"), embModel, embDim)
	if err != nil {
		log.Fatal(err)
	}

	ag, err := steward.New(steward.Config{
		KnowledgeDBPath: knowledgeDB,
		// Product knowledge is one file, built with the code and installed
		// on every node, so every cluster answers from the same docs, code
		// graph and CLI reference instead of whatever it happened to ingest.
		SharedKnowledgeDBPaths: shared,
		DomainFile:             envOr("HAIFY_AI_DOMAIN", "ai/domain.toml"),
		// Empty fields fall through to the environment, which is what makes the
		// settings file an override rather than a replacement.
		LLMBaseURL: saved.LLMBaseURL,
		LLMModel:   saved.LLMModel,
		LLMAPIKey:  saved.LLMAPIKey,
		// The knowledge index is 768-dim (embeddinggemma); the embedder model comes
		// from STEWARD_EMB_MODEL (must match how the index was built — see spec O1).
		EmbDim: embDim,
		// Mount the haify cluster tools read-only plus the day-to-day writes
		// (dailyOps). Every write is held until the operator approves it in
		// the chat panel, with the arguments it will run with in front of them.
		MCPServers:    mcpServers(),
		ApproveWrites: true,
	})
	if err != nil {
		log.Fatalf("init agent: %v", err)
	}
	defer ag.Close()

	for _, s := range ag.MCPStatus() {
		log.Printf("mcp %q connected=%v tools=%d skipped=%d err=%q",
			s.Name, s.Connected, s.Tools, s.Skipped, s.Err)
	}

	allowOrigin := envOr("HAIFY_AI_ALLOW_ORIGIN", "*")
	mux := http.NewServeMux()
	mux.HandleFunc("/ai/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "mcp": ag.MCPStatus()})
	})
	mux.HandleFunc("/ai/chat/stream", streamHandler(ag))
	mux.HandleFunc("/ai/chat/approve", approveHandler(ag))
	// Knowledge-base update surface (POST /ai/kb/{doc,ingest,refresh,purge}).
	registerKBRoutes(mux, ag)
	// Which model answers, readable always and writable only with a token.
	registerConfigRoutes(mux, ag, &configStore{path: setPath, cur: saved},
		embModel, embDim, resolveToken() != "")

	handler, err := guard(addr, withCORS(allowOrigin, mux))
	if err != nil {
		log.Fatalf("haify-ai: %v", err)
	}

	log.Printf("haify-ai listening on %s (knowledge=%s)", addr, knowledgeDB)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatal(err)
	}
}

// dailyOps are the haify-mcp write tools the agent may call, each only after the
// operator approves that call in the chat panel: the ones the server marks
// non-destructive (create, add, start, mount, resize up, set). Deleting,
// restoring, evicting, draining and stopping are not mounted at all; the agent
// proposes them with suggest_action and the operator runs them from the UI.
var dailyOps = []string{
	"haify_backup_create",
	"haify_gateway_create_iscsi",
	"haify_gateway_create_nfs",
	"haify_gateway_create_nvme",
	"haify_gateway_start",
	"haify_ha_create",
	"haify_iscsi_chap",
	"haify_node_register",
	"haify_node_set_labels",
	"haify_node_undrain",
	"haify_notify_channel_test",
	"haify_pool_add_cache",
	"haify_pool_add_disk",
	"haify_pool_create",
	"haify_resource_add_dr",
	"haify_resource_add_replica",
	"haify_resource_add_volume",
	"haify_resource_adopt",
	"haify_resource_attach_diskless",
	"haify_resource_create",
	"haify_resource_detach_diskless",
	"haify_resource_dual_primary",
	"haify_resource_mount",
	"haify_resource_profile_adjust",
	"haify_resource_profile_create",
	"haify_resource_profile_set_options",
	"haify_resource_resize_volume",
	"haify_resource_set_options",
	"haify_resource_set_profile",
	"haify_resource_set_tiebreaker",
	"haify_snapshot_create",
	"haify_snapshot_schedule_create",
	"haify_wan_repair",
	"haify_wan_set_endpoint",
	"haify_resource_verify",
	"haify_zfs_dataset_create",
	"haify_zfs_snapshot_clone",
	"haify_zfs_volume_create",
	"haify_zfs_volume_resize",
}

// k8sDailyOps are the haify-k8s write tools the agent calls directly.
var k8sDailyOps = []string{"haify_k8s_app_create"}

// mcpServers mounts the two haify-mcp servers: the bare-metal tools, which talk
// to the controller, and — when a kubeconfig is set — the Kubernetes (CSI)
// tools, which talk to the API server. Both are read-only except for their
// day-to-day operations, enforced on both sides: haify-mcp registers nothing
// else that writes, and steward mounts nothing else that does.
func mcpServers() []steward.MCPServerSpec {
	cmd := envOr("HAIFY_AI_MCP_CMD", "haify-mcp")
	specs := []steward.MCPServerSpec{{
		Name:      "haify",
		Transport: "stdio",
		Command:   cmd,
		Args: []string{"--controller", envOr("HAIFY_AI_CONTROLLER", "127.0.0.1:3374"),
			"--allow", strings.Join(dailyOps, ",")},
		ReadOnly:       true,
		WriteToolAllow: dailyOps,
	}}
	if kc := os.Getenv("HAIFY_AI_KUBECONFIG"); kc != "" {
		specs = append(specs, steward.MCPServerSpec{
			Name:           "haify-k8s",
			Transport:      "stdio",
			Command:        cmd,
			Args:           []string{"k8s", "--kubeconfig", kc, "--allow", strings.Join(k8sDailyOps, ",")},
			ReadOnly:       true,
			WriteToolAllow: k8sDailyOps,
		})
	}
	return specs
}

func withCORS(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Expose-Headers", "X-Session-Id")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type chatBody struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id"`
	// Page is what the user has open in the web UI ("Resources", "Resources,
	// filtered to degraded"), so "these two" and "this pool" have a referent.
	Page string `json:"page"`
}

// withPage puts the page the question was asked from in front of it. The page
// is the UI's own short label; anything longer or multi-line is not one and
// is left out rather than passed to the model.
func withPage(page, message string) string {
	page = strings.TrimSpace(page)
	if page == "" || len(page) > 160 || strings.ContainsAny(page, "\r\n") {
		return message
	}
	return "(Asked from the web UI's " + page + " page.)\n\n" + message
}

// streamHandler runs one turn of the agent and streams NDJSON frames matching the
// web-ui aiClient contract:
//
//	{"t":"tool","name":..,"args":{..}}   {"t":"tool_result","name":..}
//	{"t":"text","d":".."}   {"t":"reset"}   {"t":"suggestion", <Suggestion>}
//	{"t":"error","d":".."}   {"t":"done"}
func streamHandler(ag *steward.Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		var body chatBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Message == "" {
			http.Error(w, "missing message", http.StatusBadRequest)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		if body.SessionID != "" {
			w.Header().Set("X-Session-Id", body.SessionID)
		}
		w.WriteHeader(http.StatusOK)

		enc := json.NewEncoder(w)
		frame := func(v any) {
			_ = enc.Encode(v)
			flusher.Flush()
		}

		// The session id the UI sends is what makes a follow-up a follow-up.
		// It used to be accepted, echoed back in X-Session-Id, and then
		// dropped — so every turn of the sidebar started from nothing and
		// "so how do I fix it" had no idea what "it" was. An empty id still
		// runs stateless, which is what a scripted one-shot caller wants.
		_, _, err := ag.Stream(r.Context(), body.SessionID, withPage(body.Page, body.Message), func(ev steward.Event) {
			switch ev.Kind {
			case steward.EventText:
				if ev.Text != "" {
					frame(map[string]any{"t": "text", "d": ev.Text})
				}
			case steward.EventReset:
				frame(map[string]any{"t": "reset"})
			case steward.EventToolCall:
				frame(map[string]any{"t": "tool", "name": ev.Tool, "args": ev.Args})
			case steward.EventToolResult:
				frame(map[string]any{"t": "tool_result", "name": ev.Tool})
			case steward.EventSuggestion:
				if s := ev.Suggestion; s != nil {
					frame(map[string]any{
						"t":        "suggestion",
						"action":   s.Action,
						"params":   s.Params,
						"reason":   s.Reason,
						"severity": s.Severity,
						"verdict": map[string]any{
							"blocked":  s.Verdict.Blocked,
							"ruleId":   s.Verdict.RuleID,
							"reason":   s.Verdict.Reason,
							"severity": string(s.Verdict.Severity),
						},
					})
				}
			case steward.EventApproval:
				if a := ev.Approval; a != nil {
					frame(map[string]any{"t": "approval", "id": a.ID, "server": a.Server, "name": a.Tool, "args": a.Args})
				}
			case steward.EventOutcome:
				if o := ev.Outcome; o != nil {
					frame(map[string]any{"t": "outcome", "status": o.Status, "d": o.Text})
				}
			case steward.EventError:
				frame(map[string]any{"t": "error", "d": ev.Text})
			}
		})
		if err != nil {
			frame(map[string]any{"t": "error", "d": err.Error()})
		}
		frame(map[string]any{"t": "done"})
	}
}

// approveHandler takes the operator's decision on a write call the agent is
// holding (an "approval" frame on its stream): {"id", "approve", "reason"}.
// It answers 404 when nothing is waiting under the id — decided already, or
// its conversation ended — so a second click cannot run a call twice.
func approveHandler(ag *steward.Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			ID      string `json:"id"`
			Approve bool   `json:"approve"`
			Reason  string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		if err := ag.Decide(body.ID, body.Approve, body.Reason); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, steward.ErrNoSuchApproval) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
