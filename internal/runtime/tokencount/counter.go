// Package tokencount counts tokens for a piece of text given a model family.
// It is EXACT for OpenAI families via tiktoken (cl100k_base / o200k_base, fully
// offline — the dictionaries are compiled into the binary) and falls back to a
// ~4-chars/token heuristic for every other family. The provider usage anchor
// (CTX-7.1) corrects any heuristic drift at every turn boundary, so the
// heuristic only ever affects the small between-anchor delta — never the
// steady-state number.
//
// Counting a piece of text is a PURE function of (text, family) : the same
// string always has the same count. So results are memoised in a
// content-addressed, immutable cache — a string is tokenised at most once in
// its life, across all sessions (the cache key carries no identity, only a
// hash of the text + family, so sharing it cannot leak anything between
// sessions — same safety model as the BlobStore).
package tokencount

import (
	"strings"
	"sync"

	"github.com/tiktoken-go/tokenizer"
)

const heuristicCharsPerToken = 4

// Counter is a concurrency-safe token counter. Construct with New ; safe to
// share across goroutines and sessions.
type Counter struct {
	mu     sync.RWMutex
	codecs map[tokenizer.Encoding]tokenizer.Codec
	cache  *countCache
}

// New returns a ready Counter with a bounded content-addressed cache.
func New() *Counter {
	return &Counter{
		codecs: make(map[tokenizer.Encoding]tokenizer.Codec),
		cache:  newCountCache(defaultCacheMaxEntries),
	}
}

// Count returns the number of tokens text occupies for the given provider /
// model. Exact for OpenAI families, heuristic otherwise. O(1) on a cache hit ;
// on a miss it tokenises once (exact path) or divides (heuristic). Never
// errors — a tokeniser failure degrades to the heuristic.
func (c *Counter) Count(text, provider, model string) int {
	if text == "" {
		return 0
	}
	enc, exact := encodingFor(model)
	if !exact {
		// Hashing the text to consult the cache would cost more than the
		// heuristic itself, so the heuristic path is uncached.
		return heuristic(text)
	}
	famKey := string(enc)
	if n, hit := c.cache.get(famKey, text); hit {
		return n
	}
	n := heuristic(text)
	if codec := c.codecFor(enc); codec != nil {
		if got, err := codec.Count(text); err == nil {
			n = got
		}
	}
	c.cache.put(famKey, text, n)
	return n
}

// codecFor lazily builds and memoises the tiktoken codec for an encoding.
func (c *Counter) codecFor(enc tokenizer.Encoding) tokenizer.Codec {
	c.mu.RLock()
	codec := c.codecs[enc]
	c.mu.RUnlock()
	if codec != nil {
		return codec
	}
	built, err := tokenizer.Get(enc)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	c.codecs[enc] = built
	c.mu.Unlock()
	return built
}

// encodingFor maps a model name to its tiktoken encoding. Returns ok=false for
// non-OpenAI families (Claude, Gemini, Llama, Mistral, DeepSeek, …) whose exact
// count comes from the provider anchor, not a local tokeniser. A "vendor/model"
// prefix (OpenRouter-style) is stripped so "openai/gpt-4o" still resolves.
func encodingFor(model string) (tokenizer.Encoding, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	switch {
	case hasAnyPrefix(m, "gpt-4o", "chatgpt-4o", "gpt-4.1", "gpt-5", "o1", "o3", "o4"):
		return tokenizer.O200kBase, true
	case hasAnyPrefix(m, "gpt-4", "gpt-3.5", "gpt-35"):
		return tokenizer.Cl100kBase, true
	}
	return "", false
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// heuristic is the documented ~4-chars/token estimate, rounded up.
func heuristic(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + heuristicCharsPerToken - 1) / heuristicCharsPerToken
}
