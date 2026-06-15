//go:build treesitter

// Package codeast is the shared tree-sitter AST layer : it parses a source
// file into its symbol definitions (with callees + imports) and into
// symbol-level chunks. Both the code-intelligence layer (filesystem grep
// enrichment + dependency graph + repo map) and the indexation service's
// codebase connector build on it — one AST machinery, no duplication. CGO,
// gated by the `treesitter` tag (the default build uses the no-op stub).
package codeast

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	tsts "github.com/smacker/go-tree-sitter/typescript/typescript"
)

type langSpec struct {
	lang        *sitter.Language
	defKinds    map[string]string
	callTypes   map[string]bool
	importTypes map[string]bool
}

func langFor(ext string) (langSpec, bool) {
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

// Supported reports whether the extension has a tree-sitter grammar.
func Supported(ext string) bool { _, ok := langFor(ext); return ok }

// Symbol is one definition extracted from a source file.
type Symbol struct {
	Name  string
	Kind  string
	Start int
	End   int
	Body  string
	Calls []string
}

// FileParse is the result of parsing one file : its symbols + imports.
type FileParse struct {
	Syms    []Symbol
	Imports []string
}

// ParseFile extracts a file's definitions (with their callee names) and
// imports via one AST walk, tracking the enclosing-definition stack so each
// call attributes to the innermost definition.
func ParseFile(path string, src []byte) (FileParse, bool) {
	spec, ok := langFor(filepath.Ext(path))
	if !ok {
		return FileParse{}, false
	}
	p := sitter.NewParser()
	p.SetLanguage(spec.lang)
	tree, err := p.ParseCtx(context.Background(), nil, src)
	if err != nil || tree == nil {
		return FileParse{}, false
	}
	defer tree.Close()

	var fp FileParse
	syms := []Symbol{}
	var stack []int
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		typ := n.Type()
		isDef := false
		if kind, ok := spec.defKinds[typ]; ok {
			isDef = true
			syms = append(syms, Symbol{
				Name: symbolName(n, src), Kind: kind,
				Start: int(n.StartPoint().Row) + 1, End: int(n.EndPoint().Row) + 1,
				Body: n.Content(src),
			})
			stack = append(stack, len(syms)-1)
		}
		if spec.importTypes[typ] {
			if imp := importName(n, src); imp != "" {
				fp.Imports = append(fp.Imports, imp)
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
	fp.Syms = syms
	return fp, true
}

// Chunk is a symbol-level code chunk : the definition body tagged with its
// "kind name" label, plus its location.
type Chunk struct {
	Path   string
	Symbol string // "kind name", e.g. "func Deploy"
	Kind   string
	Line   int
	Text   string // label + "\n" + body
}

// Chunks turns a recognised file into symbol-level chunks. nil for unknown
// languages (callers fall back to line-window chunking).
func Chunks(path string, src []byte) []Chunk {
	fp, ok := ParseFile(path, src)
	if !ok || len(fp.Syms) == 0 {
		return nil
	}
	out := make([]Chunk, 0, len(fp.Syms))
	for _, s := range fp.Syms {
		text := strings.TrimSpace(s.Body)
		if text == "" {
			continue
		}
		label := strings.TrimSpace(s.Kind + " " + s.Name)
		out = append(out, Chunk{Path: path, Symbol: label, Kind: s.Kind, Line: s.Start, Text: label + "\n" + text})
	}
	return out
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
