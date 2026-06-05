package prompt

import (
	"sort"
	"strings"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
)

// OperatingGuideSection (doc "How to think" / "How to use tools") is
// the agent's general operating guidance : understand the goal, plan
// then execute, prefer the simplest approach, diagnose before retrying,
// know when to stop, and communicate intent. Ported from the reference
// daemon's build_system_prompt, minus the file-ops block that was
// specific to its Read/Edit/Grep toolset (digitorn exposes module tools
// instead). Rendered only for agents that actually have tools — a pure
// chat agent stays lean (anti-pollution).
type OperatingGuideSection struct{}

func (OperatingGuideSection) ID() string { return "operating_guide" }

func (OperatingGuideSection) Render(ctx PromptContext) string {
	hasTools := len(ctx.InjectedTools) > 0 || (ctx.ToolIndex != nil && len(ctx.ToolIndex.Tools) > 0)
	if !hasTools {
		return ""
	}
	return strings.TrimRight(operatingGuide, "\n")
}

const operatingGuide = `# How to think

## Understand the goal BEFORE acting
- What is the user actually trying to achieve? (not just the literal words)
- What information do I need to accomplish this?
- What is the simplest approach that works?
Do NOT start calling tools until you have a clear plan.

## Plan, then execute
For any non-trivial task:
1. State your plan in 1-3 sentences.
2. Execute it with precise tool calls.
3. Verify the result.
4. Report what you did and found.
Never discover your plan by trial and error.

## Prefer the simplest approach
- One search often answers the question — don't read 10 things when 1 works.
- Ask yourself: can I answer this in 2-3 tool calls? If yes, do that.

## Diagnose before retrying
- Read the error message — it usually says exactly what's wrong.
- Understand WHY it failed before trying again; never retry the same call blindly.
- If stuck after 2 attempts, change approach.

## Know when to stop
- Answer the question that was asked — nothing more.
- When the task is done, say so.

## Work efficiently
- Batch independent calls: when 2+ tool calls don't depend on each other (reading several files, grepping several patterns), run them together with run_parallel instead of one at a time.
- Offload slow work: for anything long-running (builds, installs, full test runs, downloads, long shell commands), launch it with background_run, keep making progress, and check back later — never sit blocked on a slow operation.
- Discover before guessing: if you lack the right tool, search for it rather than inventing a tool name or a file path.
- Delegate substantial or specialised work to the right specialist via the ` + "`agent`" + ` tool when one is available, instead of doing everything yourself.
- Don't repeat work: reuse what you already read or computed this turn instead of re-fetching it.
- The user only sees your text: briefly say what you're about to do alongside your tool calls.`

// ModuleSectionsSection injects the prompt sections contributed by the
// modules the agent is authorized for (PromptContributor.PromptSections),
// gathered by the wiring layer and already authorization-gated. This is the
// faithful port of the reference daemon's get_prompt_sections() mechanism :
// each section carries its own Title + Priority ; the assembler renders them
// priority-ordered (lower first) as "# {Title}\n{Content}" — exactly
// prompt.py's `block = f"# {title}\n{content}"`. A module needs ZERO
// assembler code : implement PromptContributor, done.
type ModuleSectionsSection struct{}

func (ModuleSectionsSection) ID() string { return "module_sections" }

func (ModuleSectionsSection) Render(ctx PromptContext) string {
	if len(ctx.ModuleSections) == 0 {
		return ""
	}
	secs := make([]domainmodule.PromptSection, 0, len(ctx.ModuleSections))
	for _, s := range ctx.ModuleSections {
		if strings.TrimSpace(s.Content) != "" {
			secs = append(secs, s)
		}
	}
	if len(secs) == 0 {
		return ""
	}
	// Stable order : Priority asc, then Title, preserving input order on ties.
	sort.SliceStable(secs, func(i, j int) bool {
		if secs[i].Priority != secs[j].Priority {
			return secs[i].Priority < secs[j].Priority
		}
		return secs[i].Title < secs[j].Title
	})
	blocks := make([]string, 0, len(secs))
	for _, s := range secs {
		content := strings.TrimSpace(s.Content)
		if t := strings.TrimSpace(s.Title); t != "" {
			blocks = append(blocks, "# "+t+"\n"+content)
		} else {
			blocks = append(blocks, content)
		}
	}
	return strings.Join(blocks, "\n\n")
}

// ToolUsageSection (doc "# Tool Usage Instructions") injects the
// per-tool usage prompt (tool.Spec.ToolPrompt) for every tool in the
// agent's index that declares one. This is how an app ships precise,
// tool-specific guidance ("always call X before Y", payload templates,
// gotchas) into the system prompt. Gated to the agent's own index, so
// an agent never sees usage notes for a tool it can't reach.
type ToolUsageSection struct{}

func (ToolUsageSection) ID() string { return "tool_usage_instructions" }

func (ToolUsageSection) Render(ctx PromptContext) string {
	if ctx.ToolIndex == nil || len(ctx.ToolIndex.Tools) == 0 {
		return ""
	}
	// Resolve the effective prompt per tool : a dynamic overlay (from an
	// authorized module's DynamicToolPrompts) WINS over the static
	// tool.Spec.ToolPrompt — mirrors prompt.py's
	// `dynamic_prompts.get(fqn) or tool.tool_prompt`.
	effective := make(map[string]string, len(ctx.ToolIndex.Tools))
	for fqn, it := range ctx.ToolIndex.Tools {
		tp := strings.TrimSpace(it.ToolPrompt)
		if dyn, ok := ctx.DynamicToolPrompts[fqn]; ok && strings.TrimSpace(dyn) != "" {
			tp = strings.TrimSpace(dyn)
		}
		if tp != "" {
			effective[fqn] = tp
		}
	}
	if len(effective) == 0 {
		return ""
	}
	fqns := make([]string, 0, len(effective))
	for fqn := range effective {
		fqns = append(fqns, fqn)
	}
	sort.Strings(fqns)
	var b strings.Builder
	b.WriteString("# Tool Usage Instructions")
	for _, fqn := range fqns {
		b.WriteString("\n\n## ")
		b.WriteString(ctx.ToolIndex.Tools[fqn].FQN)
		b.WriteString("\n")
		b.WriteString(effective[fqn])
	}
	return b.String()
}
