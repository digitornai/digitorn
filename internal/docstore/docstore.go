package docstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type Manifest struct {
	Match       string       `json:"match" yaml:"match"`
	Root        string       `json:"root" yaml:"root"`
	Collections []Collection `json:"collections" yaml:"collections"`
	Validate    Validate     `json:"validate" yaml:"validate"`
	Overview    string       `json:"overview" yaml:"overview"`
	// RootDefaults seeds constant top-level fields on the composed document when
	// absent (e.g. the format header type/version/source an app file expects).
	RootDefaults map[string]any `json:"root_defaults,omitempty" yaml:"root_defaults,omitempty"`
	// Layout turns the engine into a geometry solver: the agent declares the
	// graph (edges from/to, optional grid cells) and the resolver computes all
	// pixels — node positions and pixel-perfect edge routing — at compose. The
	// blind model never touches a coordinate.
	Layout *Layout `json:"layout,omitempty" yaml:"layout,omitempty"`
	// Defaults completes a declarative fragment into a full, renderable element
	// by filling missing fields from a per-type template. Fully generic: the
	// engine knows NO app field names — every default lives in the app's YAML.
	Defaults *Defaults `json:"defaults,omitempty" yaml:"defaults,omitempty"`
}

type Defaults struct {
	TypeField string                    `json:"type_field,omitempty" yaml:"type_field,omitempty"` // field naming the element type (default "type")
	Common    map[string]any            `json:"common,omitempty" yaml:"common,omitempty"`         // applied to every element
	ByType    map[string]map[string]any `json:"by_type,omitempty" yaml:"by_type,omitempty"`       // type value → default fields
	Generated map[string][]string       `json:"generated,omitempty" yaml:"generated,omitempty"`   // kind → fields; kind "hash_int" = deterministic int from id
}

type Layout struct {
	Route      string     `json:"route,omitempty" yaml:"route,omitempty"` // "orthogonal" (default) | "straight"
	Gap        float64    `json:"gap,omitempty" yaml:"gap,omitempty"`
	Grid       *GridSpec  `json:"grid,omitempty" yaml:"grid,omitempty"`
	Edge       *EdgeSpec  `json:"edge,omitempty" yaml:"edge,omitempty"`
	Label      *LabelSpec `json:"label,omitempty" yaml:"label,omitempty"`
	Frame      *FrameSpec `json:"frame,omitempty" yaml:"frame,omitempty"`
	Path       *PathSpec  `json:"path,omitempty" yaml:"path,omitempty"`
	GroupField string     `json:"group_field,omitempty" yaml:"group_field,omitempty"` // element field → groupIds expansion
	Derived    []string   `json:"derived,omitempty" yaml:"derived,omitempty"`         // fields computed here, stripped on decompose
}

// FrameSpec auto-sizes a container: the agent lists member ids in Contains and
// the engine sets the frame rect (members' bbox + Pad) and stamps each member's
// FrameRef. Generic — a "frame" here is whatever the app calls a container.
type FrameSpec struct {
	Contains string  `json:"contains" yaml:"contains"`                       // frame field listing child ids
	FrameRef string  `json:"frame_ref,omitempty" yaml:"frame_ref,omitempty"` // child field set to the frame id (default "frameId")
	Pad      float64 `json:"pad,omitempty" yaml:"pad,omitempty"`             // padding around the members' bbox (default 24)
}

// PathSpec is the painter mode: the agent authors vector strokes in SVG path
// syntax (a language it truly masters) and the engine samples them into
// perfectly placed point strokes — scaled and centred into the fragment's
// target box. One subpath = one stroke element; extra subpaths become
// generated siblings.
type PathSpec struct {
	Field       string     `json:"field" yaml:"field"`
	Box         string     `json:"box,omitempty" yaml:"box,omitempty"`
	View        string     `json:"view,omitempty" yaml:"view,omitempty"`
	Canvas      [4]float64 `json:"canvas,omitempty" yaml:"canvas,omitempty"`
	Type        string     `json:"type,omitempty" yaml:"type,omitempty"`
	Step        float64    `json:"step,omitempty" yaml:"step,omitempty"`
	StyleFields []string   `json:"style_fields,omitempty" yaml:"style_fields,omitempty"`
}

// LabelSpec makes a container's text label declarative: the agent writes a
// label field on a node, the resolver materialises a bound, centred text
// element (positioned above the node so it is never hidden by a filled fill).
// Generic — every field name comes from here, the engine hard-codes nothing.
type LabelSpec struct {
	Field    string  `json:"field" yaml:"field"`                                   // node field holding the label
	TextKey  string  `json:"text_key,omitempty" yaml:"text_key,omitempty"`         // key inside it holding the string ("" = field is the string)
	In       string  `json:"in,omitempty" yaml:"in,omitempty"`                     // collection to inject the text into (default: first)
	Type     string  `json:"type,omitempty" yaml:"type,omitempty"`                 // element type for the text (default "text")
	IDSuffix string  `json:"id_suffix,omitempty" yaml:"id_suffix,omitempty"`       // generated id = node id + suffix (default "__label")
	Ref      string  `json:"ref,omitempty" yaml:"ref,omitempty"`                   // field on the text pointing back at its container (default "containerId")
	BindType string  `json:"bind_type,omitempty" yaml:"bind_type,omitempty"`       // boundElements type for the back-ref (default "text")
	Align    string  `json:"align,omitempty" yaml:"align,omitempty"`               // horizontal align (default "center")
	VAlign   string  `json:"valign,omitempty" yaml:"valign,omitempty"`             // vertical align (default "middle")
	FontSize float64 `json:"font_size,omitempty" yaml:"font_size,omitempty"`       // default 20
	Pad      float64 `json:"pad,omitempty" yaml:"pad,omitempty"`                   // inner padding (default 8)
}

type GridSpec struct {
	Field                                            string  `json:"field" yaml:"field"` // element field holding [col,row]
	CellW, CellH, GutterX, GutterY, OriginX, OriginY float64 `json:",inline"`
	DefaultW, DefaultH                               float64 `json:",inline"`
}

type EdgeSpec struct {
	From string `json:"from" yaml:"from"` // field naming the source element id
	To   string `json:"to" yaml:"to"`     // field naming the target element id
	In   string `json:"in" yaml:"in"`     // collection whose ids from/to resolve against
}

type Collection struct {
	Name  string `json:"name" yaml:"name"`
	Path  string `json:"path" yaml:"path"`
	ID    string `json:"id" yaml:"id"`
	Grain string `json:"grain" yaml:"grain"`
	Order string `json:"order" yaml:"order"`
}

type Validate struct {
	UniqueID bool  `json:"unique_id" yaml:"unique_id"`
	Refs     []Ref `json:"refs" yaml:"refs"`
}

type Ref struct {
	Field string `json:"field" yaml:"field"` // dotted path inside an item, e.g. "startBinding.elementId"
	In    string `json:"in" yaml:"in"`       // collection whose id set must contain the value
}

type Diagnostic struct {
	Severity string `json:"severity"` // "error" | "warning"
	Rule     string `json:"rule"`
	File     string `json:"file"`
	Message  string `json:"message"`
	Hint     string `json:"hint,omitempty"`
}

func (c Collection) isMap() bool { return c.ID == "$key" }

func (c Collection) pointerKey() (string, error) {
	p := strings.TrimSpace(c.Path)
	if !strings.HasPrefix(p, "/") || strings.Count(p, "/") != 1 || len(p) < 2 {
		return "", fmt.Errorf("collection %q: v1 supports top-level pointers only, got %q", c.Name, c.Path)
	}
	return p[1:], nil
}

// hashRaw hashes the canonical form (sorted keys via Go's map marshaling) so a
// fragment and the same item re-emitted by the app in a different key order
// hash identically — otherwise every canvas save would mark all items changed.
func hashRaw(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err == nil {
		if canon, merr := json.Marshal(v); merr == nil {
			h := sha256.Sum256(canon)
			return hex.EncodeToString(h[:])[:16]
		}
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])[:16]
}

// sanitizeID maps an item id to a filesystem-safe fragment basename.
func sanitizeID(id string) string {
	if id == "" {
		return "_empty"
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), ".")
	if s == "" {
		s = "_id"
	}
	if s != id {
		h := sha256.Sum256([]byte(id))
		s += "-" + hex.EncodeToString(h[:])[:6]
	}
	return s
}

// fieldValue walks a dotted path ("startBinding.elementId") inside an item.
func fieldValue(item map[string]any, dotted string) (any, bool) {
	cur := any(item)
	for _, seg := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok || cur == nil {
			return nil, false
		}
	}
	return cur, true
}

// closestID suggests the nearest known id for a dangling reference.
func closestID(want string, ids []string) string {
	best, bestD := "", 1<<30
	for _, id := range ids {
		d := levenshtein(want, id)
		if d < bestD {
			bestD, best = d, id
		}
	}
	if best == "" || bestD > len(want)/2+2 {
		return ""
	}
	return best
}

func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = minInt(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func minInt(a, b, c int) int {
	if a > b {
		a = b
	}
	if a > c {
		a = c
	}
	return a
}

// orderItems sorts fragments for composition. "field:<f>" sorts by that field
// (fractional-index strings sort lexicographically by design; numbers by value),
// items missing the field go last; "name"/empty sorts by fragment basename.
func orderItems(items []fragment, order string) {
	field := ""
	if strings.HasPrefix(order, "field:") {
		field = strings.TrimPrefix(order, "field:")
	}
	sort.SliceStable(items, func(i, j int) bool {
		if field != "" {
			vi, oki := fieldValue(items[i].obj, field)
			vj, okj := fieldValue(items[j].obj, field)
			switch {
			case oki && okj:
				ni, iNum := vi.(float64)
				nj, jNum := vj.(float64)
				if iNum && jNum {
					if ni != nj {
						return ni < nj
					}
				} else {
					si, sj := fmt.Sprint(vi), fmt.Sprint(vj)
					if si != sj {
						return si < sj
					}
				}
			case oki != okj:
				return oki
			}
		}
		return items[i].file < items[j].file
	})
}
