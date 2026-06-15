package docextract

import (
	"context"
	"strings"
	"sync"
	"time"
)

// OCRBackend recognises text from a SCANNED document (a PDF/image with no text
// layer). It is satisfied by the pluggable, config-selected backends in
// internal/ocr — docextract never knows which engine runs. Defined here (not
// imported) so this package keeps no dependency on the OCR implementations.
type OCRBackend interface {
	OCR(ctx context.Context, mime string, data []byte) (string, error)
}

// Service turns a document blob into text : the synchronous text-layer core
// (Extract) first, then — only if that is empty and the mime is a scan-prone
// document — an ASYNCHRONOUS OCR fallback via the configured backend. Results are
// content-addressed cached (a blob hash always yields the same text), so OCR runs
// at most once per document and NEVER on the turn loop : the turn that first sees
// a scanned doc gets the file block, a later turn gets the OCR text.
type Service struct {
	mu      sync.Mutex
	cache   map[string]string   // hash → text ("" = extracted to nothing / OCR failed)
	pending map[string]struct{} // hash → OCR in flight (dedup)
	ocr     OCRBackend
	timeout time.Duration
}

const cacheCap = 256

// NewService builds a Service. ocr=nil disables the OCR fallback (a scanned doc
// then keeps its file block — fully graceful). timeout<=0 defaults to 60s.
func NewService(ocr OCRBackend, timeout time.Duration) *Service {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Service{
		cache:   make(map[string]string),
		pending: make(map[string]struct{}),
		ocr:     ocr,
		timeout: timeout,
	}
}

// SetOCR swaps the OCR backend (used by bootstrap once config is resolved).
func (s *Service) SetOCR(b OCRBackend) {
	s.mu.Lock()
	s.ocr = b
	s.mu.Unlock()
}

// Extract returns the document text and ok=true when available. The text-layer
// path is synchronous and fast; the OCR path is fired in the background and its
// result is read on a subsequent call (cache hit). Signature matches the adapter
// ExtractDoc hook.
func (s *Service) Extract(hash, mime string, data []byte) (string, bool) {
	if hash != "" {
		s.mu.Lock()
		if t, hit := s.cache[hash]; hit {
			s.mu.Unlock()
			return t, t != ""
		}
		s.mu.Unlock()
	}
	if t, ok := Extract(mime, data); ok { // text layer (PDF text, OOXML)
		s.store(hash, t)
		return t, true
	}
	// No text layer : OCR fallback (async) for scan-prone documents.
	s.mu.Lock()
	hasOCR := s.ocr != nil
	s.mu.Unlock()
	if hasOCR && ocrEligible(mime) {
		s.enqueueOCR(hash, mime, data)
	}
	return "", false // file block this turn; OCR text on a later one
}

func (s *Service) store(hash, text string) {
	if hash == "" {
		return
	}
	s.mu.Lock()
	if len(s.cache) >= cacheCap {
		s.cache = make(map[string]string) // crude bound; content-addressed → no stale risk
	}
	s.cache[hash] = text
	s.mu.Unlock()
}

// enqueueOCR runs the backend once per hash (deduped), caching the result —
// including "" on failure (a negative cache, so a bad scan isn't retried forever).
func (s *Service) enqueueOCR(hash, mime string, data []byte) {
	s.mu.Lock()
	if hash != "" {
		if _, busy := s.pending[hash]; busy {
			s.mu.Unlock()
			return
		}
		s.pending[hash] = struct{}{}
	}
	ocr, timeout := s.ocr, s.timeout
	s.mu.Unlock()
	if ocr == nil {
		return
	}

	cp := make([]byte, len(data)) // copy : the adapter may reuse the buffer
	copy(cp, data)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		text, err := ocr.OCR(ctx, mime, cp)
		if err != nil {
			text = ""
		}
		s.store(hash, strings.TrimSpace(text))
		if hash != "" {
			s.mu.Lock()
			delete(s.pending, hash)
			s.mu.Unlock()
		}
	}()
}

// ocrEligible reports whether a mime is worth OCR'ing when it has no text layer.
// PDFs (a scan = images of text) and images qualify; OOXML files with no text are
// genuinely empty, not scanned, so they are excluded.
func ocrEligible(mime string) bool {
	m := normalizeMime(mime)
	return m == "application/pdf" || strings.HasPrefix(m, "image/")
}

// defaultService backs the package-level CachedExtract / SetOCR so the engine
// wiring (adapter ExtractDoc) needs no change : bootstrap just calls SetOCR.
var defaultService = NewService(nil, 0)

// SetOCR installs the process-wide OCR backend (bootstrap, once).
func SetOCR(b OCRBackend) { defaultService.SetOCR(b) }

// CachedExtract is the package entry the adapter wires to ExtractDoc : text layer
// + content-addressed cache + (if configured) the async OCR fallback.
func CachedExtract(hash, mime string, data []byte) (string, bool) {
	return defaultService.Extract(hash, mime, data)
}
