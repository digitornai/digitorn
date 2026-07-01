package filesystem

import (
	"bytes"
	"os"
	"testing"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

// pngBytes is a PNG signature + filler — enough for detectKind to classify it
// image/png. Written to disk raw (binary can't survive the JSON write tool).
var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), []byte("....IHDR fake pixels....")...)

func TestRead_Image_EmitsVisionPart(t *testing.T) {
	m, ctx := hardenModule(t)
	abs, err := m.resolveCtx(ctx, "pic.png")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := os.WriteFile(abs, pngBytes, 0o644); err != nil {
		t.Fatalf("write png: %v", err)
	}

	r, err := m.read(ctx, mustJSON(map[string]any{"path": "pic.png"}))
	if err != nil || !r.Success {
		t.Fatalf("read image failed: %v %v", err, r.Error)
	}
	// Must carry a vision OutputPart with the exact bytes + mime, plus a text note.
	var img *tool.OutputPart
	var hasText bool
	for i := range r.OutputParts {
		switch r.OutputParts[i].Kind {
		case tool.OutputImage:
			img = &r.OutputParts[i]
		case tool.OutputText:
			hasText = true
		}
	}
	if img == nil {
		t.Fatalf("read of a PNG must emit an image OutputPart; got %+v", r.OutputParts)
	}
	if !hasText {
		t.Fatalf("read should also emit a text note alongside the image")
	}
	if img.Mime != "image/png" {
		t.Fatalf("mime = %q, want image/png", img.Mime)
	}
	if !bytes.Equal(img.Bytes, pngBytes) {
		t.Fatalf("image bytes not passed through verbatim")
	}
}

func TestRead_Image_TooLargeFallsBackToText(t *testing.T) {
	m, ctx := hardenModule(t)
	abs, _ := m.resolveCtx(ctx, "big.png")
	big := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("x"), maxVisionBytes+10)...)
	if err := os.WriteFile(abs, big, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	r, err := m.read(ctx, mustJSON(map[string]any{"path": "big.png"}))
	if err != nil || !r.Success {
		t.Fatalf("read failed: %v %v", err, r.Error)
	}
	if len(r.OutputParts) != 0 {
		t.Fatalf("an oversized image must NOT be shipped inline; got %d parts", len(r.OutputParts))
	}
	if !bytes.Contains([]byte(r.Data.(string)), []byte("too large")) {
		t.Fatalf("oversized image should be reported as text: %v", r.Data)
	}
}
