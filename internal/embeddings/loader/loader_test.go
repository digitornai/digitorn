package loader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestEnsure_DownloadsAndCaches(t *testing.T) {
	const body = "fake-onnx-bytes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	dir := t.TempDir()
	files := []File{{Name: "model.onnx", URL: srv.URL + "/model.onnx", SHA: ""}}
	if err := Ensure(context.Background(), dir, files, nil); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "model.onnx"))
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if string(got) != body {
		t.Fatalf("content = %q, want %q", got, body)
	}
}

func TestEnsure_RejectsOversizedContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Advertise a body far beyond the ceiling but send nothing — the
		// loader must refuse on the Content-Length alone, never start writing.
		w.Header().Set("Content-Length", strconv.FormatInt(maxModelBytes+1, 10))
		w.WriteHeader(200)
	}))
	defer srv.Close()

	dir := t.TempDir()
	files := []File{{Name: "model.onnx", URL: srv.URL + "/model.onnx"}}
	if err := Ensure(context.Background(), dir, files, nil); err == nil {
		t.Fatal("expected rejection of oversized Content-Length, got nil")
	}
	if _, err := os.Stat(filepath.Join(dir, "model.onnx")); !os.IsNotExist(err) {
		t.Fatalf("oversized download must not land on disk : stat err=%v", err)
	}
}

func TestEnsure_HTTPErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	files := []File{{Name: "model.onnx", URL: srv.URL + "/model.onnx"}}
	if err := Ensure(context.Background(), dir, files, nil); err == nil {
		t.Fatal("expected error on HTTP 500, got nil")
	}
}

func TestEnsure_SkipsAlreadyPresent(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "model.onnx"), []byte("already-here"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := []File{{Name: "model.onnx", URL: srv.URL + "/model.onnx"}} // empty SHA → presence is enough
	if err := Ensure(context.Background(), dir, files, nil); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if hits != 0 {
		t.Fatalf("expected 0 HTTP hits for a present file, got %d", hits)
	}
}
