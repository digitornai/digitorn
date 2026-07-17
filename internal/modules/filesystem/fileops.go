package filesystem

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	readMaxLineRunes = 2000
	globDefaultCap   = 500
)

type contentKind struct {
	kind  string
	media string
}

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

func numberedSlice(lines []string, start, end int) string {
	var b strings.Builder
	for i := start; i < end; i++ {
		line := lines[i]
		if utf8.RuneCountInString(line) > readMaxLineRunes {
			r := []rune(line)[:readMaxLineRunes]
			line = string(r) + " … [line truncated]"
		}
		fmt.Fprintf(&b, "%d\t%s\n", i+1, line)
	}
	return b.String()
}

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
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmpName, mode)
	if err := os.Rename(tmpName, abs); err != nil {
		return err
	}
	cleanup = false
	if d, e := os.Open(dir); e == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func fileMode(abs string, def os.FileMode) os.FileMode {
	if fi, err := os.Stat(abs); err == nil {
		return fi.Mode().Perm()
	}
	return def
}
