package docstore

import (
	"errors"
	"fmt"
	"math"
)

var pathStructuralFields = map[string]bool{
	"id": true, "type": true, "index": true, "points": true,
	"x": true, "y": true, "width": true, "height": true,
}

func styleFieldsFor(ps *PathSpec, parent map[string]any, fieldNames ...string) []string {
	if len(ps.StyleFields) > 0 {
		return ps.StyleFields
	}
	var out []string
	for k := range parent {
		if pathStructuralFields[k] {
			continue
		}
		skip := false
		for _, fn := range fieldNames {
			if k == fn {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, k)
		}
	}
	return out
}

// resolvePaths is the painter resolver: fragments carrying SVG path data become
// sampled point strokes, uniformly scaled and centred into their target box
// (an explicit box field, or the rect a grid cell already gave the fragment).
// The first subpath lands on the fragment itself; extra subpaths become
// generated sibling elements. The blind model draws in its own coordinate
// space and never computes a canvas pixel.
func resolvePaths(m Manifest, colFrags map[string][]fragment) []Diagnostic {
	if m.Layout == nil || m.Layout.Path == nil || m.Layout.Path.Field == "" {
		return nil
	}
	ps := m.Layout.Path
	boxField := orDefault(ps.Box, "box")
	viewField := orDefault(ps.View, "view")
	strokeType := orDefault(ps.Type, "freedraw")
	step := ps.Step
	if step == 0 {
		step = 6
	}
	canvas := rect{200, 100, 700, 700}
	if len(ps.Canvas) == 4 && ps.Canvas[2] > 0 && ps.Canvas[3] > 0 {
		canvas = rect{ps.Canvas[0], ps.Canvas[1], ps.Canvas[2], ps.Canvas[3]}
	}

	var diags []Diagnostic
	for name := range colFrags {
		var extra []fragment
		for i := range colFrags[name] {
			f := &colFrags[name][i]
			d, ok := f.obj[ps.Field].(string)
			if !ok || d == "" {
				continue
			}
			subs, err := samplePath(d, step)
			if errors.Is(err, ErrEmptyPath) {
				diags = append(diags, Diagnostic{Severity: "warning", Rule: "path",
					File: name + "/" + f.file, Message: "path draws nothing (a lone move-to?) — stroke skipped",
					Hint: "add at least one L/C/Q/A segment, e.g. \"M0,80 Q50,0 100,80\""})
				continue
			}
			if err != nil {
				diags = append(diags, Diagnostic{Severity: "warning", Rule: "path",
					File: name + "/" + f.file, Message: err.Error() + " — stroke skipped",
					Hint: "SVG path syntax: M/L/H/V/C/S/Q/T/A/Z, e.g. \"M0,80 Q50,0 100,80\""})
				f.obj["isDeleted"] = true
				remarshal(f)
				continue
			}
			// SHARED FRAME (view) wins: the agent draws every stroke in ONE art
			// coordinate space and the whole space maps to the canvas via a
			// single transform — so separate fragments keep their true relative
			// positions and proportions (a face's eyes stay small and placed).
			// Target: an explicit box on this fragment, else the manifest canvas.
			if view, okv := viewRect(f.obj[viewField]); okv {
				target := canvas
				if b, okb := boxVec(f.obj[boxField]); okb {
					target = b
				}
				mapViewToTarget(subs, view, target)
			} else {
				// Standalone figure: fit this fragment's own content into its box.
				fitToBox(subs, targetRect(f, boxField))
			}

			idx, _ := f.obj["index"].(string)
			carry := styleFieldsFor(ps, f.obj, ps.Field, viewField, boxField)
			for si, pts := range subs {
				if si == 0 {
					applyStroke(f.obj, pts, strokeType)
					remarshal(f)
					continue
				}
				id := fmt.Sprintf("%s__s%d", f.id, si)
				obj := map[string]any{"id": id}
				for _, k := range carry {
					if v, has := f.obj[k]; has {
						obj[k] = cloneJSON(v)
					}
				}
				if idx != "" {
					obj["index"] = fmt.Sprintf("%sV%d", idx, si)
				}
				applyStroke(obj, pts, strokeType)
				nf := fragment{file: sanitizeID(id) + ".json", id: id, obj: obj}
				remarshal(&nf)
				extra = append(extra, nf)
			}
		}
		if len(extra) > 0 {
			colFrags[name] = append(colFrags[name], extra...)
		}
	}
	return diags
}

// targetRect picks where the drawing should land: an explicit [x,y,w,h] box
// field wins, else the rect the fragment already has (e.g. from a grid cell).
func targetRect(f *fragment, boxField string) *rect {
	if b, ok := boxVec(f.obj[boxField]); ok {
		return &b
	}
	if r, ok := rectOf(f.obj); ok && r.w > 0 && r.h > 0 {
		return &r
	}
	return nil
}

// boxVec reads a [x,y,w,h] number array (w,h > 0) into a rect.
func boxVec(v any) (rect, bool) {
	arr, ok := v.([]any)
	if !ok || len(arr) != 4 {
		return rect{}, false
	}
	var out [4]float64
	for i, e := range arr {
		n, okn := e.(float64)
		if !okn {
			return rect{}, false
		}
		out[i] = n
	}
	if out[2] <= 0 || out[3] <= 0 {
		return rect{}, false
	}
	return rect{out[0], out[1], out[2], out[3]}, true
}

// viewRect reads a shared art-space viewport [x,y,w,h] (w,h > 0).
func viewRect(v any) (rect, bool) { return boxVec(v) }

// mapViewToTarget applies ONE uniform view→target transform to every point of
// every subpath — no per-fragment content fit — so all fragments sharing the
// same view land in one coherent coordinate frame. The view is fit into the
// target preserving aspect ratio and centred.
func mapViewToTarget(subs [][][2]float64, view, target rect) {
	s := math.Min(target.w/view.w, target.h/view.h)
	offX := target.x + (target.w-view.w*s)/2 - view.x*s
	offY := target.y + (target.h-view.h*s)/2 - view.y*s
	for _, pts := range subs {
		for i := range pts {
			pts[i][0] = pts[i][0]*s + offX
			pts[i][1] = pts[i][1]*s + offY
		}
	}
}

// fitToBox uniformly scales and centres every subpath into the target rect,
// preserving aspect ratio. Nil box = author coordinates are canvas-absolute.
func fitToBox(subs [][][2]float64, box *rect) {
	if box == nil {
		return
	}
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, pts := range subs {
		for _, p := range pts {
			minX, minY = math.Min(minX, p[0]), math.Min(minY, p[1])
			maxX, maxY = math.Max(maxX, p[0]), math.Max(maxY, p[1])
		}
	}
	w, h := maxX-minX, maxY-minY
	s := 1.0
	if w > 0 || h > 0 {
		sx, sy := math.Inf(1), math.Inf(1)
		if w > 0 {
			sx = box.w / w
		}
		if h > 0 {
			sy = box.h / h
		}
		s = math.Min(sx, sy)
		if math.IsInf(s, 1) {
			s = 1
		}
	}
	offX := box.x + (box.w-w*s)/2 - minX*s
	offY := box.y + (box.h-h*s)/2 - minY*s
	for _, pts := range subs {
		for i := range pts {
			pts[i][0] = pts[i][0]*s + offX
			pts[i][1] = pts[i][1]*s + offY
		}
	}
}

// applyStroke writes one sampled polyline onto an element: bbox-anchored
// relative points, matching x/y/width/height, stroke type if unset.
func applyStroke(obj map[string]any, pts [][2]float64, strokeType string) {
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, p := range pts {
		minX, minY = math.Min(minX, p[0]), math.Min(minY, p[1])
		maxX, maxY = math.Max(maxX, p[0]), math.Max(maxY, p[1])
	}
	rel := make([]any, len(pts))
	for i, p := range pts {
		rel[i] = []any{p[0] - minX, p[1] - minY}
	}
	if _, has := obj["type"]; !has {
		obj["type"] = strokeType
	}
	obj["x"], obj["y"] = minX, minY
	obj["width"], obj["height"] = maxX-minX, maxY-minY
	obj["points"] = rel
}
