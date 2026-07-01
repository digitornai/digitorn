package index

import (
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/policy"
)

// Builder constructs a ToolIndex from a universe of actions + the
// app's security capabilities + the per-agent restrictions. It
// pre-filters the universe through policy.BuildAgentToolset (SG-3)
// so the index NEVER contains an action the agent isn't allowed
// to see. This is the documented schema-build defence layer.
type Builder struct {
	// Synonyms is the synonym table used for query expansion. nil =
	// no synonyms (only literal matches). Use DefaultSynonyms() for
	// the bilingual FR/EN bag.
	Synonyms *SynonymTable
}

// NewBuilder returns a Builder with the default bilingual synonyms.
func NewBuilder() *Builder {
	return &Builder{Synonyms: DefaultSynonyms()}
}

// Build returns the per-agent ToolIndex. Inputs :
//
//   - appActive : appmgr.App.Enabled — feeds gate 0 (inactive)
//   - caps      : tools.capabilities block (may be nil = dev mode)
//   - agent     : the specific agent for sub-agent isolation (gates 1a/3)
//   - universe  : every action declared by every loaded module
//
// Output : an immutable ToolIndex. Safe to share across all
// sessions of this (app version, agent) pair without locks.
//
// The synonym table is shared by reference — many indices can use
// the same DefaultSynonyms() instance.
func (b *Builder) Build(
	appActive bool,
	caps *schema.CapabilitiesConfig,
	agent *schema.Agent,
	universe []policy.AvailableAction,
) *ToolIndex {
	// SG-3 : apply schema-build filter (gates 0/1a/1b/2/3/4-Allow).
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

	// Sort each category for deterministic browse output.
	for mod, fqns := range idx.Categories {
		sortStrings(fqns)
		idx.Categories[mod] = fqns
	}
	return idx
}

// indexTokens walks every searchable field of an IndexedTool and
// registers its tokens in the inverted index. Called once per tool
// at build time. Order :
//
//  1. action name (highest signal)
//  2. module name
//  3. tags
//  4. aliases (multilingual)
//  5. parameter names
//  6. description (lowest signal — many false positives)
//  7. FQN itself (so "filesystem.read" query lands)
//
// The same token may be added multiple times (once per field) ;
// the set semantics of the map de-duplicates per (token, FQN) pair
// so a tool isn't counted twice for repeated words.
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

	// FQN literal — so "filesystem.read" as a query finds it.
	add(it.FQN)
	add(it.Module + "." + it.Action)

	// 1. action + module names (high signal)
	for _, t := range tokenizeWithCamel(it.Action) {
		add(t)
		addPrefix(t)
	}
	for _, t := range tokenizeWithCamel(it.Module) {
		add(t)
		addPrefix(t)
	}

	// 2. tags
	for _, tag := range it.Tags {
		for _, t := range tokenizeWithCamel(tag) {
			add(t)
		}
	}

	// 3. aliases (multilingual)
	for _, alias := range it.Aliases {
		for _, t := range tokenizeWithCamel(alias) {
			add(t)
			addPrefix(t)
		}
	}

	// 4. parameter names
	for _, p := range it.Params {
		for _, t := range tokenizeWithCamel(p.Name) {
			add(t)
		}
	}

	// 5. description (lowest)
	for _, t := range tokenizeWithCamel(it.Description) {
		add(t)
	}
}
