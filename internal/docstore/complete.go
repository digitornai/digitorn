package docstore

import (
	"encoding/json"
	"hash/fnv"
)

// applyDefaults completes every fragment into a full element by filling ONLY
// the fields it is missing, from the app-declared template. This is what turns
// a declarative `{id,type,cell}` into a shape the app can actually render —
// generically: the engine knows no app field names, they all come from the
// manifest's Defaults (in the app YAML). Never overwrites an authored or a
// resolver-computed value (missing-only), so it composes cleanly with layout.
func applyDefaults(m Manifest, colFrags map[string][]fragment) {
	if m.Defaults == nil {
		return
	}
	d := m.Defaults
	tf := d.TypeField
	if tf == "" {
		tf = "type"
	}
	for name := range colFrags {
		for i := range colFrags[name] {
			f := &colFrags[name][i]
			changed := false
			// Precedence: authored > by_type > common. Fill the more specific
			// type template first so it wins over the common fallback.
			if t, ok := f.obj[tf].(string); ok {
				for k, v := range d.ByType[t] {
					if _, has := f.obj[k]; !has {
						f.obj[k] = cloneJSON(v)
						changed = true
					}
				}
			}
			for k, v := range d.Common {
				if _, has := f.obj[k]; !has {
					f.obj[k] = cloneJSON(v)
					changed = true
				}
			}
			for kind, fields := range d.Generated {
				for _, fld := range fields {
					if _, has := f.obj[fld]; !has {
						f.obj[fld] = generate(kind, f.id, fld)
						changed = true
					}
				}
			}
			if changed {
				remarshal(f)
			}
		}
	}
}

// generate produces a deterministic value for an auto-filled field. "hash_int"
// yields a stable positive int derived from the element id + field name (used
// for Excalidraw's seed / versionNonce, which must exist and be per-element but
// whose exact value is irrelevant). Deterministic so re-composes don't churn.
func generate(kind, id, field string) any {
	switch kind {
	case "hash_int":
		h := fnv.New32a()
		h.Write([]byte(id))
		h.Write([]byte{0})
		h.Write([]byte(field))
		return float64(h.Sum32() % 2000000000)
	default:
		return nil
	}
}

// cloneJSON deep-copies a default value so shared template slices/maps are never
// aliased across elements.
func cloneJSON(v any) any {
	switch v.(type) {
	case []any, map[string]any:
		b, err := json.Marshal(v)
		if err != nil {
			return v
		}
		var out any
		if json.Unmarshal(b, &out) != nil {
			return v
		}
		return out
	default:
		return v
	}
}
