// Package docextract turns a binary document blob (PDF, DOCX, PPTX, XLSX) into
// plain text so it reaches EVERY model as readable content — the same primitive
// that already handles text/* attachments, extended to the document formats a
// user actually attaches (a CV, a spec, a spreadsheet). It is best-effort and
// CRASH-SAFE: a malformed or unsupported file yields ("", false), never a panic,
// so a bad attachment can never break a turn.
package docextract

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"strings"

	"github.com/ledongthuc/pdf"
)

// Supported reports whether Extract can handle a mime type — a cheap pre-check
// so the caller skips loading bytes it can't use.
func Supported(mime string) bool { return kind(mime) != "" }

func kind(mime string) string {
	switch normalizeMime(mime) {
	case "application/pdf":
		return "pdf"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return "docx"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return "pptx"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return "xlsx"
	}
	return ""
}

func normalizeMime(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	return m
}

// Extract returns the document's plain text and ok=true on success. Best-effort
// and crash-safe (recovers from a panicking parser on a malformed file).
func Extract(mime string, data []byte) (text string, ok bool) {
	defer func() {
		if recover() != nil {
			text, ok = "", false
		}
	}()
	if len(data) == 0 {
		return "", false
	}
	var t string
	switch kind(mime) {
	case "pdf":
		t = extractPDF(data)
	case "docx", "pptx", "xlsx":
		t = extractOOXML(data)
	default:
		return "", false
	}
	t = strings.TrimSpace(t)
	return t, t != ""
}

func extractPDF(data []byte) string {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return ""
	}
	tr, err := r.GetPlainText()
	if err != nil {
		return ""
	}
	var b bytes.Buffer
	if _, err := io.Copy(&b, tr); err != nil {
		return ""
	}
	return b.String()
}

// extractOOXML pulls every text node from the content parts of an OOXML zip
// (docx body, pptx slides + notes, xlsx shared strings + sheets). Tag-agnostic:
// it concatenates character data, which is enough for a model to read the file.
func extractOOXML(data []byte) string {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, f := range zr.File {
		name := strings.ToLower(f.Name)
		if !strings.HasSuffix(name, ".xml") || !isContentPart(name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		dec := xml.NewDecoder(rc)
		for {
			tok, terr := dec.Token()
			if terr != nil {
				break
			}
			if cd, isCD := tok.(xml.CharData); isCD {
				if s := strings.TrimSpace(string(cd)); s != "" {
					b.WriteString(s)
					b.WriteByte(' ')
				}
			}
		}
		rc.Close()
	}
	return b.String()
}

// isContentPart keeps the OOXML parts that carry user text, skipping rels,
// theme, and property noise that would pollute the extracted output.
func isContentPart(name string) bool {
	switch {
	case strings.HasPrefix(name, "word/document"),
		strings.HasPrefix(name, "word/header"),
		strings.HasPrefix(name, "word/footer"),
		strings.HasPrefix(name, "ppt/slides/slide"),
		strings.HasPrefix(name, "ppt/notesslides/notesslide"),
		name == "xl/sharedstrings.xml",
		strings.HasPrefix(name, "xl/worksheets/sheet"):
		return true
	}
	return false
}

// CachedExtract / the content-addressed cache + OCR fallback live in service.go
// (the default Service). docextract.Extract above stays the pure text-layer core.
