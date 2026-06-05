package tui

import (
	"sort"
	"strings"
)

// fuzzy.go : a small, dependency-free fuzzy matcher for picker/search filtering.
// A query matches a target when its runes appear in order (case-insensitive
// subsequence) ; the score rewards consecutive runs and word-boundary starts so
// "ss" ranks "settings" above "passwords". Good enough for list filtering — not
// a general-purpose ranker.

// fuzzyScore reports whether query subsequence-matches target and, if so, a
// score where higher is a better match. An empty query matches everything with
// score 0 (preserving the caller's original order on ties).
func fuzzyScore(query, target string) (int, bool) {
	if query == "" {
		return 0, true
	}
	q := []rune(strings.ToLower(query))
	t := []rune(strings.ToLower(target))
	score, qi, prev := 0, 0, -2
	for ti := 0; ti < len(t) && qi < len(q); ti++ {
		if t[ti] != q[qi] {
			continue
		}
		if ti == prev+1 {
			score += 6 // consecutive match
		} else {
			score++
		}
		if ti == 0 || isWordBoundary(t[ti-1]) {
			score += 4 // start of a word
		}
		prev = ti
		qi++
	}
	if qi < len(q) {
		return 0, false // not all query runes consumed
	}
	score -= len(t) / 20 // mild preference for shorter targets
	return score, true
}

func isWordBoundary(r rune) bool {
	switch r {
	case ' ', '\t', '-', '_', '/', '\\', '.', ':', '@':
		return true
	}
	return false
}

// fuzzyFilter ranks indices [0,len(labels)) by fuzzyScore against query, keeping
// only matches, best first. Ties keep original order (stable). An empty query
// returns every index in order.
func fuzzyFilter(query string, labels []string) []int {
	type hit struct {
		idx, score, ord int
	}
	var hits []hit
	for i, l := range labels {
		if s, ok := fuzzyScore(query, l); ok {
			hits = append(hits, hit{i, s, i})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].score != hits[b].score {
			return hits[a].score > hits[b].score
		}
		return hits[a].ord < hits[b].ord
	})
	out := make([]int, len(hits))
	for i, h := range hits {
		out[i] = h.idx
	}
	return out
}
