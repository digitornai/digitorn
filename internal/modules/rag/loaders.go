package rag

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	htmltomd "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/ledongthuc/pdf"
)

// Loaded is extracted document content plus format-derived metadata.
type Loaded struct {
	Text string
	Meta map[string]any
}

// textExtensions are read verbatim as UTF-8 (markdown, code, data, config).
var textExtensions = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".rst": true, ".csv": true,
	".tsv": true, ".json": true, ".jsonl": true, ".yaml": true, ".yml": true,
	".toml": true, ".xml": true, ".log": true,
	".go": true, ".py": true, ".js": true, ".ts": true, ".tsx": true, ".jsx": true,
	".java": true, ".c": true, ".h": true, ".cpp": true, ".cc": true, ".hpp": true,
	".rs": true, ".rb": true, ".php": true, ".cs": true, ".swift": true, ".kt": true,
	".sh": true, ".bash": true, ".sql": true, ".scala": true, ".lua": true, ".r": true,
}

// SupportedExt reports whether LoadFile can extract this extension.
func SupportedExt(ext string) bool {
	ext = strings.ToLower(ext)
	switch ext {
	case ".pdf", ".html", ".htm", ".docx":
		return true
	}
	return textExtensions[ext]
}

// LoadFile extracts text from a file by extension : text-native formats
// pass through as UTF-8, HTML is converted to markdown, PDF/DOCX are
// parsed to plain text.
func LoadFile(path string) (Loaded, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pdf":
		return loadPDF(path)
	case ".html", ".htm":
		return loadHTML(path)
	case ".docx":
		return loadDOCX(path)
	default:
		b, err := os.ReadFile(path)
		if err != nil {
			return Loaded{}, err
		}
		if !utf8.Valid(b) {
			return Loaded{}, fmt.Errorf("rag: %s is not UTF-8 text", filepath.Base(path))
		}
		return Loaded{Text: string(b), Meta: map[string]any{"format": "text"}}, nil
	}
}

func loadHTML(path string) (Loaded, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Loaded{}, err
	}
	md, err := htmltomd.ConvertString(string(b))
	if err != nil {
		return Loaded{}, fmt.Errorf("rag: html %s: %w", filepath.Base(path), err)
	}
	return Loaded{Text: md, Meta: map[string]any{"format": "html"}}, nil
}

func loadPDF(path string) (Loaded, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return Loaded{}, fmt.Errorf("rag: pdf %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	tr, err := r.GetPlainText()
	if err != nil {
		return Loaded{}, fmt.Errorf("rag: pdf %s: %w", filepath.Base(path), err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, tr); err != nil {
		return Loaded{}, fmt.Errorf("rag: pdf %s: %w", filepath.Base(path), err)
	}
	return Loaded{Text: buf.String(), Meta: map[string]any{"format": "pdf", "pages": r.NumPage()}}, nil
}

// loadDOCX extracts the text of a .docx (a zip of XML) without a heavy
// dependency : read word/document.xml and concatenate the <w:t> runs,
// breaking paragraphs on <w:p>.
func loadDOCX(path string) (Loaded, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return Loaded{}, fmt.Errorf("rag: docx %s: %w", filepath.Base(path), err)
	}
	defer zr.Close()
	for _, zf := range zr.File {
		if zf.Name != "word/document.xml" {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return Loaded{}, err
		}
		defer rc.Close()
		text, err := docxText(rc)
		if err != nil {
			return Loaded{}, fmt.Errorf("rag: docx %s: %w", filepath.Base(path), err)
		}
		return Loaded{Text: text, Meta: map[string]any{"format": "docx"}}, nil
	}
	return Loaded{}, fmt.Errorf("rag: docx %s: no word/document.xml", filepath.Base(path))
}

func docxText(r io.Reader) (string, error) {
	dec := xml.NewDecoder(r)
	var b strings.Builder
	inText := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "t" {
				inText = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				b.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				b.Write(t)
			}
		}
	}
	return strings.TrimSpace(b.String()), nil
}
