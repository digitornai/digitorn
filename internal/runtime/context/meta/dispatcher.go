package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"

	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/context/index"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/digitornai/digitorn/internal/runtime/toolname"
)

type MetaDispatcher struct {
	IndexLookup func(appID, agentID string) *index.ToolIndex

	Inner runtime.ToolDispatcher

	BrowsePageSize int

	AskUser AskUserBridge

	Background BackgroundManager

	Agents AgentManager

	KV AgentKVStore

	CoordinatorLookup func(appID, agentID string) bool

	Memory MemoryWriter

	Progress func(ctx context.Context, ev sessionstore.Event)

	SkillLoader SkillLoader

	AppCaller AppCaller

	Gate SubToolGate

	Logger *slog.Logger
}

func (m *MetaDispatcher) CallAppWired() bool  { return m != nil && m.AppCaller != nil }
func (m *MetaDispatcher) AskUserWired() bool  { return m != nil && m.AskUser != nil }
func (m *MetaDispatcher) UseSkillWired() bool { return m != nil && m.SkillLoader != nil }

type SubToolGate interface {
	GateSubTool(ctx context.Context, inv runtime.ToolInvocation) *runtime.ToolOutcome
}

func (m *MetaDispatcher) gateTarget(ctx context.Context, target runtime.ToolInvocation) *runtime.ToolOutcome {
	if m.Gate == nil {
		return nil
	}
	return m.Gate.GateSubTool(ctx, target)
}

func (m *MetaDispatcher) resolveIndex(call runtime.ToolInvocation) *index.ToolIndex {
	if m == nil || m.IndexLookup == nil {
		return nil
	}
	return m.IndexLookup(call.AppID, call.AgentID)
}

func (m *MetaDispatcher) IndexFQNs(appID, agentID string) []string {
	if m == nil || m.IndexLookup == nil {
		return nil
	}
	idx := m.IndexLookup(appID, agentID)
	if idx == nil {
		return nil
	}
	return idx.FQNList()
}

func (m *MetaDispatcher) ResolveToolName(appID, agentID, name string) string {
	canonical := ResolveAlias(Canonicalize(name))
	if strings.Contains(canonical, ".") || m == nil || m.IndexLookup == nil {
		return canonical
	}
	idx := m.IndexLookup(appID, agentID)
	if idx == nil {
		return canonical
	}
	fqns := idx.FQNList()
	canonical = toolname.QualifyBareName(canonical, fqns)
	if !strings.Contains(canonical, ".") {
		canonical = toolname.ResolveMangled(canonical, fqns)
	}
	return canonical
}

const DefaultBrowsePageSize = 20

func (m *MetaDispatcher) Dispatch(ctx context.Context, call runtime.ToolInvocation) (out runtime.ToolOutcome) {
	defer func() {
		if r := recover(); r != nil {
			logger := m.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Error("tool dispatch panicked (recovered)",
				slog.String("tool", call.Name),
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())))
			out = runtime.ToolOutcome{
				Status: "errored",
				Error:  fmt.Sprintf("tool=%s: internal error (panic recovered): %v", call.Name, r),
			}
		}
	}()
	canonical := ResolveAlias(Canonicalize(call.Name))
	// UNIVERSAL bare-action recovery. This is the single chokepoint EVERY tool
	// call transits — top-level, execute_tool, run_parallel / background_run
	// children (they re-enter Dispatch), and any future primitive. A weak model
	// that drops the module prefix ("read" instead of "filesystem.read") would
	// otherwise be denied by the gates with an empty module. We qualify it from
	// the per-agent index, which automatically contains every loaded module —
	// current and future — so no module ever needs bespoke handling. Meta /
	// memory / agent_spawn names are already module-qualified by ResolveAlias
	// above (or have no module), so they're untouched. Ambiguous / unknown
	// actions are left as-is and the gate reports honestly.
	if !strings.Contains(canonical, ".") {
		if idx := m.resolveIndex(call); idx != nil {
			fqns := idx.FQNList()
			canonical = toolname.QualifyBareName(canonical, fqns)
			// Models freely swap "." and "_" in tool names (especially MCP names
			// like mcp_<server>.<tool> → mcp_<server>_<tool>); recover those
			// against the known FQN set so the gate sees the real tool, not a
			// bogus module.
			if !strings.Contains(canonical, ".") {
				canonical = toolname.ResolveMangled(canonical, fqns)
			}
		}
	}

	if IsContextBuilderMeta(canonical) {
		return m.dispatchMetaTool(ctx, canonical, call)
	}
	// Module-gated internal tools : intercepted here (they're runtime
	// subsystems, not bus modules) and never forwarded to Inner. They're
	// only injected when the app opts into the owning module, so reaching
	// this point means the LLM was offered them.
	if IsMemoryTool(canonical) {
		return m.dispatchMemoryTool(ctx, canonical, call)
	}
	if IsAgentSpawnTool(canonical) {
		return m.handleAgent(ctx, call)
	}
	if canonical == "kv" || strings.HasSuffix(canonical, ".kv") {
		return m.handleKV(call)
	}

	// Domain tool : forward to inner dispatcher. Rewrite the call's
	// Name to the canonical form so the inner dispatcher always sees
	// dots (its catalog uses dots as the FQN convention).
	if m.Inner == nil {
		return runtime.ToolOutcome{
			Status: "errored",
			Error:  "tool dispatcher not wired (tool=" + canonical + ")",
		}
	}
	canonicalCall := call
	canonicalCall.Name = canonical
	return m.Inner.Dispatch(ctx, canonicalCall)
}

// dispatchMetaTool handles the 5 context_builder meta-tools.
// The per-agent index is resolved once via IndexLookup and passed
// into each read-only handler. execute_tool delegates back to
// Dispatch (so the security gates + audit row apply, just like a
// direct LLM call would) and therefore doesn't need the index.
func (m *MetaDispatcher) dispatchMetaTool(ctx context.Context, canonical string, call runtime.ToolInvocation) runtime.ToolOutcome {
	action := canonical[len("context_builder."):]
	switch action {
	case "search_tools":
		return m.handleSearchTools(m.resolveIndex(call), call.Args)
	case "get_tool":
		return m.handleGetTool(m.resolveIndex(call), call.Args)
	case "execute_tool":
		return m.handleExecuteTool(ctx, call)
	case "list_categories":
		return m.handleListCategories(m.resolveIndex(call))
	case "browse_category":
		return m.handleBrowseCategory(m.resolveIndex(call), call.Args)
	// P-1 always-direct primitives.
	case "run_parallel":
		return m.handleRunParallel(ctx, call)
	case "ask_user":
		return m.handleAskUser(ctx, call)
	case "background_run":
		return m.handleBackgroundRun(ctx, call)
	case "use_skill":
		return m.handleUseSkill(ctx, call)
	case "call_app":
		return m.handleCallApp(ctx, call)
	default:
		return errored("unknown meta-tool: " + action)
	}
}

// dispatchMemoryTool routes the `memory` module's 4 LLM-exposed actions
// (set_goal / remember / task_create / task_update) to the MemoryWriter. Only
// reached when the app declared tools.modules.memory (the wiring then offered
// the tools). The `memory.` prefix is stripped here ; the canonical FQN is the
// doc-conform identity used by the YAML capabilities + tool catalog.
func (m *MetaDispatcher) dispatchMemoryTool(ctx context.Context, canonical string, call runtime.ToolInvocation) runtime.ToolOutcome {
	switch canonical[len("memory."):] {
	case "set_goal":
		return m.handleSetGoal(ctx, call)
	case "remember":
		return m.handleRemember(ctx, call)
	case "task_create":
		return m.handleTaskCreate(ctx, call)
	case "task_update":
		return m.handleTaskUpdate(ctx, call)
	default:
		return errored("unknown memory tool: " + canonical)
	}
}

// --- handlers -------------------------------------------------------

// handleSearchTools is the UNIFIED discovery handler (search_tools merges the
// former search_tools + list_categories + browse_category). It dispatches on
// params :
//
//   - query="..."    → hybrid search, returns ranked hits.
//   - category="..." → list every tool in that domain (delegates to browse).
//   - neither        → list the available domains/categories.
//
// Output (JSON inside Parts[0].Text) is the shape of whichever mode ran.
func (m *MetaDispatcher) handleSearchTools(idx *index.ToolIndex, args map[string]any) runtime.ToolOutcome {
	if idx == nil {
		return errored("tool index unavailable")
	}
	query, _ := args["query"].(string)
	if query == "" {
		// No query → browse a category if given, else list the categories.
		if cat, _ := args["category"].(string); cat != "" {
			return m.handleBrowseCategory(idx, args)
		}
		return m.handleListCategories(idx)
	}
	limit := intArg(args, "limit", 5)
	// Clamp: a negative limit panics make([]T,0,limit); a huge one overflows
	// fetch=limit*6 and allocates absurdly. The LLM never needs >200 hits.
	if limit < 1 {
		limit = 5
	}
	if limit > 200 {
		limit = 200
	}
	scope, _ := args["category"].(string) // optional : search WITHIN a domain
	maxRisk, _ := args["max_risk"].(string)
	detail, _ := args["detail"].(bool)

	// Over-fetch when filtering so the post-filter still yields ~limit hits.
	fetch := limit
	if scope != "" || maxRisk != "" {
		fetch = limit * 6
		if fetch < 30 {
			fetch = 30
		}
	}
	results := idx.Search(query, fetch)

	hits := make([]map[string]any, 0, limit)
	for _, r := range results {
		t := r.Tool
		if t == nil {
			continue
		}
		if scope != "" && t.Module != scope {
			continue
		}
		if maxRisk != "" && riskRank(string(t.RiskLevel)) > riskRank(maxRisk) {
			continue
		}
		hit := map[string]any{
			"name":        t.FQN,
			"description": t.Description,
			"risk_level":  string(t.RiskLevel),
			"score":       r.Score,
		}
		if detail {
			// One-hop discovery : ship the full callable signature so the model
			// can invoke the tool immediately, no get_tool round-trip.
			hit["params"] = toolParamMaps(t)
			hit["irreversible"] = t.Irreversible
			if len(t.Tags) > 0 {
				hit["tags"] = t.Tags
			}
			if len(t.Aliases) > 0 {
				hit["aliases"] = t.Aliases
			}
		}
		hits = append(hits, hit)
		if len(hits) >= limit {
			break
		}
	}
	out := map[string]any{"hits": hits}
	if detail {
		out["note"] = "params included — call these tools directly with the shown parameters (no get_tool needed)."
	}
	return jsonOutcome(out)
}

// riskRank orders risk levels so max_risk can filter (low < medium < high).
// Unknown / empty values rank as medium (the documented default).
func riskRank(r string) int {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case "low":
		return 1
	case "high":
		return 3
	default:
		return 2
	}
}

// toolParamMaps renders an indexed tool's parameters as the JSON-friendly
// list used by both get_tool and search_tools(detail=true).
func toolParamMaps(t *index.IndexedTool) []map[string]any {
	params := make([]map[string]any, 0, len(t.Params))
	for _, p := range t.Params {
		params = append(params, map[string]any{
			"name":        p.Name,
			"type":        p.Type,
			"description": p.Description,
			"required":    p.Required,
			"default":     p.Default,
		})
	}
	return params
}

// intArg extracts an integer arg tolerant of the float64 JSON numbers carry.
func intArg(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
}

// handleGetTool : return the full schema of one tool by FQN.
//
// Args : {"name": "filesystem.read"}.
// Output : {name, description, params: [...], risk_level,
//
//	irreversible, tags, aliases, permissions}.
func (m *MetaDispatcher) handleGetTool(idx *index.ToolIndex, args map[string]any) runtime.ToolOutcome {
	if idx == nil {
		return errored("tool index unavailable")
	}
	name, _ := args["name"].(string)
	if name == "" {
		return errored("get_tool: 'name' is required")
	}
	canonical := Canonicalize(name)
	t := idx.Get(canonical)
	if t == nil {
		return errored("tool not found: " + canonical)
	}
	return jsonOutcome(map[string]any{
		"name":         t.FQN,
		"description":  t.Description,
		"params":       toolParamMaps(t),
		"risk_level":   string(t.RiskLevel),
		"irreversible": t.Irreversible,
		"tags":         t.Tags,
		"aliases":      t.Aliases,
		"permissions":  t.Permissions,
	})
}

// handleExecuteTool : dispatch any tool by name. Re-enters
// Dispatch() so the call goes through the same security pipeline
// as a direct LLM call would.
//
// Documented shape : {"name": "filesystem.read", "params": {"path": "..."}}.
//
// Tolerant shapes : real LLMs frequently flatten the nested params
// object (especially smaller models), passing
// {"name": "filesystem.read", "path": "..."} directly. We accept
// either form so a doc-correct prompt is not the only path to a
// successful execute_tool call. The "name" key is reserved.
//
// "arguments" / "args" are also accepted as synonyms of "params"
// because OpenAI's own tool-call surface uses "arguments" and some
// LLMs reproduce that habit.
//
// Output : the wrapped tool's ToolOutcome, verbatim.
func (m *MetaDispatcher) handleExecuteTool(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	name, _ := call.Args["name"].(string)
	if name == "" {
		return errored("execute_tool: 'name' is required")
	}

	params, perr := extractExecuteToolParams(call.Args)
	if perr != nil {
		return errored(fmt.Sprintf(
			"execute_tool: `params` for %q is not valid JSON — %s. Resend `params` as a JSON object (NOT a stringified blob); for a large value, verify every quote inside it is escaped.",
			name, perr.Error()))
	}

	// Build a fresh ToolInvocation with the resolved target. CallID,
	// AppID, AgentID, UserID, AgentRunID and UserJWT are preserved so the
	// audit row / dispatcher / IndexLookup receive the original routing
	// context — and so delegation via execute_tool keeps the caller's
	// identity and gateway credential.
	target := runtime.ToolInvocation{
		CallID:     call.CallID,
		Name:       ResolveAlias(Canonicalize(name)),
		Args:       params,
		AppID:      call.AppID,
		AgentID:    call.AgentID,
		UserID:     call.UserID,
		SessionID:  call.SessionID,
		AgentRunID: call.AgentRunID,
		UserJWT:    call.UserJWT,
	}
	// UNIVERSAL bare-action recovery : qualify bare names before the gate
	// so the module check sees the correct module (e.g. "read" → "filesystem.read").
	if !strings.Contains(target.Name, ".") {
		if idx := m.resolveIndex(call); idx != nil {
			fqns := idx.FQNList()
			target.Name = toolname.QualifyBareName(target.Name, fqns)
			if !strings.Contains(target.Name, ".") {
				target.Name = toolname.ResolveMangled(target.Name, fqns)
			}
		}
	}
	// SG-4 chokepoint : gate the resolved target so a denied / approve
	// tool can't slip through the execute_tool indirection. Meta-tool
	// targets bypass inside the gate ; their own children gate on
	// re-entry.
	if blocked := m.gateTarget(ctx, target); blocked != nil {
		return *blocked
	}
	out := m.Dispatch(ctx, target)
	if out.Status == "errored" {
		out.Error = appendParamSchema(out.Error, target.Name, m.resolveIndex(call))
	}
	return out
}

// appendParamSchema tacks the target tool's parameter list onto a param-shaped
// error so the model sees exactly what to send on retry.
func appendParamSchema(errMsg, fqn string, idx *index.ToolIndex) string {
	if idx == nil || !looksLikeParamError(errMsg) {
		return errMsg
	}
	t := idx.Get(Canonicalize(fqn))
	if t == nil || len(t.Params) == 0 {
		return errMsg
	}
	parts := make([]string, 0, len(t.Params))
	for _, p := range t.Params {
		s := p.Name + " (" + p.Type
		if p.Required {
			s += ", required"
		}
		parts = append(parts, s+")")
	}
	return errMsg + "\n" + fqn + " params: " + strings.Join(parts, ", ")
}

func looksLikeParamError(msg string) bool {
	for _, kw := range []string{"must not be empty", "required", "no edit locator"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// paramParseError marks a params value present as a non-empty string that is
// not valid JSON, so the caller reports an honest parse error instead of
// silently coercing it to an empty map.
type paramParseError struct{ detail string }

func (e *paramParseError) Error() string { return e.detail }

// coerceParams decodes a tool-params value: (m,nil) for an object or a string
// that parses; (nil,nil) when absent/blank; (nil,err) when a non-empty string
// is present but invalid JSON. No "repair" — a wrong guess could corrupt a write.
func coerceParams(v any) (map[string]any, error) {
	switch t := v.(type) {
	case map[string]any:
		return t, nil
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return nil, nil
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			return nil, &paramParseError{detail: describeJSONError(s, err)}
		}
		return m, nil
	}
	return nil, nil
}

func coerceParamMap(v any) map[string]any {
	m, _ := coerceParams(v)
	return m
}

// describeJSONError points the model at the exact break: message + byte offset
// + a snippet around it.
func describeJSONError(s string, err error) string {
	off := -1
	if se, ok := err.(*json.SyntaxError); ok {
		off = int(se.Offset)
	} else if ute, ok := err.(*json.UnmarshalTypeError); ok {
		off = int(ute.Offset)
	}
	if off < 0 {
		return err.Error()
	}
	if off > len(s) {
		off = len(s)
	}
	lo, hi := off-28, off+28
	if lo < 0 {
		lo = 0
	}
	if hi > len(s) {
		hi = len(s)
	}
	return fmt.Sprintf("%s (at byte %d of %d, near: %q)", err.Error(), off, len(s), s[lo:hi])
}

// extractExecuteToolParams returns the inner param map from whichever shape the
// LLM produced: params > arguments > args > flattened top-level. A present but
// malformed JSON string returns an error, never the empty-map fallthrough.
func extractExecuteToolParams(args map[string]any) (map[string]any, error) {
	for _, key := range []string{"params", "arguments", "args"} {
		v, ok := args[key]
		if !ok {
			continue
		}
		m, err := coerceParams(v)
		if err != nil {
			return nil, err
		}
		if m != nil {
			return m, nil
		}
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		switch k {
		case "name", "params", "arguments", "args":
			continue
		default:
			out[k] = v
		}
	}
	return out, nil
}

// handleListCategories : return the list of modules currently
// visible in the index.
//
// Output : {categories: [{name, tool_count}, ...]}.
func (m *MetaDispatcher) handleListCategories(idx *index.ToolIndex) runtime.ToolOutcome {
	if idx == nil {
		return jsonOutcome(map[string]any{"categories": []any{}})
	}
	cats := idx.CategoryList()
	out := make([]map[string]any, 0, len(cats))
	for _, c := range cats {
		out = append(out, map[string]any{
			"name":       c,
			"tool_count": len(idx.Categories[c]),
		})
	}
	return jsonOutcome(map[string]any{"categories": out})
}

// handleBrowseCategory : return one page of tools from a category.
//
// Args : {"category": "filesystem", "page": 1}.
// Output : {category, page, page_size, total, tools: [{name,
//
//	description, risk_level}, ...]}.
func (m *MetaDispatcher) handleBrowseCategory(idx *index.ToolIndex, args map[string]any) runtime.ToolOutcome {
	if idx == nil {
		return errored("tool index unavailable")
	}
	category, _ := args["category"].(string)
	if category == "" {
		return errored("browse_category: 'category' is required")
	}
	page := 1
	if v, ok := args["page"].(float64); ok {
		page = int(v)
	} else if v, ok := args["page"].(int); ok {
		page = v
	}
	if page < 1 {
		page = 1
	}
	pageSize := m.BrowsePageSize
	if pageSize <= 0 {
		pageSize = DefaultBrowsePageSize
	}

	fqns, ok := idx.Categories[category]
	if !ok {
		return errored("category not found: " + category)
	}
	// Clamp page so (page-1)*pageSize can't overflow int into a NEGATIVE slice
	// bound (a huge page like 4.7e17 wraps past MaxInt64). Anything past the last
	// page is handled as an empty page below.
	if maxPage := len(fqns)/pageSize + 2; page > maxPage {
		page = maxPage
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if start >= len(fqns) {
		// Past the end — return an empty page (not an error).
		return jsonOutcome(map[string]any{
			"category":  category,
			"page":      page,
			"page_size": pageSize,
			"total":     len(fqns),
			"tools":     []any{},
		})
	}
	if end > len(fqns) {
		end = len(fqns)
	}
	tools := make([]map[string]any, 0, end-start)
	for _, fqn := range fqns[start:end] {
		t := idx.Get(fqn)
		if t == nil {
			continue
		}
		tools = append(tools, map[string]any{
			"name":        t.FQN,
			"description": t.Description,
			"risk_level":  string(t.RiskLevel),
		})
	}
	return jsonOutcome(map[string]any{
		"category":  category,
		"page":      page,
		"page_size": pageSize,
		"total":     len(fqns),
		"tools":     tools,
	})
}

// --- outcome helpers ------------------------------------------------

func errored(msg string) runtime.ToolOutcome {
	return runtime.ToolOutcome{
		Status: "errored",
		Error:  msg,
	}
}

// jsonOutcome marshals `obj` into a single text Part and returns
// a completed outcome. JSON is the lingua franca the LLM sees for
// every meta-tool result.
func jsonOutcome(obj map[string]any) runtime.ToolOutcome {
	b, err := json.Marshal(obj)
	if err != nil {
		return errored(fmt.Sprintf("marshal failed: %v", err))
	}
	return runtime.ToolOutcome{
		Status: "completed",
		Parts: []sessionstore.MessagePart{{
			Type: sessionstore.PartTypeText,
			Text: string(b),
		}},
	}
}
