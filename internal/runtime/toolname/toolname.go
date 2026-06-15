// Package toolname owns the canonical-vs-wire encoding of tool FQNs.
//
// Two encodings exist in the system, and only here :
//
//	canonical : dotted FQN, e.g. "filesystem.read". The form used by
//	            the YAML catalog, the dispatcher, the policy gates,
//	            the hook conditions, the persisted session events,
//	            and the audit trail. EVERYTHING internal speaks dots.
//
//	wire      : underscored, e.g. "filesystem__read". The encoding
//	            shipped to LLM providers that enforce OpenAI's strict
//	            tool-name regex ^[a-zA-Z0-9_-]{1,128}$. The injection
//	            planner converts on the outbound boundary ; the runtime
//	            engine converts back on the inbound boundary (see
//	            runtime.canonicalizeToolCallNames).
//
// Keeping the encoding logic in a leaf package with zero internal
// dependencies prevents the historical import cycle (runtime → meta →
// runtime) and gives every consumer (runtime engine, meta dispatcher,
// injection planner, future trace tooling) the SAME source of truth.
// Doc reference : docs-site/language/04-tools.md "Tool name sanitization".
package toolname

import "strings"

// Canonicalize maps any of the separator forms a model may emit to the
// dotted FQN.
//
//	"filesystem__read" → "filesystem.read"   (OpenAI wire form)
//	"filesystem::read" → "filesystem.read"   (a form some models emit)
//	"filesystem.read"  → "filesystem.read"   (idempotent)
//	""                 → ""
//
// Only the FIRST separator is converted ; downstream ones are preserved
// because they may belong to a parameter or sub-action name (e.g.
// "internal__handler" must not be split twice). The dot is the canonical
// separator everything internal speaks, so a name already carrying one is
// returned untouched. The `::` form is normalised because real models
// (e.g. deepseek) substitute it for the underscored wire name — without
// this the module half is lost and every such call is denied at gate1a.
func Canonicalize(name string) string {
	if name == "" {
		return name
	}
	if strings.Contains(name, ".") {
		return name
	}
	if idx := strings.Index(name, "::"); idx != -1 {
		return name[:idx] + "." + name[idx+2:]
	}
	idx := strings.Index(name, "__")
	if idx == -1 {
		return name
	}
	return name[:idx] + "." + name[idx+2:]
}

// Sanitize maps the canonical dotted FQN to the OpenAI-compatible
// underscored wire form.
//
//	"filesystem.read"  → "filesystem__read"
//	"filesystem__read" → "filesystem__read"   (idempotent)
//	""                 → ""
//
// Inverse of Canonicalize for any name that has at most one dot
// (the FQN convention).
func Sanitize(name string) string {
	if name == "" {
		return name
	}
	idx := strings.Index(name, ".")
	if idx == -1 {
		return name
	}
	return name[:idx] + "__" + name[idx+1:]
}

// internalAliases maps the documented short names for the runtime-internal
// tools (docs-site/docs/language/04b-builtin-tools.md "Short-name aliases") to
// their canonical FQN. These tools are NOT in the per-agent index (memory /
// agent_spawn are runtime subsystems), so unlike domain-tool aliases they can't
// be resolved via the index — they're resolved here. Real models routinely emit
// the short name (e.g. execute_tool(name="agent")) or the PascalCase alias
// instead of the underscored FQN, so resolving them is required for delegation
// and memory to work end-to-end.
//
// Keyed on the BARE name only (no module prefix). A name that already carries a
// module ("memory.remember") has a '.' and never matches, so resolution is a
// no-op on already-qualified names.
var internalAliases = map[string]string{
	"agent":       "agent_spawn.agent",
	"Agent":       "agent_spawn.agent",
	"remember":    "memory.remember",
	"Remember":    "memory.remember",
	"set_goal":    "memory.set_goal",
	"SetGoal":     "memory.set_goal",
	"task_create": "memory.task_create",
	"TaskCreate":  "memory.task_create",
	"task_update": "memory.task_update",
	"TaskUpdate":  "memory.task_update",
}

// ResolveAlias maps a documented short alias for a runtime-internal tool to its
// canonical FQN. Apply AFTER Canonicalize. Unknown names (every domain tool,
// every already-qualified FQN, the context_builder meta-tools) pass through
// unchanged.
func ResolveAlias(name string) string {
	if fqn, ok := internalAliases[name]; ok {
		return fqn
	}
	return name
}

// SplitFQN returns (module, action) given either form. When the name
// has no separator, module is "" and action carries the full input —
// the convention used by the always-direct meta-tools ("search_tools",
// "run_parallel", …) that have no module prefix.
func SplitFQN(name string) (module, action string) {
	canonical := Canonicalize(name)
	idx := strings.Index(canonical, ".")
	if idx == -1 {
		return "", canonical
	}
	return canonical[:idx], canonical[idx+1:]
}

// QualifyBareName recovers the module for a module-less action name by matching
// it against the agent's known canonical FQNs. Weak models routinely drop the
// module prefix and call "read" instead of "filesystem.read" / "filesystem__read".
// Such a name has no module, which the security gates (gate 1a / gate 3) then
// deny with an empty module. This restores the prefix when EXACTLY ONE known
// tool has that action.
//
// Conservative by design : a name that already carries a module (contains '.'),
// an action exposed by two or more modules (ambiguous), or an unknown action is
// returned UNCHANGED — we never guess across a genuine ambiguity ; the gate then
// reports a clear, honest error instead of silently dispatching the wrong tool.
//
// Pure and table-agnostic : the caller supplies knownFQNs (the per-agent tool
// index, which automatically contains every loaded module — current and future),
// so a new module is covered the instant its tools are in the index, with no
// change here. Apply AFTER Canonicalize + ResolveAlias.
func QualifyBareName(name string, knownFQNs []string) string {
	if name == "" || strings.Contains(name, ".") {
		return name
	}
	match := ""
	for _, fqn := range knownFQNs {
		dot := strings.IndexByte(fqn, '.')
		if dot < 0 || fqn[dot+1:] != name {
			continue
		}
		if match != "" && match != fqn {
			return name // two modules expose this action — ambiguous, leave bare
		}
		match = fqn
	}
	if match != "" {
		return match
	}
	return name
}

// flattenKey reduces a tool name to a separator-insensitive comparison key:
// the FQN dot and any "__"/"_" separators collapse to a single "_". So the
// canonical "mcp_notion.notion-search", its sanitized wire form
// "mcp_notion__notion-search", and a model's mangled "mcp_notion_notion-search"
// all map to the same key.
func flattenKey(s string) string {
	s = strings.ReplaceAll(s, "__", "_")
	s = strings.ReplaceAll(s, ".", "_")
	return s
}

// ResolveMangled maps a model-mangled tool name (dot/single-underscore/
// double-underscore variants of an FQN) back to a known canonical FQN by
// comparing separator-insensitive keys. Real models — especially on MCP names
// like mcp_<server>.<tool> — freely swap "." and "_", which Canonicalize alone
// can't undo (it can't know the module boundary). Returns the FQN on a unique
// match, else the input unchanged (so an ambiguous or unknown name is left for
// the gate to report honestly). Apply after Canonicalize + QualifyBareName when
// the name still carries no ".".
func ResolveMangled(name string, knownFQNs []string) string {
	if name == "" || strings.Contains(name, ".") {
		return name
	}
	key := flattenKey(name)
	match := ""
	for _, fqn := range knownFQNs {
		if flattenKey(fqn) != key {
			continue
		}
		if match != "" && match != fqn {
			return name // two FQNs collapse to the same key — ambiguous, leave as-is
		}
		match = fqn
	}
	if match != "" {
		return match
	}
	return name
}
