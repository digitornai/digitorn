package prompt

import "strings"

// Assembler builds the final system prompt by rendering each
// Section in order and joining them with a blank line separator.
//
// The ordering follows the REFERENCE DAEMON's build_system_prompt
// (prompt.py) — the ground truth — which differs from the doc's
// idealized 9-source list (context_builder.md §2). In particular the
// reference injects the agent pool + memory snapshot LATE, as module
// get_prompt_sections() after skills, NOT at the top:
//
//	preamble → identity → tool-instructions (+structural hints) →
//	how-to-think/use → plan_first → channels → skills →
//	agent-pool → memory → other module sections → tool-usage →
//	user-prompt (ALWAYS LAST)
//
// Construct via NewAssembler() to get the default ordering ; tests
// or experimental builds can replace Sections directly.
type Assembler struct {
	Sections []Section
}

// NewAssembler returns the default-ordered assembler matching the
// reference daemon's build_system_prompt section order. Sections
// without backing data (memory, agent pool, channels, module
// contributions) render empty and are dropped from the joined output.
func NewAssembler() *Assembler {
	return &Assembler{
		Sections: []Section{
			AuthorityPreambleSection{}, // 1  supervisor protocol — ALWAYS FIRST
			IdentitySection{},          // 2  you are agent X
			ToolInstructionsSection{},  // 3  tool catalogue (mode-aware)
			StructuralHintsSection{},   // 4  (reference embeds these in the tool block)
			OperatingGuideSection{},    // 5  how to think / use tools
			CommunicateSection{},       // 6  plan_first "how to communicate"
			ChannelInfoSection{},       // 7  channels
			SkillsSection{},            // 8  available /commands
			// 9-11 : the reference injects agent-pool + memory as module
			// get_prompt_sections() — LATE, after skills, NOT at the top. Go
			// keeps them as dedicated per-turn sections (memory is turn-dynamic
			// and can't ride the app/agent-scoped contributor cache) but places
			// them at the reference position.
			AgentPoolSection{},          // 9
			MemorySnapshotSection{},     // 10
			MemoryInstructionsSection{}, // 11
			ModuleSectionsSection{},     // 12 other modules' contributed sections
			ToolUsageSection{},          // 13 per-tool usage prompts (+ dynamic overlay)
			UserPromptSection{},         // 14 — ALWAYS LAST (# APP-DEFINED PERSONALITY)
		},
	}
}

// Assemble renders every section in order and joins non-empty
// blocks with a blank line (\n\n). The doc invariant is :
//
//   - The user_prompt section is the LAST line of the final string
//     (so app-level personality doesn't get overridden by runtime
//     instructions appended after).
//   - The order of all sections matches the doc bit-for-bit.
//   - Empty sections are dropped (no double blank lines).
//
// Returns the empty string only when EVERY section produced empty
// output (e.g. a fully-default no-agent context).
func (a *Assembler) Assemble(ctx PromptContext) string {
	if a == nil {
		return ""
	}
	parts := make([]string, 0, len(a.Sections))
	for _, s := range a.Sections {
		if r := strings.TrimRight(s.Render(ctx), "\n"); r != "" {
			parts = append(parts, r)
		}
	}
	return strings.Join(parts, "\n\n")
}

// SectionIDs returns the ordered list of section IDs the assembler
// will render. Useful for observability and snapshot tests.
func (a *Assembler) SectionIDs() []string {
	if a == nil {
		return nil
	}
	out := make([]string, len(a.Sections))
	for i, s := range a.Sections {
		out[i] = s.ID()
	}
	return out
}
