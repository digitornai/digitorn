//go:build !treesitter

// Package codeast : no-op stub for the default (pure-Go, no-CGO) build.
// Build with `-tags treesitter` for the real AST extraction.
package codeast

type Symbol struct {
	Name  string
	Kind  string
	Start int
	End   int
	Body  string
	Calls []string
}

type FileParse struct {
	Syms    []Symbol
	Imports []string
}

type Chunk struct {
	Path   string
	Symbol string
	Kind   string
	Line   int
	Text   string
}

func Supported(string) bool                     { return false }
func ParseFile(string, []byte) (FileParse, bool) { return FileParse{}, false }
func Chunks(string, []byte) []Chunk              { return nil }
