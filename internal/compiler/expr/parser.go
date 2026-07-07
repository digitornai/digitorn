package expr

import (
	"fmt"
	"strings"
	"unicode"
)

// Parse parses the contents of a {{...}} placeholder (the text between braces).
//
// Filter syntax `expr | filter | filter` is recognized but filters are stripped
// (filters apply at runtime; the compiler validates whitelisted names only).
func Parse(s string) (Expr, error) {
	base, filters := splitFilters(s)
	p := &parser{src: base, pos: 0}
	expr, err := p.parseFallback()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos != len(p.src) {
		return nil, fmt.Errorf("unexpected %q at offset %d", p.src[p.pos:], p.pos)
	}
	if ref, ok := expr.(Ref); ok && len(filters) > 0 {
		ref.Filters = filters
		return ref, nil
	}
	return expr, nil
}

// ParseWithFilters parses an expression and returns the base expression plus
// the chain of filter names.
func ParseWithFilters(s string) (Expr, []string, error) {
	base, filters := splitFilters(s)
	p := &parser{src: base, pos: 0}
	expr, err := p.parseFallback()
	if err != nil {
		return nil, nil, err
	}
	p.skipSpace()
	if p.pos != len(p.src) {
		return nil, nil, fmt.Errorf("unexpected %q at offset %d", p.src[p.pos:], p.pos)
	}
	return expr, filters, nil
}

// splitFilters splits "expr | f1 | f2" into ("expr", ["f1", "f2"]). Pipes
// inside quoted literals are preserved.
func splitFilters(s string) (string, []string) {
	parts := []string{}
	cur := strings.Builder{}
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '|':
			if !inSingle && !inDouble {
				parts = append(parts, cur.String())
				cur.Reset()
				continue
			}
		}
		cur.WriteByte(c)
	}
	parts = append(parts, cur.String())
	base := strings.TrimSpace(parts[0])
	if len(parts) == 1 {
		return base, nil
	}
	filters := make([]string, 0, len(parts)-1)
	for _, f := range parts[1:] {
		if name := strings.TrimSpace(f); name != "" {
			filters = append(filters, name)
		}
	}
	return base, filters
}

type parser struct {
	src string
	pos int
}

func (p *parser) parseFallback() (Expr, error) {
	first, err := p.parseAtom()
	if err != nil {
		return nil, err
	}
	alts := []Expr{first}
	for {
		p.skipSpace()
		if !strings.HasPrefix(p.src[p.pos:], "??") {
			break
		}
		p.pos += 2
		next, err := p.parseAtom()
		if err != nil {
			return nil, err
		}
		alts = append(alts, next)
	}
	if len(alts) == 1 {
		return alts[0], nil
	}
	return Fallback{Alternatives: alts}, nil
}

func (p *parser) parseAtom() (Expr, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("expected expression, got end of input")
	}
	switch c := p.src[p.pos]; {
	case c == '\'' || c == '"':
		return p.parseString(c)
	case isIdentStart(rune(c)):
		return p.parseIdentExpr()
	default:
		return nil, fmt.Errorf("unexpected character %q at offset %d", string(c), p.pos)
	}
}

func (p *parser) parseString(quote byte) (Expr, error) {
	start := p.pos + 1
	for i := start; i < len(p.src); i++ {
		if p.src[i] == quote {
			p.pos = i + 1
			return Literal{Value: p.src[start:i]}, nil
		}
	}
	return nil, fmt.Errorf("unterminated string literal at offset %d", p.pos)
}

func (p *parser) parseIdentExpr() (Expr, error) {
	first := p.readIdent()
	if first == "" {
		return nil, fmt.Errorf("expected identifier at offset %d", p.pos)
	}
	if p.pos < len(p.src) && p.src[p.pos] == ':' && first == "include" {
		p.pos++
		path := p.readUntilFallbackOrEnd()
		if path == "" {
			return nil, fmt.Errorf("include: expected path at offset %d", p.pos)
		}
		return Include{Path: path}, nil
	}
	if p.pos >= len(p.src) || p.src[p.pos] != '.' {
		// Bare identifier — looked up in the "var" namespace at eval time.
		return Ref{Namespace: "var", Path: []string{first}}, nil
	}
	path := []string{}
	for p.pos < len(p.src) && p.src[p.pos] == '.' {
		p.pos++
		seg := p.readIdent()
		if seg == "" {
			return nil, fmt.Errorf("expected identifier after '.' at offset %d", p.pos)
		}
		path = append(path, seg)
	}
	return Ref{Namespace: first, Path: path}, nil
}

func (p *parser) readIdent() string {
	start := p.pos
	for p.pos < len(p.src) {
		c := rune(p.src[p.pos])
		if isIdentPart(c) {
			p.pos++
			continue
		}
		break
	}
	return p.src[start:p.pos]
}

func (p *parser) readUntilFallbackOrEnd() string {
	start := p.pos
	for p.pos < len(p.src) {
		if strings.HasPrefix(p.src[p.pos:], "??") {
			break
		}
		p.pos++
	}
	return strings.TrimSpace(p.src[start:p.pos])
}

func (p *parser) skipSpace() {
	for p.pos < len(p.src) && unicode.IsSpace(rune(p.src[p.pos])) {
		p.pos++
	}
}

func isIdentStart(c rune) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c rune) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '-'
}
