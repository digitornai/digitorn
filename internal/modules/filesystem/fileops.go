package filesystem

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// fileops.go : the building blocks behind read / write — content-kind detection
// (so the agent never gets a wall of binary bytes) and crash-safe atomic writes.
// The glob matcher lives in glob.go.

const (
	readMaxLineRunes = 2000 // a single line longer than this is clipped in read output
	globDefaultCap   = 1000 // glob result cap before truncation is signalled
)

// contentKind classifies a file from its leading bytes + name so read can return
// a useful descriptor for non-text content instead of dumping raw bytes.
type contentKind struct {
	kind  string // "text" | "binary" | "image" | "pdf"
	media string // media type for image/pdf, "" otherwise
}

// detectKind inspects up to the first few KiB of a file. Magic numbers win over
// extension so a mislabelled file is still classified correctly.
func detectKind(head []byte) contentKind {
	switch {
	case bytes.HasPrefix(head, []byte("%PDF-")):
		return contentKind{"pdf", "application/pdf"}
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return contentKind{"image", "image/png"}
	case bytes.HasPrefix(head, []byte("\xff\xd8\xff")):
		return contentKind{"image", "image/jpeg"}
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return contentKind{"image", "image/gif"}
	case len(head) >= 12 && bytes.Equal(head[0:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return contentKind{"image", "image/webp"}
	case bytes.HasPrefix(head, []byte("BM")):
		return contentKind{"image", "image/bmp"}
	case isBinary(head):
		return contentKind{"binary", ""}
	default:
		return contentKind{"text", ""}
	}
}

// splitLines splits file content into logical lines, dropping the artificial
// trailing empty element a final newline produces ("a\nb\n" → ["a","b"]).
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// numberedSlice renders lines[start:end] (0-based, end-exclusive) in cat -n style
// with right-aligned 1-based line numbers, clipping any over-long line so a
// minified bundle can't blow the context window.
func numberedSlice(lines []string, start, end int) string {
	width := len(fmt.Sprintf("%d", end)) // align to the largest number shown
	if width < 4 {
		width = 4
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		line := lines[i]
		if utf8.RuneCountInString(line) > readMaxLineRunes {
			r := []rune(line)[:readMaxLineRunes]
			line = string(r) + " … [line truncated]"
		}
		fmt.Fprintf(&b, "%*d\t%s\n", width, i+1, line)
	}
	return b.String()
}

// atomicWrite writes data to abs crash-safely : a sibling temp file is fully
// written + fsync'd, then atomically renamed over the target (Go's os.Rename uses
// MoveFileEx/REPLACE_EXISTING on Windows and rename(2) on unix — both atomic). A
// crash or power loss therefore leaves either the old file or the new one, never
// a half-written one. The parent dir is fsync'd best-effort for rename durability.
func atomicWrite(abs string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(abs)
	tmp, err := os.CreateTemp(dir, ".dgt-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil { // durable on disk before the rename
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmpName, mode) // best-effort : preserve the intended permissions
	if err := os.Rename(tmpName, abs); err != nil {
		return err
	}
	cleanup = false // renamed into place : nothing to remove
	if d, e := os.Open(dir); e == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// fileMode returns the existing file's mode (so a rewrite preserves permissions),
// or def for a new file.
func fileMode(abs string, def os.FileMode) os.FileMode {
	if fi, err := os.Stat(abs); err == nil {
		return fi.Mode().Perm()
	}
	return def
}
