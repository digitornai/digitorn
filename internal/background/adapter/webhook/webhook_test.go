package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mbathepaul/digitorn/internal/background/adapter"
)

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func post(a *Adapter, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}

func TestInbound_HMAC(t *testing.T) {
	p := Provider{Name: "gh", Path: "/hook/gh", Auth: "signature", Secret: "s3cr3t", SigHeader: "X-Sig"}
	a := New([]Provider{p})
	var got []adapter.Event
	a.sink = func(_ context.Context, ev adapter.Event) error { got = append(got, ev); return nil }

	body := `{"user":"bob"}`
	// Valid signature → 202 + event delivered.
	if w := post(a, "/hook/gh", body, map[string]string{"Content-Type": "application/json", "X-Sig": sign("s3cr3t", body)}); w.Code != http.StatusAccepted {
		t.Fatalf("valid sig: code=%d", w.Code)
	}
	if len(got) != 1 || got[0].Payload["user"] != "bob" {
		t.Fatalf("event not delivered: %+v", got)
	}
	// Wrong signature → 401, no event.
	if w := post(a, "/hook/gh", body, map[string]string{"Content-Type": "application/json", "X-Sig": "sha256=deadbeef"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad sig: code=%d", w.Code)
	}
	if len(got) != 1 {
		t.Fatalf("bad-sig event leaked: %+v", got)
	}
}

func TestInbound_SizeAndContentType(t *testing.T) {
	a := New([]Provider{{Name: "p", Path: "/h", MaxBytes: 16}})
	a.sink = func(context.Context, adapter.Event) error { return nil }

	if w := post(a, "/h", strings.Repeat("x", 100), map[string]string{"Content-Type": "application/json"}); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: code=%d", w.Code)
	}
	if w := post(a, "/h", "x", map[string]string{"Content-Type": "application/xml"}); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("bad content-type: code=%d", w.Code)
	}
	if w := post(a, "/unknown", "x", nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown path: code=%d", w.Code)
	}
}

func TestInbound_DeliveryIDDedup(t *testing.T) {
	a := New([]Provider{{Name: "p", Path: "/h", Auth: "none"}})
	var got []adapter.Event
	a.sink = func(_ context.Context, ev adapter.Event) error { got = append(got, ev); return nil }
	post(a, "/h", `{}`, map[string]string{"Content-Type": "application/json", "X-Delivery-Id": "abc123"})
	if len(got) != 1 || got[0].DedupKey != "abc123" {
		t.Fatalf("delivery id not used as dedup key: %+v", got)
	}
}

func TestOutbound_SendAndSSRF(t *testing.T) {
	var mu sync.Mutex
	var received string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = string(b)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	a := New(nil)
	a.AllowPrivate = true // the httptest server is loopback
	if err := a.Send(context.Background(), map[string]any{"url": srv.URL}, "the reply"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(received, "the reply") {
		t.Fatalf("server did not receive reply: %q", received)
	}

	// SSRF guard ON: loopback + metadata are blocked.
	a.AllowPrivate = false
	if err := a.Send(context.Background(), map[string]any{"url": srv.URL}, "x"); err == nil {
		t.Fatal("loopback destination should be blocked by SSRF guard")
	}
	if err := a.Send(context.Background(), map[string]any{"url": "http://169.254.169.254/latest/meta-data/"}, "x"); err == nil {
		t.Fatal("metadata endpoint should be blocked")
	}
	if err := a.Send(context.Background(), map[string]any{"url": "file:///etc/passwd"}, "x"); err == nil {
		t.Fatal("non-http scheme should be blocked")
	}
}
