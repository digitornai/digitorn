package validate

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/digitornai/digitorn/internal/compiler/catalog"
	"github.com/digitornai/digitorn/internal/compiler/diagnostic"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/compiler/suggest"
)

// widgetFilterRe matches `{{ expr | filter | filter:arg }}` segments — used
// to extract filter names for whitelist validation.
var widgetFilterRe = regexp.MustCompile(`\{\{[^}]*\}\}`)

// widgetPrimitives is the closed set of `type:` values accepted on a widget node.
var widgetPrimitives = map[string]struct{}{
	// Layout
	"column": {}, "row": {}, "card": {}, "section": {}, "tabs": {},
	"split": {}, "grid": {}, "spacer": {}, "divider": {},
	// Content
	"markdown": {}, "text": {}, "image": {}, "icon": {},
	// Data
	"list": {}, "table": {}, "chart": {}, "stat": {}, "timeline": {},
	"tree": {}, "kanban": {},
	// Input
	"form": {}, "text_input": {}, "textarea": {}, "select": {},
	"multi_select": {}, "radio": {}, "checkbox": {}, "switch": {},
	"slider": {}, "date": {}, "time": {}, "datetime": {},
	"file_upload": {}, "code_editor": {},
	// Action
	"button": {}, "icon_button": {}, "link": {}, "confirm": {},
	// Feedback
	"alert": {}, "badge": {}, "progress": {}, "skeleton": {}, "empty_state": {},
}

var widgetActions = map[string]struct{}{
	"chat": {}, "tool": {}, "http": {}, "open_url": {},
	"open_workspace": {}, "open_modal": {}, "close": {},
	"set_state": {}, "refresh": {}, "copy": {}, "download": {},
	"navigate": {}, "confirm": {}, "sequence": {}, "alert": {},
}

// CheckWidgets walks every named widget tree under ui.widgets and validates
// each node's `type:`, every action ref, every filter expression, and detects
// unknown primitives.
func CheckWidgets(file string, def *schema.AppDefinition, cat *catalog.Catalog, bag *diagnostic.Bag) {
	if def.UI == nil || def.UI.Widgets == nil {
		return
	}
	w := def.UI.Widgets
	if w.ChatSide != nil {
		walkWidget(w.ChatSide, "ui.widgets.chat_side", cat, bag)
	}
	for i, tab := range w.WorkspaceTabs {
		walkWidget(tab, fmt.Sprintf("ui.widgets.workspace_tabs.%d", i), cat, bag)
	}
	for name, modal := range w.Modals {
		walkWidget(modal, fmt.Sprintf("ui.widgets.modals.%s", name), cat, bag)
	}
	for name, inline := range w.Inline {
		walkWidget(inline, fmt.Sprintf("ui.widgets.inline.%s", name), cat, bag)
	}
}

// walkWidget recurses through a widget tree (typed as `map[string]any` here
// because the schema is intentionally loose at decode time).
func walkWidget(node any, path string, cat *catalog.Catalog, bag *diagnostic.Bag) {
	m, ok := node.(map[string]any)
	if !ok {
		// Strings can carry `{{ ... | filter }}` expressions; validate filters.
		if s, ok := node.(string); ok {
			checkWidgetFilters(s, path, cat, bag)
		}
		return
	}
	if t, ok := m["type"].(string); ok && t != "" {
		if _, known := widgetPrimitives[t]; !known {
			pool := keysOf(widgetPrimitives)
			d := diagnostic.Errorf(diagnostic.CodeBadEnum, posUnknown,
				"%s.type: unknown widget primitive %q", path, t)
			if s, ok := suggest.Closest(t, pool, 2); ok {
				d = d.WithSuggestion(s, fmt.Sprintf("did you mean %q?", s))
			}
			bag.Add(d)
		}
	}
	for _, key := range []string{"action", "on_click", "onClick", "on_submit", "onSubmit", "on_change", "onChange"} {
		if act, ok := m[key]; ok {
			validateAction(act, path+"."+key, bag)
		}
	}
	for k, v := range m {
		if s, ok := v.(string); ok && strings.Contains(s, "|") {
			checkWidgetFilters(s, path+"."+k, cat, bag)
		}
	}
	for _, key := range []string{"children", "items", "tabs", "branches", "fields", "columns", "rows", "panels", "sections", "actions"} {
		if children, ok := m[key].([]any); ok {
			for i, child := range children {
				walkWidget(child, fmt.Sprintf("%s.%s.%d", path, key, i), cat, bag)
			}
		}
	}
	for _, key := range []string{"body", "header", "footer", "left", "right", "main"} {
		if child, ok := m[key]; ok {
			walkWidget(child, path+"."+key, cat, bag)
		}
	}
}

// checkWidgetFilters scans a string for `{{ ... | filter | filter:arg }}` and
// flags any filter name not in the catalog's whitelist.
func checkWidgetFilters(s, path string, cat *catalog.Catalog, bag *diagnostic.Bag) {
	if cat == nil {
		return
	}
	for _, placeholder := range widgetFilterRe.FindAllString(s, -1) {
		body := strings.TrimSuffix(strings.TrimPrefix(placeholder, "{{"), "}}")
		parts := strings.Split(body, "|")
		if len(parts) < 2 {
			continue
		}
		for _, raw := range parts[1:] {
			name := strings.TrimSpace(raw)
			if i := strings.Index(name, ":"); i >= 0 {
				name = name[:i]
			}
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if !cat.HasWidgetFilter(name) {
				d := diagnostic.Errorf(diagnostic.CodeBadEnum, posUnknown,
					"%s: unknown widget filter %q", path, name)
				if s, ok := suggest.Closest(name, cat.WidgetFilters(), 2); ok {
					d = d.WithSuggestion(s, fmt.Sprintf("did you mean %q?", s))
				}
				bag.Add(d)
			}
		}
	}
}

func validateAction(act any, path string, bag *diagnostic.Bag) {
	switch a := act.(type) {
	case string:
		if _, ok := widgetActions[a]; !ok {
			pool := keysOf(widgetActions)
			d := diagnostic.Errorf(diagnostic.CodeBadEnum, posUnknown,
				"%s: unknown widget action %q", path, a)
			if s, ok := suggest.Closest(a, pool, 2); ok {
				d = d.WithSuggestion(s, fmt.Sprintf("did you mean %q?", s))
			}
			bag.Add(d)
		}
	case map[string]any:
		kind, _ := a["action"].(string)
		if kind == "" {
			kind, _ = a["type"].(string)
		}
		if kind != "" {
			validateAction(kind, path+".action", bag)
		}
	case []any:
		for i, x := range a {
			validateAction(x, fmt.Sprintf("%s.%d", path, i), bag)
		}
	}
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
