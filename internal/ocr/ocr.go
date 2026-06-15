// Package ocr is the PLUGGABLE OCR layer for scanned documents. Nothing about the
// engine is frozen: a Backend is chosen by config ("http" today; in-process
// engines plug in behind the same interface), and every backend-specific knob
// (model, endpoint, languages, credentials) is config. The runtime calls a
// Backend only as an async, content-addressed fallback when a document has no
// text layer — never on the turn loop.
package ocr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Backend extracts text from a document's bytes. Implementations are selected by
// config and are interchangeable — the caller never knows which engine ran.
type Backend interface {
	// OCR returns the recognised text. An error (or empty text) makes the caller
	// keep the document's native file block, so a failing engine degrades
	// gracefully rather than breaking a turn.
	OCR(ctx context.Context, mime string, data []byte) (string, error)
}

// Config is the resolved OCR configuration (mirrors config.OCR, decoupled so the
// runtime doesn't import the daemon config package).
type Config struct {
	Backend   string
	Model     string
	Endpoint  string
	APIKey    string
	Languages []string
	MaxPages  int
	Timeout   time.Duration
	Headers   map[string]string
}

// New builds the configured Backend. A blank / "none" backend returns (nil, nil)
// — OCR disabled, fully graceful. An unknown backend is a configuration error.
func New(cfg Config) (Backend, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Backend)) {
	case "", "none", "off", "disabled":
		return nil, nil
	case "http":
		if strings.TrimSpace(cfg.Endpoint) == "" {
			return nil, errors.New("ocr: backend \"http\" requires workers.ocr.endpoint")
		}
		return newHTTP(cfg), nil
	default:
		return nil, fmt.Errorf("ocr: unknown backend %q (supported: none, http)", cfg.Backend)
	}
}

// httpBackend POSTs the raw document to a configurable OCR endpoint and reads the
// text back. Point it at any service: a local OCR microservice, a cloud API, a
// sidecar wrapping Tesseract/PaddleOCR/TrOCR. Model, languages, key and extra
// headers are all forwarded from config, so the engine choice lives in YAML.
type httpBackend struct {
	endpoint string
	model    string
	apiKey   string
	langs    []string
	headers  map[string]string
	client   *http.Client
}

func newHTTP(cfg Config) *httpBackend {
	to := cfg.Timeout
	if to <= 0 {
		to = 60 * time.Second
	}
	return &httpBackend{
		endpoint: cfg.Endpoint,
		model:    cfg.Model,
		apiKey:   cfg.APIKey,
		langs:    cfg.Languages,
		headers:  cfg.Headers,
		client:   &http.Client{Timeout: to},
	}
}

func (b *httpBackend) OCR(ctx context.Context, mime string, data []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mime)
	if b.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
	}
	if b.model != "" {
		req.Header.Set("X-OCR-Model", b.model)
	}
	if len(b.langs) > 0 {
		req.Header.Set("X-OCR-Languages", strings.Join(b.langs, ","))
	}
	for k, v := range b.headers {
		req.Header.Set(k, v)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("ocr: http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return parseText(body), nil
}

// parseText accepts either a JSON envelope ({"text": "..."}) or raw text, so the
// backend works with the broadest set of OCR services without per-service code.
func parseText(body []byte) string {
	if t := strings.TrimSpace(string(body)); strings.HasPrefix(t, "{") {
		var env struct {
			Text   string `json:"text"`
			Result string `json:"result"`
		}
		if json.Unmarshal(body, &env) == nil {
			if env.Text != "" {
				return env.Text
			}
			if env.Result != "" {
				return env.Result
			}
		}
	}
	return string(body)
}
