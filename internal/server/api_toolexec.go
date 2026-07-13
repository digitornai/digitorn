package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/go-chi/chi/v5"
)

// toolExecRequest is what POST /api/apps/{app_id}/sessions/{session_id}/tools/execute
// accepts: one tool call, exactly as an agent would emit it (bare or dotted
// name, raw params — the meta-dispatcher does its usual resolution/coercion).
type toolExecRequest struct {
	Tool    string         `json:"tool"`
	Params  map[string]any `json:"params,omitempty"`
	AgentID string         `json:"agent_id,omitempty"`
}

// devToolExecute drives ONE gated tool call through the exact path an agent
// turn uses (Engine.ExecuteToolGated: gates → meta-dispatcher name resolution →
// param coercion → module → doc-sentinel). This is the dogfooding surface: an
// external operator (or another AI) exercises a session's tools and sees
// byte-for-byte what the in-app agent would see. Dev-mode only.
func (d *Daemon) devToolExecute(w http.ResponseWriter, r *http.Request) {
	eng, ok := d.engine.(*runtime.Engine)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "no_engine", "runtime engine not wired")
		return
	}
	var req toolExecRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if strings.TrimSpace(req.Tool) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "tool required")
		return
	}
	sid := sessionIDParam(r)
	if _, err := d.requireOwnedSession(r.Context(), sid); err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	ctx := tool.WithEventBus(r.Context(), d.eventBus)

	out := eng.ExecuteToolGated(ctx, runtime.ToolInvocation{
		CallID:    fmt.Sprintf("ext-%d", time.Now().UnixNano()),
		Name:      req.Tool,
		Args:      req.Params,
		AppID:     chi.URLParam(r, "app_id"),
		AgentID:   req.AgentID,
		SessionID: sid,
		UserID:    userIDOf(ctx),
		UserJWT:   jwt,
	})

	var text strings.Builder
	for _, p := range out.Parts {
		text.WriteString(p.Text)
	}
	resp := map[string]any{
		"status":      out.Status,
		"result":      text.String(),
		"duration_ms": out.DurationMs,
	}
	if out.Error != "" {
		resp["error"] = out.Error
	}
	if len(out.Metadata) > 0 {
		resp["metadata"] = out.Metadata
	}
	writeJSON(w, http.StatusOK, resp)
}

// devToolSurface returns the exact tool surface an agent of this app sees —
// names, descriptions, JSON-Schema params — via the same assembly as a turn.
// Lets an external tester verify what is exposed/injected without opening a
// chat session. Dev-mode only.
func (d *Daemon) devToolSurface(w http.ResponseWriter, r *http.Request) {
	eng, ok := d.engine.(*runtime.Engine)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "no_engine", "runtime engine not wired")
		return
	}
	appID := chi.URLParam(r, "app_id")
	sysPrompt, tools, err := eng.VoiceContext(r.Context(), appID, r.URL.Query().Get("agent"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "surface_failed", err.Error())
		return
	}
	list := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		list = append(list, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"parameters":  t.Parameters,
		})
	}
	resp := map[string]any{
		"app_id":            appID,
		"tools":             list,
		"system_prompt_len": len(sysPrompt),
		"index_fqns":        eng.ToolIndexFQNs(r.Context(), appID, r.URL.Query().Get("agent")),
	}
	if cb, ok := eng.Context.(interface{ CacheDebug() []string }); ok {
		resp["index_cache"] = cb.CacheDebug()
	}
	writeJSON(w, http.StatusOK, resp)
}
