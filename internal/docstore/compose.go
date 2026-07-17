package docstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var ErrInvalid = errors.New("docstore: fragments invalid — compose refused")

type fragment struct {
	file  string
	raw   json.RawMessage
	obj   map[string]any
	id    string
	dirty bool
}

func Compose(m Manifest, dir string) (composed []byte, diags []Diagnostic, err error) {
	root := map[string]json.RawMessage{}
	rootFile := m.Root
	if rootFile == "" {
		rootFile = "meta.json"
	}
	if b, rerr := os.ReadFile(filepath.Join(dir, rootFile)); rerr == nil {
		if derr := json.Unmarshal(b, &root); derr != nil {
			diags = append(diags, parseDiag(rootFile, b, derr))
		}
	}
	for k, v := range m.RootDefaults {
		if _, has := root[k]; !has {
			if b, merr := json.Marshal(v); merr == nil {
				root[k] = b
			}
		}
	}

	idSets := map[string]map[string]string{}
	colFrags := map[string][]fragment{}
	for _, c := range m.Collections {
		frags, ids, ds := loadCollection(dir, c)
		diags = append(diags, ds...)
		colFrags[c.Name] = frags
		idSets[c.Name] = ids
	}

	diags = append(diags, checkRefs(m, colFrags, idSets)...)

	for _, d := range diags {
		if d.Severity == "error" {
			return nil, diags, ErrInvalid
		}
	}

	resolveLayout(m, colFrags)
	pdiags := resolvePaths(m, colFrags)
	diags = append(diags, pdiags...)
	for _, d := range pdiags {
		if d.Severity == "error" {
			return nil, diags, ErrInvalid
		}
	}
	resolveFrames(m, colFrags)
	resolveGroups(m, colFrags)
	resolveLabels(m, colFrags)
	applyDefaults(m, colFrags)

	for _, c := range m.Collections {
		key, perr := c.pointerKey()
		if perr != nil {
			return nil, append(diags, Diagnostic{Severity: "error", Rule: "manifest", Message: perr.Error()}), ErrInvalid
		}
		frags := colFrags[c.Name]
		orderItems(frags, c.Order)
		root[key] = assemble(frags, c.isMap())
	}

	out, merr := json.Marshal(root)
	if merr != nil {
		return nil, diags, merr
	}
	return out, diags, nil
}

func loadCollection(dir string, c Collection) (frags []fragment, ids map[string]string, diags []Diagnostic) {
	ids = map[string]string{}
	base := filepath.Join(dir, filepath.FromSlash(c.Name))
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, ids, nil
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		rel := c.Name + "/" + e.Name()
		b, rerr := os.ReadFile(filepath.Join(base, e.Name()))
		if rerr != nil {
			diags = append(diags, Diagnostic{Severity: "warning", Rule: "io", File: rel, Message: rerr.Error()})
			continue
		}
		var obj map[string]any
		if derr := json.Unmarshal(b, &obj); derr != nil {
			d := parseDiag(rel, b, derr)
			d.Severity = "warning"
			diags = append(diags, d)
			continue
		}
		f := fragment{file: e.Name(), raw: json.RawMessage(b), obj: obj}
		if c.isMap() {
			f.id = strings.TrimSuffix(e.Name(), ".json")
		} else {
			f.id, _ = obj[c.ID].(string)
			if f.id == "" {
				diags = append(diags, Diagnostic{Severity: "warning", Rule: "id", File: rel,
					Message: fmt.Sprintf("item has no %q field — every fragment needs a stable id; skipped", c.ID)})
				continue
			}
			if prev, dup := ids[f.id]; dup {
				diags = append(diags, Diagnostic{Severity: "warning", Rule: "unique_id", File: rel,
					Message: fmt.Sprintf("id %q already defined in %s/%s — this one skipped", f.id, c.Name, prev)})
				continue
			}
			if want := sanitizeID(f.id) + ".json"; e.Name() != want {
				diags = append(diags, Diagnostic{Severity: "warning", Rule: "filename", File: rel,
					Message: fmt.Sprintf("filename does not match id %q", f.id),
					Hint:    "rename to " + c.Name + "/" + want})
			}
		}
		ids[f.id] = e.Name()
		frags = append(frags, f)
	}
	return frags, ids, diags
}

func checkRefs(m Manifest, colFrags map[string][]fragment, idSets map[string]map[string]string) []Diagnostic {
	var diags []Diagnostic
	type refCheck struct{ field, in string }
	checks := make([]refCheck, 0, len(m.Validate.Refs)+2)
	for _, r := range m.Validate.Refs {
		checks = append(checks, refCheck{r.Field, r.In})
	}
	if e := edgeSpec(m); e != nil && e.In != "" {
		checks = append(checks, refCheck{e.From, e.In}, refCheck{e.To, e.In})
	}
	if len(checks) == 0 {
		return nil
	}
	for _, c := range m.Collections {
		for _, f := range colFrags[c.Name] {
			for _, ref := range checks {
				v, ok := fieldValue(f.obj, ref.field)
				if !ok {
					continue
				}
				want, _ := v.(string)
				if want == "" {
					continue
				}
				targets := idSets[ref.in]
				if _, exists := targets[want]; exists {
					continue
				}
				known := make([]string, 0, len(targets))
				for id := range targets {
					known = append(known, id)
				}
				sort.Strings(known)
				d := Diagnostic{Severity: "warning", Rule: "refs", File: c.Name + "/" + f.file,
					Message: fmt.Sprintf("%s %q references no item in %s", ref.field, want, ref.in)}
				if close := closestID(want, known); close != "" {
					d.Hint = fmt.Sprintf("closest id: %q", close)
				}
				diags = append(diags, d)
			}
		}
	}
	return diags
}

func edgeSpec(m Manifest) *EdgeSpec {
	if m.Layout == nil {
		return nil
	}
	return m.Layout.Edge
}

func assemble(frags []fragment, asMap bool) json.RawMessage {
	var b bytes.Buffer
	open, close := byte('['), byte(']')
	if asMap {
		open, close = '{', '}'
	}
	b.WriteByte(open)
	for i, f := range frags {
		if i > 0 {
			b.WriteByte(',')
		}
		if asMap {
			k, _ := json.Marshal(f.id)
			b.Write(k)
			b.WriteByte(':')
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, f.raw); err == nil {
			b.Write(compact.Bytes())
		} else {
			b.Write(f.raw)
		}
	}
	b.WriteByte(close)
	return json.RawMessage(b.Bytes())
}

func parseDiag(file string, src []byte, err error) Diagnostic {
	d := Diagnostic{Severity: "error", Rule: "parse", File: file, Message: err.Error()}
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		off := int(syn.Offset)
		lo, hi := off-25, off+25
		if lo < 0 {
			lo = 0
		}
		if hi > len(src) {
			hi = len(src)
		}
		d.Message = fmt.Sprintf("%v (at byte %d, near: %q)", err, off, string(src[lo:hi]))
	}
	return d
}
