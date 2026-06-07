package rag

import (
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"
)

const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

type bm25Doc struct {
	tf     map[string]int
	length int
}

// BM25 is an in-memory Okapi BM25 keyword index keyed by document id.
// Re-adding an id replaces its entry, so it stays consistent with the
// vector store under re-ingestion. Safe for concurrent use.
type BM25 struct {
	mu    sync.Mutex
	docs  map[string]*bm25Doc
	dirty bool
	df    map[string]int
	avgdl float64
}

type bm25Hit struct {
	ID    string
	Score float64
}

func NewBM25() *BM25 {
	return &BM25{docs: map[string]*bm25Doc{}, df: map[string]int{}}
}

func (b *BM25) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.docs)
}

func (b *BM25) Add(id, text string) {
	toks := tokenize(text)
	d := &bm25Doc{tf: map[string]int{}, length: len(toks)}
	for _, t := range toks {
		d.tf[t]++
	}
	b.mu.Lock()
	b.docs[id] = d
	b.dirty = true
	b.mu.Unlock()
}

func (b *BM25) recompute() {
	df := make(map[string]int, len(b.df))
	var total int
	for _, d := range b.docs {
		total += d.length
		for t := range d.tf {
			df[t]++
		}
	}
	b.df = df
	if len(b.docs) > 0 {
		b.avgdl = float64(total) / float64(len(b.docs))
	}
	b.dirty = false
}

func (b *BM25) Search(query string, topN int) []bm25Hit {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dirty {
		b.recompute()
	}
	if len(b.docs) == 0 {
		return nil
	}
	n := float64(len(b.docs))
	qterms := tokenize(query)
	hits := make([]bm25Hit, 0, len(b.docs))
	for id, d := range b.docs {
		var score float64
		for _, t := range qterms {
			f := d.tf[t]
			if f == 0 {
				continue
			}
			idf := math.Log(1 + (n-float64(b.df[t])+0.5)/(float64(b.df[t])+0.5))
			denom := float64(f) + bm25K1*(1-bm25B+bm25B*float64(d.length)/b.avgdl)
			score += idf * (float64(f) * (bm25K1 + 1)) / denom
		}
		if score > 0 {
			hits = append(hits, bm25Hit{ID: id, Score: score})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if topN > 0 && len(hits) > topN {
		hits = hits[:topN]
	}
	return hits
}

// tokenize lower-cases and splits on non-letter/digit runes (unicode-aware
// so multilingual text tokenizes sensibly).
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
