package filesystem

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/tidwall/gjson"
	"github.com/tidwall/pretty"
)

// Surgical JSON editing for the `edit` / `read` tools. Instead of rewriting a
// whole .json every turn (token-heavy, truncation-prone), an agent sends a tiny
// RFC 6902 JSON Patch, and `read` shows the document's structure in depth so the
// agent knows exactly which pointers/indices to target.

// looksJSON reports whether `data` is a JSON object/array document (by content,
// so it covers .json, .excalidraw, .geojson… without an extension whitelist).
func looksJSON(data []byte) bool {
	t := strings.TrimSpace(string(data))
	if len(t) == 0 || (t[0] != '{' && t[0] != '[') {
		return false
	}
	return json.Valid([]byte(t))
}

// applyJSONPatch applies an RFC 6902 patch (evanphx/json-patch) to `content`
// and returns the re-serialized document (2-space indent) plus the op count.
// An empty file is treated as `{}` so a patch can build it up from nothing.
func applyJSONPatch(content string, patchRaw json.RawMessage) (string, int, error) {
	p, err := jsonpatch.DecodePatch(patchRaw)
	if err != nil {
		return "", 0, fmt.Errorf("invalid JSON Patch (RFC 6902): %w", err)
	}
	doc := []byte(content)
	if strings.TrimSpace(content) == "" {
		doc = []byte("{}")
	}
	out, err := p.ApplyIndent(doc, "  ")
	if err != nil {
		return "", 0, fmt.Errorf("patch failed: %w", err)
	}
	return string(out) + "\n", len(p), nil
}

// selectJSONPath returns the value at `path` within `content`, pretty-printed.
// `path` is a gjson path — which supports querying arrays by field, e.g.
// `elements.#(id=="r1")` (first match) or `elements.#(type=="text")#` (all) —
// and a leading-slash JSON Pointer (`/elements/3/x`) is accepted too and
// converted, so it stays consistent with the patch pointers.
func selectJSONPath(content, path string) (string, error) {
	gp := path
	if strings.HasPrefix(path, "/") {
		gp = jsonPointerToGJSON(path)
	}
	res := gjson.Get(content, gp)
	if !res.Exists() {
		return "", fmt.Errorf("no value at json_path %q", path)
	}
	return string(pretty.Pretty([]byte(res.Raw))), nil
}

// jsonPointerToGJSON converts an RFC 6901 JSON Pointer to a gjson path:
// `/elements/3/x` → `elements.3.x`, unescaping ~1→/ and ~0→~.
func jsonPointerToGJSON(ptr string) string {
	ptr = strings.TrimPrefix(ptr, "/")
	if ptr == "" {
		return "@this"
	}
	parts := strings.Split(ptr, "/")
	for i, p := range parts {
		p = strings.ReplaceAll(p, "~1", "/")
		p = strings.ReplaceAll(p, "~0", "~")
		parts[i] = p
	}
	return strings.Join(parts, ".")
}

// ── read: deep structure view ─────────────────────────────────────────────

const (
	structMaxDepth    = 6  // how deep to recurse into nested objects
	structMaxItems    = 40 // array items summarised before "… N more"
	structSalientKeys = 5  // salient fields shown per array-of-objects item
	structStrCap      = 48 // string value truncation
)

// preferredKeys are surfaced first in an object one-liner — they identify the
// item (id/type/name…) so the agent can target it.
var preferredKeys = []string{"id", "type", "name", "key", "label", "title", "role"}

// jsonStructure renders a compact, deep structural map of a JSON document:
// object keys with types, array counts, and a one-line summary (id/type/salient
// fields) for every array-of-objects item. The agent SEES the whole shape
// without reading the full content, then edits by pointer.
func jsonStructure(content string) (string, error) {
	var root any
	if err := json.Unmarshal([]byte(content), &root); err != nil {
		return "", fmt.Errorf("file is not valid JSON: %w", err)
	}
	var b strings.Builder
	renderStruct(&b, root, 0)
	return strings.TrimRight(b.String(), "\n"), nil
}

func indent(d int) string { return strings.Repeat("  ", d) }

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func renderStruct(b *strings.Builder, node any, depth int) {
	switch v := node.(type) {
	case map[string]any:
		fmt.Fprintf(b, "object · %s\n", plural(len(v), "key"))
		if depth >= structMaxDepth {
			return
		}
		for _, k := range sortedKeys(v) {
			fmt.Fprintf(b, "%s%s: ", indent(depth+1), k)
			renderChild(b, v[k], depth+1)
		}
	case []any:
		fmt.Fprintf(b, "array · %s\n", plural(len(v), "item"))
		if depth >= structMaxDepth {
			return
		}
		shown := len(v)
		if shown > structMaxItems {
			shown = structMaxItems
		}
		for i := 0; i < shown; i++ {
			fmt.Fprintf(b, "%s[%d] %s\n", indent(depth+1), i, itemSummary(v[i]))
		}
		if len(v) > shown {
			fmt.Fprintf(b, "%s… %d more\n", indent(depth+1), len(v)-shown)
		}
	default:
		fmt.Fprintf(b, "%s\n", scalar(node))
	}
}

// renderChild renders a value that follows a "key: " prefix already written.
func renderChild(b *strings.Builder, node any, depth int) {
	switch node.(type) {
	case map[string]any, []any:
		renderStruct(b, node, depth)
	default:
		fmt.Fprintf(b, "%s\n", scalar(node))
	}
}

// itemSummary is the one-liner for an array element: type + salient fields.
func itemSummary(node any) string {
	switch v := node.(type) {
	case map[string]any:
		fields := salientFields(v)
		return fmt.Sprintf("object · %s {%s}", plural(len(v), "key"), strings.Join(fields, ", "))
	case []any:
		return fmt.Sprintf("array · %s", plural(len(v), "item"))
	default:
		return scalar(node)
	}
}

func salientFields(m map[string]any) []string {
	var out []string
	seen := map[string]bool{}
	add := func(k string) {
		if seen[k] || len(out) >= structSalientKeys {
			return
		}
		if val, ok := m[k]; ok {
			out = append(out, fmt.Sprintf("%s:%s", k, scalar(val)))
			seen[k] = true
		}
	}
	for _, k := range preferredKeys {
		add(k)
	}
	for _, k := range sortedKeys(m) {
		if len(out) >= structSalientKeys {
			break
		}
		if _, isScalar := m[k].(map[string]any); isScalar {
			continue
		}
		if _, isArr := m[k].([]any); isArr {
			continue
		}
		add(k)
	}
	return out
}

// scalar renders a leaf value compactly; containers become shape stubs.
func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case string:
		s := t
		if len(s) > structStrCap {
			s = s[:structStrCap] + "…"
		}
		return strconv.Quote(s)
	case map[string]any:
		return fmt.Sprintf("{…%d}", len(t))
	case []any:
		return fmt.Sprintf("[%d]", len(t))
	default:
		return fmt.Sprint(v)
	}
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
