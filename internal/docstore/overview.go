package docstore

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// GenerateOverview refreshes _index/overview.md + _index/stats.json — the
// derived, always-fresh global view the agent reads first. A few KB whatever
// the document size: this is what makes precise work on huge docs possible.
func GenerateOverview(m Manifest, dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, journalDir), 0o755); err != nil {
		return err
	}
	var md strings.Builder
	stats := map[string]any{}
	name := filepath.Base(ComposedPath(dir))
	fmt.Fprintf(&md, "# %s — fragmented document\n\n", name)

	// The composed document holds the RESOLVED scene (declarative fragments get
	// their geometry at compose) — overviews must describe it, not the raw
	// fragments, or every declarative element would look position-less.
	resolved := loadResolved(m, dir)

	for _, c := range m.Collections {
		frags, _, _ := loadCollection(dir, c)
		stats[c.Name] = len(frags)
		fmt.Fprintf(&md, "## %s — %d item(s)\n", c.Name, len(frags))
		if len(frags) == 0 {
			md.WriteString("(empty — add fragments under " + c.Name + "/<id>.json)\n\n")
			continue
		}
		els := resolved[c.Name]
		if m.Overview == "canvas" && len(els) > 0 {
			writeCanvasSection(&md, stats, c, els)
		} else {
			writeAutoSection(&md, c, frags)
		}
	}
	if m.Overview == "canvas" {
		var all []map[string]any
		for _, c := range m.Collections {
			all = append(all, resolved[c.Name]...)
		}
		if art := renderBraille(all, 96, 36); art != "" {
			md.WriteString("\n### Visual raster — the rendered scene, braille dots (LOOK at it:\n")
			md.WriteString("silhouettes, alignment, overlaps, gaps — then correct fragments)\n```\n")
			md.WriteString(art)
			md.WriteString("```\n")
		}
	}
	md.WriteString("\n> Edit one fragment per change; the composed file is derived — never edit it directly.\n")

	if err := os.WriteFile(filepath.Join(dir, journalDir, "overview.md"), []byte(md.String()), 0o644); err != nil {
		return err
	}
	sb, _ := json.MarshalIndent(stats, "", "  ")
	return os.WriteFile(filepath.Join(dir, journalDir, "stats.json"), sb, 0o644)
}

// loadResolved parses the composed document into per-collection resolved items,
// composing in-memory when the composed file isn't on disk yet.
func loadResolved(m Manifest, dir string) map[string][]map[string]any {
	out := map[string][]map[string]any{}
	b, err := os.ReadFile(ComposedPath(dir))
	if err != nil {
		if cb, _, cerr := Compose(m, dir); cerr == nil {
			b = cb
		} else {
			return out
		}
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(b, &top) != nil {
		return out
	}
	for _, c := range m.Collections {
		key, kerr := c.pointerKey()
		if kerr != nil {
			continue
		}
		raw := top[key]
		if len(raw) == 0 {
			continue
		}
		if c.isMap() {
			var mm map[string]json.RawMessage
			if json.Unmarshal(raw, &mm) == nil {
				for id := range mm {
					out[c.Name] = append(out[c.Name], map[string]any{"id": id})
				}
			}
			continue
		}
		var items []map[string]any
		if json.Unmarshal(raw, &items) == nil {
			out[c.Name] = items
		}
	}
	return out
}

func writeAutoSection(md *strings.Builder, c Collection, frags []fragment) {
	ids := make([]string, 0, len(frags))
	for _, f := range frags {
		ids = append(ids, f.id)
	}
	sort.Strings(ids)
	fmt.Fprintf(md, "ids: %s\n\n", clipJoin(ids, 40))
}

func writeCanvasSection(md *strings.Builder, stats map[string]any, c Collection, els []map[string]any) {
	types := map[string]int{}
	colors := map[string]int{}
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	type box struct {
		id         string
		x, y, w, h float64
		hasPos     bool
	}
	boxes := make([]box, 0, len(els))
	var arrows []string

	for _, e := range els {
		id, _ := e["id"].(string)
		if t, ok := e["type"].(string); ok {
			types[t]++
		}
		for _, k := range []string{"strokeColor", "backgroundColor"} {
			if col, ok := e[k].(string); ok && col != "" && col != "transparent" {
				colors[col]++
			}
		}
		b := box{id: id}
		if b.x, b.hasPos = e["x"].(float64); b.hasPos {
			b.y, _ = e["y"].(float64)
			b.w, _ = e["width"].(float64)
			b.h, _ = e["height"].(float64)
			minX, minY = math.Min(minX, b.x), math.Min(minY, b.y)
			maxX, maxY = math.Max(maxX, b.x+b.w), math.Max(maxY, b.y+b.h)
		}
		boxes = append(boxes, b)
		src, _ := fieldValue(e, "startBinding.elementId")
		dst, _ := fieldValue(e, "endBinding.elementId")
		if src != nil || dst != nil {
			arrows = append(arrows, fmt.Sprintf("%s: %v → %v", id, orDash(src), orDash(dst)))
		}
	}

	tkeys := sortedKeys(types)
	for _, t := range tkeys {
		fmt.Fprintf(md, "  %-12s %d\n", t, types[t])
	}
	if !math.IsInf(minX, 1) {
		fmt.Fprintf(md, "bbox: (%.0f,%.0f) → (%.0f,%.0f)\n", minX, minY, maxX, maxY)
		stats[c.Name+"_bbox"] = []float64{minX, minY, maxX, maxY}
		md.WriteString("\n### Spatial grid (3×3, ids per cell)\n")
		w, h := maxX-minX, maxY-minY
		if w <= 0 {
			w = 1
		}
		if h <= 0 {
			h = 1
		}
		grid := map[int][]string{}
		for _, b := range boxes {
			if !b.hasPos {
				continue
			}
			cx := int(math.Min(2, math.Max(0, (b.x+b.w/2-minX)/w*3)))
			cy := int(math.Min(2, math.Max(0, (b.y+b.h/2-minY)/h*3)))
			grid[cy*3+cx] = append(grid[cy*3+cx], b.id)
		}
		for cell := 0; cell < 9; cell++ {
			ids := grid[cell]
			if len(ids) == 0 {
				continue
			}
			x0 := minX + float64(cell%3)*w/3
			y0 := minY + float64(cell/3)*h/3
			fmt.Fprintf(md, "  (%.0f-%.0f, %.0f-%.0f): %s\n", x0, x0+w/3, y0, y0+h/3, clipJoin(ids, 8))
		}
	}
	if len(arrows) > 0 {
		md.WriteString("\n### Bindings\n")
		sort.Strings(arrows)
		for _, a := range arrows {
			md.WriteString("  " + a + "\n")
		}
	}
	if len(colors) > 0 {
		md.WriteString("\n### Colors\n")
		for _, col := range sortedKeys(colors) {
			fmt.Fprintf(md, "  %s ×%d\n", col, colors[col])
		}
	}
	if strings.HasPrefix(c.Order, "field:") {
		field := strings.TrimPrefix(c.Order, "field:")
		parts := make([]string, 0, len(els)) // composed order IS the z-order
		for _, e := range els {
			id, _ := e["id"].(string)
			if v, ok := fieldValue(e, field); ok {
				parts = append(parts, fmt.Sprintf("%s(%v)", id, v))
			} else {
				parts = append(parts, id)
			}
		}
		md.WriteString("\n### Z-order\n  " + clipJoin(parts, 60) + "\n")
	}
	md.WriteString("\n")
}

func orDash(v any) any {
	if v == nil {
		return "—"
	}
	return v
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if m[out[i]] != m[out[j]] {
			return m[out[i]] > m[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

func clipJoin(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(" … (+%d)", len(items)-max)
}
