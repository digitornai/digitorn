package docstore

import (
	"math"
	"strings"
)

// renderBraille rasterises resolved elements into braille art — the agent's
// eyes. Each char cell is 2×4 dots, so an 96×36 char frame is a 192×144 dot
// canvas: enough to SEE silhouettes, alignment, overlaps and gaps, in text.
func renderBraille(els []map[string]any, cols, rows int) string {
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	type shape struct {
		kind string
		r    rect
		pts  [][2]float64
	}
	var shapes []shape
	for _, e := range els {
		if del, _ := e["isDeleted"].(bool); del {
			continue
		}
		kind, _ := e["type"].(string)
		x, ok1 := e["x"].(float64)
		y, ok2 := e["y"].(float64)
		if !ok1 || !ok2 {
			continue
		}
		w, _ := e["width"].(float64)
		h, _ := e["height"].(float64)
		s := shape{kind: kind, r: rect{x, y, w, h}}
		if raw, ok := e["points"].([]any); ok && len(raw) > 1 {
			for _, p := range raw {
				pp, okp := p.([]any)
				if !okp || len(pp) != 2 {
					continue
				}
				px, okx := pp[0].(float64)
				py, oky := pp[1].(float64)
				if okx && oky {
					s.pts = append(s.pts, [2]float64{x + px, y + py})
				}
			}
		}
		minX, minY = math.Min(minX, s.r.x), math.Min(minY, s.r.y)
		maxX, maxY = math.Max(maxX, s.r.x+s.r.w), math.Max(maxY, s.r.y+s.r.h)
		shapes = append(shapes, s)
	}
	if len(shapes) == 0 || math.IsInf(minX, 1) {
		return ""
	}
	w, h := maxX-minX, maxY-minY
	if w <= 0 {
		w = 1
	}
	if h <= 0 {
		h = 1
	}
	dw, dh := float64(cols*2), float64(rows*4)
	scale := math.Min(dw/w, dh/h)
	ox := (dw - w*scale) / 2
	oy := (dh - h*scale) / 2

	dots := make([][]bool, int(dh))
	for i := range dots {
		dots[i] = make([]bool, int(dw))
	}
	plot := func(x, y float64) {
		dx := int((x-minX)*scale + ox)
		dy := int((y-minY)*scale + oy)
		if dx >= 0 && dx < int(dw) && dy >= 0 && dy < int(dh) {
			dots[dy][dx] = true
		}
	}
	seg := func(x1, y1, x2, y2 float64) {
		n := int(math.Max(math.Abs(x2-x1), math.Abs(y2-y1))*scale) + 1
		for i := 0; i <= n; i++ {
			t := float64(i) / float64(n)
			plot(x1+(x2-x1)*t, y1+(y2-y1)*t)
		}
	}
	for _, s := range shapes {
		switch {
		case len(s.pts) > 1: // freedraw / line / arrow
			for i := 1; i < len(s.pts); i++ {
				seg(s.pts[i-1][0], s.pts[i-1][1], s.pts[i][0], s.pts[i][1])
			}
		case s.kind == "ellipse":
			cx, cy := s.r.cx(), s.r.cy()
			for i := 0; i <= 64; i++ {
				a := float64(i) / 64 * 2 * math.Pi
				plot(cx+s.r.w/2*math.Cos(a), cy+s.r.h/2*math.Sin(a))
			}
		case s.kind == "diamond":
			cx, cy := s.r.cx(), s.r.cy()
			seg(cx, s.r.y, s.r.x+s.r.w, cy)
			seg(s.r.x+s.r.w, cy, cx, s.r.y+s.r.h)
			seg(cx, s.r.y+s.r.h, s.r.x, cy)
			seg(s.r.x, cy, cx, s.r.y)
		default: // rectangle, frame, text, image — outline
			seg(s.r.x, s.r.y, s.r.x+s.r.w, s.r.y)
			seg(s.r.x+s.r.w, s.r.y, s.r.x+s.r.w, s.r.y+s.r.h)
			seg(s.r.x+s.r.w, s.r.y+s.r.h, s.r.x, s.r.y+s.r.h)
			seg(s.r.x, s.r.y+s.r.h, s.r.x, s.r.y)
		}
	}

	// braille dot bit layout per 2×4 cell
	bits := [4][2]rune{{0x01, 0x08}, {0x02, 0x10}, {0x04, 0x20}, {0x40, 0x80}}
	var out strings.Builder
	blank := 0
	for row := 0; row < rows; row++ {
		var line strings.Builder
		empty := true
		for col := 0; col < cols; col++ {
			ch := rune(0x2800)
			for sy := 0; sy < 4; sy++ {
				for sx := 0; sx < 2; sx++ {
					if dots[row*4+sy][col*2+sx] {
						ch |= bits[sy][sx]
						empty = false
					}
				}
			}
			line.WriteRune(ch)
		}
		if empty {
			blank++
			continue
		}
		for ; blank > 0; blank-- {
			out.WriteString("\n")
		}
		out.WriteString(strings.TrimRight(line.String(), "⠀") + "\n")
	}
	return out.String()
}
