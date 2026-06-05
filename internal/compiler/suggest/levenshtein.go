// Package suggest provides "did you mean..." suggestions via Levenshtein distance.
package suggest

import "strings"

func Levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	la, lb := len(a), len(b)
	switch {
	case la == 0:
		return lb
	case lb == 0:
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			m := prev[j] + 1
			if v := curr[j-1] + 1; v < m {
				m = v
			}
			if v := prev[j-1] + cost; v < m {
				m = v
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func Closest(target string, pool []string, maxDistance int) (string, bool) {
	if maxDistance <= 0 {
		maxDistance = 2
	}
	best := ""
	bestDist := maxDistance + 1
	for _, c := range pool {
		if d := Levenshtein(target, c); d < bestDist {
			bestDist, best = d, c
		}
	}
	return best, best != ""
}

func All(target string, pool []string, maxDistance, limit int) []string {
	if maxDistance <= 0 {
		maxDistance = 2
	}
	if limit <= 0 {
		limit = 5
	}
	type hit struct {
		name string
		dist int
	}
	hits := make([]hit, 0)
	for _, c := range pool {
		if d := Levenshtein(target, c); d <= maxDistance {
			hits = append(hits, hit{name: c, dist: d})
		}
	}
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && (hits[j].dist < hits[j-1].dist ||
			(hits[j].dist == hits[j-1].dist && hits[j].name < hits[j-1].name)); j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.name
	}
	return out
}

func ContainsPrefix(target string, pool []string) (string, bool) {
	if target == "" {
		return "", false
	}
	t := strings.ToLower(target)
	var match string
	n := 0
	for _, c := range pool {
		if strings.HasPrefix(strings.ToLower(c), t) {
			match = c
			n++
		}
	}
	if n == 1 {
		return match, true
	}
	return "", false
}
