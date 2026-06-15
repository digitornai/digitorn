package docextract

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

const docxMime = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

func TestSupported(t *testing.T) {
	yes := []string{
		"application/pdf", docxMime,
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"APPLICATION/PDF; charset=binary",
	}
	for _, m := range yes {
		if !Supported(m) {
			t.Errorf("%q should be supported", m)
		}
	}
	no := []string{"image/png", "text/plain", "application/zip", "application/octet-stream", ""}
	for _, m := range no {
		if Supported(m) {
			t.Errorf("%q should NOT be supported", m)
		}
	}
}

func TestExtract_DOCX(t *testing.T) {
	docx := buildDOCX("Amelie Granger is a Senior Rust Engineer. SECRET-TAG ZQ7-MARKER.")
	out, ok := Extract(docxMime, docx)
	if !ok {
		t.Fatal("docx extraction failed")
	}
	if !strings.Contains(out, "Granger") || !strings.Contains(out, "ZQ7-MARKER") {
		t.Fatalf("docx text missing: %q", out)
	}
}

func TestExtract_PDF(t *testing.T) {
	out, ok := Extract("application/pdf", buildMinimalPDF("Amelie Granger Rust ZQ7-MARKER"))
	if !ok {
		t.Fatalf("pdf extraction failed; out=%q", out)
	}
	flat := strings.ReplaceAll(out, " ", "")
	if !strings.Contains(flat, "Granger") || !strings.Contains(flat, "ZQ7-MARKER") {
		t.Fatalf("pdf text missing: out=%q flat=%q", out, flat)
	}
}

// A malformed / unsupported / empty blob must yield ok=false, never a panic.
func TestExtract_GarbageIsSafe(t *testing.T) {
	if _, ok := Extract("application/pdf", []byte("definitely not a pdf")); ok {
		t.Error("garbage pdf must be ok=false")
	}
	if _, ok := Extract("application/pdf", nil); ok {
		t.Error("empty must be ok=false")
	}
	if _, ok := Extract(docxMime, []byte("not a zip")); ok {
		t.Error("garbage docx must be ok=false")
	}
	if _, ok := Extract("image/png", []byte{1, 2, 3}); ok {
		t.Error("unsupported mime must be ok=false")
	}
}

// CachedExtract returns the same result on a hash hit even when the second call
// passes no bytes — proving the content-addressed cache is consulted.
func TestCachedExtract_Caches(t *testing.T) {
	docx := buildDOCX("cached Granger ZQ7-MARKER")
	a, ok1 := CachedExtract("hash-unique-1", docxMime, docx)
	b, ok2 := CachedExtract("hash-unique-1", docxMime, nil) // nil data → only a cache hit can succeed
	if !ok1 || !ok2 || a != b {
		t.Fatalf("cache not consulted: a=%q ok1=%v b=%q ok2=%v", a, ok1, b, ok2)
	}
}

// buildDOCX makes a minimal valid .docx (a zip with word/document.xml).
func buildDOCX(text string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	fmt.Fprintf(w, `<?xml version="1.0"?><w:document xmlns:w="x"><w:body><w:p><w:r><w:t>%s</w:t></w:r></w:p></w:body></w:document>`, text)
	_ = zw.Close()
	return buf.Bytes()
}

// buildMinimalPDF makes a tiny valid PDF whose single page draws `text`, with a
// correct cross-reference table (byte-exact offsets) so a parser can read it.
func buildMinimalPDF(text string) []byte {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		"", // content stream, filled below
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	stream := "BT /F1 24 Tf 72 720 Td (" + text + ") Tj ET\n"
	objs[3] = fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(stream), stream)

	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs)+1)
	for i, body := range objs {
		offsets[i+1] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&b, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF", len(objs)+1, xref)
	return b.Bytes()
}
