package docstore

import "math"

// resolveFrames turns a declarative container into a correctly-sized frame: the
// agent lists the member ids (`contains`) and the engine computes the frame's
// rect as the members' bounding box plus padding, and stamps each member's
// frameId. Auto-sizing a container around its members is exactly the geometry a
// blind model gets wrong and a human must redo on every change — here it is free
// and always correct. Runs after grid placement (members have rects).
func resolveFrames(m Manifest, colFrags map[string][]fragment) {
	if m.Layout == nil || m.Layout.Frame == nil || m.Layout.Frame.Contains == "" {
		return
	}
	fs := m.Layout.Frame
	ref := orDefault(fs.FrameRef, "frameId")
	pad := fs.Pad
	if pad == 0 {
		pad = 24
	}
	byID := map[string]*fragment{}
	for name := range colFrags {
		for i := range colFrags[name] {
			byID[colFrags[name][i].id] = &colFrags[name][i]
		}
	}
	// a member id also claims its generated siblings (painter strokes "id__sN")
	siblings := map[string][]*fragment{}
	for id, f := range byID {
		if i := lastGenSuffix(id); i > 0 {
			parent := id[:i]
			siblings[parent] = append(siblings[parent], f)
		}
	}
	for _, f := range byID {
		ids, ok := stringSlice(f.obj[fs.Contains])
		if !ok || len(ids) == 0 {
			continue
		}
		minX, minY := math.Inf(1), math.Inf(1)
		maxX, maxY := math.Inf(-1), math.Inf(-1)
		var members []*fragment
		for _, id := range ids {
			cf := byID[id]
			if cf == nil {
				continue
			}
			for _, sib := range append([]*fragment{cf}, siblings[id]...) {
				r, okr := rectOf(sib.obj)
				if !okr {
					continue
				}
				minX, minY = math.Min(minX, r.x), math.Min(minY, r.y)
				maxX, maxY = math.Max(maxX, r.x+r.w), math.Max(maxY, r.y+r.h)
				members = append(members, sib)
			}
		}
		if len(members) == 0 {
			continue
		}
		f.obj["x"], f.obj["y"] = minX-pad, minY-pad
		f.obj["width"], f.obj["height"] = (maxX-minX)+2*pad, (maxY-minY)+2*pad
		if _, has := f.obj["type"]; !has {
			f.obj["type"] = "frame"
		}
		for _, cf := range members {
			cf.obj[ref] = f.id
			remarshal(cf)
		}
		remarshal(f)
	}
}

// resolveGroups expands a declarative group membership field into Excalidraw's
// groupIds so several elements move and style as one — the agent writes
// `group:"g1"` (or a list) and never manages the array by hand.
func resolveGroups(m Manifest, colFrags map[string][]fragment) {
	if m.Layout == nil || m.Layout.GroupField == "" {
		return
	}
	field := m.Layout.GroupField
	for name := range colFrags {
		for i := range colFrags[name] {
			f := &colFrags[name][i]
			ids, ok := stringSlice(f.obj[field])
			if !ok || len(ids) == 0 {
				continue
			}
			existing, _ := stringSlice(f.obj["groupIds"])
			seen := map[string]bool{}
			out := []any{}
			for _, g := range append(existing, ids...) {
				if !seen[g] {
					seen[g] = true
					out = append(out, g)
				}
			}
			f.obj["groupIds"] = out
			remarshal(f)
		}
	}
}

// lastGenSuffix returns the index where a generated-sibling suffix ("__s<n>")
// starts in id, or 0 if the id is not a generated sibling.
func lastGenSuffix(id string) int {
	for i := len(id) - 1; i > 2; i-- {
		if id[i] < '0' || id[i] > '9' {
			if id[i] == 's' && id[i-1] == '_' && id[i-2] == '_' && i+1 < len(id) {
				return i - 2
			}
			return 0
		}
	}
	return 0
}

// stringSlice coerces a JSON value into a []string: a bare string becomes a
// one-element slice, an array keeps its string members.
func stringSlice(v any) ([]string, bool) {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil, false
		}
		return []string{t}, true
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out, len(out) > 0
	}
	return nil, false
}
