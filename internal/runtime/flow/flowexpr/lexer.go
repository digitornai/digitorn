package flowexpr

import (
	"fmt"
	"strings"
)

type tokenKind int

const (
	tEOF tokenKind = iota
	tIdent
	tString
	tNumber
	tEq
	tNeq
	tLt
	tGt
	tLte
	tGte
	tAnd
	tOr
	tNot
	tLParen
	tRParen
	tTrue
	tFalse
)

type token struct {
	kind tokenKind
	text string
	pos  int
}

type lexer struct {
	src string
	pos int
}

func lex(src string) ([]token, error) {
	l := &lexer{src: src}
	var toks []token
	for {
		tok, err := l.next()
		if err != nil {
			return nil, err
		}
		toks = append(toks, tok)
		if tok.kind == tEOF {
			return toks, nil
		}
	}
}

func (l *lexer) next() (token, error) {
	for l.pos < len(l.src) && (l.src[l.pos] == ' ' || l.src[l.pos] == '\t' || l.src[l.pos] == '\n') {
		l.pos++
	}
	if l.pos >= len(l.src) {
		return token{kind: tEOF, pos: l.pos}, nil
	}

	start := l.pos
	c := l.src[l.pos]

	switch c {
	case '(':
		l.pos++
		return token{kind: tLParen, text: "(", pos: start}, nil
	case ')':
		l.pos++
		return token{kind: tRParen, text: ")", pos: start}, nil
	case '\'', '"':
		return l.lexString(c)
	case '=':
		if l.peek(1) == '=' {
			l.pos += 2
			return token{kind: tEq, text: "==", pos: start}, nil
		}
		return token{}, fmt.Errorf("flowexpr: unexpected '=' at %d (use '==')", start)
	case '!':
		if l.peek(1) == '=' {
			l.pos += 2
			return token{kind: tNeq, text: "!=", pos: start}, nil
		}
		l.pos++
		return token{kind: tNot, text: "!", pos: start}, nil
	case '<':
		if l.peek(1) == '=' {
			l.pos += 2
			return token{kind: tLte, text: "<=", pos: start}, nil
		}
		l.pos++
		return token{kind: tLt, text: "<", pos: start}, nil
	case '>':
		if l.peek(1) == '=' {
			l.pos += 2
			return token{kind: tGte, text: ">=", pos: start}, nil
		}
		l.pos++
		return token{kind: tGt, text: ">", pos: start}, nil
	case '&':
		if l.peek(1) == '&' {
			l.pos += 2
			return token{kind: tAnd, text: "&&", pos: start}, nil
		}
		return token{}, fmt.Errorf("flowexpr: unexpected '&' at %d (use '&&' or 'and')", start)
	case '|':
		if l.peek(1) == '|' {
			l.pos += 2
			return token{kind: tOr, text: "||", pos: start}, nil
		}
		return token{}, fmt.Errorf("flowexpr: unexpected '|' at %d (use '||' or 'or')", start)
	}

	if c >= '0' && c <= '9' || (c == '-' && isDigit(l.peek(1))) {
		return l.lexNumber()
	}
	if isIdentStart(c) {
		return l.lexIdent()
	}
	return token{}, fmt.Errorf("flowexpr: unexpected character %q at %d", string(c), start)
}

func (l *lexer) peek(n int) byte {
	if l.pos+n < len(l.src) {
		return l.src[l.pos+n]
	}
	return 0
}

func (l *lexer) lexString(quote byte) (token, error) {
	start := l.pos
	l.pos++ // skip opening quote
	var sb strings.Builder
	for l.pos < len(l.src) {
		ch := l.src[l.pos]
		if ch == '\\' && l.pos+1 < len(l.src) {
			sb.WriteByte(l.src[l.pos+1])
			l.pos += 2
			continue
		}
		if ch == quote {
			l.pos++
			return token{kind: tString, text: sb.String(), pos: start}, nil
		}
		sb.WriteByte(ch)
		l.pos++
	}
	return token{}, fmt.Errorf("flowexpr: unterminated string starting at %d", start)
}

func (l *lexer) lexNumber() (token, error) {
	start := l.pos
	if l.src[l.pos] == '-' {
		l.pos++
	}
	for l.pos < len(l.src) && (isDigit(l.src[l.pos]) || l.src[l.pos] == '.') {
		l.pos++
	}
	return token{kind: tNumber, text: l.src[start:l.pos], pos: start}, nil
}

func (l *lexer) lexIdent() (token, error) {
	start := l.pos
	for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
		l.pos++
	}
	text := l.src[start:l.pos]
	switch strings.ToLower(text) {
	case "and":
		return token{kind: tAnd, text: text, pos: start}, nil
	case "or":
		return token{kind: tOr, text: text, pos: start}, nil
	case "not":
		return token{kind: tNot, text: text, pos: start}, nil
	case "true":
		return token{kind: tTrue, text: text, pos: start}, nil
	case "false":
		return token{kind: tFalse, text: text, pos: start}, nil
	}
	return token{kind: tIdent, text: text, pos: start}, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || isDigit(c) || c == '.'
}
