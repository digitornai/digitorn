// Package webhook is the webhook adapter: inbound HTTP deliveries become Events
// (HMAC / API-key verified, size- and content-type-bounded, payload sanitized)
// and replies (reply:auto) are POSTed back to a callback URL with an SSRF guard.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/digitornai/digitorn/internal/background/adapter"
)

const (
	defaultMaxBytes  = 1 << 20 // 1 MB
	defaultSigHeader = "X-Signature-256"
)

var allowedContentTypes = map[string]struct{}{
	"application/json":                  {},
	"application/x-www-form-urlencoded": {},
	"text/plain":                        {},
}

// Provider is one inbound webhook endpoint.
type Provider struct {
	Name         string
	Path         string // inbound path (e.g. /hook/github)
	Auth         string // "signature" | "api_key" | "none"
	Secret       string // HMAC-SHA256 secret (signature auth)
	SigHeader    string // signature header (default X-Signature-256)
	APIKey       string // expected api key (api_key auth)
	APIKeyHeader string // api-key header (default X-API-Key)
	MaxBytes     int64  // payload cap (default 1 MB)
	CallbackURL  string // reply:auto outbound target
}

// Adapter handles a set of webhook providers.
type Adapter struct {
	byPath map[string]Provider
	hc     *http.Client

	mu   sync.RWMutex
	sink adapter.Sink

	// AllowPrivate disables the outbound SSRF guard (tests only).
	AllowPrivate bool
}

// New builds the adapter from providers (keyed by path).
func New(providers []Provider) *Adapter {
	byPath := make(map[string]Provider, len(providers))
	for _, p := range providers {
		if p.MaxBytes <= 0 {
			p.MaxBytes = defaultMaxBytes
		}
		if p.SigHeader == "" {
			p.SigHeader = defaultSigHeader
		}
		if p.APIKeyHeader == "" {
			p.APIKeyHeader = "X-API-Key"
		}
		byPath[p.Path] = p
	}
	return &Adapter{byPath: byPath, hc: &http.Client{Timeout: 15 * time.Second}}
}

func (a *Adapter) Name() string { return "webhook" }

// Start stores the sink; serving is done by the service mounting Handler().
func (a *Adapter) Start(ctx context.Context, sink adapter.Sink) error {
	a.mu.Lock()
	a.sink = sink
	a.mu.Unlock()
	<-ctx.Done()
	return nil
}

func (a *Adapter) currentSink() adapter.Sink {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.sink
}

// Handler is the inbound HTTP handler the service mounts.
func (a *Adapter) Handler() http.Handler { return http.HandlerFunc(a.serve) }

func (a *Adapter) serve(w http.ResponseWriter, r *http.Request) {
	p, ok := a.byPath[r.URL.Path]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	sink := a.currentSink()
	if sink == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if ct := contentType(r.Header.Get("Content-Type")); ct != "" {
		if _, allowed := allowedContentTypes[ct]; !allowed {
			http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, p.MaxBytes+1))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > p.MaxBytes {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !a.authOK(p, r, body) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	payload := map[string]any{}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &payload) // non-JSON bodies still deliver (empty payload + raw)
	}
	ev := adapter.Event{
		Provider: p.Name,
		Adapter:  "webhook",
		DedupKey: deliveryID(r, body),
		Source:   clientIP(r),
		Payload:  payload,
		Metadata: map[string]any{"path": p.Path},
	}
	if p.CallbackURL != "" {
		ev.ReplyRef = map[string]any{"url": p.CallbackURL}
	}
	if err := sink(r.Context(), ev); err != nil {
		http.Error(w, "intake failed", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"accepted"}`))
}

func (a *Adapter) authOK(p Provider, r *http.Request, body []byte) bool {
	switch p.Auth {
	case "", "none":
		return true
	case "signature":
		mac := hmac.New(sha256.New, []byte(p.Secret))
		mac.Write(body)
		want := hex.EncodeToString(mac.Sum(nil))
		got := strings.TrimPrefix(r.Header.Get(p.SigHeader), "sha256=")
		return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
	case "api_key":
		return subtle.ConstantTimeCompare([]byte(p.APIKey), []byte(r.Header.Get(p.APIKeyHeader))) == 1
	default:
		return false
	}
}

// Send POSTs the reply text to the event's callback URL (SSRF-guarded).
func (a *Adapter) Send(ctx context.Context, ref map[string]any, text string) error {
	raw, _ := ref["url"].(string)
	if raw == "" {
		return fmt.Errorf("webhook: no reply url")
	}
	if err := a.safeURL(raw); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, raw, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook: reply POST %d", resp.StatusCode)
	}
	return nil
}

// safeURL blocks SSRF: only http/https, and (unless AllowPrivate) no private,
// loopback, link-local, unspecified, or cloud-metadata destinations.
func (a *Adapter) safeURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("webhook: bad url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("webhook: scheme %q not allowed", u.Scheme)
	}
	if a.AllowPrivate {
		return nil
	}
	host := u.Hostname()
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("webhook: resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("webhook: destination %s is not allowed", ip)
		}
		if ip.String() == "169.254.169.254" {
			return fmt.Errorf("webhook: metadata endpoint blocked")
		}
	}
	return nil
}

func contentType(h string) string {
	if i := strings.IndexByte(h, ';'); i >= 0 {
		h = h[:i]
	}
	return strings.ToLower(strings.TrimSpace(h))
}

// deliveryID uses a provider-supplied delivery header when present (the natural
// idempotency key), else a content hash.
func deliveryID(r *http.Request, body []byte) string {
	for _, h := range []string{"X-Delivery-Id", "X-GitHub-Delivery", "X-Request-Id", "Idempotency-Key"} {
		if v := r.Header.Get(h); v != "" {
			return v
		}
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:16])
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
