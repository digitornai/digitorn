package tokencount

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/tiktoken-go/tokenizer"
)

// TestCount_ExactMatchesTiktokenForOpenAI proves the exact path really runs the
// model's tokeniser (not the heuristic) and routes to the right encoding :
// gpt-4o → o200k_base, gpt-4 → cl100k_base. We compare against the library's
// own count for the same encoding.
func TestCount_ExactMatchesTiktokenForOpenAI(t *testing.T) {
	c := New()
	text := "The quick brown fox jumps over the lazy dog, 12345 — café déjà vu. " +
		strings.Repeat("tokens tokens tokens ", 20)

	for _, tc := range []struct {
		model string
		enc   tokenizer.Encoding
	}{
		{"gpt-4o", tokenizer.O200kBase},
		{"gpt-4o-mini", tokenizer.O200kBase},
		{"gpt-5", tokenizer.O200kBase},
		{"o1-preview", tokenizer.O200kBase},
		{"openai/gpt-4.1", tokenizer.O200kBase},
		{"gpt-4-turbo", tokenizer.Cl100kBase},
		{"gpt-3.5-turbo", tokenizer.Cl100kBase},
	} {
		codec, err := tokenizer.Get(tc.enc)
		if err != nil {
			t.Fatalf("get %s: %v", tc.enc, err)
		}
		want, _ := codec.Count(text)
		got := c.Count(text, "openai", tc.model)
		if got != want {
			t.Errorf("%s: Count=%d, want exact tiktoken %d (%s)", tc.model, got, want, tc.enc)
		}
		// It must NOT be the heuristic (proves real tokenisation happened).
		if got == heuristic(text) {
			t.Errorf("%s: exact count equals heuristic — tokeniser not used", tc.model)
		}
	}
}

func TestCount_HeuristicForNonOpenAI(t *testing.T) {
	c := New()
	text := "some text body for an open or closed non-openai model"
	want := heuristic(text)
	for _, model := range []string{"claude-3-5-sonnet", "gemini-1.5-pro", "deepseek-chat", "mistral-large", "llama-3.1-70b"} {
		if got := c.Count(text, "anthropic", model); got != want {
			t.Errorf("%s: Count=%d, want heuristic %d", model, got, want)
		}
	}
}

func TestCount_CacheHitIsDeterministicAndPopulates(t *testing.T) {
	c := New()
	text := "deterministic caching check with enough length to matter"
	first := c.Count(text, "openai", "gpt-4o")
	if c.cache.size() != 1 {
		t.Fatalf("exact-path miss must populate the cache, size=%d", c.cache.size())
	}
	second := c.Count(text, "openai", "gpt-4o")
	if first != second {
		t.Errorf("cache returned a different value: %d != %d", first, second)
	}
	if c.cache.size() != 1 {
		t.Errorf("a hit must not add a new entry, size=%d", c.cache.size())
	}
}

func TestCount_EmptyIsZero(t *testing.T) {
	if n := New().Count("", "openai", "gpt-4o"); n != 0 {
		t.Errorf("empty text must be 0 tokens, got %d", n)
	}
}

func TestCountCache_Bounded(t *testing.T) {
	cache := newCountCache(2)
	cache.put("f", "a", 1)
	cache.put("f", "b", 2)
	cache.put("f", "c", 3) // over cap → skipped
	if sz := cache.size(); sz != 2 {
		t.Fatalf("cache must be bounded at 2, got %d", sz)
	}
	if _, ok := cache.get("f", "c"); ok {
		t.Error("over-cap entry must not be cached")
	}
}

// TestCount_ConcurrentNoRace hammers the counter from many goroutines on the
// exact (cached) path. Run with -race : the codec memoisation and the cache
// must be safe under concurrency (the daemon counts many sessions in parallel).
func TestCount_ConcurrentNoRace(t *testing.T) {
	c := New()
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				txt := fmt.Sprintf("message %d body shared-ish %d", g%4, i%8)
				_ = c.Count(txt, "openai", "gpt-4o")
				_ = c.Count("non openai "+txt, "deepseek", "deepseek-chat")
			}
		}(g)
	}
	wg.Wait()
}
