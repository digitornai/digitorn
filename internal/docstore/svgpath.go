package docstore

import (
	"fmt"
	"math"
)

// samplePath converts SVG path data into polylines — one per subpath. This is
// the painter bridge: the model authors curves in a syntax it has seen millions
// of times, the engine turns them into evenly sampled stroke points. Supports
// M/L/H/V/C/S/Q/T/A/Z, absolute and relative.
func samplePath(d string, step float64) ([][][2]float64, error) {
	if step <= 0 {
		step = 6
	}
	p := &pathScanner{s: d}
	var subs [][][2]float64
	var cur [][2]float64
	var x, y, startX, startY float64
	var lastCX, lastCY float64 // reflected control point memory
	var lastCmd byte

	flush := func() {
		if len(cur) > 1 {
			subs = append(subs, cur)
		}
		cur = nil
	}
	moveTo := func(nx, ny float64) {
		flush()
		x, y, startX, startY = nx, ny, nx, ny
		cur = [][2]float64{{x, y}}
	}
	lineTo := func(nx, ny float64) {
		x, y = nx, ny
		cur = append(cur, [2]float64{x, y})
	}
	curveTo := func(eval func(t float64) (float64, float64), est float64, ex, ey float64) {
		n := int(math.Ceil(est / step))
		if n < 8 {
			n = 8
		}
		if n > 128 {
			n = 128
		}
		for i := 1; i <= n; i++ {
			px, py := eval(float64(i) / float64(n))
			cur = append(cur, [2]float64{px, py})
		}
		x, y = ex, ey
	}

	for {
		cmd, ok := p.command()
		if !ok {
			break
		}
		rel := cmd >= 'a'
		up := cmd &^ 0x20 // uppercase
		for first := true; first || p.hasNumber(); first = false {
			switch up {
			case 'M':
				nx, ny, err := p.pair()
				if err != nil {
					return nil, err
				}
				if rel {
					nx, ny = x+nx, y+ny
				}
				if first {
					moveTo(nx, ny)
				} else {
					lineTo(nx, ny) // extra M pairs are implicit LineTos
				}
			case 'L':
				nx, ny, err := p.pair()
				if err != nil {
					return nil, err
				}
				if rel {
					nx, ny = x+nx, y+ny
				}
				lineTo(nx, ny)
			case 'H':
				n, err := p.number()
				if err != nil {
					return nil, err
				}
				if rel {
					n += x
				}
				lineTo(n, y)
			case 'V':
				n, err := p.number()
				if err != nil {
					return nil, err
				}
				if rel {
					n += y
				}
				lineTo(x, n)
			case 'C', 'S':
				var x1, y1 float64
				var err error
				if up == 'C' {
					x1, y1, err = p.pair()
					if err != nil {
						return nil, err
					}
					if rel {
						x1, y1 = x+x1, y+y1
					}
				} else { // S: first control = reflection of previous C/S control
					if lastCmd == 'C' || lastCmd == 'S' {
						x1, y1 = 2*x-lastCX, 2*y-lastCY
					} else {
						x1, y1 = x, y
					}
				}
				x2, y2, err := p.pair()
				if err != nil {
					return nil, err
				}
				ex, ey, err := p.pair()
				if err != nil {
					return nil, err
				}
				if rel {
					x2, y2, ex, ey = x+x2, y+y2, x+ex, y+ey
				}
				x0, y0 := x, y
				est := dist(x0, y0, x1, y1) + dist(x1, y1, x2, y2) + dist(x2, y2, ex, ey)
				curveTo(func(t float64) (float64, float64) {
					u := 1 - t
					px := u*u*u*x0 + 3*u*u*t*x1 + 3*u*t*t*x2 + t*t*t*ex
					py := u*u*u*y0 + 3*u*u*t*y1 + 3*u*t*t*y2 + t*t*t*ey
					return px, py
				}, est, ex, ey)
				lastCX, lastCY = x2, y2
			case 'Q', 'T':
				var x1, y1 float64
				var err error
				if up == 'Q' {
					x1, y1, err = p.pair()
					if err != nil {
						return nil, err
					}
					if rel {
						x1, y1 = x+x1, y+y1
					}
				} else { // T: control = reflection of previous Q/T control
					if lastCmd == 'Q' || lastCmd == 'T' {
						x1, y1 = 2*x-lastCX, 2*y-lastCY
					} else {
						x1, y1 = x, y
					}
				}
				ex, ey, err := p.pair()
				if err != nil {
					return nil, err
				}
				if rel {
					ex, ey = x+ex, y+ey
				}
				x0, y0 := x, y
				est := dist(x0, y0, x1, y1) + dist(x1, y1, ex, ey)
				curveTo(func(t float64) (float64, float64) {
					u := 1 - t
					px := u*u*x0 + 2*u*t*x1 + t*t*ex
					py := u*u*y0 + 2*u*t*y1 + t*t*ey
					return px, py
				}, est, ex, ey)
				lastCX, lastCY = x1, y1
			case 'A':
				rx, err := p.number()
				if err != nil {
					return nil, err
				}
				ry, err := p.number()
				if err != nil {
					return nil, err
				}
				rot, err := p.number()
				if err != nil {
					return nil, err
				}
				laf, err := p.number()
				if err != nil {
					return nil, err
				}
				swf, err := p.number()
				if err != nil {
					return nil, err
				}
				ex, ey, err := p.pair()
				if err != nil {
					return nil, err
				}
				if rel {
					ex, ey = x+ex, y+ey
				}
				sampleArc(&cur, x, y, math.Abs(rx), math.Abs(ry), rot*math.Pi/180, laf != 0, swf != 0, ex, ey, step)
				x, y = ex, ey
			case 'Z':
				lineTo(startX, startY)
				flush()
				x, y = startX, startY
			default:
				return nil, fmt.Errorf("unsupported path command %q", string(cmd))
			}
			if up == 'Z' {
				break
			}
			lastCmd = up
		}
	}
	flush()
	if len(subs) == 0 {
		return nil, fmt.Errorf("path has no drawable segments")
	}
	return subs, nil
}

// sampleArc appends an elliptical arc (SVG endpoint parameterisation, W3C
// conversion to centre form) as sampled points.
func sampleArc(cur *[][2]float64, x1, y1, rx, ry, phi float64, largeArc, sweep bool, x2, y2, step float64) {
	if rx == 0 || ry == 0 {
		*cur = append(*cur, [2]float64{x2, y2})
		return
	}
	cosP, sinP := math.Cos(phi), math.Sin(phi)
	dx, dy := (x1-x2)/2, (y1-y2)/2
	x1p := cosP*dx + sinP*dy
	y1p := -sinP*dx + cosP*dy
	lam := x1p*x1p/(rx*rx) + y1p*y1p/(ry*ry)
	if lam > 1 {
		s := math.Sqrt(lam)
		rx, ry = rx*s, ry*s
	}
	num := rx*rx*ry*ry - rx*rx*y1p*y1p - ry*ry*x1p*x1p
	den := rx*rx*y1p*y1p + ry*ry*x1p*x1p
	co := 0.0
	if den != 0 && num > 0 {
		co = math.Sqrt(num / den)
	}
	if largeArc == sweep {
		co = -co
	}
	cxp := co * rx * y1p / ry
	cyp := -co * ry * x1p / rx
	cx := cosP*cxp - sinP*cyp + (x1+x2)/2
	cy := sinP*cxp + cosP*cyp + (y1+y2)/2
	ang := func(ux, uy, vx, vy float64) float64 {
		d := math.Sqrt((ux*ux + uy*uy) * (vx*vx + vy*vy))
		if d == 0 {
			return 0
		}
		c := (ux*vx + uy*vy) / d
		c = math.Max(-1, math.Min(1, c))
		a := math.Acos(c)
		if ux*vy-uy*vx < 0 {
			a = -a
		}
		return a
	}
	th1 := ang(1, 0, (x1p-cxp)/rx, (y1p-cyp)/ry)
	dth := ang((x1p-cxp)/rx, (y1p-cyp)/ry, (-x1p-cxp)/rx, (-y1p-cyp)/ry)
	if !sweep && dth > 0 {
		dth -= 2 * math.Pi
	}
	if sweep && dth < 0 {
		dth += 2 * math.Pi
	}
	est := math.Abs(dth) * math.Max(rx, ry)
	n := int(math.Ceil(est / step))
	if n < 8 {
		n = 8
	}
	if n > 128 {
		n = 128
	}
	for i := 1; i <= n; i++ {
		t := th1 + dth*float64(i)/float64(n)
		px := cx + rx*math.Cos(t)*cosP - ry*math.Sin(t)*sinP
		py := cy + rx*math.Cos(t)*sinP + ry*math.Sin(t)*cosP
		*cur = append(*cur, [2]float64{px, py})
	}
}

func dist(x1, y1, x2, y2 float64) float64 { return math.Hypot(x2-x1, y2-y1) }

// pathScanner tokenises SVG path data: command letters and numbers separated
// by spaces/commas, with SVG's compact quirks ("-" starts a new number, a
// second "." ends one).
type pathScanner struct {
	s string
	i int
}

func (p *pathScanner) skip() {
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == ' ' || c == ',' || c == '\t' || c == '\n' || c == '\r' {
			p.i++
		} else {
			break
		}
	}
}

func (p *pathScanner) command() (byte, bool) {
	p.skip()
	if p.i >= len(p.s) {
		return 0, false
	}
	c := p.s[p.i]
	if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
		p.i++
		return c, true
	}
	return 0, false
}

func (p *pathScanner) hasNumber() bool {
	p.skip()
	if p.i >= len(p.s) {
		return false
	}
	c := p.s[p.i]
	return (c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.'
}

func (p *pathScanner) number() (float64, error) {
	p.skip()
	start := p.i
	if p.i < len(p.s) && (p.s[p.i] == '-' || p.s[p.i] == '+') {
		p.i++
	}
	dot := false
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c >= '0' && c <= '9' {
			p.i++
		} else if c == '.' && !dot {
			dot = true
			p.i++
		} else if (c == 'e' || c == 'E') && p.i > start {
			p.i++
			if p.i < len(p.s) && (p.s[p.i] == '-' || p.s[p.i] == '+') {
				p.i++
			}
		} else {
			break
		}
	}
	if p.i == start {
		return 0, fmt.Errorf("expected number at offset %d in path data", start)
	}
	var f float64
	if _, err := fmt.Sscanf(p.s[start:p.i], "%g", &f); err != nil {
		return 0, fmt.Errorf("bad number %q at offset %d", p.s[start:p.i], start)
	}
	return f, nil
}

func (p *pathScanner) pair() (float64, float64, error) {
	a, err := p.number()
	if err != nil {
		return 0, 0, err
	}
	b, err := p.number()
	if err != nil {
		return 0, 0, err
	}
	return a, b, nil
}
