package prompt

import "strings"

// The five sections below are documented in
// docs-site/docs/reference/modules/context_builder.md but rely on
// modules / features not yet ported to Go :
//
//   1. Memory snapshot     → needs memory module (CB follow-up)
//   2. Memory instructions → needs memory module
//   5. Structural hints    → needs MCP module / per-tool hints
//   6. Agent pool info     → needs agent_spawn module (multi-agent)
//   8. Channel info        → needs channels module
//
// They're implemented as empty placeholders so the assembler's
// section order matches the doc bit-for-bit TODAY. When the
// backing modules land, their Render method becomes non-empty and
// the prompt grows automatically — no assembler refactor needed.

// MemorySnapshotSection (doc step 1) — the agent's current goal, tasks, and
// remembered facts, re-rendered from durable session state every turn so it
// survives context compaction and resume. Empty when nothing's remembered yet.
type MemorySnapshotSection struct{}

func (MemorySnapshotSection) ID() string { return "memory_snapshot" }
func (MemorySnapshotSection) Render(ctx PromptContext) string {
	if ctx.Memory == nil {
		return ""
	}
	return RenderWorkingMemory(*ctx.Memory)
}

// MemoryInstructionsSection (doc step 2) — tells the LLM how to use the
// set_goal / remember / task_create / task_update tools. Rendered whenever the
// memory tools are available (even with empty memory, so the agent knows it
// can use them).
type MemoryInstructionsSection struct{}

func (MemoryInstructionsSection) ID() string { return "memory_instructions" }
func (MemoryInstructionsSection) Render(ctx PromptContext) string {
	if !ctx.MemoryEnabled {
		return ""
	}
	return memoryInstructions
}

// StructuralHintsSection (doc step 5) — would inject per-tool
// parameter templates and JSON examples (especially valuable for
// MCP servers with complex schemas). Empty until CB follow-up.
type StructuralHintsSection struct{}

func (StructuralHintsSection) ID() string                    { return "structural_hints" }
func (StructuralHintsSection) Render(_ PromptContext) string { return "" }

// AgentPoolSection (doc step 6) — lists the available specialists when the
// agent is a coordinator, and tells it to delegate via the `agent` tool.
// Without this, a coordinator only sees the discovery/execute_tool workflow
// and does the work itself instead of delegating.
type AgentPoolSection struct{}

func (AgentPoolSection) ID() string { return "agent_pool" }

func (AgentPoolSection) Render(ctx PromptContext) string {
	if len(ctx.Specialists) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("You are a coordinator : you delegate work to specialist sub-agents rather than doing it yourself.\n")
	b.WriteString("Available specialists:\n")
	for _, s := range ctx.Specialists {
		b.WriteString("- ")
		b.WriteString(s.ID)
		if s.Specialty != "" {
			b.WriteString(" — ")
			b.WriteString(s.Specialty)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nTo delegate, call the `agent` tool directly :\n")
	b.WriteString("  agent(agent=\"<specialist-id>\", task=\"<full instruction>\", wait=true)\n")
	b.WriteString("This spawns the sub-agent AND returns its result in one call. Prefer delegating substantial or specialised work to the right specialist. ")
	b.WriteString("Do NOT use execute_tool, run_parallel, or background_run to delegate — call `agent` directly, and never use background_run to wait on an agent run.")
	return b.String()
}

// ChannelInfoSection (doc step 8) — would list available
// notification targets (Slack / Discord / email) from the
// channels module. Empty until channels are ported.
type ChannelInfoSection struct{}

func (ChannelInfoSection) ID() string                    { return "channel_info" }
func (ChannelInfoSection) Render(_ PromptContext) string { return "" }
