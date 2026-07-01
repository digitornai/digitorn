// Package diagnostic carries typed compiler messages and aggregates them in a Bag.
package diagnostic

import (
	"fmt"
	"sort"

	"github.com/digitornai/digitorn/internal/compiler/position"
)

type Severity int

const (
	SeverityError Severity = iota
	SeverityWarning
	SeverityInfo
	SeverityHelp
)

func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	case SeverityInfo:
		return "info"
	case SeverityHelp:
		return "help"
	default:
		return "unknown"
	}
}

type Diagnostic struct {
	Code       Code
	Severity   Severity
	Message    string
	Pos        position.Pos
	Span       position.Span
	Hints      []string
	Suggestion *Suggestion
}

type Suggestion struct {
	Replacement string
	Span        position.Span
	Reason      string
}

func (d Diagnostic) WithSpan(s position.Span) Diagnostic { d.Span = s; return d }
func (d Diagnostic) WithHint(h string) Diagnostic        { d.Hints = append(d.Hints, h); return d }
func (d Diagnostic) WithSuggestion(repl, reason string) Diagnostic {
	d.Suggestion = &Suggestion{Replacement: repl, Span: d.Span, Reason: reason}
	return d
}

func Errorf(code Code, pos position.Pos, format string, args ...any) Diagnostic {
	return Diagnostic{Code: code, Severity: SeverityError, Message: fmt.Sprintf(format, args...), Pos: pos}
}

func Warningf(code Code, pos position.Pos, format string, args ...any) Diagnostic {
	return Diagnostic{Code: code, Severity: SeverityWarning, Message: fmt.Sprintf(format, args...), Pos: pos}
}

type Bag struct {
	items []Diagnostic
}

func NewBag() *Bag                        { return &Bag{} }
func (b *Bag) Add(d Diagnostic)           { b.items = append(b.items, d) }
func (b *Bag) All() []Diagnostic          { return b.items }
func (b *Bag) Errors() []Diagnostic       { return b.filter(SeverityError) }
func (b *Bag) Warnings() []Diagnostic     { return b.filter(SeverityWarning) }
func (b *Bag) ByCode(c Code) []Diagnostic { return b.filterCode(c) }

func (b *Bag) HasErrors() bool {
	for _, d := range b.items {
		if d.Severity == SeverityError {
			return true
		}
	}
	return false
}

func (b *Bag) Sorted() []Diagnostic {
	out := append([]Diagnostic(nil), b.items...)
	sort.SliceStable(out, func(i, j int) bool {
		a, c := out[i].Pos, out[j].Pos
		switch {
		case a.File != c.File:
			return a.File < c.File
		case a.Line != c.Line:
			return a.Line < c.Line
		default:
			return a.Column < c.Column
		}
	})
	return out
}

func (b *Bag) filter(sev Severity) []Diagnostic {
	out := make([]Diagnostic, 0)
	for _, d := range b.items {
		if d.Severity == sev {
			out = append(out, d)
		}
	}
	return out
}

func (b *Bag) filterCode(c Code) []Diagnostic {
	out := make([]Diagnostic, 0)
	for _, d := range b.items {
		if d.Code == c {
			out = append(out, d)
		}
	}
	return out
}
