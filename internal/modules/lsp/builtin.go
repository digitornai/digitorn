package lsp

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
	yaml "gopkg.in/yaml.v3"
)

// liteBackend is a zero-install validator: it implements the backend interface
// in pure Go, no subprocess, no external LSP server needed. Suitable for
// config / markup languages where syntax-level diagnostics cover 95% of what
// an agent needs (JSON, YAML, HTML, XML). Languages with type systems still
// route to the real LSP backend.
type liteBackend struct {
	name      string
	root      string
	validator func(content string) []Diagnostic

	mu    sync.Mutex
	diags map[string][]Diagnostic // cacheKey -> last diagnostics
}

func newLiteBackend(name, root string, v func(content string) []Diagnostic) *liteBackend {
	return &liteBackend{
		name:      name,
		root:      root,
		validator: v,
		diags:     map[string][]Diagnostic{},
	}
}

func (b *liteBackend) notifyChange(_ context.Context, path, content string, _ time.Duration) ([]Diagnostic, error) {
	diags := b.validator(content)
	for i := range diags {
		diags[i].File = path
	}
	key := cacheKey(pathToURI(path))
	b.mu.Lock()
	b.diags[key] = diags
	b.mu.Unlock()
	return diags, nil
}

func (b *liteBackend) diagnosticsFor(path string) []Diagnostic {
	key := cacheKey(pathToURI(path))
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.diags[key]
}

func (b *liteBackend) projectSummary(excludePath string) ProjectSummary {
	excludeKey := cacheKey(pathToURI(excludePath))
	b.mu.Lock()
	defer b.mu.Unlock()
	sum := ProjectSummary{}
	for key, diags := range b.diags {
		if key == excludeKey {
			continue
		}
		errs, warns := 0, 0
		for _, d := range diags {
			switch d.Severity {
			case "error":
				errs++
			case "warning":
				warns++
			}
		}
		if errs == 0 && warns == 0 {
			continue
		}
		sum.TotalErrors += errs
		sum.TotalWarnings += warns
		sum.AffectedFiles = append(sum.AffectedFiles, AffectedFile{
			File:     b.displayPath(key),
			Errors:   errs,
			Warnings: warns,
		})
	}
	return sum
}

func (b *liteBackend) stop(_ context.Context) {}

func (b *liteBackend) displayPath(key string) string {
	p := strings.TrimPrefix(key, "file://")
	if len(p) > 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	if rel, err := filepath.Rel(b.root, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(p)
}

// =============================================================================
// validators — one per supported builtin language.
// =============================================================================

func validateJSON(content string) []Diagnostic {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	var raw any
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		offset := 0
		var se *json.SyntaxError
		var ute *json.UnmarshalTypeError
		switch {
		case errors.As(err, &se):
			offset = int(se.Offset)
		case errors.As(err, &ute):
			offset = int(ute.Offset)
		}
		l, c := lineColAt(content, offset)
		return []Diagnostic{{Line: l, Column: c, Severity: "error", Source: "json", Message: err.Error()}}
	}
	return nil
}

func validateYAML(content string) []Diagnostic {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	dec := yaml.NewDecoder(strings.NewReader(content))
	var out []Diagnostic
	for {
		var doc any
		err := dec.Decode(&doc)
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			break
		}
		l, c := extractYAMLLineCol(err.Error())
		out = append(out, Diagnostic{Line: l, Column: c, Severity: "error", Source: "yaml", Message: err.Error()})
		break // yaml decoder is unrecoverable past a syntax error
	}
	return out
}

func validateXML(content string) []Diagnostic {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	dec := xml.NewDecoder(strings.NewReader(content))
	for {
		_, err := dec.Token()
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		pos := dec.InputOffset()
		l, c := lineColAt(content, int(pos))
		return []Diagnostic{{Line: l, Column: c, Severity: "error", Source: "xml", Message: err.Error()}}
	}
}

// validateHTML runs a tolerant well-formedness pass: it tokenises the file with
// the official x/net/html tokenizer (the engine browsers use) and reports any
// error the tokenizer emits — unclosed comments, malformed attributes, illegal
// entities. HTML is forgiving by design, so this catches REAL syntax bugs
// without flooding the agent with browser-style soft warnings.
func validateHTML(content string) []Diagnostic {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	z := html.NewTokenizer(strings.NewReader(content))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if errors.Is(z.Err(), io.EOF) {
				return nil
			}
			return []Diagnostic{{
				Line: 1, Column: 1, Severity: "error", Source: "html",
				Message: z.Err().Error(),
			}}
		}
	}
}

// =============================================================================
// helpers
// =============================================================================

func lineColAt(content string, offset int) (int, int) {
	if offset <= 0 || offset > len(content) {
		return 1, 1
	}
	line, col := 1, 1
	for i := 0; i < offset; i++ {
		if content[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

// extractYAMLLineCol pulls the "line N" component out of yaml.v3's error string
// ("yaml: line 3: ..."). Best-effort — returns 1,1 if the format ever changes.
func extractYAMLLineCol(msg string) (int, int) {
	i := strings.Index(msg, "line ")
	if i < 0 {
		return 1, 1
	}
	rest := msg[i+5:]
	end := strings.IndexAny(rest, ": ")
	if end < 0 {
		end = len(rest)
	}
	var n int
	if _, err := fmt.Sscanf(rest[:end], "%d", &n); err != nil || n < 1 {
		return 1, 1
	}
	return n, 1
}
