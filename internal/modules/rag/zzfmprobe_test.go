package rag

import (
	"fmt"
	"testing"
)

func TestFMProbe_KBNameCollision(t *testing.T) {
	names := []string{"My-KB", "my_kb", "my.kb", "My KB", "my/kb", "default", "Default"}
	pg := map[string][]string{}
	es := map[string][]string{}
	for _, n := range names {
		pt := pgTable(n)
		ei := esIndex(n)
		pg[pt] = append(pg[pt], n)
		es[ei] = append(es[ei], n)
		fmt.Printf("PROBE kb=%-10q pgTable=%-12q esIndex=%-12q\n", n, pt, ei)
	}
	for tbl, ns := range pg {
		if len(ns) > 1 {
			fmt.Printf("PROBE COLLISION pgvector table=%q shared by KBs %v\n", tbl, ns)
		}
	}
	for ix, ns := range es {
		if len(ns) > 1 {
			fmt.Printf("PROBE COLLISION elastic index=%q shared by KBs %v\n", ix, ns)
		}
	}
}

func TestFMProbe_CosineCacheFalseHit(t *testing.T) {
	sim := cosineCache([]float32{1, 0, 99, 99}, []float32{1, 0})
	fmt.Printf("PROBE cosine 4d-vs-2d prefix-aligned sim=%.4f false-hit=%v\n", sim, sim >= 0.97)
	c := newSemCache(CacheConfig{Enabled: true})
	c.put("kb", "", 5, []float32{1, 0}, []SearchHit{{Document: Document{ID: "STALE-2D"}}})
	hits, ok := c.get("kb", "", 5, []float32{1, 0, 50, 50})
	fmt.Printf("PROBE cache returns stale 2d entry for 4d query: ok=%v hits=%v\n", ok, hits)
}

func TestFMProbe_ValueInAllowedNumeric(t *testing.T) {
	fmt.Printf("PROBE valueInAllowed(nil, [user]) = %v (missing owner field)\n", valueInAllowed(nil, []string{"user"}))
	fmt.Printf("PROBE valueInAllowed(int 5, [5]) = %v (numeric ACL field never matches)\n", valueInAllowed(5, []string{"5"}))
	fmt.Printf("PROBE valueInAllowed(int64 5, [5]) = %v\n", valueInAllowed(int64(5), []string{"5"}))
	fmt.Printf("PROBE valueInAllowed(bool true, [true]) = %v\n", valueInAllowed(true, []string{"true"}))
	fmt.Printf("PROBE valueInAllowed('user', [user]) = %v (string works)\n", valueInAllowed("user", []string{"user"}))
}
