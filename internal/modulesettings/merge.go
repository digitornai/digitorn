package modulesettings

import "encoding/json"

// DeepMerge returns base with overlay applied: nested objects merge recursively;
// every other value (scalar, array) in overlay REPLACES the one in base. Neither
// input is mutated. Arrays are replaced wholesale (a config form edits the full
// list), which is the intended behaviour for e.g. databases[].
func DeepMerge(base, overlay map[string]any) map[string]any {
	out := cloneMap(base)
	for k, ov := range overlay {
		if om, ok := ov.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = DeepMerge(bm, om)
				continue
			}
		}
		out[k] = ov
	}
	return out
}

// Diff returns only the fields where `submitted` differs from `defaults` — the
// sparse deltas to persist. Objects recurse; scalars/arrays are kept when not
// deep-equal. A field absent from defaults is always kept. This is what lets an
// untouched field keep following the YAML default (even after an app update).
func Diff(submitted, defaults map[string]any) map[string]any {
	out := map[string]any{}
	for k, sv := range submitted {
		dv, has := defaults[k]
		if !has {
			out[k] = sv
			continue
		}
		if sm, ok := sv.(map[string]any); ok {
			if dm, ok := dv.(map[string]any); ok {
				if sub := Diff(sm, dm); len(sub) > 0 {
					out[k] = sub
				}
				continue
			}
		}
		if !jsonEqual(sv, dv) {
			out[k] = sv
		}
	}
	return out
}

func jsonEqual(a, b any) bool {
	ab, e1 := json.Marshal(a)
	bb, e2 := json.Marshal(b)
	return e1 == nil && e2 == nil && string(ab) == string(bb)
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{}
	}
	return out
}
