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

type PromptContext struct {
	Agent *schema.Agent

	AppName    string
	AppVersion string

	InjectionMode injection.Mode

	ToolIndex *index.ToolIndex

	InjectedTools []llm.ToolSpec

	Skills []SkillEntry

	Specialists []SpecialistEntry

	MemoryEnabled bool

	Memory *WorkingMemoryView

	ModuleSections []domainmodule.PromptSection

	DynamicToolPrompts map[string]string
}

type SkillEntry struct {
	Name        string
	Description string
}

type SpecialistEntry struct {
	ID        string
	Specialty string
}

type Section interface {
	ID() string

	Render(ctx PromptContext) string
}

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

func moduleOf(name string) string {
	if i := strings.Index(name, "__"); i > 0 {
		return name[:i]
	}
	return name
}

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
