package injection

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime/context/index"
	"github.com/digitornai/digitorn/internal/runtime/toolname"
)

// sanitizeToolName converts a dotted FQN like "filesystem.read" to
// the underscored form "filesystem__read" demanded by OpenAI-
// compatible APIs (and the digitorn gateway's strict regex
// ^[a-zA-Z0-9_-]{1,128}$). The runtime dispatcher accepts both
// forms via meta.Canonicalize on the inbound tool_calls, so this
// is a one-way outbound transformation only.
//
// Per docs-site/language/04-tools.md "Tool name sanitization" :
//
//	dispatcher catalog uses dots, OpenAI wire uses underscores.
//
// Centralised here so the planner is the single source of the
// outbound form for every injection mode.
func sanitizeToolName(name string) string {
	idx := strings.Index(name, ".")
	if idx == -1 {
		return name
	}
	return name[:idx] + "__" + name[idx+1:]
}

// mcpModulePrefix marks the synthetic module id of an MCP virtual tool.
const mcpModulePrefix = "mcp_"

// mcpWireAlias gives an MCP virtual tool a short, model-friendly wire name —
// the bare tool/action name ("echo") — instead of the long
// "mcp_<server>__<tool>" form. That long form is not just verbose: some
// providers' tool-calling (observed on moonshotai/kimi-k2) derail on the
// "mcp_<x>__<y>" token shape and emit garbage or unparsed tokens. Returns the
// alias plus the canonical dotted FQN to dispatch to, and ok=false for any
// non-MCP tool (which keeps the standard sanitizeToolName wire form).
// Collisions (two servers exposing the same tool, or an alias shadowing a
// native/builtin name) are resolved by disambiguateMCPAliases once the full
// toolset is assembled.
func mcpWireAlias(fqn string) (alias, canonical string, ok bool) {
	canonical = toolname.Canonicalize(fqn) // mcp_<server>__<tool> -> mcp_<server>.<tool>
	if !strings.HasPrefix(canonical, mcpModulePrefix) {
		return "", "", false
	}
	dot := strings.IndexByte(canonical, '.')
	if dot < 0 || dot+1 >= len(canonical) {
		return "", "", false
	}
	return canonical[dot+1:], canonical, true
}

// mcpServerOf extracts the server id from a canonical MCP FQN
// "mcp_<server>.<tool>" — used to qualify a colliding alias.
func mcpServerOf(canonical string) string {
	s := strings.TrimPrefix(canonical, mcpModulePrefix)
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		return s[:dot]
	}
	return s
}

// disambiguateMCPAliases guarantees every wire Name in the assembled toolset is
// unique. Non-MCP tools (builtins + native modules) own their names and are
// never renamed. An MCP tool keeps its bare alias only when no fixed tool owns
// it AND it is the sole MCP claimant; otherwise it falls back to
// "<tool>_<server>", then "<tool>_<server>_<n>". The canonical FQN carried on
// the spec is untouched, so dispatch and the inbound mapping stay correct.
func disambiguateMCPAliases(specs []llm.ToolSpec) {
	fixed := make(map[string]bool, len(specs))
	mcpCount := make(map[string]int, len(specs))
	for i := range specs {
		if specs[i].Canonical == "" {
			fixed[specs[i].Name] = true
		} else {
			mcpCount[specs[i].Name]++
		}
	}
	taken := make(map[string]bool, len(specs))
	for name := range fixed {
		taken[name] = true
	}
	for i := range specs {
		if specs[i].Canonical == "" {
			continue
		}
		name := specs[i].Name
		if !fixed[name] && mcpCount[name] == 1 {
			taken[name] = true
			continue
		}
		server := mcpServerOf(specs[i].Canonical)
		cand := name + "_" + server
		for n := 2; taken[cand]; n++ {
			cand = name + "_" + server + "_" + strconv.Itoa(n)
		}
		specs[i].Name = cand
		taken[cand] = true
	}
}

// Planner is the stateless decision-maker. Construct one per daemon
// (or just call package functions — Planner has no fields). Held as
// a struct so future extensions (custom budget ratio, alternative
// token estimators) plug in without breaking the API.
type Planner struct {
	// ContextRatio overrides MaxContextRatio for testing. 0 = use
	// the documented default.
	ContextRatio float64
}

// Plan computes the injection Decision for the given (idx, agent, runtime).
//
//	idx     : the per-agent ToolIndex (built by CB-1)
//	agent   : the agent whose brain context window drives the budget
//	runtime : the app's runtime block (for override + context defaults)
//
// Returns a Decision describing the mode chosen, the tool specs to
// ship to the LLM, and the budget calculations for observability.
//
// nil idx is treated as an empty index. nil agent / runtime → the
// algorithm uses DefaultContextWindow and no override.
func (p *Planner) Plan(
	idx *index.ToolIndex,
	agent *schema.Agent,
	runtime *schema.RuntimeBlock,
) Decision {
	if p == nil {
		p = &Planner{}
	}

	mode, overrideUsed := resolveModeOverride(runtime)
	contextWindow := resolveContextWindow(agent, runtime)
	ratio := p.ContextRatio
	if ratio <= 0 {
		ratio = MaxContextRatio
	}
	budget := int(float64(contextWindow) * ratio)

	// Always assemble the domain-tool full schemas first ; we need
	// their JSON size for the auto-decision even if we end up not
	// shipping them.
	directSchemas := buildDirectSchemas(idx)
	toolTokens := estimateToolTokens(directSchemas, idx)
	total := 0
	if idx != nil {
		total = len(idx.Tools)
	}
	compactTokens := total * CompactBytesPerTool

	if !overrideUsed {
		mode = autoChooseMode(toolTokens, compactTokens, budget)
	}

	reason := ""
	if overrideUsed && runtime != nil {
		reason = "runtime.tool_injection forced " + string(runtime.ToolInjection)
	}

	return Decision{
		Mode:           mode,
		Tools:          assembleToolList(mode, directSchemas, idx),
		ContextWindow:  contextWindow,
		Budget:         budget,
		ToolTokens:     toolTokens,
		CompactTokens:  compactTokens,
		OverrideUsed:   overrideUsed,
		OverrideReason: reason,
	}
}

// autoChooseMode applies the documented decision chain.
//
//	if tool_tokens <= budget    → direct
//	elif compact_tokens <= budget → compact_direct
//	else                         → discovery
//
// Verbatim from docs-site/docs/language/04-tools.md.
func autoChooseMode(toolTokens, compactTokens, budget int) Mode {
	switch {
	case toolTokens <= budget:
		return ModeDirect
	case compactTokens <= budget:
		return ModeCompactDirect
	default:
		return ModeDiscovery
	}
}

// buildDirectSchemas converts every IndexedTool into a full llm.ToolSpec.
// Used as input to estimateToolTokens (size of the direct mode payload)
// and as the final output for direct mode.
//
// nil idx returns an empty slice.
func hasDynamicCatalog(idx *index.ToolIndex) bool {
	if idx == nil {
		return false
	}
	for _, fqn := range idx.FQNList() {
		if t := idx.Get(fqn); t != nil && t.DiscoveryOnly {
			return true
		}
	}
	return false
}

func buildDirectSchemas(idx *index.ToolIndex) []llm.ToolSpec {
	if idx == nil {
		return nil
	}
	fqns := idx.FQNList() // sorted for deterministic output
	out := make([]llm.ToolSpec, 0, len(fqns))
	for _, fqn := range fqns {
		t := idx.Get(fqn)
		if t == nil {
			continue
		}
		if t.DiscoveryOnly {
			continue // never inject as direct schema — only discoverable via search/get_tool
		}
		name, canonical := sanitizeToolName(t.FQN), ""
		if alias, canon, ok := mcpWireAlias(t.FQN); ok {
			name, canonical = alias, canon
		}
		out = append(out, llm.ToolSpec{
			Name:        name,
			Description: t.Description,
			Parameters:  paramsToJSONSchema(t.Params),
			Canonical:   canonical,
		})
	}
	return out
}

// buildCompactSchemas produces the compact form : name + 1-line
// description, NO params. The LLM must call get_tool to fetch the
// full schema before invoking the tool — that's the documented
// compact-direct workflow.
func buildCompactSchemas(idx *index.ToolIndex) []llm.ToolSpec {
	if idx == nil {
		return nil
	}
	fqns := idx.FQNList()
	out := make([]llm.ToolSpec, 0, len(fqns))
	for _, fqn := range fqns {
		t := idx.Get(fqn)
		if t == nil {
			continue
		}
		desc := t.Description
		if i := indexOf(desc, '.'); i > 0 && i < 120 {
			desc = desc[:i+1] // keep first sentence
		}
		name, canonical := sanitizeToolName(t.FQN), ""
		if alias, canon, ok := mcpWireAlias(t.FQN); ok {
			name, canonical = alias, canon
		}
		out = append(out, llm.ToolSpec{
			Name:        name,
			Description: desc,
			Canonical:   canonical,
		})
	}
	return out
}

// assembleToolList builds the final []llm.ToolSpec given the chosen
// mode. Only the builtins RELEVANT for the mode are injected (the
// activation-by-relevance policy — see builtinsForMode), so a pure-chat
// agent with no tools gets none, and direct mode isn't polluted with the
// discovery meta-tools. The mode-relevant builtins go first.
func assembleToolList(mode Mode, direct []llm.ToolSpec, idx *index.ToolIndex) []llm.ToolSpec {
	hasTools := idx != nil && len(idx.Tools) > 0
	builtins := builtinsForMode(mode, hasTools, hasDynamicCatalog(idx))
	// Sanitize builtin names to match the doc-conform OpenAI wire
	// form. The Inner side (MetaDispatcher) canonicalises back.
	for i := range builtins {
		builtins[i].Name = sanitizeToolName(builtins[i].Name)
	}
	switch mode {
	case ModeDirect:
		out := make([]llm.ToolSpec, 0, len(builtins)+len(direct))
		out = append(out, builtins...)
		out = append(out, direct...)
		disambiguateMCPAliases(out)
		return out
	case ModeCompactDirect:
		compact := buildCompactSchemas(idx)
		out := make([]llm.ToolSpec, 0, len(builtins)+len(compact))
		out = append(out, builtins...)
		out = append(out, compact...)
		disambiguateMCPAliases(out)
		return out
	case ModeDiscovery:
		// Only the meta-tools — domain tools sit behind execute_tool.
		return builtins
	default:
		return builtins
	}
}

// estimateToolTokens approximates the input-token cost of shipping
// `direct` as the ChatRequest.Tools payload. Uses the documented
// formula : sum(len(json.dumps(t)) // 4 for t in tools). Falls back
// to total_tools * FallbackTokensPerTool when direct is empty.
func estimateToolTokens(direct []llm.ToolSpec, idx *index.ToolIndex) int {
	if len(direct) == 0 {
		total := 0
		if idx != nil {
			total = len(idx.Tools)
		}
		return total * FallbackTokensPerTool
	}
	total := 0
	for _, t := range direct {
		b, err := json.Marshal(t)
		if err == nil {
			total += len(b) / 4
		}
	}
	return total
}

// paramsToJSONSchema converts the tool.ParamSpec list into the JSON
// Schema shape an LLM provider expects. Adds the wrapper
// {type: object, properties, required} every provider wants.
func paramsToJSONSchema(params []tool.ParamSpec) map[string]any {
	props := make(map[string]any, len(params))
	required := make([]string, 0, len(params))
	for _, p := range params {
		props[p.Name] = paramToSchema(p)
		if p.Required {
			required = append(required, p.Name)
		}
	}
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

// AddIntentParam adds a required `intent` string to every tool schema, so the
// model narrates each call (ui.tool_calls.inject_intent). Skips tools with no
// params (compact/discovery) or that already declare `intent`.
func AddIntentParam(tools []llm.ToolSpec) {
	for i := range tools {
		params := tools[i].Parameters
		if params == nil {
			continue
		}
		props, _ := params["properties"].(map[string]any)
		if props == nil {
			continue
		}
		if _, exists := props["intent"]; exists {
			continue
		}
		props["intent"] = map[string]any{
			"type":        "string",
			"description": "A short present-tense phrase, in the user's language, describing what you're about to do — shown live to the user (e.g. \"Reading the cart page\").",
		}
		if req, ok := params["required"].([]string); ok {
			params["required"] = append([]string{"intent"}, req...)
		} else {
			params["required"] = []string{"intent"}
		}
	}
}

func paramToSchema(p tool.ParamSpec) map[string]any {
	m := jsonSchemaType(p.Type)
	m["description"] = p.Description
	if p.Default != nil {
		m["default"] = p.Default
	}
	if len(p.Enum) > 0 {
		m["enum"] = p.Enum
	}
	return m
}

// jsonSchemaType maps a Digitorn param type to a VALID JSON Schema fragment.
// Provider function-calling validators (DeepSeek, OpenAI) reject non-standard
// type names, so internal types like "string_list" must be translated, not
// passed through verbatim.
func jsonSchemaType(t string) map[string]any {
	switch t {
	case "string_list":
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	case "string_or_string_list":
		return map[string]any{"anyOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		}}
	case "int", "integer":
		return map[string]any{"type": "integer"}
	case "float", "number", "double":
		return map[string]any{"type": "number"}
	case "bool", "boolean":
		return map[string]any{"type": "boolean"}
	case "array", "list":
		return map[string]any{"type": "array"}
	case "object", "map", "dict":
		return map[string]any{"type": "object"}
	case "regex", "string", "":
		return map[string]any{"type": "string"}
	default:
		return map[string]any{"type": "string"}
	}
}

// indexOf is a tiny strings.IndexByte replacement to avoid the
// strings import on a hot path that does many short calls.
func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
