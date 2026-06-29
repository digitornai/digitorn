package server

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mbathepaul/digitorn/internal/llm"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// resolveEntryAgent returns the entry agent's id + Brain (session entry override →
// runtime.entry_agent → first agent), matching the runtime's resolveAgent order.
func resolveEntryAgent(def *schema.AppDefinition, sessionEntry string) (string, *schema.Brain) {
	if def == nil || len(def.Agents) == 0 {
		return "", nil
	}
	pick := func(id string) (string, *schema.Brain) {
		for i := range def.Agents {
			if def.Agents[i].ID == id {
				return def.Agents[i].ID, &def.Agents[i].Brain
			}
		}
		return "", nil
	}
	if sessionEntry != "" {
		if id, b := pick(sessionEntry); b != nil {
			return id, b
		}
	}
	if def.Runtime != nil && def.Runtime.EntryAgent != "" {
		if id, b := pick(def.Runtime.EntryAgent); b != nil {
			return id, b
		}
	}
	return def.Agents[0].ID, &def.Agents[0].Brain
}

// agentBrainByID returns the Brain of the agent with the given logical id, or nil.
func agentBrainByID(def *schema.AppDefinition, id string) *schema.Brain {
	if def == nil {
		return nil
	}
	for i := range def.Agents {
		if def.Agents[i].ID == id {
			return &def.Agents[i].Brain
		}
	}
	return nil
}

// --- gateway model catalog (id → kind), cached for switch validation + window resolution ---
const gwCatalogTTL = 15 * time.Minute

var gwCatalog struct {
	mu      sync.Mutex
	at      time.Time
	kinds   map[string]string
	windows map[string]int
}

// gatewayModelKinds fetches the gateway's /models and returns id→kind. The model
// catalog (id → kind/window) is stable infrastructure, so it is cached for
// gwCatalogTTL — long enough that a single warm (e.g. the client opening a
// session) keeps the authoritative window available across a turn sequence.
// Returns an error when the gateway is unreachable — the caller then stays lenient
// (the gateway re-validates the model at turn time anyway).
func (d *Daemon) gatewayModelKinds(ctx context.Context, bearer string) (map[string]string, error) {
	gwCatalog.mu.Lock()
	if gwCatalog.kinds != nil && time.Since(gwCatalog.at) < gwCatalogTTL {
		k := gwCatalog.kinds
		gwCatalog.mu.Unlock()
		return k, nil
	}
	gwCatalog.mu.Unlock()

	base := ""
	if d.cfg != nil {
		base = strings.TrimRight(d.cfg.Workers.LLM.GatewayURL, "/")
	}
	if base == "" {
		return nil, fmt.Errorf("no gateway url configured")
	}
	// The gateway's OpenAI-compatible catalog lives at /v1/models. The configured
	// base may or may not already include /v1 (prod omits it, local includes it),
	// so trim it first to never produce /v1/v1/models.
	base = strings.TrimSuffix(base, "/v1")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID               string `json:"id"`
			Kind             string `json:"kind"`
			MaxContextTokens int    `json:"max_context_tokens"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	kinds := make(map[string]string, len(body.Data))
	windows := make(map[string]int, len(body.Data))
	for _, m := range body.Data {
		kinds[m.ID] = m.Kind
		if m.MaxContextTokens > 0 {
			windows[m.ID] = m.MaxContextTokens
		}
	}
	gwCatalog.mu.Lock()
	gwCatalog.kinds, gwCatalog.windows, gwCatalog.at = kinds, windows, time.Now()
	gwCatalog.mu.Unlock()
	return kinds, nil
}

// gatewayModelWindow returns the gateway's documented context window for a model
// (max_context_tokens), or 0 when unknown — the cache is filled by the session-model
// endpoints' authenticated gatewayModelKinds calls (the daemon has no service token
// of its own). 0 lets the caller fall back to the app's configured window.
func (d *Daemon) gatewayModelWindow(model string) int {
	if model == "" {
		return 0
	}
	gwCatalog.mu.Lock()
	defer gwCatalog.mu.Unlock()
	return gwCatalog.windows[model]
}

// getSessionModel lists, per agent, the effective model + declared default /
// alternatives / kind + current override, plus the routing mode (gateway/BYOK).
func (d *Daemon) getSessionModel(w http.ResponseWriter, r *http.Request) {
	sid := sessionIDParam(r)
	appID := chi.URLParam(r, "app_id")
	state, err := d.requireOwnedSession(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	state.RLock()
	sessionEntry := state.EntryAgent
	overrides := make(map[string]string, len(state.ModelOverrides))
	maps.Copy(overrides, state.ModelOverrides)
	effortOverrides := make(map[string]string, len(state.EffortOverrides))
	maps.Copy(effortOverrides, state.EffortOverrides)
	sessionEffort := state.ReasoningEffort
	state.RUnlock()

	// Warm the gateway window cache while we hold a user token, so the background
	// recount can resolve the SELECTED model's real window without a token of its own.
	if bearer := extractBearer(r); bearer != "" {
		go func() { _, _ = d.gatewayModelKinds(context.Background(), bearer) }()
	}

	def, err := d.appMgr.GetManifest(r.Context(), appID)
	if err != nil {
		writeError(w, appMgrErrStatus(err), "manifest_failed", err.Error())
		return
	}
	entryID, _ := resolveEntryAgent(def, sessionEntry)
	byok := false
	if app, err := d.appMgr.GetApp(r.Context(), appID); err == nil && app != nil {
		byok = app.BYOK
	}

	agents := make([]map[string]any, 0, len(def.Agents))
	for i := range def.Agents {
		a := &def.Agents[i]
		ov := overrides[a.ID]
		effective := a.Brain.Model
		if ov != "" {
			effective = ov
		}
		effOv := effortOverrides[a.ID]
		effEffective := effOv
		if effEffective == "" {
			effEffective = sessionEffort
		}
		agents = append(agents, map[string]any{
			"agent":           a.ID,
			"role":            a.Role,
			"entry":           a.ID == entryID,
			"model":           effective,
			"override":        ov,
			"default":         a.Brain.Model,
			"declared":        a.Brain.Models,
			"kind":            a.Brain.Kind,
			"effort":          effEffective,
			"effort_override": effOv,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entry":  entryID,
		"byok":   byok,
		"agents": agents,
	})
}

// putSessionModel sets (empty model clears) the override for one agent, defaulting
// to the entry agent. The model is validated against that agent's brain.
func (d *Daemon) putSessionModel(w http.ResponseWriter, r *http.Request) {
	sid := sessionIDParam(r)
	appID := chi.URLParam(r, "app_id")
	userID := userIDOf(r.Context())
	state, err := d.requireOwnedSession(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	var req struct {
		Agent          string `json:"agent"`
		Model          string `json:"model"`
		MaxContextTokens int  `json:"max_ctx_tokens"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	model := strings.TrimSpace(req.Model)
	agentID := strings.TrimSpace(req.Agent)

	state.RLock()
	sessionEntry := state.EntryAgent
	state.RUnlock()
	def, err := d.appMgr.GetManifest(r.Context(), appID)
	if err != nil {
		writeError(w, appMgrErrStatus(err), "manifest_failed", err.Error())
		return
	}
	// No agent named → target the entry agent. Otherwise resolve that exact agent.
	var brain *schema.Brain
	if agentID == "" {
		agentID, brain = resolveEntryAgent(def, sessionEntry)
	} else {
		brain = agentBrainByID(def, agentID)
	}
	if agentID == "" || brain == nil {
		writeError(w, http.StatusBadRequest, "agent_unknown", "no such agent in this app")
		return
	}

	if model != "" {
		byok := false
		if app, err := d.appMgr.GetApp(r.Context(), appID); err == nil && app != nil {
			byok = app.BYOK
		}
		if byok {
			// Direct mode: switch only within the declared models.
			if model != brain.Model && !slices.Contains(brain.Models, model) {
				writeError(w, http.StatusBadRequest, "model_not_declared",
					"direct mode: the model must be one declared in the agent brain")
				return
			}
		} else if kinds, err := d.gatewayModelKinds(r.Context(), extractBearer(r)); err == nil {
			// Gateway mode: the model must be served with the agent's kind.
			k, ok := kinds[model]
			if !ok {
				writeError(w, http.StatusBadRequest, "model_unknown", "model not provided by the gateway")
				return
			}
			if brain.Kind != "" && k != brain.Kind {
				writeError(w, http.StatusBadRequest, "kind_mismatch",
					fmt.Sprintf("model kind %q does not match the agent kind %q", k, brain.Kind))
				return
			}
		}
	}

	ctxApp, cancel := appendCtx(r.Context())
	defer cancel()
	// Resolve max_context_tokens: client-provided > gateway cache > 0
	if req.MaxContextTokens > 0 {
		// Client sent it (e.g. TUI knows via ACP)
	} else if bearer := extractBearer(r); bearer != "" {
		// Try the gateway cache
		_, _ = d.gatewayModelKinds(ctxApp, bearer)
		req.MaxContextTokens = d.gatewayModelWindow(model)
	} else {
		// No bearer — try cached value from a previous GET
		req.MaxContextTokens = d.gatewayModelWindow(model)
	}
	maxCtx := req.MaxContextTokens
	if _, err := d.sessionStore.AppendDurable(ctxApp, sessionstore.Event{
		Type:      sessionstore.EventModelChanged,
		SessionID: sid,
		AppID:     appID,
		UserID:    userID,
		Meta:      &sessionstore.MetaPayload{Model: model, AgentID: agentID, MaxContextTokens: maxCtx, ReasoningEffort: strings.TrimSpace(req.ReasoningEffort)},
	}); err != nil {
		writeError(w, appendErrStatus(err), "append_failed", err.Error())
		return
	}
	if err := d.sessionStore.SyncMetaToDisk(sid); err != nil {
		d.logger.Warn("putSessionModel: sync meta failed", "sid", sid, "err", err.Error())
	}

	// Refresh the context tracker so the new model's window is reflected
	// immediately (display + next-turn pressure check) without waiting for a
	// background recount cycle.
	d.touchContext(sid)

	// Reactive compaction: if the session's current context already exceeds the
	// new model's usable window, compact NOW rather than waiting for the next
	// turn_start hook or per-round guard. This covers the common case where the
	// user switches to a model with a smaller context window mid-session.
	// We fire it in a detached goroutine so the PUT response returns immediately.
	// The compactor resolves the proper strategy/keep_recent from the app config
	// and is a no-op when there is nothing to drop (SafeSplitIndex == 0).
	if maxCtx > 0 && d.compactor != nil {
		if st, err := d.sessionStore.State(sid); err == nil && st != nil {
			snap := st.Snapshot()
			if snap.ContextTokens > maxCtx {
				jwt := extractBearer(r)
				go func() {
					cctx := llm.WithUserJWT(context.Background(), jwt)
					if cerr := d.compactor.CompactSession(cctx, sid, "", 0); cerr != nil {
						d.logger.Warn("putSessionModel: reactive compaction failed",
							"sid", sid, "err", cerr.Error())
					}
				}()
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"agent": agentID, "model": model})
}
