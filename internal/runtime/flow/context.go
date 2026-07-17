package flow

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/digitornai/digitorn/internal/runtime/flow/flowexpr"
)

type nodeResult struct {
	Output string
	Result string
	Text   string
	Fields map[string]any
}

var replyTextKeys = []string{"reply", "message", "text", "answer", "response", "content", "output", "body"}

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
