package adapter

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

func TestIsTextualMime(t *testing.T) {
	textual := []string{
		"text/plain", "text/markdown", "text/csv", "TEXT/HTML",
		"text/plain; charset=utf-8", "application/json", "application/x-yaml",
		"application/xml", "application/javascript",
	}
	for _, m := range textual {
		if !isTextualMime(m) {
			t.Errorf("%q should be textual", m)
		}
	}
	binary := []string{
		"image/png", "audio/mpeg", "video/mp4", "application/pdf",
		"application/octet-stream", "application/zip",
	}
	for _, m := range binary {
		if isTextualMime(m) {
			t.Errorf("%q should NOT be textual", m)
		}
	}
}

func textFilePart(mime string) sessionstore.MessagePart {
	return sessionstore.MessagePart{Type: sessionstore.PartTypeFile, Blob: &sessionstore.BlobRef{Hash: "h", Mime: mime}}
}

func bytesOpts(data []byte) Options {
	return Options{LoadBlob: func(context.Context, string) ([]byte, error) { return data, nil }}
}

// THE regression guard : a text-document attachment must reach EVERY model as a
// readable TEXT content part, never an opaque "file" block (the bug that made a
// vision-less model ignore an attached CV).
func TestLoadBinaryPart_TextBecomesReadableText(t *testing.T) {
	cv := "CANDIDATE: Amelie Granger. ROLE: Rust engineer. SECRET-TAG: ZQ7."
	cp, ok := loadBinaryPart(context.Background(), textFilePart("text/plain"), bytesOpts([]byte(cv)))
	if !ok {
		t.Fatal("expected ok")
	}
	if cp.Type != llm.ContentTypeText {
		t.Fatalf("want a TEXT content part, got %q", cp.Type)
	}
	if !strings.Contains(cp.Text, "Amelie Granger") || !strings.Contains(cp.Text, "ZQ7") {
		t.Fatalf("attachment text not inlined: %q", cp.Text)
	}
	if !strings.Contains(cp.Text, "[Attached document") {
		t.Errorf("missing labelled delimiter: %q", cp.Text)
	}
}

// A binary DOCUMENT (PDF, DOCX…) is extracted to a readable text block when an
// ExtractDoc hook is wired — so every model reads the document, not an opaque
// file block. A non-document binary (image) is left untouched.
func TestLoadBinaryPart_DocumentExtractedToText(t *testing.T) {
	opts := bytesOpts([]byte("%PDF-fake-bytes"))
	opts.ExtractDoc = func(hash, mime string, data []byte) (string, bool) {
		if mime == "application/pdf" {
			return "Amelie Granger — Rust — ZQ7-MARKER", true
		}
		return "", false
	}
	cp, ok := loadBinaryPart(context.Background(), textFilePart("application/pdf"), opts)
	if !ok {
		t.Fatal("expected ok")
	}
	if cp.Type != llm.ContentTypeText {
		t.Fatalf("a PDF must become a TEXT block when extractable, got %q", cp.Type)
	}
	if !strings.Contains(cp.Text, "Granger") || !strings.Contains(cp.Text, "ZQ7-MARKER") {
		t.Fatalf("extracted text not inlined: %q", cp.Text)
	}

	// An image is NOT a document : ExtractDoc returns false → native image block.
	img := sessionstore.MessagePart{Type: sessionstore.PartTypeImage, Blob: &sessionstore.BlobRef{Hash: "h", Mime: "image/png"}}
	icp, _ := loadBinaryPart(context.Background(), img, opts)
	if icp.Type != llm.ContentTypeImage {
		t.Fatalf("image must stay an image block, got %q", icp.Type)
	}
}

// A genuinely binary attachment keeps its native block (vision / audio / file).
func TestLoadBinaryPart_BinaryKeepsNativeBlock(t *testing.T) {
	p := sessionstore.MessagePart{Type: sessionstore.PartTypeImage, Blob: &sessionstore.BlobRef{Hash: "h", Mime: "image/png"}}
	cp, ok := loadBinaryPart(context.Background(), p, bytesOpts([]byte{0x89, 0x50, 0x4e, 0x47}))
	if !ok {
		t.Fatal("expected ok")
	}
	if cp.Type != llm.ContentTypeImage {
		t.Fatalf("image must stay an image block, got %q", cp.Type)
	}
}

// A text MIME whose bytes are NOT valid UTF-8 falls back to a file block — we
// never mangle binary that was mislabeled as text.
func TestLoadBinaryPart_InvalidUTF8FallsBack(t *testing.T) {
	cp, ok := loadBinaryPart(context.Background(), textFilePart("text/plain"), bytesOpts([]byte{0xff, 0xfe, 0x00, 0x01}))
	if !ok {
		t.Fatal("expected ok")
	}
	if cp.Type == llm.ContentTypeText {
		t.Fatal("invalid-UTF8 text MIME must NOT become a text block")
	}
}

// Oversize text is truncated on a rune boundary with a visible marker so a huge
// document can't blow the context window.
func TestInlineTextAttachment_Truncates(t *testing.T) {
	big := strings.Repeat("é", maxInlinedTextBytes) // 2 bytes each → well over the cap
	out := inlineTextAttachment("text/plain", []byte(big))
	if !strings.Contains(out, "truncated") {
		t.Fatal("expected a truncation marker")
	}
	if len(out) > maxInlinedTextBytes+256 {
		t.Fatalf("content was not truncated: %d bytes", len(out))
	}
	if !utf8.ValidString(out) {
		t.Fatal("truncation broke a UTF-8 rune boundary")
	}
}
