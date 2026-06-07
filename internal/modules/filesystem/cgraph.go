//go:build treesitter

package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	tsts "github.com/smacker/go-tree-sitter/typescript/typescript"
)

// cgraph.go : AST-aware code parsing + a per-workdir dependency graph
// (tree-sitter). Definitions become symbol-level chunks (astChunks) AND
// nodes of a graph whose edges are calls (caller→callee), imports and
// enclosing-symbol — the "total comprehension" layer that lets grep tell
// the agent "this hit is in func X, defined here, called by Y, file
// imports Z". Ephemeral per workdir (LRU+TTL, async build), CGO, gated by
// the `treesitter` tag (default build uses the no-op stub).

type langSpec struct {
	lang        *sitter.Language
	defKinds    map[string]string
	callTypes   map[string]bool
	importTypes map[string]bool
}

func langForExt(ext string) (langSpec, bool) {
	switch strings.ToLower(ext) {
	case ".go":
		return langSpec{golang.GetLanguage(),
			map[string]string{"function_declaration": "func", "method_declaration": "method", "type_spec": "type"},
			map[string]bool{"call_expression": true},
			map[string]bool{"import_spec": true}}, true
	case ".py":
		return langSpec{python.GetLanguage(),
			map[string]string{"function_definition": "func", "class_definition": "class"},
			map[string]bool{"call": true},
			map[string]bool{"import_statement": true, "import_from_statement": true}}, true
	case ".js", ".jsx", ".mjs", ".cjs":
		return langSpec{javascript.GetLanguage(),
			map[string]string{"function_declaration": "func", "class_declaration": "class", "method_definition": "method"},
			map[string]bool{"call_expression": true},
			map[string]bool{"import_statement": true}}, true
	case ".ts", ".tsx":
		return langSpec{tsts.GetLanguage(),
			map[string]string{"function_declaration": "func", "class_declaration": "class", "method_definition": "method", "interface_declaration": "interface", "type_alias_declaration": "type"},
			map[string]bool{"call_expression": true},
			map[string]bool{"import_statement": true}}, true
	}
	return langSpec{}, false
}

type symFull struct {
	Name  string
	Kind  string
	Start int
	End   int
	Body  string
	Calls []string
}

type fileParse struct {
	syms    []symFull
	imports []string
}

// parseFile extracts a file's definitions (with their callee names) and
// imports via one AST walk, tracking the enclosing-definition stack so
// each call attributes to the innermost definition.
func parseFile(path string, src []byte) (fileParse, bool) {
	spec, ok := langForExt(filepath.Ext(path))
	if !ok {
		return fileParse{}, false
	}
	p := sitter.NewParser()
	p.SetLanguage(spec.lang)
	tree, err := p.ParseCtx(context.Background(), nil, src)
	if err != nil || tree == nil {
		return fileParse{}, false
	}
	defer tree.Close()

	var fp fileParse
	syms := []symFull{}
	var stack []int
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		typ := n.Type()
		isDef := false
		if kind, ok := spec.defKinds[typ]; ok {
			isDef = true
			syms = append(syms, symFull{
				Name: symbolName(n, src), Kind: kind,
				Start: int(n.StartPoint().Row) + 1, End: int(n.EndPoint().Row) + 1,
				Body: n.Content(src),
			})
			stack = append(stack, len(syms)-1)
		}
		if spec.importTypes[typ] {
			if imp := importName(n, src); imp != "" {
				fp.imports = append(fp.imports, imp)
			}
		}
		if spec.callTypes[typ] && len(stack) > 0 {
			if callee := calleeName(n, src); callee != "" {
				idx := stack[len(stack)-1]
				syms[idx].Calls = append(syms[idx].Calls, callee)
			}
		}
		for i := 0; i < int(n.NamedChildCount()); i++ {
			visit(n.NamedChild(i))
		}
		if isDef {
			stack = stack[:len(stack)-1]
		}
	}
	visit(tree.RootNode())
	fp.syms = syms
	return fp, true
}

func symbolName(n *sitter.Node, src []byte) string {
	if f := n.ChildByFieldName("name"); f != nil {
		return f.Content(src)
	}
	return trailingIdent(n, src)
}

func calleeName(n *sitter.Node, src []byte) string {
	f := n.ChildByFieldName("function")
	if f == nil && n.NamedChildCount() > 0 {
		f = n.NamedChild(0)
	}
	if f == nil {
		return ""
	}
	return trailingIdent(f, src)
}

// trailingIdent returns an identifier node's text, or the last identifier
// of a selector/member/attribute expression (so a.b.Call → "Call").
func trailingIdent(n *sitter.Node, src []byte) string {
	switch n.Type() {
	case "identifier", "field_identifier", "property_identifier", "type_identifier":
		return n.Content(src)
	}
	for i := int(n.NamedChildCount()) - 1; i >= 0; i-- {
		c := n.NamedChild(i)
		switch c.Type() {
		case "identifier", "field_identifier", "property_identifier", "type_identifier":
			return c.Content(src)
		}
	}
	return ""
}

func importName(n *sitter.Node, src []byte) string {
	t := strings.TrimSpace(n.Content(src))
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[:i]
	}
	return strings.Trim(t, "\"'`")
}

// astChunks turns a recognised file into symbol-level chunks (definition
// bodies tagged with "kind name"). nil for unknown languages.
func astChunks(path string, src []byte) []sChunk {
	fp, ok := parseFile(path, src)
	if !ok || len(fp.syms) == 0 {
		return nil
	}
	out := make([]sChunk, 0, len(fp.syms))
	for _, s := range fp.syms {
		text := strings.TrimSpace(s.Body)
		if text == "" {
			continue
		}
		label := strings.TrimSpace(s.Kind + " " + s.Name)
		out = append(out, sChunk{path: path, line: s.Start, text: label + "\n" + text, sym: label})
	}
	return out
}

// ---- dependency graph ----

type defLoc struct {
	Name, Kind, Path string
	Start, End       int
}

type codeGraph struct {
	byFile  map[string][]defLoc // enclosing-symbol lookup
	callers map[string][]string // callee name → caller "kind name" labels
	imports map[string][]string // path → imports
}

func (g *codeGraph) context(path string, line int) symContext {
	var sc symContext
	sc.Imports = g.imports[path]
	best := -1
	bestSpan := 1 << 30
	defs := g.byFile[path]
	for i, d := range defs {
		if line >= d.Start && line <= d.End {
			if span := d.End - d.Start; span < bestSpan {
				best, bestSpan = i, span
			}
		}
	}
	if best >= 0 {
		d := defs[best]
		sc.Symbol = strings.TrimSpace(d.Kind + " " + d.Name)
		sc.Callers = dedupStrings(g.callers[d.Name], 8)
	}
	return sc
}

// ---- per-workdir ephemeral graph manager (mirrors sindex) ----

type cgEntry struct {
	root     string
	maxBytes int64
	mu       sync.Mutex
	building bool
	ready    bool
	g        *codeGraph
	builtAt  time.Time
	usedAt   time.Time
	dirty    bool
}

type cgManager struct {
	mu     sync.Mutex
	byRoot map[string]*cgEntry
}

var cgraphs = &cgManager{byRoot: map[string]*cgEntry{}}

func (m *cgManager) get(root string, maxBytes int64) *cgEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.byRoot[root]; ok {
		return e
	}
	if len(m.byRoot) >= sindexMaxRoots {
		var oldestKey string
		var oldest time.Time
		for k, e := range m.byRoot {
			e.mu.Lock()
			u := e.usedAt
			e.mu.Unlock()
			if oldestKey == "" || u.Before(oldest) {
				oldestKey, oldest = k, u
			}
		}
		if oldestKey != "" {
			delete(m.byRoot, oldestKey)
		}
	}
	e := &cgEntry{root: root, maxBytes: maxBytes}
	m.byRoot[root] = e
	return e
}

func (m *cgManager) markDirty(abs string) {
	m.mu.Lock()
	es := make([]*cgEntry, 0, len(m.byRoot))
	for _, e := range m.byRoot {
		es = append(es, e)
	}
	m.mu.Unlock()
	for _, e := range es {
		if underRoot(e.root, abs) {
			e.mu.Lock()
			e.dirty = true
			e.mu.Unlock()
		}
	}
}

func (e *cgEntry) maybeBuild() {
	e.mu.Lock()
	stale := e.dirty || (e.ready && time.Since(e.builtAt) > sindexTTL)
	if e.building || (e.ready && !stale) {
		e.mu.Unlock()
		return
	}
	e.building = true
	e.mu.Unlock()
	go func() {
		defer func() {
			recover()
			e.mu.Lock()
			e.building = false
			e.mu.Unlock()
		}()
		g := buildGraph(e.root, e.maxBytes)
		e.mu.Lock()
		e.g = g
		e.ready = true
		e.dirty = false
		e.builtAt = time.Now()
		e.mu.Unlock()
	}()
}

func (e *cgEntry) context(path string, line int) symContext {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.ready || e.g == nil {
		return symContext{}
	}
	e.usedAt = time.Now()
	return e.g.context(path, line)
}

// buildGraph parses every recognised source file under root and assembles
// the dependency graph. Parsing (tree-sitter, CPU-bound) runs on a pool of
// NumCPU workers — one parser per goroutine — while the graph itself is
// merged on a single goroutine, so no lock guards the maps.
func buildGraph(root string, maxBytes int64) *codeGraph {
	g := &codeGraph{byFile: map[string][]defLoc{}, callers: map[string][]string{}, imports: map[string][]string{}}

	var paths []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || sindexIgnoredDirs[d.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := langForExt(filepath.Ext(path)); !ok {
			return nil
		}
		if info, e := d.Info(); e != nil || (maxBytes > 0 && info.Size() > maxBytes) {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if len(paths) == 0 {
		return g
	}

	type result struct {
		rel string
		fp  fileParse
	}
	workers := runtime.NumCPU()
	if workers > len(paths) {
		workers = len(paths)
	}
	jobs := make(chan string, len(paths))
	for _, p := range paths {
		jobs <- p
	}
	close(jobs)
	results := make(chan result, workers*2)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				b, e := os.ReadFile(path)
				if e != nil || !utf8.Valid(b) {
					continue
				}
				rel, _ := filepath.Rel(root, path)
				rel = filepath.ToSlash(rel)
				if fp, ok := parseFile(rel, b); ok {
					results <- result{rel: rel, fp: fp}
				}
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()

	for r := range results {
		if len(r.fp.imports) > 0 {
			g.imports[r.rel] = r.fp.imports
		}
		for _, s := range r.fp.syms {
			g.byFile[r.rel] = append(g.byFile[r.rel], defLoc{Name: s.Name, Kind: s.Kind, Path: r.rel, Start: s.Start, End: s.End})
			label := strings.TrimSpace(s.Kind + " " + s.Name + " @" + r.rel)
			for _, callee := range s.Calls {
				g.callers[callee] = append(g.callers[callee], label)
			}
		}
	}
	return g
}

// codeContextFor returns the code-graph context for a matched location.
func codeContextFor(root string, maxBytes int64, path string, line int) symContext {
	e := cgraphs.get(root, maxBytes)
	e.maybeBuild()
	return e.context(path, line)
}

func dedupStrings(in []string, max int) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
			if len(out) >= max {
				break
			}
		}
	}
	return out
}
