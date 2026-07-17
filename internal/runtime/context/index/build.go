package index

import (
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/policy"
)

type Builder struct {
	Synonyms *SynonymTable
}

func NewBuilder() *Builder {
	return &Builder{Synonyms: DefaultSynonyms()}
}

func (b *Builder) Build(
	appActive bool,
	caps *schema.CapabilitiesConfig,
	agent *schema.Agent,
	universe []policy.AvailableAction,
) *ToolIndex {
	visible := policy.BuildAgentToolset(appActive, caps, agent, universe)

	idx := &ToolIndex{
		Tools:      make(map[string]*IndexedTool, len(visible)),
		Categories: make(map[string][]string),
		keyword:    make(map[string]map[string]struct{}),
		prefixes:   make(map[string]map[string]struct{}),
		synonyms:   b.Synonyms,
	}

	for _, a := range visible {
		fqn := a.Module + "." + a.Action
		it := &IndexedTool{
			FQN:    fqn,
			Module: a.Module,
			Action: a.Action,
		}
		if a.Spec != nil {
			it.Description = a.Spec.Description
			it.Params = a.Spec.Params
			it.RiskLevel = a.Spec.RiskLevel
			it.Irreversible = a.Spec.Irreversible
			it.Tags = a.Spec.Tags
			it.Aliases = a.Spec.Aliases
			it.Permissions = a.Spec.Permissions
			it.ToolPrompt = a.Spec.ToolPrompt
		}
		it.DiscoveryOnly = a.DiscoveryOnly
		idx.Tools[fqn] = it
		idx.Categories[a.Module] = append(idx.Categories[a.Module], fqn)
		b.indexTokens(idx, it)
	}

	for mod, fqns := range idx.Categories {
		sortStrings(fqns)
		idx.Categories[mod] = fqns
	}
	return idx
}

func (b *Builder) indexTokens(idx *ToolIndex, it *IndexedTool) {
	add := func(tok string) {
		if tok == "" {
			return
		}
		set, ok := idx.keyword[tok]
		if !ok {
			set = make(map[string]struct{})
			idx.keyword[tok] = set
		}
		set[it.FQN] = struct{}{}
	}
	addPrefix := func(tok string) {
		if len(tok) < 2 {
			return
		}
		for i := 2; i <= len(tok); i++ {
			pfx := tok[:i]
			set, ok := idx.prefixes[pfx]
			if !ok {
				set = make(map[string]struct{})
				idx.prefixes[pfx] = set
			}
			set[it.FQN] = struct{}{}
		}
	}

	add(it.FQN)
	add(it.Module + "." + it.Action)

	for _, t := range tokenizeWithCamel(it.Action) {
		add(t)
		addPrefix(t)
	}
	for _, t := range tokenizeWithCamel(it.Module) {
		add(t)
		addPrefix(t)
	}

	for _, tag := range it.Tags {
		for _, t := range tokenizeWithCamel(tag) {
			add(t)
		}
	}

	for _, alias := range it.Aliases {
		for _, t := range tokenizeWithCamel(alias) {
			add(t)
			addPrefix(t)
		}
	}

	for _, p := range it.Params {
		for _, t := range tokenizeWithCamel(p.Name) {
			add(t)
		}
	}

	for _, t := range tokenizeWithCamel(it.Description) {
		add(t)
	}
}
