package prompt

import "strings"

type MemorySnapshotSection struct{}

func (MemorySnapshotSection) ID() string { return "memory_snapshot" }
func (MemorySnapshotSection) Render(ctx PromptContext) string {
	if ctx.Memory == nil {
		return ""
	}
	return RenderWorkingMemory(*ctx.Memory)
}

type MemoryInstructionsSection struct{}

func (MemoryInstructionsSection) ID() string { return "memory_instructions" }
func (MemoryInstructionsSection) Render(ctx PromptContext) string {
	if !ctx.MemoryEnabled {
		return ""
	}
	return memoryInstructions
}

type StructuralHintsSection struct{}

func (StructuralHintsSection) ID() string                    { return "structural_hints" }
func (StructuralHintsSection) Render(_ PromptContext) string { return "" }

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

type ChannelInfoSection struct{}

func (ChannelInfoSection) ID() string                    { return "channel_info" }
func (ChannelInfoSection) Render(_ PromptContext) string { return "" }
