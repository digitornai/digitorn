package toolmw

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

// semanticCache serves a cached result when a new call is semantically close to
// a recent one (not just byte-identical, like dedup). Embeddings come from the
// injected Embedder ; with none it is inert (passthrough), matching the
// reference's behaviour without an embedding model.
//
// Fix vs the reference, whose entries were shared across sessions : the cache
// is keyed by session, so a user's tool output never leaks into another's. The
// MCP-specific risk-based invalidation is dropped — in this per-module model
// the app author controls cacheability by attaching semantic_cache only to the
// read-heavy modules where it makes sense.
type semanticCache struct {
	threshold  float64
	ttl        time.Duration
	maxEntries int
	embedder   Embedder
	logger     *slog.Logger

	mu       sync.Mutex
	sessions map[string][]cacheEntry
}

type cacheEntry struct {
	embedding []float32
	res       tool.Result
	at        time.Time
}

func newSemanticCache(cfg map[string]any, deps Deps) (Middleware, error) {
	return &semanticCache{
		threshold:  cfgFloat(cfg, "similarity_threshold", 0.85),
		ttl:        secs(cfgFloat(cfg, "ttl", 300.0)),
		maxEntries: cfgInt(cfg, "max_entries", 100),
		embedder:   deps.Embedder,
		logger:     deps.Logger,
		sessions:   map[string][]cacheEntry{},
	}, nil
}

func (s *semanticCache) Name() string { return "semantic_cache" }

func (s *semanticCache) Handle(ctx context.Context, cc CallContext, next Next) (tool.Result, error) {
	if s.embedder == nil {
		return next(ctx, cc)
	}
	emb, err := s.embedder.Embed(ctx, buildQuery(cc.ToolName, cc.Params))
	if err != nil || len(emb) == 0 {
		return next(ctx, cc) // best-effort, like the reference
	}
	now := time.Now()

	s.mu.Lock()
	entries := s.prune(cc.SessionID, now)
	bestSim, bestIdx := 0.0, -1
	for i := range entries {
		sim := cosine(emb, entries[i].embedding)
		if sim > bestSim {
			bestSim, bestIdx = sim, i
		}
	}
	if bestIdx >= 0 && bestSim >= s.threshold {
		res := entries[bestIdx].res
		s.mu.Unlock()
		if s.logger != nil {
			s.logger.Debug("semantic_cache_hit", slog.String("session", cc.SessionID),
				slog.String("tool", cc.ToolName), slog.Float64("sim", bestSim))
		}
		return res, nil
	}
	s.mu.Unlock()

	res, err := next(ctx, cc)
	if err != nil || !res.Success {
		return res, err
	}

	s.mu.Lock()
	entries = append(s.sessions[cc.SessionID], cacheEntry{embedding: emb, res: res, at: now})
	if len(entries) > s.maxEntries {
		entries = entries[len(entries)-s.maxEntries:]
	}
	s.sessions[cc.SessionID] = entries
	s.mu.Unlock()
	return res, nil
}

// prune drops expired entries for the session and returns the live slice. The
// caller holds s.mu.
func (s *semanticCache) prune(session string, now time.Time) []cacheEntry {
	entries := s.sessions[session]
	if len(entries) == 0 {
		return entries
	}
	live := entries[:0]
	for _, e := range entries {
		if now.Sub(e.at) <= s.ttl {
			live = append(live, e)
		}
	}
	s.sessions[session] = live
	return live
}

func buildQuery(toolName string, params []byte) string {
	parts := []string{strings.ReplaceAll(toolName, "_", " ")}
	var m map[string]any
	if json.Unmarshal(params, &m) == nil {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch v := m[k].(type) {
			case string:
				if len(v) < 200 {
					parts = append(parts, k+": "+v)
				}
			case float64, bool:
				parts = append(parts, fmt.Sprintf("%s=%v", k, v))
			}
		}
	}
	return strings.Join(parts, " ")
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(na) * math.Sqrt(nb)
	if denom == 0 {
		return 0
	}
	return dot / denom
}
