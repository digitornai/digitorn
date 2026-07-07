package flow

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/digitornai/digitorn/internal/runtime/flow/flowexpr"
)

// nodeResult is the per-node data exposed in the flow context under the node's
// id: agent nodes populate Output, tool nodes populate Result, and any
// JSON object an agent emits is parsed into Fields for dotted access. Text is
// the human-facing string a write-back node should send: for a JSON-wrapped
// agent reply it's the unwrapped canonical text, else the raw output.
type nodeResult struct {
	Output string
	Result string
	Text   string
	Fields map[string]any
}

// replyTextKeys are the canonical keys an LLM wraps its human-facing answer in.
// When an agent emits a JSON object carrying one, a bare {{node.output}} (and
// {{last}}) resolves to that text — so a write-back node posts the reply, not
// the raw JSON blob. Dotted access ({{node.output.field}}) still sees the object.
var replyTextKeys = []string{"reply", "message", "text", "answer", "response", "content", "output", "body"}

// unwrapReplyText returns the human-facing text from a parsed agent object, or ""
// when the object carries no canonical string key (caller keeps the raw output).
func unwrapReplyText(fields map[string]any) string {
	if fields == nil {
		return ""
	}
	for _, k := range replyTextKeys {
		if v, ok := fields[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

// fctx is the flow execution context the doc describes: `event.*`,
// `<node_id>.output` / `.result`, `approvals.<node_id>`, plus the most-recent
// agent's structured (JSON) output promoted to the root so bare identifiers
// like `category` resolve. It implements flowexpr.Context and drives template
// interpolation. Single-goroutine: parallel branches operate on clones.
type fctx struct {
	event        map[string]any
	nodes        map[string]nodeResult
	approvals    map[string]string
	promoted     map[string]any
	lastError    map[string]any
	lastID       string
	secretLookup func(key string) (string, bool)
}

func newContext(event map[string]any, secretLookup func(string) (string, bool)) *fctx {
	if event == nil {
		event = map[string]any{}
	}
	return &fctx{
		event:        event,
		nodes:        map[string]nodeResult{},
		approvals:    map[string]string{},
		promoted:     map[string]any{},
		secretLookup: secretLookup,
	}
}

func (c *fctx) clone() *fctx {
	n := &fctx{
		event:        c.event,
		nodes:        make(map[string]nodeResult, len(c.nodes)),
		approvals:    make(map[string]string, len(c.approvals)),
		promoted:     make(map[string]any, len(c.promoted)),
		lastID:       c.lastID,
		secretLookup: c.secretLookup,
	}
	for k, v := range c.nodes {
		n.nodes[k] = v
	}
	for k, v := range c.approvals {
		n.approvals[k] = v
	}
	for k, v := range c.promoted {
		n.promoted[k] = v
	}
	if c.lastError != nil {
		n.lastError = make(map[string]any, len(c.lastError))
		for k, v := range c.lastError {
			n.lastError[k] = v
		}
	}
	return n
}

func (c *fctx) merge(src *fctx) {
	for k, v := range src.nodes {
		if _, ok := c.nodes[k]; !ok {
			c.nodes[k] = v
		}
	}
	for k, v := range src.approvals {
		if _, ok := c.approvals[k]; !ok {
			c.approvals[k] = v
		}
	}
}

func (c *fctx) recordAgent(nodeID, output string) {
	fields := parseJSONObject(output)
	text := output
	if unwrapped := unwrapReplyText(fields); unwrapped != "" {
		text = unwrapped
	}
	c.nodes[nodeID] = nodeResult{Output: output, Text: text, Fields: fields}
	if fields != nil {
		c.promoted = fields
	}
	c.lastID = nodeID
}

func (c *fctx) recordTool(nodeID, result string) {
	c.nodes[nodeID] = nodeResult{Result: result, Fields: parseJSONObject(result)}
	c.lastID = nodeID
}

func (c *fctx) recordApproval(nodeID, choice string) {
	c.approvals[nodeID] = choice
	c.lastID = nodeID
}

// recordError exposes the failure that triggered an on_error route under
// `error.*`, so a notification / human-handoff node can name what broke:
// {{error.message}}, {{error.node}}, {{error.type}}.
func (c *fctx) recordError(nodeID, nodeType, msg string) {
	c.lastError = map[string]any{"node": nodeID, "type": nodeType, "message": msg}
}

func (c *fctx) lastText() string {
	if c.lastID == "" {
		return ""
	}
	r := c.nodes[c.lastID]
	if r.Text != "" {
		return r.Text
	}
	if r.Output != "" {
		return r.Output
	}
	return r.Result
}

// Lookup implements flowexpr.Context with the documented namespace precedence.
func (c *fctx) Lookup(path []string) (any, bool) {
	if len(path) == 0 {
		return nil, false
	}
	switch path[0] {
	case "event":
		return walk(c.event, path[1:])
	case "approvals":
		if len(path) == 2 {
			v, ok := c.approvals[path[1]]
			return v, ok
		}
		return nil, false
	case "secret":
		if len(path) == 2 && c.secretLookup != nil {
			return c.secretLookup(path[1])
		}
		return nil, false
	case "error":
		return walk(c.lastError, path[1:])
	}
	if nr, ok := c.nodes[path[0]]; ok && len(path) >= 2 {
		switch path[1] {
		case "output":
			return tailWalk(nr.textOr(nr.Output), nr.Fields, path[2:])
		case "result":
			return tailWalk(nr.Result, nr.Fields, path[2:])
		}
		return walk(nr.Fields, path[1:])
	}
	if v, ok := walk(c.promoted, path); ok {
		return v, true
	}
	return nil, false
}

// textOr returns the unwrapped human text for a bare {{node.output}}, falling
// back to the raw output when the agent didn't wrap its reply in a JSON object.
func (nr nodeResult) textOr(raw string) string {
	if nr.Text != "" {
		return nr.Text
	}
	return raw
}

func tailWalk(text string, fields map[string]any, rest []string) (any, bool) {
	if len(rest) == 0 {
		return text, true
	}
	return walk(fields, rest)
}

func walk(m map[string]any, path []string) (any, bool) {
	if len(path) == 0 {
		return m, m != nil
	}
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// parseJSONObject extracts a JSON object from an agent's reply, tolerating the
// ways LLMs wrap it: a ```json fence, a ``` fence, or surrounding prose. It
// locates the first balanced {...} span and unmarshals it.
func parseJSONObject(s string) map[string]any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if i := strings.IndexByte(s, '{'); i >= 0 {
		if span := balancedObject(s[i:]); span != "" {
			var m map[string]any
			if err := json.Unmarshal([]byte(span), &m); err == nil {
				return m
			}
		}
	}
	return nil
}

// balancedObject returns the substring from the leading '{' to its matching
// '}', honoring strings and escapes so braces inside string values don't fool
// the depth counter. Returns "" if no balanced object is found.
func balancedObject(s string) string {
	depth := 0
	inStr := false
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[:i+1]
			}
		}
	}
	return ""
}

var tmplRe = regexp.MustCompile(`\{\{([^}]+)\}\}`)

// interpolate substitutes {{path}} placeholders using the flow context.
// Unresolved placeholders become empty strings.
func (c *fctx) interpolate(s string) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	return tmplRe.ReplaceAllStringFunc(s, func(m string) string {
		key := strings.TrimSpace(m[2 : len(m)-2])
		key, filters := splitTemplateFilters(key)
		v, ok := c.Lookup(flowexpr.SplitPath(key))
		out := ""
		if ok {
			out = flowexpr.ValueToString(v)
		}
		for _, f := range filters {
			switch f {
			case "json":
				if !ok {
					return "null"
				}
				if b, err := json.Marshal(v); err == nil {
					out = string(b)
				}
			}
		}
		return out
	})
}

// splitTemplateFilters splits "path | f1 | f2" into the base path and its
// filter chain — the compiler-reserved pipe syntax, applied here at runtime.
func splitTemplateFilters(key string) (string, []string) {
	if !strings.Contains(key, "|") {
		return key, nil
	}
	parts := strings.Split(key, "|")
	base := strings.TrimSpace(parts[0])
	filters := make([]string, 0, len(parts)-1)
	for _, p := range parts[1:] {
		if f := strings.TrimSpace(p); f != "" {
			filters = append(filters, f)
		}
	}
	return base, filters
}
