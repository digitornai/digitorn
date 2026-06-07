package rag

import "sort"

const rrfK = 60

// rrfFuse merges ranked id lists via Reciprocal Rank Fusion. Each list
// contributes weight/(k+rank) to an id's score ; weights default to 1.
// Returns the fused ids, best first, capped at topK.
func rrfFuse(rankings [][]string, weights []float64, topK int) []string {
	score := map[string]float64{}
	for li, ids := range rankings {
		w := 1.0
		if li < len(weights) && weights[li] > 0 {
			w = weights[li]
		}
		for rank, id := range ids {
			score[id] += w / float64(rrfK+rank+1)
		}
	}
	type pair struct {
		id string
		sc float64
	}
	arr := make([]pair, 0, len(score))
	for id, sc := range score {
		arr = append(arr, pair{id, sc})
	}
	sort.Slice(arr, func(i, j int) bool {
		if arr[i].sc != arr[j].sc {
			return arr[i].sc > arr[j].sc
		}
		return arr[i].id < arr[j].id
	})
	out := make([]string, 0, topK)
	for _, p := range arr {
		if topK > 0 && len(out) >= topK {
			break
		}
		out = append(out, p.id)
	}
	return out
}
