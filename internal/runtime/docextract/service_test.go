package docextract

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubOCR records how many times it ran and returns a fixed text.
type stubOCR struct {
	calls int32
	text  string
}

func (s *stubOCR) OCR(_ context.Context, _ string, _ []byte) (string, error) {
	atomic.AddInt32(&s.calls, 1)
	return s.text, nil
}

// A scanned PDF (no text layer) triggers the ASYNC OCR fallback : the first call
// returns nothing (file block), a later call returns the OCR text from cache.
// OCR is never run on the calling goroutine and runs at most once per hash.
func TestService_OCRFallbackAsync(t *testing.T) {
	ocr := &stubOCR{text: "Amelie Granger Rust ZQ7-MARKER"}
	s := NewService(ocr, 5*time.Second)

	scanned := []byte("%PDF-1.4 no extractable text") // docextract.Extract → empty

	// First call: text layer empty → returns false, OCR enqueued in background.
	if txt, ok := s.Extract("h1", "application/pdf", scanned); ok || txt != "" {
		t.Fatalf("first call must be empty (file block), got %q ok=%v", txt, ok)
	}

	// Poll for the async OCR result to land in the cache.
	var got string
	for i := 0; i < 50; i++ {
		if txt, ok := s.Extract("h1", "application/pdf", scanned); ok {
			got = txt
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(got, "Granger") || !strings.Contains(got, "ZQ7-MARKER") {
		t.Fatalf("OCR text not surfaced after async run: %q", got)
	}

	// A few more reads must NOT re-run OCR (cache hit + dedup).
	for i := 0; i < 5; i++ {
		s.Extract("h1", "application/pdf", scanned)
	}
	if c := atomic.LoadInt32(&ocr.calls); c != 1 {
		t.Fatalf("OCR ran %d times, want exactly 1 (cached/deduped)", c)
	}
}

// With no OCR backend, a scanned PDF stays a file block (graceful) — and OCR is
// never attempted.
func TestService_NoBackendGraceful(t *testing.T) {
	s := NewService(nil, 0)
	if txt, ok := s.Extract("h2", "application/pdf", []byte("%PDF no text")); ok || txt != "" {
		t.Fatalf("no backend → file block, got %q ok=%v", txt, ok)
	}
}

// The text-layer path is unaffected: a real DOCX is extracted synchronously and
// OCR is never consulted.
func TestService_TextLayerWins(t *testing.T) {
	ocr := &stubOCR{text: "should-not-be-used"}
	s := NewService(ocr, time.Second)
	docx := buildDOCX("Hello Granger ZQ7-MARKER")
	txt, ok := s.Extract("h3", docxMime, docx)
	if !ok || !strings.Contains(txt, "Granger") {
		t.Fatalf("text layer must win: %q ok=%v", txt, ok)
	}
	if atomic.LoadInt32(&ocr.calls) != 0 {
		t.Fatal("OCR must NOT run when the text layer succeeds")
	}
}

// ocrEligible: PDFs + images, never OOXML (genuinely-empty ≠ scanned).
func TestOCREligible(t *testing.T) {
	for _, m := range []string{"application/pdf", "image/png", "image/jpeg", "APPLICATION/PDF; x=1"} {
		if !ocrEligible(m) {
			t.Errorf("%q should be OCR-eligible", m)
		}
	}
	for _, m := range []string{docxMime, "text/plain", "application/zip"} {
		if ocrEligible(m) {
			t.Errorf("%q should NOT be OCR-eligible", m)
		}
	}
}
