package flowexpr

import (
	"fmt"
	"strconv"
)

type parser struct {
	toks []token
	pos  int
}

func parse(src string) (node, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	if p.cur().kind == tIdent && p.cur().text == "default" && p.peek().kind == tEOF {
		return defaultSentinel{}, nil
	}
	n, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.cur().kind != tEOF {
		return nil, fmt.Errorf("flowexpr: unexpected token %q at %d", p.cur().text, p.cur().pos)
	}
	return n, nil
}

func (p *parser) cur() token  { return p.toks[p.pos] }
func (p *parser) peek() token {
	if p.pos+1 < len(p.toks) {
		return p.toks[p.pos+1]
	}
	return p.toks[len(p.toks)-1]
}
func (p *parser) advance() token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *parser) parseOr() (node, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == tOr {
		p.advance()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = binary{op: tOr, l: left, r: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (node, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == tAnd {
		p.advance()
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = binary{op: tAnd, l: left, r: right}
	}
	return left, nil
}

func (p *parser) parseNot() (node, error) {
	if p.cur().kind == tNot {
		p.advance()
		x, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return unaryNot{x: x}, nil
	}
	return p.parseComparison()
}

func (p *parser) parseComparison() (node, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	switch p.cur().kind {
	case tEq, tNeq, tLt, tGt, tLte, tGte:
		op := p.advance().kind
		right, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		return binary{op: op, l: left, r: right}, nil
	}
	return left, nil
}

func (p *parser) parsePrimary() (node, error) {
	t := p.cur()
	switch t.kind {
	case tLParen:
		p.advance()
		n, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.cur().kind != tRParen {
			return nil, fmt.Errorf("flowexpr: expected ')' at %d", p.cur().pos)
		}
		p.advance()
		return n, nil
	case tString:
		p.advance()
		return litString{v: t.text}, nil
	case tNumber:
		p.advance()
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, fmt.Errorf("flowexpr: bad number %q at %d", t.text, t.pos)
		}
		return litNumber{v: f}, nil
	case tTrue:
		p.advance()
		return litBool{v: true}, nil
	case tFalse:
		p.advance()
		return litBool{v: false}, nil
	case tIdent:
		p.advance()
		return ref{path: splitPath(t.text)}, nil
	}
	return nil, fmt.Errorf("flowexpr: unexpected token %q at %d", t.text, t.pos)
}

// SplitPath splits a dotted path for callers building flowexpr.Context lookups.
func SplitPath(s string) []string { return splitPath(s) }

func splitPath(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}
