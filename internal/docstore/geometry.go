package docstore

import "math"

// rect is an element's axis-aligned box in canvas coordinates.
type rect struct{ x, y, w, h float64 }

func (r rect) cx() float64 { return r.x + r.w/2 }
func (r rect) cy() float64 { return r.y + r.h/2 }

// edgePoint returns the point on r's border where a straight line toward
// (tx,ty) crosses it — the exact attach point for a connector, with an outward
// gap. This is the deterministic edge math a blind model cannot do reliably.
func edgePoint(r rect, tx, ty, gap float64) (float64, float64) {
	cx, cy := r.cx(), r.cy()
	dx, dy := tx-cx, ty-cy
	if dx == 0 && dy == 0 {
		return cx, r.y - gap
	}
	hw, hh := r.w/2+gap, r.h/2+gap
	// scale the direction so it lands on the padded border (whichever axis hits first)
	sx, sy := math.Inf(1), math.Inf(1)
	if dx != 0 {
		sx = hw / math.Abs(dx)
	}
	if dy != 0 {
		sy = hh / math.Abs(dy)
	}
	s := math.Min(sx, sy)
	return cx + dx*s, cy + dy*s
}

// routeStraight connects two rects centre-to-centre, clipped to their padded
// edges. Returns the arrow's origin (x,y) and its relative points.
func routeStraight(a, b rect, gap float64) (x, y float64, points [][2]float64) {
	ax, ay := edgePoint(a, b.cx(), b.cy(), gap)
	bx, by := edgePoint(b, a.cx(), a.cy(), gap)
	return ax, ay, [][2]float64{{0, 0}, {bx - ax, by - ay}}
}

// routeOrthogonal produces an elbow path (right angles only). It leaves the
// source from the edge facing the target, turns at the mid axis, and enters the
// target from the facing edge — the clean "flowchart" look, deterministic.
func routeOrthogonal(a, b rect, gap float64) (x, y float64, points [][2]float64) {
	dxc, dyc := b.cx()-a.cx(), b.cy()-a.cy()
	horizontal := math.Abs(dxc) >= math.Abs(dyc)

	var ax, ay, bx, by float64
	if horizontal {
		// exit right/left face, enter opposite face
		if dxc >= 0 {
			ax, ay = a.x+a.w+gap, a.cy()
			bx, by = b.x-gap, b.cy()
		} else {
			ax, ay = a.x-gap, a.cy()
			bx, by = b.x+b.w+gap, b.cy()
		}
		midx := (ax + bx) / 2
		return ax, ay, [][2]float64{
			{0, 0}, {midx - ax, 0}, {midx - ax, by - ay}, {bx - ax, by - ay},
		}
	}
	// vertical
	if dyc >= 0 {
		ax, ay = a.cx(), a.y+a.h+gap
		bx, by = b.cx(), b.y-gap
	} else {
		ax, ay = a.cx(), a.y-gap
		bx, by = b.cx(), b.y+b.h+gap
	}
	midy := (ay + by) / 2
	return ax, ay, [][2]float64{
		{0, 0}, {0, midy - ay}, {bx - ax, midy - ay}, {bx - ax, by - ay},
	}
}

// pointsBounds returns the width/height an arrow needs so Excalidraw's bbox
// matches its points (w/h must equal the points' extent or the arrow renders
// clipped).
func pointsBounds(points [][2]float64) (w, h float64) {
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, p := range points {
		minX, minY = math.Min(minX, p[0]), math.Min(minY, p[1])
		maxX, maxY = math.Max(maxX, p[0]), math.Max(maxY, p[1])
	}
	return maxX - minX, maxY - minY
}

// gridToXY maps a logical cell [col,row] to an absolute top-left, with fixed
// cell size and gutter so a grid layout can NEVER overlap.
type gridSpec struct {
	cellW, cellH, gutterX, gutterY, originX, originY float64
}

func defaultGrid() gridSpec {
	return gridSpec{cellW: 200, cellH: 90, gutterX: 120, gutterY: 90, originX: 100, originY: 60}
}

func (g gridSpec) cellXY(col, row int) (x, y float64) {
	return g.originX + float64(col)*(g.cellW+g.gutterX),
		g.originY + float64(row)*(g.cellH+g.gutterY)
}
