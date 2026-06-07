package rag

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSupportedExt(t *testing.T) {
	for _, ext := range []string{".pdf", ".html", ".docx", ".md", ".go", ".csv", ".json"} {
		if !SupportedExt(ext) {
			t.Errorf("%s should be supported", ext)
		}
	}
	for _, ext := range []string{".exe", ".png", ".zip"} {
		if SupportedExt(ext) {
			t.Errorf("%s should NOT be supported", ext)
		}
	}
}

func TestLoadFile_Text(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(p, []byte("# Title\n\nbody text"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFile(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(got.Text, "body text") {
		t.Errorf("text = %q", got.Text)
	}
}

func TestLoadFile_HTML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "page.html")
	html := `<html><body><h1>Heading</h1><p>Para with <a href="x">link</a>.</p></body></html>`
	if err := os.WriteFile(p, []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFile(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(got.Text, "Heading") || !strings.Contains(got.Text, "Para") {
		t.Errorf("markdown = %q", got.Text)
	}
}

func TestLoadFile_DOCX(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "doc.docx")
	writeMinimalDocx(t, p, "Hello from docx")
	got, err := LoadFile(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(got.Text, "Hello from docx") {
		t.Errorf("docx text = %q", got.Text)
	}
}

func writeMinimalDocx(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	doc := `<?xml version="1.0"?><w:document xmlns:w="x"><w:body><w:p><w:r><w:t>` +
		text + `</w:t></w:r></w:p></w:body></w:document>`
	if _, err := w.Write([]byte(doc)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}
