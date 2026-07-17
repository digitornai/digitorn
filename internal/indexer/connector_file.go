package indexer

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/tsawler/tabula"
)

func init() { Register(&fileConnector{}) }

type fileConnector struct{}

func (*fileConnector) Type() string                                         { return "file" }
func (*fileConnector) Capabilities() Caps                                    { return Caps{Walk: true} }
func (*fileConnector) Watch(context.Context, SourceSpec, Sink, Cursor) error { return nil }

var docExts = map[string]bool{
	".pdf": true, ".docx": true, ".xlsx": true, ".pptx": true,
	".html": true, ".htm": true, ".epub": true, ".odt": true,
}

var textExts = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".rst": true, ".csv": true,
	".tsv": true, ".json": true, ".jsonl": true, ".yaml": true, ".yml": true,
	".toml": true, ".xml": true, ".log": true,
	".go": true, ".py": true, ".js": true, ".ts": true, ".tsx": true, ".jsx": true,
	".java": true, ".c": true, ".h": true, ".cpp": true, ".cc": true, ".hpp": true,
	".rs": true, ".rb": true, ".php": true, ".cs": true, ".swift": true, ".kt": true,
	".sh": true, ".bash": true, ".sql": true, ".scala": true, ".lua": true, ".r": true,
}

func supportedFileExt(ext string) bool { return docExts[ext] || textExts[ext] }

type fileOpts struct {
	Path      string
	Recursive bool
	MaxFiles  int
	Allow     map[string]bool
}

func parseFileOpts(opts map[string]any) fileOpts {
	o := fileOpts{Recursive: true, MaxFiles: 100000, Allow: map[string]bool{}}
	o.Path = optString(opts, "path")
	if v, ok := optBool(opts, "recursive"); ok {
		o.Recursive = v
	}
	if v, ok := optInt(opts, "max_files"); ok && v > 0 {
		o.MaxFiles = v
	}
	for _, e := range optStrings(opts, "extensions") {
		o.Allow[strings.ToLower(e)] = true
	}
	return o
}

func (*fileConnector) Walk(_ context.Context, spec SourceSpec, emit func(Document) error) error {
	o := parseFileOpts(spec.Opts)
	if strings.TrimSpace(o.Path) == "" {
		return errNoPath
	}
	info, err := os.Stat(o.Path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		text, format := extractFile(o.Path)
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return emit(Document{ID: filepath.Base(o.Path), Text: text, Meta: map[string]any{"path": o.Path, "format": format}})
	}

	root := filepath.Clean(o.Path)
	count := 0
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if !o.Recursive && path != root {
				return fs.SkipDir
			}
			return nil
		}
		if count >= o.MaxFiles {
			return fs.SkipAll
		}
		ext := strings.ToLower(filepath.Ext(path))
		if !supportedFileExt(ext) || (len(o.Allow) > 0 && !o.Allow[ext]) {
			return nil
		}
		text, format := extractFile(path)
		if strings.TrimSpace(text) == "" {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		count++
		return emit(Document{ID: rel, Text: text, Meta: map[string]any{"path": rel, "format": format}})
	})
}

func extractFile(path string) (text, format string) {
	ext := strings.ToLower(filepath.Ext(path))
	if docExts[ext] {
		defer func() { _ = recover() }()
		e := tabula.Open(path)
		defer e.Close()
		if md, _, err := e.ToMarkdown(); err == nil && strings.TrimSpace(md) != "" {
			return md, strings.TrimPrefix(ext, ".")
		}
		if txt, _, err := e.Text(); err == nil {
			return txt, strings.TrimPrefix(ext, ".")
		}
		return "", ""
	}
	b, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(b) {
		return "", ""
	}
	return string(b), "text"
}
