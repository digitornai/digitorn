package docstore

import (
	"encoding/json"
	"hash/fnv"
)

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
