package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	pkgmodule "github.com/mbathepaul/digitorn/pkg/module"
)

// sindex.go : the EPHEMERAL semantic index that makes grep code-aware.
// Per workspace root we keep an in-memory set of embedded code chunks,
// built asynchronously (never on the loop) and bounded by an LRU cap +
// TTL so a session's workdir index disappears on its own — no memory
// saturation, no persistence. grep fuses the trigram-exact matches with
// the semantically-nearest chunks here. Mirrors tindexManager's lifecycle
// (LRU / TTL / dirty / async build / recover-and-degrade).

const (
	sindexTTL        = 15 * time.Minute // idle index ages out
	sindexMaxRoots   = 4                // hot per-workdir indexes kept (memory cap)
	sindexMaxChunks  = 8000             // per-root chunk cap (memory cap)
	sindexChunkLines = 60
	sindexOverlap    = 10
	sindexBatch      = 64
)

// ignoredDirs are skipped during a code-index walk (heavy / non-source).
var sindexIgnoredDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, "target": true, ".idea": true, ".vscode": true,
	".next": true, "__pycache__": true, ".venv": true, "venv": true,
}

type sChunk struct {
	path  string
	line  int
	text  string
	vec   []float32
}

type sHit struct {
	Path    string  `json:"path"`
	Line    int     `json:"line"`
	Snippet string  `json:"snippet"`
	Score   float32 `json:"score"`
}

type sindex struct {
	root     string
	maxBytes int64

	mu       sync.Mutex
	building bool
	ready    bool
	model    string
	chunks   []sChunk
	builtAt  time.Time
	usedAt   time.Time
	dirty    bool
}

type sindexManager struct {
	mu     sync.Mutex
	byRoot map[string]*sindex
}

var sindexes = &sindexManager{byRoot: map[string]*sindex{}}

func (m *sindexManager) get(root string, maxBytes int64) *sindex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.byRoot[root]; ok {
		return s
	}
	if len(m.byRoot) >= sindexMaxRoots {
		m.evictLRULocked()
	}
	s := &sindex{root: root, maxBytes: maxBytes}
	m.byRoot[root] = s
	return s
}

func (m *sindexManager) markDirty(abs string) {
	m.mu.Lock()
	roots := make([]*sindex, 0, len(m.byRoot))
	for _, s := range m.byRoot {
		roots = append(roots, s)
	}
	m.mu.Unlock()
	for _, s := range roots {
		if underRoot(s.root, abs) {
			s.mu.Lock()
			s.dirty = true
			s.mu.Unlock()
		}
	}
}

func (m *sindexManager) evictLRULocked() {
	var oldestKey string
	var oldest time.Time
	for k, s := range m.byRoot {
		s.mu.Lock()
		u := s.usedAt
		s.mu.Unlock()
		if oldestKey == "" || u.Before(oldest) {
			oldestKey, oldest = k, u
		}
	}
	if oldestKey != "" {
		delete(m.byRoot, oldestKey)
	}
}

// maybeBuild kicks an async (re)build when there is no index yet or the
// current one is stale/dirty, unless one is already running. recover()
// guards the build so a failure degrades to "no semantic results", never
// a crash.
func (s *sindex) maybeBuild(emb pkgmodule.Embedder, model string) {
	s.mu.Lock()
	stale := s.dirty || (s.ready && time.Since(s.builtAt) > sindexTTL)
	if s.building || (s.ready && !stale) {
		s.mu.Unlock()
		return
	}
	s.building = true
	s.mu.Unlock()

	go func() {
		defer func() {
			recover()
			s.mu.Lock()
			s.building = false
			s.mu.Unlock()
		}()
		chunks := s.build(emb, model)
		s.mu.Lock()
		s.chunks = chunks
		s.ready = true
		s.dirty = false
		s.builtAt = time.Now()
		s.mu.Unlock()
	}()
}

func (s *sindex) build(emb pkgmodule.Embedder, model string) []sChunk {
	var pending []sChunk
	var texts []string
	out := make([]sChunk, 0, 1024)

	flush := func() {
		if len(texts) == 0 {
			return
		}
		vecs, _, err := emb.EmbedModel(context.Background(), model, "document", texts)
		if err == nil && len(vecs) == len(pending) {
			for i := range pending {
				pending[i].vec = vecs[i]
				out = append(out, pending[i])
			}
		}
		pending = pending[:0]
		texts = texts[:0]
	}

	_ = filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != s.root && (strings.HasPrefix(d.Name(), ".") || sindexIgnoredDirs[d.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(out)+len(pending) >= sindexMaxChunks {
			return filepath.SkipAll
		}
		info, e := d.Info()
		if e != nil || info.Size() == 0 || (s.maxBytes > 0 && info.Size() > s.maxBytes) {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil || !utf8.Valid(b) {
			return nil
		}
		rel, _ := filepath.Rel(s.root, path)
		rel = filepath.ToSlash(rel)
		for _, ch := range chunkLines(string(b), rel) {
			pending = append(pending, ch)
			texts = append(texts, ch.text)
			if len(texts) >= sindexBatch {
				flush()
			}
			if len(out)+len(pending) >= sindexMaxChunks {
				break
			}
		}
		return nil
	})
	flush()
	return out
}

// search embeds the query and returns the topK nearest code chunks. nil
// when the index is not ready yet (building) — grep then shows exact-only.
func (s *sindex) search(ctx context.Context, emb pkgmodule.Embedder, model, query string, topK int) []sHit {
	s.mu.Lock()
	if !s.ready || len(s.chunks) == 0 {
		s.mu.Unlock()
		return nil
	}
	s.usedAt = time.Now()
	chunks := s.chunks
	s.mu.Unlock()

	vecs, _, err := emb.EmbedModel(ctx, model, "query", []string{query})
	if err != nil || len(vecs) == 0 {
		return nil
	}
	q := vecs[0]
	hits := make([]sHit, 0, len(chunks))
	for i := range chunks {
		score := cosineF(q, chunks[i].vec)
		if score <= 0 {
			continue
		}
		hits = append(hits, sHit{Path: chunks[i].path, Line: chunks[i].line, Snippet: snippet(chunks[i].text), Score: score})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if topK > 0 && len(hits) > topK {
		hits = hits[:topK]
	}
	return hits
}

// chunkLines splits source into overlapping line windows, tagged with the
// 1-based start line for citation.
func chunkLines(src, path string) []sChunk {
	lines := strings.Split(src, "\n")
	var out []sChunk
	step := sindexChunkLines - sindexOverlap
	if step <= 0 {
		step = sindexChunkLines
	}
	for i := 0; i < len(lines); i += step {
		end := i + sindexChunkLines
		if end > len(lines) {
			end = len(lines)
		}
		text := strings.TrimSpace(strings.Join(lines[i:end], "\n"))
		if text != "" {
			out = append(out, sChunk{path: path, line: i + 1, text: text})
		}
		if end == len(lines) {
			break
		}
	}
	return out
}

func snippet(s string) string {
	const max = 400
	if len(s) > max {
		return s[:max] + "\n…"
	}
	return s
}

func cosineF(a, b []float32) float32 {
	var dot float64
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
	}
	return float32(dot) // both L2-normalized → dot == cosine
}
