package filesystem

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/digitornai/digitorn/internal/modules/rag"
	pkgmodule "github.com/digitornai/digitorn/pkg/module"
)

const (
	sindexTTL        = 15 * time.Minute
	sindexMaxRoots   = 2
	sindexMaxChunks  = 3000
	sindexChunkLines = 60
	sindexOverlap    = 10
	sindexBatch      = 64
)

var sindexIgnoredDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, "target": true, ".idea": true, ".vscode": true,
	".next": true, "__pycache__": true, ".venv": true, "venv": true,
}

type sChunk struct {
	path string
	line int
	text string
	sym  string
	end  int
	vec  []float32
}

type sHit struct {
	Path    string   `json:"path"`
	Line    int      `json:"line"`
	Symbol  string   `json:"symbol,omitempty"`
	Callers []string `json:"callers,omitempty"`
	Imports []string `json:"imports,omitempty"`
	Snippet string   `json:"snippet"`
	Score   float32  `json:"score"`
}

type sindex struct {
	root     string
	maxBytes int64

	mu       sync.Mutex
	building bool
	ready    bool
	model    string
	chunks   []sChunk
	bm25     *rag.BM25
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
		chunks, bm := s.build(emb, model)
		s.mu.Lock()
		s.chunks = chunks
		s.bm25 = bm
		s.ready = true
		s.dirty = false
		s.builtAt = time.Now()
		s.mu.Unlock()
	}()
}

func (s *sindex) build(emb pkgmodule.Embedder, model string) ([]sChunk, *rag.BM25) {
	var pending []sChunk
	var texts []string
	out := make([]sChunk, 0, 1024)
	bm := rag.NewBM25()

	flush := func() {
		if len(texts) == 0 {
			return
		}
		vecs, _, err := emb.EmbedModel(context.Background(), model, "document", texts)
		if err == nil && len(vecs) == len(pending) {
			for i := range pending {
				pending[i].vec = vecs[i]
				pending[i].text = ""
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
		chs := astChunks(rel, b)
		if chs == nil {
			chs = chunkLines(string(b), rel)
		}
		for _, ch := range chs {
			bm.Add(fmt.Sprintf("%s:%d", ch.path, ch.line), ch.text)
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
	return out, bm
}

func (s *sindex) search(ctx context.Context, emb pkgmodule.Embedder, model, query string, topK int) []sHit {
	s.mu.Lock()
	if !s.ready || len(s.chunks) == 0 {
		s.mu.Unlock()
		return nil
	}
	s.usedAt = time.Now()
	chunks := s.chunks
	bm := s.bm25
	s.mu.Unlock()

	vecs, _, err := emb.EmbedModel(ctx, model, "query", []string{query})
	if err != nil || len(vecs) == 0 {
		return nil
	}
	q := vecs[0]

	func() {
		defer func() { recover() }()
		hydeDoc := hydeExpand(query)
		if hydeDoc == "" {
			return
		}
		hctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()
		hydeVecs, _, herr := emb.EmbedModel(hctx, model, "document", []string{hydeDoc})
		if herr != nil || len(hydeVecs) == 0 || len(hydeVecs[0]) != len(q) {
			return
		}
		hv := hydeVecs[0]
		for i := range q {
			q[i] = 0.55*q[i] + 0.45*hv[i]
		}
	}()

	bm25Map := make(map[string]float32, 64)
	func() {
		defer func() { recover() }()
		if bm == nil {
			return
		}
		bmHits := bm.Search(query, 100)
		var maxScore float64
		for _, h := range bmHits {
			if h.Score > maxScore {
				maxScore = h.Score
			}
		}
		if maxScore > 0 {
			for _, h := range bmHits {
				bm25Map[h.ID] = float32(h.Score / maxScore)
			}
		}
	}()

	hits := make([]sHit, 0, len(chunks))
	for i := range chunks {
		vec := cosineF(q, chunks[i].vec)
		if vec <= 0 {
			continue
		}
		id := fmt.Sprintf("%s:%d", chunks[i].path, chunks[i].line)
		fused := 0.6*vec + 0.4*bm25Map[id]
		hits = append(hits, sHit{Path: chunks[i].path, Line: chunks[i].line, Symbol: chunks[i].sym, Score: fused})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if topK > 0 && len(hits) > topK {
		hits = hits[:topK]
	}
	for i := range hits {
		hits[i].Snippet = readChunkSnippet(s.root, hits[i].Path, hits[i].Line)
	}
	return hits
}

func chunkLines(src, path string) []sChunk {
	lines := strings.Split(src, "\n")
	var out []sChunk
	step := sindexChunkLines - sindexOverlap
	if step <= 0 {
		step = sindexChunkLines
	}
	for i := 0; i < len(lines); i += step {
		endIdx := i + sindexChunkLines
		if endIdx > len(lines) {
			endIdx = len(lines)
		}
		text := strings.TrimSpace(strings.Join(lines[i:endIdx], "\n"))
		if text != "" {
			out = append(out, sChunk{path: path, line: i + 1, end: endIdx, text: text})
		}
		if endIdx == len(lines) {
			break
		}
	}
	return out
}

func readChunkSnippet(root, relPath string, startLine int) string {
	abs := filepath.Join(root, filepath.FromSlash(relPath))
	b, err := os.ReadFile(abs)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	lo := startLine - 1
	if lo < 0 {
		lo = 0
	}
	hi := lo + sindexChunkLines
	if hi > len(lines) {
		hi = len(lines)
	}
	s := strings.TrimSpace(strings.Join(lines[lo:hi], "\n"))
	const max = 800
	if len(s) > max {
		return s[:max] + "\n…"
	}
	return s
}

// hydeExpand generates a template hypothetical Go code snippet from a natural
// language query (Hypothetical Document Embeddings). Returns "" for short or
// low-information queries where HyDE adds no value. The snippet is embedded
// and averaged with the query vector to improve conceptual recall.
func hydeExpand(query string) string {
	words := strings.Fields(strings.ToLower(query))
	if len(words) < 4 {
		return "" // short queries: BM25+vector already handles them well
	}
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "is": true, "are": true,
		"that": true, "for": true, "with": true, "to": true, "of": true,
		"in": true, "on": true, "at": true, "by": true, "from": true,
		"and": true, "or": true, "but": true, "how": true, "when": true,
		"what": true, "which": true, "code": true, "function": true,
	}
	codeKws := []string{
		"error", "context", "http", "json", "auth", "token", "rate",
		"limit", "retry", "timeout", "cache", "parse", "decode", "encode",
		"validate", "handle", "process", "fetch", "send", "connect",
		"read", "write", "create", "delete", "update", "list", "search",
		"filter", "stream", "event", "notify", "log", "metric", "trace",
		"middleware", "handler", "client", "server", "session", "request",
		"response", "database", "query", "transaction", "pool", "worker",
	}
	q := strings.Join(words, " ")
	var tags []string
	for _, kw := range codeKws {
		if strings.Contains(q, kw) {
			tags = append(tags, kw)
		}
	}

	// Build function name from first 2-3 content words.
	var nameParts []string
	for _, w := range words {
		if stopWords[w] || len(w) < 3 {
			continue
		}
		nameParts = append(nameParts, strings.ToUpper(w[:1])+w[1:])
		if len(nameParts) == 3 {
			break
		}
	}
	if len(nameParts) == 0 {
		return ""
	}

	params := "ctx context.Context"
	ret := "error"
	for _, t := range tags {
		switch t {
		case "list", "fetch", "search", "query":
			ret = "([]interface{}, error)"
		case "parse", "decode":
			params += ", data []byte"
		case "http", "request", "handler":
			params += ", r *http.Request, w http.ResponseWriter"
		case "token", "auth", "session":
			params += ", token string"
		case "database", "transaction":
			params += ", db *sql.DB"
		}
	}

	comment := query
	if len(tags) > 0 {
		comment = fmt.Sprintf("%s // keywords: %s", query, strings.Join(tags, ", "))
	}
	return fmt.Sprintf("func %s(%s) %s {\n\t// %s\n\treturn nil\n}",
		strings.Join(nameParts, ""), params, ret, comment)
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
