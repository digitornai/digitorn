package runtime

import (
	"context"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime/context/prompt"
)

// ContextBuilder is the opt-in seam between the runtime engine and
// the context_builder package (CB-1..5). When wired, the engine
// consults it once per turn to obtain :
//
//   - The per-agent tool list shipped to the LLM (with the adaptive
//     injection mode applied : direct / compact_direct / discovery)
//   - The assembled system prompt (9 documented sections + the
//     user's system_prompt LAST)
//
// Documented architecture from docs-site/docs/reference/modules/
// context_builder.md : the context_builder is the "central nervous
// system" that decides what tools the agent sees and what
// instructions surround them.
//
// Concurrency : BuildFor MUST be safe under concurrent calls. The
// engine fans out per-session turns and may call BuildFor for
// different (app, agent) pairs in parallel.
//
// Errors : the engine treats a non-nil error as a turn failure
// (logged + propagated). Implementations should reserve errors for
// real misconfigurations ; an empty toolset or default prompt is
// not an error.
type ContextBuilder interface {
	BuildFor(ctx context.Context, in ContextRequest) (ContextResult, error)
}

// ContextRequest carries the per-turn inputs ContextBuilder needs to
// produce a result.
type ContextRequest struct {
	App   *appmgr.RuntimeApp
	Agent *schema.Agent

	// AppName / AppVersion : passed through to the system prompt's
	// identity section. Optional ; some impls infer from App.
	AppName    string
	AppVersion string

	// MemoryEnabled tells the assembler to render the memory-instructions
	// section AND the wiring to offer the memory.* tools. True when the app
	// declared the memory module (tools.modules.memory).
	MemoryEnabled bool

	// AgentEnabled tells the wiring to offer the agent_spawn.agent delegation
	// tool. True when the app loaded the agent_spawn module (declared under
	// tools.modules.agent_spawn or granted in tools.capabilities). A second
	// coordinator-role gate still applies at dispatch time.
	AgentEnabled bool

	// CallAppEnabled / AskUserEnabled / UseSkillEnabled gate the three
	// NON-universal context_builder primitives. They are offered ONLY when
	// actually usable, so the model is never shown a tool that returns "not
	// wired" / has nothing to act on (which derails small models) :
	//   - CallAppEnabled : an AppCaller bridge is wired.
	//   - AskUserEnabled : an AskUser bridge is wired AND the app granted
	//     context_builder.ask_user (doc contract).
	//   - UseSkillEnabled : a SkillLoader is wired AND the app declares skills.
	CallAppEnabled  bool
	AskUserEnabled  bool
	UseSkillEnabled bool

	// Memory is the agent's current durable working memory, re-rendered from
	// session state each turn so it survives compaction + resume. nil = omit
	// the snapshot section.
	Memory *prompt.WorkingMemoryView
}

// ContextResult is what BuildFor returns. The engine ships Tools
// in ChatRequest.Tools and prepends SystemPrompt before the
// conversation history.
//
// Mode is the documented injection mode chosen by the planner
// (direct / compact_direct / discovery). Surfaced for observability
// and for the system-prompt assembler's mode-dependent block ;
// the engine doesn't act on it directly.
type ContextResult struct {
	Tools        []llm.ToolSpec
	SystemPrompt string
	Mode         string
}
