package prompt

import "strings"

type Assembler struct {
	Sections []Section
}

func NewAssembler() *Assembler {
	return &Assembler{
		Sections: []Section{
			AuthorityPreambleSection{},
			IdentitySection{},
			ToolInstructionsSection{},
			IntentSection{},
			StructuralHintsSection{},
			OperatingGuideSection{},
			CommunicateSection{},
			ChannelInfoSection{},
			SkillsSection{},
			AgentPoolSection{},
			MemorySnapshotSection{},
			MemoryInstructionsSection{},
			ModuleSectionsSection{},
			ToolUsageSection{},
			UserPromptSection{},
		},
	}
}

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
