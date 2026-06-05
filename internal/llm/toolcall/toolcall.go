// Package toolcall recovers tool calls from models that emit them as TEXT
// instead of native structured tool_calls (DeepSeek, Hermes/Qwen builds, older
// Claude, many open-weight models served without a tool-call parser).
//
// It is a pure, self-contained library : no I/O, no daemon types, no runtime
// coupling. A Registry holds ordered Parsers ; Extract runs them against an
// assistant message and returns the first format that matches, with the markup
// stripped from the surviving prose. New formats plug in via Register without
// touching any caller.
package toolcall

import "strings"

// Call is one recovered tool invocation. Arguments are decoded JSON values
// (string, float64, bool, map, slice) keyed by parameter name.
type Call struct {
	Name      string
	Arguments map[string]any
}

// Result is the outcome of an Extract. Format is the matching parser's name,
// empty when nothing matched (Cleaned then equals the input).
type Result struct {
	Calls   []Call
	Cleaned string
	Format  string
}

// Matched reports whether any parser recovered at least one call.
func (r Result) Matched() bool { return r.Format != "" && len(r.Calls) > 0 }

// Parser recognises one textual tool-call format. Parse returns ok=false when
// the format is absent so the registry falls through to the next parser. When
// ok=true, cleaned is content with the parser's markup removed.
type Parser interface {
	Name() string
	Parse(content string) (calls []Call, cleaned string, ok bool)
}

// Registry runs parsers in registration order, first match wins. Safe for
// concurrent Extract once constructed ; Register is construction-time only.
type Registry struct {
	parsers []Parser
}

func New(parsers ...Parser) *Registry {
	return &Registry{parsers: append([]Parser(nil), parsers...)}
}

// Register appends a parser at the lowest priority. Call before the registry is
// shared across goroutines.
func (r *Registry) Register(p Parser) {
	if p != nil {
		r.parsers = append(r.parsers, p)
	}
}

// Extract returns the first format that recovers a call. A cheap content scan
// short-circuits the common case (plain prose, no markup) before any parser
// runs, so models that already use native tool_calls pay almost nothing.
func (r *Registry) Extract(content string) Result {
	if content == "" || !mightContainCall(content) {
		return Result{Cleaned: content}
	}
	for _, p := range r.parsers {
		if calls, cleaned, ok := p.Parse(content); ok && len(calls) > 0 {
			return Result{Calls: calls, Cleaned: strings.TrimSpace(cleaned), Format: p.Name()}
		}
	}
	return Result{Cleaned: content}
}

// mightContainCall is a fast pre-filter : the markers every supported format
// relies on. Keeps Extract O(len) on plain text instead of running every regexp.
func mightContainCall(s string) bool {
	for _, marker := range []string{"<invoke", "<function_calls", "<tool_call", "tool▁call", "tool_call", "```"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// Default is the registry of built-in formats, ordered most-specific first.
var Default = New(
	anthropicXML{},
	deepseekTokens{},
	hermesTags{},
	fencedJSON{},
)

// Extract runs the Default registry.
func Extract(content string) Result { return Default.Extract(content) }
