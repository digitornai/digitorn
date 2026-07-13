package docstore

import (
	"fmt"
	"math"
)

// styleFields are carried from a path fragment onto its generated sibling
// strokes so a multi-stroke figure stays visually coherent.
var styleFields = []string{
	"strokeColor", "backgroundColor", "fillStyle", "strokeWidth", "strokeStyle",
	"roughness", "opacity", "groupIds", "frameId",
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
	strokeType := orDefault(ps.Type, "freedraw")
	step := ps.Step
	if step == 0 {
		step = 6
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
			if err != nil {
				diags = append(diags, Diagnostic{Severity: "error", Rule: "path",
					File: name + "/" + f.file, Message: err.Error(),
					Hint: "SVG path syntax: M/L/H/V/C/S/Q/T/A/Z, e.g. \"M0,80 Q50,0 100,80\""})
				continue
			}
			fitToBox(subs, targetRect(f, boxField))

			idx, _ := f.obj["index"].(string)
			for si, pts := range subs {
				if si == 0 {
					applyStroke(f.obj, pts, strokeType)
					remarshal(f)
					continue
				}
				id := fmt.Sprintf("%s__s%d", f.id, si)
				obj := map[string]any{"id": id}
				for _, k := range styleFields {
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
	if arr, ok := f.obj[boxField].([]any); ok && len(arr) == 4 {
		v := [4]float64{}
		good := true
		for i, e := range arr {
			n, okn := e.(float64)
			if !okn {
				good = false
				break
			}
			v[i] = n
		}
		if good && v[2] > 0 && v[3] > 0 {
			return &rect{v[0], v[1], v[2], v[3]}
		}
	}
	if r, ok := rectOf(f.obj); ok && r.w > 0 && r.h > 0 {
		return &r
	}
	return nil
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
