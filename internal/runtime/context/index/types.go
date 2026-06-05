// Package index implements the tool index that powers the
// context_builder's tool-discovery surface (CB-1). The index is
// built per (app version, agent) tuple and pre-filtered by the
// security gates (SG-3 BuildAgentToolset) so the LLM never sees
// tools its agent isn't allowed to call.
//
// Documented in docs-site/docs/reference/modules/context_builder.md
// section "Tool indexing" :
//
//	"At app start, scans every loaded module and builds a searchable
//	 index of all available actions. Per action it records :
//	   - Fully qualified name (filesystem.read, git.status)
//	   - The @action description
//	   - Tags + multilingual aliases
//	   - Parameter names + descriptions
//	   - Side effects + risk level
//	   - Synonym expansions (e.g. delete indexes remove, destroy,
//	     erase)"
//
// The semantic side (FastEmbed + Qdrant HNSW) lives in package
// semantic (CB-5). This package owns the keyword half : inverted
// index, prefix matching, fuzzy matching, synonym expansion.
//
// Construction is one-shot, then the index is read-only — safe to
// share across all sessions of an (app, agent) pair without locks.
package index

import (
	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

// IndexedTool is the per-action record stored in the ToolIndex.
// Every field is read directly by either the LLM-facing surface
// (Description, Params, Tags) or the search-side (Tokens, Aliases,
// Synonyms expanded from Tags / Description).
//
// FQN is the canonical "module.action" key the rest of the
// runtime uses (the dispatcher, the gates, the audit log). It
// MUST equal `Module + "." + Action`.
type IndexedTool struct {
	FQN          string // "filesystem.read"
	Module       string // "filesystem"
	Action       string // "read"
	Description  string // @action description (one paragraph)
	Params       []tool.ParamSpec
	RiskLevel    tool.RiskLevel
	Irreversible bool

	// ToolPrompt : optional extra usage instructions for this action,
	// surfaced verbatim in the prompt's "Tool Usage Instructions"
	// section. Mirrors tool.Spec.ToolPrompt.
	ToolPrompt string

	// Tags : free-form classification keywords from @action(tags=...).
	// e.g. ["io", "files", "read"]. Used by browse_category and by
	// the keyword search for tag-boost scoring.
	Tags []string

	// Aliases : multilingual short names declared on @action(aliases=...).
	// e.g. ["lire", "read file"]. Used by the keyword search to find
	// the tool from non-English queries.
	Aliases []string

	// Permissions : the symbolic permissions the action requires.
	// Mirrors tool.Spec.Permissions. Used by gate 3.
	Permissions []string
}

// ToolIndex is the in-memory index built by Builder. Immutable after
// construction ; safe to share across all sessions of one (app
// version, agent) pair.
//
// Four access surfaces :
//
//	Tools      : map[FQN]*IndexedTool for O(1) lookup by canonical name.
//	Categories : map[module]→[]FQN for browse_category meta-tool.
//	keyword    : inverted index (token → []FQN) for search_tools.
//	Semantic   : optional vector index for cosine similarity (CB-5).
//
// FQNs in alphabetical order for deterministic snapshot tests.
type ToolIndex struct {
	Tools      map[string]*IndexedTool
	Categories map[string][]string // module → sorted FQNs

	// Inverted index : every token (from action name, description,
	// tags, aliases, parameters) → set of FQNs that contain it.
	// Built once at construction ; queries iterate over query
	// tokens and intersect the resulting sets.
	keyword map[string]map[string]struct{}

	// prefixes : suffixes-of-name index for cheap prefix matches.
	// "fil" → [filesystem.read, filesystem.write, ...]
	prefixes map[string]map[string]struct{}

	// synonyms : the bag of synonym expansions used by Search. Held
	// by reference so the same SynonymTable can be shared across
	// many indices (it's read-only).
	synonyms *SynonymTable

	// Semantic is the optional vector index built by CB-5. When
	// non-nil, Search adds a `semantic_score * 10` term to the
	// keyword score before ranking. When nil, Search falls back to
	// keyword-only (CB-1 behaviour, preserved).
	//
	// Typed as `any` so this package doesn't import embeddings
	// (which would create a circular import). The actual type is
	// *embeddings.SemanticIndex ; callers use AttachSemantic.
	Semantic SemanticSearcher
}

// SemanticSearcher is the interface ToolIndex sees of the semantic
// index. Lets the embeddings package implement it without this
// package importing embeddings.
type SemanticSearcher interface {
	// SearchVector returns the top-`limit` FQNs ranked by cosine
	// similarity to the query vector.
	SearchVector(queryVec []float32, limit int) []SemanticHit

	// EmbedQuery returns the embedding vector for a raw query string.
	// Used when the caller has only the string, not the vector.
	EmbedQuery(query string) ([]float32, error)
}

// SemanticHit mirrors embeddings.SemanticHit but is declared here
// so the keyword package can use it without importing embeddings.
type SemanticHit struct {
	FQN   string
	Score float32
}

// FQNList returns every FQN in the index, alphabetically sorted.
// Useful for snapshot tests and for emitting the "tool count"
// metric.
func (i *ToolIndex) FQNList() []string {
	if i == nil || len(i.Tools) == 0 {
		return nil
	}
	out := make([]string, 0, len(i.Tools))
	for k := range i.Tools {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// CategoryList returns the modules in the index, alphabetically.
// Backs the list_categories meta-tool.
func (i *ToolIndex) CategoryList() []string {
	if i == nil {
		return nil
	}
	out := make([]string, 0, len(i.Categories))
	for k := range i.Categories {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// Get returns the IndexedTool for a given FQN, or nil if not found.
// O(1). The LLM's calls all flow through here.
func (i *ToolIndex) Get(fqn string) *IndexedTool {
	if i == nil {
		return nil
	}
	return i.Tools[fqn]
}

// sortStrings is a small allocation-free sort to avoid importing the
// stdlib sort package across every helper. The indices we sort are
// tiny (< 1000 entries even on large apps) so insertion sort is
// fine ; switch to sort.Strings if the catalog ever grows beyond
// ~5000 entries.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
