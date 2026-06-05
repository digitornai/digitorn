package index

import (
	"sort"
	"strings"
)

// SearchResult is one hit returned by Search. Score is a relative
// ranking value — meaningful only when comparing results of the
// same query. Higher is better.
type SearchResult struct {
	Tool  *IndexedTool
	Score int
	Match string // the token that produced the highest sub-score (for debug)
}

// Search runs a keyword query against the index and returns up to
// `limit` hits ranked by relevance. The scoring formula is documented
// inline (see scoreTool).
//
// Algorithm :
//
//  1. Tokenize the query
//  2. For each token : expand synonyms
//  3. For each (token, synonym) : find every FQN that contains it
//     in the inverted or prefix index
//  4. Accumulate score per FQN
//  5. Sort by score descending, FQN ascending (stable tie-break)
//  6. Return top `limit`
//
// limit <= 0 means "no limit" (return everything that matched).
func (i *ToolIndex) Search(query string, limit int) []SearchResult {
	if i == nil || query == "" {
		return nil
	}
	tokens := tokenizeWithCamel(query)
	if len(tokens) == 0 {
		return nil
	}

	// scores[FQN] = accumulated score from every matching token.
	scores := make(map[string]int, 32)
	bestMatch := make(map[string]string)
	add := func(fqn, matchedToken string, contribution int) {
		scores[fqn] += contribution
		// Remember the highest single-token contribution per tool —
		// useful for debug strings.
		if contribution > scores[fqn]/2 {
			bestMatch[fqn] = matchedToken
		}
	}

	for _, raw := range tokens {
		// Expand synonyms : "supprimer" → ["supprimer", "delete",
		// "remove", "destroy", "erase", "effacer"]
		variants := i.synonyms.Expand(raw)
		seen := make(map[string]struct{}, len(variants))
		for _, v := range variants {
			if _, dup := seen[v]; dup {
				continue
			}
			seen[v] = struct{}{}
			// Direct token hit.
			if fqns, ok := i.keyword[v]; ok {
				for fqn := range fqns {
					add(fqn, v, i.scoreTool(v, raw, fqn))
				}
			}
			// Prefix hit — only on the FIRST (literal, non-synonym)
			// token to avoid synonym-prefix bloat. e.g. "fil" matches
			// every filesystem action.
			if v == raw {
				if fqns, ok := i.prefixes[v]; ok {
					for fqn := range fqns {
						// Prefix score is a small constant — lower
						// than a full-word match so direct matches
						// rank above prefix matches.
						add(fqn, v+"*", scorePrefix)
					}
				}
			}
		}
	}

	// CB-5 : add the semantic contribution to every hit. The
	// documented hybrid formula is `semantic * 10 + keyword_boost`,
	// so semantic dominates for unrelated queries (no keyword
	// match) while exact keyword hits still rank above semantically
	// adjacent tools.
	//
	// nil Semantic = keyword-only (CB-1 behaviour preserved).
	if i.Semantic != nil {
		if qVec, err := i.Semantic.EmbedQuery(query); err == nil && len(qVec) > 0 {
			semHits := i.Semantic.SearchVector(qVec, 0) // every tool
			for _, h := range semHits {
				if h.Score <= 0 {
					continue
				}
				contribution := int(h.Score * float32(scoreSemanticWeight))
				if contribution == 0 {
					continue
				}
				if _, hadKeyword := scores[h.FQN]; !hadKeyword {
					bestMatch[h.FQN] = "semantic"
				}
				scores[h.FQN] += contribution
			}
		}
	}

	if len(scores) == 0 {
		return nil
	}

	out := make([]SearchResult, 0, len(scores))
	for fqn, score := range scores {
		t := i.Tools[fqn]
		if t == nil {
			continue // shouldn't happen but be defensive
		}
		out = append(out, SearchResult{Tool: t, Score: score, Match: bestMatch[fqn]})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		return out[a].Tool.FQN < out[b].Tool.FQN
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// scoreSemanticWeight is the multiplier from the documented hybrid
// formula `semantic_score * 10 + keyword_boost`. Semantic score is
// in [0, 1] so the maximum semantic contribution is 10 — below
// an exact FQN match (100) and exact action name match (50), but
// above tag (15) and description token (5) matches. This matches
// the doc behaviour : "semantic dominates, but exact-name matches
// get a significant bonus".
const scoreSemanticWeight = 10

// Score constants — tuned so :
//
//   - Exact FQN match dominates everything (100)
//   - Exact action name match is close behind (50)
//   - Tag match outranks description match (15 vs 5)
//   - Alias match earns a boost (25) so multilingual queries
//     surface the right tool even when the description is in
//     another language
//   - Prefix match is the floor (3)
//
// These weights come from a calibration against the reference
// daemon's scoring.py (semantic * 10 + keyword bonus +2-3) — we
// drop the semantic part for CB-1 (handled in CB-5) and use a
// pure keyword formula that mimics the keyword bonus column.
const (
	scoreExactFQN    = 100
	scoreActionName  = 50
	scoreModuleName  = 20
	scoreAlias       = 25
	scoreTag         = 15
	scoreParam       = 8
	scoreDescription = 5
	scorePrefix      = 3
)

// scoreTool reports how much credit token `v` (a possibly-expanded
// synonym of the original `original`) earns for tool `fqn`. The
// score depends on WHERE the match landed (action name vs
// description vs alias vs tag).
func (i *ToolIndex) scoreTool(v, original, fqn string) int {
	t := i.Tools[fqn]
	if t == nil {
		return 0
	}
	score := 0

	// Direct FQN match : the user typed the canonical name verbatim.
	if v == strings.ToLower(fqn) {
		return scoreExactFQN
	}
	// Action / module name match.
	if v == strings.ToLower(t.Action) {
		score += scoreActionName
	}
	if v == strings.ToLower(t.Module) {
		score += scoreModuleName
	}
	// Alias match — multilingual.
	for _, a := range t.Aliases {
		la := strings.ToLower(a)
		if v == la {
			score += scoreAlias
			break
		}
		// Multi-word aliases : check tokenized form too.
		for _, tok := range tokenizeWithCamel(a) {
			if v == tok {
				score += scoreAlias / 2
				break
			}
		}
	}
	// Tag match.
	for _, tag := range t.Tags {
		if v == strings.ToLower(tag) {
			score += scoreTag
			break
		}
		for _, tok := range tokenizeWithCamel(tag) {
			if v == tok {
				score += scoreTag / 2
				break
			}
		}
	}
	// Description match.
	for _, tok := range tokenizeWithCamel(t.Description) {
		if v == tok {
			score += scoreDescription
			break
		}
	}
	// Parameter name match.
	for _, p := range t.Params {
		if v == strings.ToLower(p.Name) {
			score += scoreParam
			break
		}
	}

	// If the synonym path produced the match (v != original), give
	// it a small bonus so direct matches rank above synonym matches
	// when both apply.
	if v != strings.ToLower(original) && score > 0 {
		// Penalise synonym path slightly so a direct match always
		// edges out a synonym match for the same FQN.
		score = (score * 80) / 100
	}

	return score
}
