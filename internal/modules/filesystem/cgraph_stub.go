//go:build !treesitter

package filesystem

// astChunks is the no-tree-sitter stub : returns nil so sindex falls back
// to line-window chunking. Build with `-tags treesitter` (CGO) for the
// AST-aware, symbol-level code chunks.
func astChunks(path string, src []byte) []sChunk { return nil }

// codeContextFor is the no-tree-sitter stub : no dependency graph, so grep
// gets no symbol/caller/import context. Build with `-tags treesitter`.
func codeContextFor(root string, maxBytes int64, path string, line int) symContext {
	return symContext{}
}
