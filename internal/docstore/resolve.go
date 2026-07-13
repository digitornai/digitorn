package docstore

import "encoding/json"

// resolveLayout is the geometry solver. It runs during compose, AFTER fragments
// are loaded and BEFORE assembly: it reads the declarative graph (grid cells,
// edge from/to) and writes the computed pixels back onto each element's obj —
// node positions with no overlap, and edge routing that lands exactly on the
// box borders with proper bindings + back-refs. Idempotent and deterministic:
// re-running after a box moves re-routes every arrow touching it.
func resolveLayout(m Manifest, colFrags map[string][]fragment) {
	if m.Layout == nil {
		return
	}
	lay := m.Layout
	gap := lay.Gap
	if gap == 0 {
		gap = 8
	}

	// index every element by id (pointer into the slice so mutations persist)
	byID := map[string]*fragment{}
	for name := range colFrags {
		for i := range colFrags[name] {
			byID[colFrags[name][i].id] = &colFrags[name][i]
		}
	}

	// 1) grid placement — logical cell → absolute top-left, never overlapping.
	if lay.Grid != nil && lay.Grid.Field != "" {
		g := gridFromSpec(lay.Grid)
		for _, f := range byID {
			cell, ok := intPair(f.obj[lay.Grid.Field])
			if !ok {
				continue
			}
			x, y := g.cellXY(cell[0], cell[1])
			f.obj["x"], f.obj["y"] = x, y
			if _, has := f.obj["width"]; !has && lay.Grid.DefaultW > 0 {
				f.obj["width"] = lay.Grid.DefaultW
			}
			if _, has := f.obj["height"]; !has && lay.Grid.DefaultH > 0 {
				f.obj["height"] = lay.Grid.DefaultH
			}
			remarshal(f)
		}
	}

	if lay.Edge == nil || lay.Edge.From == "" {
		return
	}

	// rects of every element that now has geometry
	rects := map[string]rect{}
	for id, f := range byID {
		if r, ok := rectOf(f.obj); ok {
			rects[id] = r
		}
	}

	// 2) edge routing — declarative from/to → pixel-perfect connector.
	orthogonal := lay.Route != "straight"
	for _, f := range byID {
		from, okF := f.obj[lay.Edge.From].(string)
		to, okT := f.obj[lay.Edge.To].(string)
		if !okF || !okT || from == "" || to == "" {
			continue
		}
		a, okA := rects[from]
		b, okB := rects[to]
		if !okA || !okB {
			f.obj["isDeleted"] = true
			remarshal(f)
			continue
		}
		var x, y float64
		var pts [][2]float64
		if orthogonal {
			x, y, pts = routeOrthogonal(a, b, gap)
		} else {
			x, y, pts = routeStraight(a, b, gap)
		}
		w, h := pointsBounds(pts)
		f.obj["x"], f.obj["y"] = x, y
		f.obj["width"], f.obj["height"] = w, h
		f.obj["points"] = toPoints(pts)
		f.obj["startBinding"] = map[string]any{"elementId": from, "focus": 0, "gap": gap}
		f.obj["endBinding"] = map[string]any{"elementId": to, "focus": 0, "gap": gap}
		addBoundElement(byID[from], f.id, "arrow")
		addBoundElement(byID[to], f.id, "arrow")
		remarshal(f)
	}
	// re-marshal endpoints whose boundElements changed
	for _, f := range byID {
		if f.dirty {
			remarshal(f)
			f.dirty = false
		}
	}
}

func gridFromSpec(g *GridSpec) gridSpec {
	out := defaultGrid()
	if g.CellW > 0 {
		out.cellW = g.CellW
	}
	if g.CellH > 0 {
		out.cellH = g.CellH
	}
	if g.GutterX > 0 {
		out.gutterX = g.GutterX
	}
	if g.GutterY > 0 {
		out.gutterY = g.GutterY
	}
	// Origin is used as-is (0 is a valid, common origin — not "unset").
	out.originX, out.originY = g.OriginX, g.OriginY
	return out
}

func rectOf(obj map[string]any) (rect, bool) {
	x, ok1 := obj["x"].(float64)
	y, ok2 := obj["y"].(float64)
	w, ok3 := obj["width"].(float64)
	h, ok4 := obj["height"].(float64)
	if ok1 && ok2 && ok3 && ok4 {
		return rect{x, y, w, h}, true
	}
	return rect{}, false
}

func intPair(v any) ([2]int, bool) {
	arr, ok := v.([]any)
	if !ok || len(arr) != 2 {
		return [2]int{}, false
	}
	a, ok1 := arr[0].(float64)
	b, ok2 := arr[1].(float64)
	if !ok1 || !ok2 {
		return [2]int{}, false
	}
	return [2]int{int(a), int(b)}, true
}

func toPoints(pts [][2]float64) []any {
	out := make([]any, len(pts))
	for i, p := range pts {
		out[i] = []any{p[0], p[1]}
	}
	return out
}

// addBoundElement records a bound child (arrow or text) on a container's
// boundElements — the back-reference the app needs to keep them attached.
func addBoundElement(f *fragment, childID, kind string) {
	if f == nil {
		return
	}
	var list []any
	if cur, ok := f.obj["boundElements"].([]any); ok {
		for _, e := range cur {
			if m, ok := e.(map[string]any); ok && m["id"] == childID {
				return // already present
			}
		}
		list = cur
	}
	f.obj["boundElements"] = append(list, map[string]any{"id": childID, "type": kind})
	f.dirty = true
}

func remarshal(f *fragment) {
	if b, err := json.Marshal(f.obj); err == nil {
		f.raw = b
	}
}

// authoredKeys are the declarative fields the agent owns (grid cell, edge
// from/to, label) — the source of truth the resolver reads. They must survive a
// lossy canvas round-trip that drops app-unknown fields.
func authoredKeys(m Manifest) []string {
	if m.Layout == nil {
		return nil
	}
	var ks []string
	if m.Layout.Grid != nil && m.Layout.Grid.Field != "" {
		ks = append(ks, m.Layout.Grid.Field)
	}
	if m.Layout.Edge != nil {
		if m.Layout.Edge.From != "" {
			ks = append(ks, m.Layout.Edge.From)
		}
		if m.Layout.Edge.To != "" {
			ks = append(ks, m.Layout.Edge.To)
		}
	}
	if m.Layout.Label != nil && m.Layout.Label.Field != "" {
		ks = append(ks, m.Layout.Label.Field)
	}
	if m.Layout.Frame != nil && m.Layout.Frame.Contains != "" {
		ks = append(ks, m.Layout.Frame.Contains)
	}
	if m.Layout.GroupField != "" {
		ks = append(ks, m.Layout.GroupField)
	}
	if m.Layout.Path != nil && m.Layout.Path.Field != "" {
		ks = append(ks, m.Layout.Path.Field,
			orDefault(m.Layout.Path.Box, "box"),
			orDefault(m.Layout.Path.View, "view"))
	}
	return ks
}

// preserveAuthored carries authored declarative fields from the stored fragment
// into an app-written item that dropped them (the app doesn't know these
// fields, so a canvas save would silently erase the agent's intent). Merges
// only missing keys — anything the app kept wins.
func preserveAuthored(m Manifest, old, updated json.RawMessage) json.RawMessage {
	keys := authoredKeys(m)
	if len(keys) == 0 {
		return updated
	}
	var o, n map[string]any
	if json.Unmarshal(old, &o) != nil || json.Unmarshal(updated, &n) != nil {
		return updated
	}
	changed := false
	for _, k := range keys {
		if _, has := n[k]; !has {
			if v, ok := o[k]; ok {
				n[k] = v
				changed = true
			}
		}
	}
	if !changed {
		return updated
	}
	if b, err := json.Marshal(n); err == nil {
		return b
	}
	return updated
}

// stripDerived keeps a decomposed RESOLVED fragment declarative: geometry the
// resolver computes (points, x/y, bindings…) is removed so the stored fragment
// carries only the agent's intent (from/to, contains, path). Plain items pass
// through untouched — a box the user dragged keeps its new position. No-op
// without a layout/derived declaration.
func stripDerived(m Manifest, raw json.RawMessage) json.RawMessage {
	if m.Layout == nil || len(m.Layout.Derived) == 0 {
		return raw
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return raw
	}
	resolved := false
	if e := m.Layout.Edge; e != nil {
		if _, ok := obj[e.From].(string); ok {
			resolved = true
		}
	}
	if fr := m.Layout.Frame; !resolved && fr != nil && fr.Contains != "" {
		if _, ok := stringSlice(obj[fr.Contains]); ok {
			resolved = true
		}
	}
	if p := m.Layout.Path; !resolved && p != nil && p.Field != "" {
		if s, ok := obj[p.Field].(string); ok && s != "" {
			resolved = true
		}
	}
	if !resolved {
		return raw
	}
	for _, k := range m.Layout.Derived {
		delete(obj, k)
	}
	if b, err := json.Marshal(obj); err == nil {
		return b
	}
	return raw
}
