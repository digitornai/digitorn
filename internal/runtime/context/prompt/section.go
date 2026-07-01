// Package prompt implements the system-prompt assembler. The section
// order is a faithful port of the REFERENCE DAEMON's build_system_prompt
// (digitorn-bridge .../context_builder/prompt.py) — the ground truth.
// It deliberately diverges from the doc's idealized 9-source list
// (context_builder.md §2) where the doc and the code disagree: the
// reference injects the agent pool + memory snapshot LATE (as module
// get_prompt_sections() after skills), not at the top, and wraps the
// user prompt in "# APP-DEFINED PERSONALITY" as the final block.
//
// See assembler.go (NewAssembler) for the exact ordered section list.
// The user only defines personality + behaviour; the context builder
// provides everything else (preamble, tool instructions, operating
// guide, module-contributed sections, per-tool usage prompts).
package prompt

import (
	"sort"
	"strings"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime/context/index"
	"github.com/digitornai/digitorn/internal/runtime/context/injection"
)

// PromptContext carries every input a section might need. Build one
// per turn (or cache per app version when the agent doesn't change).
// Pointers may be nil ; sections SHOULD handle that gracefully and
// produce empty output rather than panicking.
type PromptContext struct {
	// Agent : the agent we're building the prompt for. The doc
	// invariant is that every section reads from this single source
	// of truth.
	Agent *schema.Agent

	// AppName / AppVersion : surfaced in the identity section for
	// observability ("You are agent X of app foo v1.0.").
	AppName    string
	AppVersion string

	// InjectionMode : decides which tool-instructions block to use.
	// One of injection.ModeDirect / ModeCompactDirect / ModeDiscovery.
	InjectionMode injection.Mode

	// ToolIndex : the agent's full discoverable tool universe (SG-3
	// filtered). In discovery mode these are NAMED in the prompt but
	// reached via execute_tool ; in direct/compact mode they are the
	// directly-callable set.
	ToolIndex *index.ToolIndex

	// InjectedTools : the EXACT native tool list the LLM receives this
	// turn (meta-tools + primitives, plus the domain tools in direct/
	// compact mode). The tool-instructions section describes these and
	// ONLY these as directly-callable, so the prompt never advertises a
	// tool the model wasn't actually given (anti-pollution invariant).
	InjectedTools []llm.ToolSpec

	// Skills : the agent's declared /command skills. nil/empty =
	// no skills section.
	Skills []SkillEntry

	// Specialists : the sub-agents a coordinator may delegate to (its
	// delegate_to targets, resolved to id + specialty). nil/empty = not a
	// coordinator, so the agent-pool section is omitted.
	Specialists []SpecialistEntry

	// MemoryEnabled reports whether the memory tools (set_goal, remember,
	// task_create, task_update) are available to this agent. Drives the
	// memory-instructions section ; true even when memory is currently empty
	// (so the agent learns it CAN use the tools).
	MemoryEnabled bool

	// Memory is the agent's current durable working memory (goal, tasks, key
	// facts), re-rendered every turn from session state so it survives context
	// compaction and resume. nil = nothing remembered yet (snapshot omitted).
	Memory *WorkingMemoryView

	// ModuleSections holds the prompt sections contributed by the modules the
	// agent is AUTHORIZED for (PromptContributor.PromptSections), gathered by
	// the wiring layer. The framework injects them automatically — a module
	// writes no assembler code. Already authorization-gated upstream, so the
	// assembler renders them verbatim (priority-ordered).
	ModuleSections []domainmodule.PromptSection

	// DynamicToolPrompts overlays per-tool usage prompts keyed by FQN on top
	// of the static tool.Spec.ToolPrompt (dynamic wins). Contributed by
	// authorized modules (PromptContributor.DynamicToolPrompts) for tools a
	// manifest can't declare statically (e.g. MCP-discovered tools).
	DynamicToolPrompts map[string]string
}

// SkillEntry is one entry in the skills section. The Description
// shows in the prompt ; the Name is the /command the LLM uses to
// invoke the skill.
type SkillEntry struct {
	Name        string // e.g. "review_pr"
	Description string // one-liner shown to the LLM
}

// SpecialistEntry is one delegate target shown to a coordinator in the
// agent-pool section : the sub-agent id + its one-line specialty.
type SpecialistEntry struct {
	ID        string
	Specialty string
}

// Section is the per-doc-bullet unit of the system prompt. Each
// section renders its own block — the assembler joins them with
// blank lines.
//
// Render MUST be deterministic given a PromptContext : the same
// inputs produce the same string, so cache/snapshot tests work.
// Sections that have no content to add return "" ; the assembler
// drops empty sections.
type Section interface {
	// ID returns a stable identifier used in observability and
	// tests ("identity", "tool_instructions", "user_prompt", ...).
	ID() string

	// Render produces the text block for this section. Empty string
	// = "skip me" (the assembler doesn't insert blank padding for
	// empty sections).
	Render(ctx PromptContext) string
}

// specSignature renders an injected tool's parameter signature like
// "(query, category?, limit?)" — required params bare, optional ones
// suffixed "?". Hidden params (leading "_") are dropped.
func specSignature(s llm.ToolSpec) string {
	props, _ := s.Parameters["properties"].(map[string]any)
	if len(props) == 0 {
		return "()"
	}
	required := map[string]bool{}
	switch req := s.Parameters["required"].(type) {
	case []string:
		for _, r := range req {
			required[r] = true
		}
	case []any:
		for _, r := range req {
			if rs, ok := r.(string); ok {
				required[rs] = true
			}
		}
	}
	names := make([]string, 0, len(props))
	for n := range props {
		if strings.HasPrefix(n, "_") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		if required[n] {
			parts = append(parts, n)
		} else {
			parts = append(parts, n+"?")
		}
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// paramSignature renders a domain tool's signature from its indexed
// params (same convention as specSignature).
func paramSignature(it *index.IndexedTool) string {
	if it == nil || len(it.Params) == 0 {
		return "()"
	}
	parts := make([]string, 0, len(it.Params))
	for _, p := range it.Params {
		if strings.HasPrefix(p.Name, "_") {
			continue
		}
		if p.Required {
			parts = append(parts, p.Name)
		} else {
			parts = append(parts, p.Name+"?")
		}
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// moduleOf returns the module segment of a sanitized native tool name
// ("filesystem__read" -> "filesystem", "context_builder__search_tools"
// -> "context_builder"). Names without a "__" separator group under
// themselves.
func moduleOf(name string) string {
	if i := strings.Index(name, "__"); i > 0 {
		return name[:i]
	}
	return name
}

// firstSentence trims a description to its first sentence for compact
// one-line listings.
func firstSentence(desc string) string {
	desc = strings.TrimSpace(desc)
	if i := strings.IndexByte(desc, '.'); i > 0 {
		return strings.TrimSpace(desc[:i])
	}
	if i := strings.IndexByte(desc, '\n'); i > 0 {
		return strings.TrimSpace(desc[:i])
	}
	return desc
}
