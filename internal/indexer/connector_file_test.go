package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileConnector_Walk(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("notes.txt", "plain deployment notes about the production server")
	write("guide.md", "# Guide\nhow to deploy an application")
	write("page.html", "<html><head><title>Deploy</title></head><body><h1>Deployment</h1><p>Deploy the application to the production server.</p></body></html>")
	write("skip.bin", "\x00\x01\x02 binary not text")

	conn := &fileConnector{}
	spec := SourceSpec{Name: "docs", Type: "file", KB: "kb", Opts: map[string]any{"path": dir}}
	docs := map[string]Document{}
	if err := conn.Walk(context.Background(), spec, func(d Document) error {
		docs[d.ID] = d
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}

	for _, want := range []string{"notes.txt", "guide.md", "page.html"} {
		if _, ok := docs[want]; !ok {
			t.Errorf("missing %q (got %v)", want, keysOf(docs))
		}
	}
	if _, indexed := docs["skip.bin"]; indexed {
		t.Error("binary file should be skipped")
	}
	if h, ok := docs["page.html"]; ok {
		if !strings.Contains(strings.ToLower(h.Text), "deploy") {
			t.Errorf("html extraction lost content: %q", h.Text)
		}
		if h.Meta["format"] != "html" {
			t.Errorf("html format meta = %v", h.Meta["format"])
		}
	}
}

func keysOf(m map[string]Document) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
