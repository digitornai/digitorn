// Package position defines source positions for compiler diagnostics.
package position

import "fmt"

type Pos struct {
	File   string
	Line   int
	Column int
}

func (p Pos) Zero() bool { return p.File == "" && p.Line == 0 && p.Column == 0 }

func (p Pos) String() string {
	switch {
	case p.Zero():
		return "<unknown>"
	case p.File == "":
		return fmt.Sprintf("%d:%d", p.Line, p.Column)
	default:
		return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Column)
	}
}

type Span struct {
	File  string
	Start Pos
	End   Pos
}

func (s Span) Zero() bool { return s.Start.Zero() && s.End.Zero() }

func (s Span) String() string {
	switch {
	case s.Zero():
		return "<unknown>"
	case s.Start.Line == s.End.Line:
		return fmt.Sprintf("%s:%d:%d-%d", s.File, s.Start.Line, s.Start.Column, s.End.Column)
	default:
		return fmt.Sprintf("%s:%d:%d-%d:%d", s.File, s.Start.Line, s.Start.Column, s.End.Line, s.End.Column)
	}
}

func Point(file string, line, column int) Pos {
	return Pos{File: file, Line: line, Column: column}
}

func Range(file string, startLine, startCol, endLine, endCol int) Span {
	return Span{
		File:  file,
		Start: Pos{File: file, Line: startLine, Column: startCol},
		End:   Pos{File: file, Line: endLine, Column: endCol},
	}
}
