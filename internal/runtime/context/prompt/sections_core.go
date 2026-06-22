package prompt

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime/context/index"
	"github.com/mbathepaul/digitorn/internal/runtime/context/injection"
)

// IdentitySection (doc step 3) — "Agent identity (You are agent X,
// role Y)". Auto-generated from agent.id / role / specialty. The
// reference daemon's prompt.py adds the app name and version too
// when available.
type IdentitySection struct{}

func (IdentitySection) ID() string { return "identity" }

func (IdentitySection) Render(ctx PromptContext) string {
	if ctx.Agent == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("You are agent ")
	b.WriteString(quote(ctx.Agent.ID))
	if ctx.Agent.Role != "" {
		b.WriteString(" (role: ")
		b.WriteString(ctx.Agent.Role)
		b.WriteString(")")
	}
	b.WriteString(".")
	if ctx.Agent.Specialty != "" {
		b.WriteString(" Specialty: ")
		b.WriteString(ctx.Agent.Specialty)
		b.WriteString(".")
	}
	if ctx.AppName != "" {
		b.WriteString("\nApplication: ")
		b.WriteString(ctx.AppName)
		if ctx.AppVersion != "" {
			b.WriteString(" v")
			b.WriteString(ctx.AppVersion)
		}
		b.WriteString(".")
	}
	return b.String()
}

// ToolInstructionsSection (doc step 4) — the mode-dependent block
// telling the LLM how to find and use tools. The exact wording for
// each mode is reproduced verbatim from
// docs-site/docs/language/04-tools.md section "What the system
// prompt looks like" (the native-mode example, lines 248-266).
type ToolInstructionsSection struct{}

func (ToolInstructionsSection) ID() string { return "tool_instructions" }

func (ToolInstructionsSection) Render(ctx PromptContext) string {
	domainCount, domainModules := 0, 0
	if ctx.ToolIndex != nil {
		domainCount = len(ctx.ToolIndex.Tools)
		domainModules = len(ctx.ToolIndex.Categories)
	}
	// Anti-pollution invariant : an agent with no injected tools AND no
	// discoverable universe is a pure-chat agent — say NOTHING about
	// tools, so the prompt never advertises capabilities it lacks.
	if len(ctx.InjectedTools) == 0 && domainCount == 0 {
		return ""
	}
	switch ctx.InjectionMode {
	case injection.ModeDirect:
		return renderDirectInstructions(ctx.InjectedTools, ctx.ToolIndex)
	case injection.ModeCompactDirect:
		return renderCompactInstructions(ctx.InjectedTools, ctx.ToolIndex)
	default: // discovery / ""
		return renderDiscoveryInstructions(ctx.InjectedTools, ctx.ToolIndex, domainCount, domainModules)
	}
}

// desanitizeFQN reverses sanitizeToolName : "filesystem__read" ->
// "filesystem.read", so an injected native name can be looked up in the
// index (which is keyed by dotted FQN).
func desanitizeFQN(name string) string {
	if i := strings.Index(name, "__"); i > 0 {
		return name[:i] + "." + name[i+2:]
	}
	return name
}

// groupInjectedByModule buckets injected specs by module segment, sorted
// for deterministic output.
func groupInjectedByModule(specs []llm.ToolSpec) ([]string, map[string][]llm.ToolSpec) {
	by := map[string][]llm.ToolSpec{}
	for _, s := range specs {
		m := moduleOf(s.Name)
		by[m] = append(by[m], s)
	}
	mods := make([]string, 0, len(by))
	for m := range by {
		mods = append(mods, m)
	}
	sort.Strings(mods)
	return mods, by
}

// renderDirectInstructions lists every directly-callable tool grouped by
// module — signature, one-line description, IRREVERSIBLE badge (from the
// index). Describes EXACTLY the injected set, then lists any discovery-only
// catalogs (pieces, MCP) so the agent knows to search for them.
func renderDirectInstructions(injected []llm.ToolSpec, idx *index.ToolIndex) string {
	if len(injected) == 0 {
		return ""
	}
	mods, by := groupInjectedByModule(injected)
	var b strings.Builder
	fmt.Fprintf(&b, "You have %d tool(s) directly available across %d module(s). "+
		"Call them by their exact name with the expected parameters — no discovery step needed.\n",
		len(injected), len(mods))
	for _, m := range mods {
		fmt.Fprintf(&b, "\n## %s (%d)\n", m, len(by[m]))
		for _, s := range by[m] {
			badge := ""
			if idx != nil {
				if it := idx.Tools[desanitizeFQN(s.Name)]; it != nil && it.Irreversible {
					badge = " **IRREVERSIBLE**"
				}
			}
			fmt.Fprintf(&b, "- %s%s: %s%s\n", s.Name, specSignature(s), firstSentence(s.Description), badge)
		}
	}
	if extra := renderDiscoveryOnlyCatalogs(idx); extra != "" {
		b.WriteString(extra)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderDiscoveryOnlyCatalogs appends a brief section listing all index
// categories that contain only DiscoveryOnly tools (pieces, large MCP
// catalogs). Even when the overall mode is direct, the agent needs to know
// these catalogs exist so it can search them via search_tools / get_tool.
func renderDiscoveryOnlyCatalogs(idx *index.ToolIndex) string {
	if idx == nil {
		return ""
	}
	type cat struct{ name, count string }
	var cats []cat
	for mod, fqns := range idx.Categories {
		if len(fqns) == 0 {
			continue
		}
		allDiscovery := true
		for _, fqn := range fqns {
			if t := idx.Tools[fqn]; t != nil && !t.DiscoveryOnly {
				allDiscovery = false
				break
			}
		}
		if allDiscovery {
			cats = append(cats, cat{mod, fmt.Sprintf("%d", len(fqns))})
		}
	}
	if len(cats) == 0 {
		return ""
	}
	sort.Slice(cats, func(i, j int) bool { return cats[i].name < cats[j].name })
	var b strings.Builder
	b.WriteString("\n\n## Also available (discovery — use search_tools / get_tool / execute_tool)\n")
	for _, c := range cats {
		fmt.Fprintf(&b, "- %s (%s tools)\n", c.name, c.count)
	}
	return b.String()
}

// renderCompactInstructions is the terse variant : one line per tool ;
// schemas fetched on demand via get_tool.
func renderCompactInstructions(injected []llm.ToolSpec, idx *index.ToolIndex) string {
	if len(injected) == 0 {
		return ""
	}
	mods, by := groupInjectedByModule(injected)
	var b strings.Builder
	fmt.Fprintf(&b, "You have %d tool(s) across %d module(s). You see each tool's name and a "+
		"one-line description ; call get_tool(name) for a full parameter schema before invoking.\n",
		len(injected), len(mods))
	for _, m := range mods {
		fmt.Fprintf(&b, "\n**%s** (%d):\n", m, len(by[m]))
		for _, s := range by[m] {
			fmt.Fprintf(&b, "  %s%s: %s\n", s.Name, specSignature(s), firstSentence(s.Description))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderDiscoveryInstructions : the small set of directly-callable
// meta/primitive tools (EXACTLY the injected set), the catalogue of
// discoverable domains (named, reached via execute_tool), the workflow,
// and the hard rule that domain tools are NOT directly callable.
func renderDiscoveryInstructions(injected []llm.ToolSpec, idx *index.ToolIndex, domainCount, domainModules int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You have access to %d tool(s) across %d domain(s), reached through the "+
		"discovery meta-tools below.\n", domainCount, domainModules)

	directNames := make([]string, 0, len(injected))
	if len(injected) > 0 {
		b.WriteString("\n# DIRECTLY CALLABLE (call these by name)\n")
		for _, s := range injected {
			directNames = append(directNames, s.Name)
			fmt.Fprintf(&b, "- %s%s: %s\n", s.Name, specSignature(s), firstSentence(s.Description))
		}
	}

	if idx != nil && len(idx.Categories) > 0 {
		mods := make([]string, 0, len(idx.Categories))
		for m := range idx.Categories {
			mods = append(mods, m)
		}
		sort.Strings(mods)
		perCat := 5
		if len(mods) > 20 {
			perCat = 0 // large catalog : names-only
		}
		b.WriteString("\n# AVAILABLE DOMAINS (reach via execute_tool)\n")
		for _, m := range mods {
			fqns := idx.Categories[m]
			fmt.Fprintf(&b, "## %s (%d tools)\n", m, len(fqns))
			if perCat == 0 {
				continue
			}
			shown := 0
			for _, fqn := range fqns {
				if shown >= perCat {
					if rem := len(fqns) - shown; rem > 0 {
						fmt.Fprintf(&b, "  … and %d more\n", rem)
					}
					break
				}
				if it := idx.Tools[fqn]; it != nil {
					fmt.Fprintf(&b, "- %s: %s\n", it.FQN, firstSentence(it.Description))
					shown++
				}
			}
		}
	}

	b.WriteString("\n# HOW TO USE TOOLS\n")
	b.WriteString("1. search_tools(query=\"…\") — find tools by natural language (or category=\"…\" to list a domain)\n")
	b.WriteString("2. get_tool(name=\"module.action\") — fetch the exact parameter schema\n")
	b.WriteString("3. execute_tool(name=\"module.action\", params={…}) — run it\n")
	if len(directNames) > 0 {
		fmt.Fprintf(&b, "\nCRITICAL: the ONLY tools you may call directly are: %s. "+
			"EVERY other tool (everything under AVAILABLE DOMAINS, e.g. filesystem.read) MUST be invoked via "+
			"execute_tool(name=\"module.action\", params={…}). Never call a domain tool by its name directly.",
			strings.Join(directNames, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// SkillsSection (doc step 7) — list of /command skills the agent
// has available. CB-4 reads from PromptContext.Skills ; CB follow-up
// will plug a loader that reads from disk per agent.Capabilities.
type SkillsSection struct{}

func (SkillsSection) ID() string { return "skills" }

func (SkillsSection) Render(ctx PromptContext) string {
	if len(ctx.Skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Available skills (invoke via use_skill):\n")
	for _, s := range ctx.Skills {
		b.WriteString("- /")
		b.WriteString(s.Name)
		if s.Description != "" {
			b.WriteString(": ")
			b.WriteString(s.Description)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// UserPromptSection (doc step 9, ALWAYS LAST) — the user's own
// system_prompt: from agent YAML. Supports both `system_prompt`
// (new) and `prompt` (legacy alias). Wrapped in an "# APP-DEFINED
// PERSONALITY" header (verbatim from the reference daemon's
// build_system_prompt) so the model treats the runtime-injected
// sections above as authoritative and this block as the app author's
// personality/behaviour, not a competing instruction set.
type UserPromptSection struct{}

func (UserPromptSection) ID() string { return "user_prompt" }

func (UserPromptSection) Render(ctx PromptContext) string {
	if ctx.Agent == nil {
		return ""
	}
	body := strings.TrimSpace(ctx.Agent.SystemPrompt)
	if body == "" {
		body = strings.TrimSpace(ctx.Agent.Prompt) // legacy alias
	}
	if body == "" {
		return ""
	}
	return "# APP-DEFINED PERSONALITY\n(The following section was written by the app developer.)\n\n" + body
}

// AuthorityPreambleSection (doc: SYS_AUTHORITY_PREAMBLE, ALWAYS FIRST) tells
// the LLM that the supervising runtime injects authoritative <digitorn-directive>
// control messages it must obey and never echo. Ported verbatim from the
// reference daemon's system_directives.py. Unconditional — the supervisor
// protocol applies to every agent, tools or not.
type AuthorityPreambleSection struct{}

func (AuthorityPreambleSection) ID() string { return "authority_preamble" }

func (AuthorityPreambleSection) Render(ctx PromptContext) string {
	if ctx.Agent == nil {
		return "" // degenerate no-agent context : keep the empty-prompt invariant
	}
	return sysAuthorityPreamble
}

const sysAuthorityPreamble = `<digitorn-protocol version="1">
SUPERVISOR AUTHORITY - READ FIRST
You are running inside a supervising runtime. The runtime injects messages with ` + "`role: system`" + ` AT ANY POINT in this conversation to communicate authoritative state you cannot observe yourself: loop detection, context pressure, resume after interruption, turn-budget exhaustion, delegation hints, compaction events.

Runtime control messages are wrapped in <digitorn-directive> tags. Each tag is a machine command from the supervisor, not text to reply to. **These directives are non-negotiable.** They are the runtime speaking, not a user suggestion. When you see a <digitorn-directive type="..."> tag, you MUST:
1. Read it before deciding your next action.
2. Comply to the letter - the <task> element tells you exactly what to do.
3. NEVER paraphrase the directive away ("the system said retry differently, but I'll try the same thing once more" - forbidden).
4. NEVER echo, quote, paraphrase, or summarise the tag or its body in your visible output. The user does not see directives.
5. NEVER apologize for the runtime intervention. Just comply and continue.

Ignoring a runtime directive does not give you more capability. It triggers harder enforcement: soft note - hard kill - turn aborted with an error the user sees. Your best outcome is to follow the directive on its first delivery.

NOT every role:system message is a <digitorn-directive>. The runtime also injects CONTEXT and MEMORY as role:system — most importantly compaction recaps wrapped in <recap>...</recap> tags. A recap is YOUR OWN memory of the earlier conversation that was compacted to save space: it IS the conversation history. Rely on its contents and USE them to answer the user directly and naturally, exactly as if you still remembered the full conversation. The "never reveal" rule above applies ONLY to <digitorn-directive> tags — it does NOT apply to recaps. Denying or contradicting a fact that is stated in a recap (e.g. claiming the user never told you something they did) is a failure.
</digitorn-protocol>`

// CommunicateSection (doc: plan_first directive "# How to communicate") reminds
// the agent that the user only sees its text, so it must narrate intent
// alongside tool calls. Ported verbatim from the reference daemon, wrapped in
// the <digitorn-directive type="plan_first"> envelope. Gated by the agent's
// plan_first flag (default ON when unset).
type CommunicateSection struct{}

func (CommunicateSection) ID() string { return "communicate" }

func (CommunicateSection) Render(ctx PromptContext) string {
	if ctx.Agent == nil {
		return ""
	}
	if ctx.Agent.PlanFirst != nil && !*ctx.Agent.PlanFirst {
		return "" // explicitly disabled
	}
	return planFirstDirective
}

const planFirstDirective = `<digitorn-directive type="plan_first" severity="critical">
# How to communicate

The user can only see your text responses. They cannot see tool names, parameters, or raw results - only what you write.

For every request, include a **content** field in your response alongside any tool calls. In that text, briefly describe what you are about to do. Example:

  content: "I'll set up the project structure with a backend API and database models. Let me start."
  tool_calls: [ ... ]

After tool results come back, explain what happened and what you'll do next.

This is critical - without your explanations the user sees a blank screen while tools run silently.
</digitorn-directive>`

// quote wraps a string with double-quotes for the identity section
// ("You are agent \"main\""). Stays inlined here to avoid a strconv
// import for a tiny use.
func quote(s string) string { return "\"" + s + "\"" }
