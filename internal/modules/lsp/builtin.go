package lsp

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
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

// mermaidReserved are keywords that break the parser when used as an identifier
// (node id, classDef name, or class-application style name).
var mermaidReserved = map[string]bool{
	"end": true, "graph": true, "subgraph": true, "style": true,
	"classDef": true, "class": true, "click": true, "direction": true,
	"linkStyle": true, "default": true, "flowchart": true,
}

var (
	mermaidHeaderRe = regexp.MustCompile(`(?i)^(flowchart|graph|sequenceDiagram|classDiagram(-v2)?|erDiagram|stateDiagram(-v2)?|journey|gantt|pie|mindmap|timeline|gitGraph|quadrantChart|requirementDiagram|sankey(-beta)?|xychart(-beta)?|block(-beta)?|C4Context|C4Container|C4Component|C4Dynamic|C4Deployment)\b`)
	mermaidClassDefRe  = regexp.MustCompile(`^classDef\s+([A-Za-z_][\w-]*)\b`)
	mermaidClassApplyRe = regexp.MustCompile(`^class\s+[\w,\s]+?\s+([A-Za-z_][\w-]*)\s*$`)
	// `end` glued to a shape bracket ( end[..], end(..), end{..} ) or as an
	// explicit edge endpoint ( --> end , end --> ) — unambiguously a node id.
	mermaidEndNodeRe = regexp.MustCompile(`(?i)(\bend\s*[\[({])|((--+>|--+|-\.-+>?|==+>)\s*end\b)|(\bend\s*(--+>|--+|==+>))`)
)

// validateMermaid is a fast, zero-dependency linter for the LLM-common Mermaid
// mistakes that silently blank the canvas. It is NOT a full parser (mermaid.js
// runs in the browser) — it catches the high-confidence breakages so the agent
// gets them back in the SAME turn via the lsp_diagnose hook and self-corrects.
func validateMermaid(content string) []Diagnostic {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	var diags []Diagnostic
	headerChecked := false
	subgraphDepth := 0
	for i, raw := range strings.Split(content, "\n") {
		ln := i + 1
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "```") {
			diags = append(diags, Diagnostic{Line: ln, Column: 1, Severity: "error", Source: "mermaid",
				Message: "remove the ``` markdown fence — the file must contain only Mermaid, starting with a diagram header"})
			continue
		}
		if strings.HasPrefix(line, "%%") {
			continue
		}
		if !headerChecked {
			headerChecked = true
			if !mermaidHeaderRe.MatchString(line) {
				diags = append(diags, Diagnostic{Line: ln, Column: 1, Severity: "error", Source: "mermaid",
					Message: "first line must be a diagram header (flowchart TD, sequenceDiagram, classDiagram, erDiagram, stateDiagram-v2, …)"})
			}
		}
		low := strings.ToLower(line)
		if low == "subgraph" || strings.HasPrefix(low, "subgraph ") {
			subgraphDepth++
		}
		if low == "end" { // legitimate block close — never a node here
			subgraphDepth--
			continue
		}
		if m := mermaidClassDefRe.FindStringSubmatch(line); m != nil && mermaidReserved[m[1]] {
			diags = append(diags, Diagnostic{Line: ln, Column: 1, Severity: "error", Source: "mermaid",
				Message: "'" + m[1] + "' is a reserved word and cannot be a classDef name — rename the style (e.g. '" + m[1] + "_s')"})
		} else if m := mermaidClassApplyRe.FindStringSubmatch(line); m != nil && mermaidReserved[m[1]] {
			diags = append(diags, Diagnostic{Line: ln, Column: 1, Severity: "error", Source: "mermaid",
				Message: "'" + m[1] + "' is a reserved word and cannot be a class/style name — rename it (e.g. '" + m[1] + "_s')"})
		}
		if mermaidEndNodeRe.MatchString(line) {
			diags = append(diags, Diagnostic{Line: ln, Column: 1, Severity: "error", Source: "mermaid",
				Message: "'end' is a reserved word and cannot be a node id — use 'End'/'Done' or a quoted label like n1[\"end\"]"})
		}
	}
	if subgraphDepth > 0 {
		diags = append(diags, Diagnostic{Line: 1, Column: 1, Severity: "error", Source: "mermaid",
			Message: fmt.Sprintf("unbalanced subgraph: %d subgraph block(s) missing a matching 'end'", subgraphDepth)})
	}
	return diags
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
