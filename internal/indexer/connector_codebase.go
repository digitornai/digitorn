//go:build treesitter

package indexer

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/digitornai/digitorn/internal/codeast"
)

func init() { Register(&codebaseConnector{}) }

// codebaseConnector indexes a code repository for semantic code search :
// each source file is split into symbol-level chunks (func/method/type/
// class/interface) via the shared codeast tree-sitter layer — the same
// machinery that powers grep enrichment + the repo map. One chunk → one
// document keyed by path#line. Walk-only for now (fsnotify watch later).
// Only built with `-tags treesitter` (CGO); absent in the pure-Go build.
type codebaseConnector struct{}

func (*codebaseConnector) Type() string                                          { return "codebase" }
func (*codebaseConnector) Capabilities() Caps                                     { return Caps{Walk: true} }
func (*codebaseConnector) Watch(context.Context, SourceSpec, Sink, Cursor) error { return nil }

var codebaseIgnoreDirs = map[string]bool{
	"node_modules": true, "vendor": true, ".git": true, "dist": true,
	"build": true, "target": true, ".venv": true, "__pycache__": true, ".next": true,
}

func (*codebaseConnector) Walk(_ context.Context, spec SourceSpec, emit func(Document) error) error {
	root := optString(spec.Opts, "path")
	if strings.TrimSpace(root) == "" {
		return errNoPath
	}
	maxBytes := int64(1 << 20)
	if v, ok := optInt(spec.Opts, "max_file_bytes"); ok && v > 0 {
		maxBytes = int64(v)
	}
	symbolLevel := true
	if v, ok := optBool(spec.Opts, "symbol_chunks"); ok {
		symbolLevel = v
	}
	root = filepath.Clean(root)

	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			n := d.Name()
			if path != root && (strings.HasPrefix(n, ".") || codebaseIgnoreDirs[n]) {
				return fs.SkipDir
			}
			return nil
		}
		if !codeast.Supported(filepath.Ext(path)) {
			return nil
		}
		if info, e := d.Info(); e != nil || (maxBytes > 0 && info.Size() > maxBytes) {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil || !utf8.Valid(b) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		if symbolLevel {
			if chs := codeast.Chunks(rel, b); len(chs) > 0 {
				for _, c := range chs {
					doc := Document{
						ID:   rel + "#" + strconv.Itoa(c.Line),
						Text: c.Text,
						Meta: map[string]any{"path": rel, "symbol": c.Symbol, "kind": c.Kind, "line": c.Line},
					}
					if err := emit(doc); err != nil {
						return err
					}
				}
				return nil
			}
		}
		// whole-file fallback (unparsed or symbol_chunks=false)
		return emit(Document{ID: rel, Text: string(b), Meta: map[string]any{"path": rel}})
	})
}
