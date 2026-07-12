package docstore

import "strings"

// resolveLabels materialises declarative node labels into bound text elements.
// It runs during compose AFTER resolveLayout (so nodes have geometry) and
// BEFORE applyDefaults (so the generated text is completed like any element).
// The blind agent writes only `label: {text: "…"}` on a node; the engine
// creates the centred text, wires it both ways (containerId + boundElements),
// and z-orders it just above its node so a filled fill never hides it.
func resolveLabels(m Manifest, colFrags map[string][]fragment) {
	if m.Layout == nil || m.Layout.Label == nil || m.Layout.Label.Field == "" {
		return
	}
	ls := m.Layout.Label
	coll := ls.In
	if coll == "" && len(m.Collections) > 0 {
		coll = m.Collections[0].Name
	}
	suffix := orDefault(ls.IDSuffix, "__label")
	typ := orDefault(ls.Type, "text")
	ref := orDefault(ls.Ref, "containerId")
	bind := orDefault(ls.BindType, "text")
	align := orDefault(ls.Align, "center")
	valign := orDefault(ls.VAlign, "middle")
	fs := ls.FontSize
	if fs == 0 {
		fs = 20
	}
	pad := ls.Pad
	if pad == 0 {
		pad = 8
	}
	lineH := fs * 1.25

	var labels []fragment
	for name := range colFrags {
		for i := range colFrags[name] {
			f := &colFrags[name][i]
			if strings.HasSuffix(f.id, suffix) {
				continue // a label never labels itself
			}
			text := labelText(f.obj[ls.Field], ls.TextKey)
			if text == "" {
				continue
			}
			r, ok := rectOf(f.obj)
			if !ok {
				continue
			}
			id := f.id + suffix
			idx, _ := f.obj["index"].(string)
			lo := map[string]any{
				"id": id, "type": typ, ref: f.id,
				"text": text, "originalText": text,
				"textAlign": align, "verticalAlign": valign,
				"fontSize": fs,
				"width":    r.w - 2*pad, "height": lineH,
				"x": r.x + pad, "y": r.cy() - lineH/2,
			}
			if idx != "" {
				lo["index"] = idx + "V" // sorts right after its node → drawn on top
			}
			addBoundElement(f, id, bind)
			lf := fragment{file: sanitizeID(id) + ".json", id: id, obj: lo}
			remarshal(&lf)
			labels = append(labels, lf)
		}
	}
	for name := range colFrags {
		for i := range colFrags[name] {
			if colFrags[name][i].dirty {
				remarshal(&colFrags[name][i])
				colFrags[name][i].dirty = false
			}
		}
	}
	if len(labels) > 0 {
		colFrags[coll] = append(colFrags[coll], labels...)
	}
}

// labelText pulls the label string out of a node's label field: either the
// value directly, or the value under textKey when the label is an object.
func labelText(v any, textKey string) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		if textKey == "" {
			return ""
		}
		s, _ := t[textKey].(string)
		return s
	}
	return ""
}

// isGeneratedLabel reports whether an item id is one the label resolver
// produces — so decompose never persists it as a hand-authored fragment.
func isGeneratedLabel(m Manifest, id string) bool {
	if m.Layout == nil || m.Layout.Label == nil {
		return false
	}
	return strings.HasSuffix(id, orDefault(m.Layout.Label.IDSuffix, "__label"))
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
