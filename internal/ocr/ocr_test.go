package ocr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// New is config-driven and nothing is frozen : the backend is selected by name,
// disabled gracefully, and a bad name / missing endpoint is a clear error.
func TestNew_Backends(t *testing.T) {
	if b, err := New(Config{Backend: "none"}); b != nil || err != nil {
		t.Errorf("none → (nil,nil), got (%v,%v)", b, err)
	}
	if b, err := New(Config{}); b != nil || err != nil {
		t.Errorf("empty → (nil,nil) [OCR off], got (%v,%v)", b, err)
	}
	if _, err := New(Config{Backend: "bogus"}); err == nil {
		t.Error("unknown backend must error")
	}
	if _, err := New(Config{Backend: "http"}); err == nil {
		t.Error("http backend without endpoint must error")
	}
	if b, err := New(Config{Backend: "HTTP", Endpoint: "http://x"}); err != nil || b == nil {
		t.Errorf("http+endpoint must build, got (%v,%v)", b, err)
	}
}

// The HTTP backend forwards every config knob (model, languages, key) and reads
// either a JSON envelope or raw text — so the OCR engine choice lives in config.
func TestHTTPBackend(t *testing.T) {
	var gotModel, gotLangs, gotAuth, gotMime string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotModel, gotLangs, gotAuth, gotMime = r.Header.Get("X-OCR-Model"), r.Header.Get("X-OCR-Languages"), r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"text":"Amelie Granger ZQ7-MARKER"}`))
	}))
	defer srv.Close()

	b, err := New(Config{Backend: "http", Endpoint: srv.URL, Model: "paddle-v4", Languages: []string{"eng", "fra"}, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	txt, err := b.OCR(context.Background(), "application/pdf", []byte("<<scanned bytes>>"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(txt, "Granger") || !strings.Contains(txt, "ZQ7-MARKER") {
		t.Fatalf("text not returned: %q", txt)
	}
	if gotModel != "paddle-v4" || gotLangs != "eng,fra" || gotAuth != "Bearer k" || gotMime != "application/pdf" {
		t.Fatalf("config not forwarded: model=%q langs=%q auth=%q mime=%q", gotModel, gotLangs, gotAuth, gotMime)
	}
}

// A raw-text (non-JSON) OCR response is returned as-is.
func TestHTTPBackend_RawText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("plain recognised text"))
	}))
	defer srv.Close()
	b, _ := New(Config{Backend: "http", Endpoint: srv.URL})
	txt, err := b.OCR(context.Background(), "image/png", []byte("x"))
	if err != nil || txt != "plain recognised text" {
		t.Fatalf("raw text: %q err=%v", txt, err)
	}
}

// A non-2xx OCR response surfaces as an error (→ caller keeps the file block).
func TestHTTPBackend_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model busy", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	b, _ := New(Config{Backend: "http", Endpoint: srv.URL})
	if _, err := b.OCR(context.Background(), "application/pdf", []byte("x")); err == nil {
		t.Fatal("503 must surface as an error")
	}
}
