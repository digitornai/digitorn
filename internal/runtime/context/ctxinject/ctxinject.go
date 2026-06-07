// Package ctxinject renders the per-turn, YAML-declared context sections into the
// system prompt. App authors declare sections (static text, {{placeholder}}
// templates, or named builtins) in the manifest ; this builds the text from the
// turn's data bag (user / app / agent / session / date). It is pure and
// session-scoped, so it runs every turn and never leaks one user's data into
// another's cached prompt.
package ctxinject

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

// Data is the turn's facts the sections draw from. Every field may be empty ; a
// missing value renders as "" rather than an error.
type Data struct {
	User    map[string]any // id, name, email, region, locale, roles, + raw JWT claims
	App     map[string]any // id, name, version
	Agent   map[string]any // id, role
	Session map[string]any // goal, mode, turn, workdir
	Env     map[string]any // os, arch, platform (the daemon's runtime)
	Now     time.Time
}

// bag builds the dotted-path lookup tree (user.*, app.*, agent.*, session.*) plus
// derived top-level date/time keys.
func (d Data) bag() map[string]any {
	b := map[string]any{
		"user":    d.User,
		"app":     d.App,
		"agent":   d.Agent,
		"session": d.Session,
		"env":     d.Env,
	}
	if !d.Now.IsZero() {
		b["date"] = d.Now.Format("2006-01-02")
		b["time"] = d.Now.Format("15:04")
		b["datetime"] = d.Now.Format("2006-01-02 15:04")
		b["weekday"] = d.Now.Weekday().String()
	}
	return b
}

var placeholder = regexp.MustCompile(`\{\{\s*([\w.]+)\s*\}\}`)

// Merge layers agent sections on top of app sections : an agent section sharing a
// non-empty id REPLACES the app's (in place) ; the rest are appended.
func Merge(app, agent *schema.ContextBlock) []schema.ContextSection {
	var out []schema.ContextSection
	idx := map[string]int{}
	add := func(s schema.ContextSection) {
		if s.ID != "" {
			if i, ok := idx[s.ID]; ok {
				out[i] = s
				return
			}
			idx[s.ID] = len(out)
		}
		out = append(out, s)
	}
	if app != nil {
		for _, s := range app.Sections {
			add(s)
		}
	}
	if agent != nil {
		for _, s := range agent.Sections {
			add(s)
		}
	}
	return out
}

// Render builds the injected context text. Each section's body is its builtin >
// template > text (first non-empty) ; a `when` path that resolves empty/false drops
// it. Output is priority-ordered (stable on ties), each block "# Title\nbody" (or
// just body when untitled), joined by blank lines. Empty when nothing renders.
func Render(sections []schema.ContextSection, d Data) string {
	if len(sections) == 0 {
		return ""
	}
	bag := d.bag()
	type block struct {
		prio, idx int
		text      string
	}
	var blocks []block
	for i, s := range sections {
		if w := strings.TrimSpace(s.When); w != "" && !truthy(resolve(bag, w)) {
			continue
		}
		body := strings.TrimRight(sectionBody(s, d, bag), "\n")
		if strings.TrimSpace(body) == "" {
			continue
		}
		text := body
		if t := strings.TrimSpace(s.Title); t != "" {
			text = "# " + t + "\n" + body
		}
		blocks = append(blocks, block{s.Priority, i, text})
	}
	sort.SliceStable(blocks, func(a, b int) bool {
		if blocks[a].prio != blocks[b].prio {
			return blocks[a].prio < blocks[b].prio
		}
		return blocks[a].idx < blocks[b].idx
	})
	parts := make([]string, len(blocks))
	for i, bl := range blocks {
		parts[i] = bl.text
	}
	return strings.Join(parts, "\n\n")
}

func sectionBody(s schema.ContextSection, d Data, bag map[string]any) string {
	if b := strings.TrimSpace(s.Builtin); b != "" {
		if fn, ok := builtins[strings.ToLower(b)]; ok {
			return fn(d)
		}
		return "" // unknown builtin → skip rather than emit a broken block
	}
	if s.Template != "" {
		return interp(s.Template, bag)
	}
	return s.Text
}

// interp fills {{path}} placeholders from the bag ; an unknown path becomes "".
func interp(tmpl string, bag map[string]any) string {
	return placeholder.ReplaceAllStringFunc(tmpl, func(m string) string {
		v, _ := resolve(bag, placeholder.FindStringSubmatch(m)[1])
		return toString(v)
	})
}

// resolve walks a dotted path through the bag.
func resolve(bag map[string]any, path string) (any, bool) {
	var cur any = bag
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := m[p]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

func toString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []string:
		return strings.Join(x, ", ")
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, toString(e))
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprint(x)
	}
}

// truthy decides whether a `when` path counts as present.
func truthy(v any, ok bool) bool {
	if !ok || v == nil {
		return false
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x) != ""
	case bool:
		return x
	case []string:
		return len(x) > 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	default:
		return true
	}
}
