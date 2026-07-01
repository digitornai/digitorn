package webhook

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/digitornai/digitorn/internal/background/adapter"
)

// collect builds an adapter whose sink appends every delivered event, returning
// the adapter and a thread-safe accessor for the captured slice.
func collect(providers []Provider) (*Adapter, func() []adapter.Event) {
	a := New(providers)
	var mu sync.Mutex
	var got []adapter.Event
	a.sink = func(_ context.Context, ev adapter.Event) error {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
		return nil
	}
	return a, func() []adapter.Event {
		mu.Lock()
		defer mu.Unlock()
		out := make([]adapter.Event, len(got))
		copy(out, got)
		return out
	}
}

// TestInbound_SignatureRejections asserts every malformed/forged/missing HMAC
// shape is rejected with 401 and leaks no event (security).
func TestInbound_SignatureRejections(t *testing.T) {
	p := Provider{Name: "gh", Path: "/h", Auth: "signature", Secret: "topsecret", SigHeader: "X-Sig"}
	body := `{"a":1}`

	cases := []struct {
		name   string
		setSig bool
		sig    string
	}{
		{"missing header", false, ""},
		{"empty value", true, ""},
		{"wrong hex", true, "sha256=00000000000000000000000000000000"},
		{"valid-for-other-secret", true, sign("WRONG", body)},
		{"valid-for-other-body", true, sign("topsecret", `{"a":2}`)},
		{"prefix-only", true, "sha256="},
		{"truncated digest", true, strings.TrimPrefix(sign("topsecret", body), "sha256=")[:10]},
		{"digest plus trailing garbage", true, sign("topsecret", body) + "extra"},
		{"uppercased digest", true, strings.ToUpper(sign("topsecret", body))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, events := collect([]Provider{p})
			h := map[string]string{"Content-Type": "application/json"}
			if c.setSig {
				h["X-Sig"] = c.sig
			}
			w := post(a, "/h", body, h)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d", w.Code)
			}
			if len(events()) != 0 {
				t.Fatalf("forged delivery leaked an event")
			}
		})
	}

	// Sanity: the genuine signature still passes (guards against an over-strict
	// check that would reject everything and pass the table above vacuously).
	a, events := collect([]Provider{p})
	w := post(a, "/h", body, map[string]string{"Content-Type": "application/json", "X-Sig": sign("topsecret", body)})
	if w.Code != http.StatusAccepted || len(events()) != 1 {
		t.Fatalf("genuine signature must pass: code=%d events=%d", w.Code, len(events()))
	}
}

// TestInbound_APIKeyAuth covers the api_key auth mode: wrong/missing key → 401,
// correct key → 202.
func TestInbound_APIKeyAuth(t *testing.T) {
	p := Provider{Name: "k", Path: "/h", Auth: "api_key", APIKey: "expected-key", APIKeyHeader: "X-API-Key"}

	a, events := collect([]Provider{p})
	if w := post(a, "/h", `{}`, map[string]string{"Content-Type": "application/json"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing key: %d", w.Code)
	}
	if w := post(a, "/h", `{}`, map[string]string{"Content-Type": "application/json", "X-API-Key": "wrong"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: %d", w.Code)
	}
	if len(events()) != 0 {
		t.Fatalf("unauthorized request leaked an event")
	}
	if w := post(a, "/h", `{}`, map[string]string{"Content-Type": "application/json", "X-API-Key": "expected-key"}); w.Code != http.StatusAccepted {
		t.Fatalf("correct key: %d", w.Code)
	}
	if len(events()) != 1 {
		t.Fatalf("authorized request not delivered")
	}
}

// TestInbound_SizeBoundary pins the cap behaviour: exactly at the cap is
// accepted, one byte over is rejected with 413.
func TestInbound_SizeBoundary(t *testing.T) {
	const cap = 32
	a, events := collect([]Provider{{Name: "p", Path: "/h", MaxBytes: cap}})

	atCap := strings.Repeat("x", cap)
	if w := post(a, "/h", atCap, map[string]string{"Content-Type": "text/plain"}); w.Code != http.StatusAccepted {
		t.Fatalf("body exactly at cap should pass, got %d", w.Code)
	}
	overCap := strings.Repeat("x", cap+1)
	if w := post(a, "/h", overCap, map[string]string{"Content-Type": "text/plain"}); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("body one byte over cap should be 413, got %d", w.Code)
	}
	if len(events()) != 1 {
		t.Fatalf("expected exactly the at-cap event, got %d", len(events()))
	}
}

// TestInbound_ContentTypeMatrix: allowed types pass, params on the type are
// tolerated, an empty content-type is accepted, and unknown types are 415.
func TestInbound_ContentTypeMatrix(t *testing.T) {
	a, events := collect([]Provider{{Name: "p", Path: "/h"}})

	ok := []string{
		"application/json",
		"application/json; charset=utf-8",
		"APPLICATION/JSON",
		"text/plain",
		"application/x-www-form-urlencoded",
	}
	for _, ct := range ok {
		if w := post(a, "/h", `{}`, map[string]string{"Content-Type": ct}); w.Code != http.StatusAccepted {
			t.Fatalf("content-type %q should pass, got %d", ct, w.Code)
		}
	}
	// Empty content-type is allowed (many webhook senders omit it).
	if w := post(a, "/h", `{}`, nil); w.Code != http.StatusAccepted {
		t.Fatalf("empty content-type should pass, got %d", w.Code)
	}
	bad := []string{"application/xml", "text/html", "multipart/form-data", "application/octet-stream"}
	for _, ct := range bad {
		if w := post(a, "/h", `{}`, map[string]string{"Content-Type": ct}); w.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("content-type %q should be 415, got %d", ct, w.Code)
		}
	}
	if n := len(events()); n != len(ok)+1 {
		t.Fatalf("expected %d delivered events, got %d", len(ok)+1, n)
	}
}

// TestInbound_MalformedJSON: a non-JSON / malformed body is still delivered (the
// adapter is content-agnostic) but yields an empty payload rather than panicking.
func TestInbound_MalformedJSON(t *testing.T) {
	a, events := collect([]Provider{{Name: "p", Path: "/h"}})

	w := post(a, "/h", `{not valid json`, map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("malformed json: code=%d", w.Code)
	}
	ev := events()
	if len(ev) != 1 {
		t.Fatalf("expected one event, got %d", len(ev))
	}
	if len(ev[0].Payload) != 0 {
		t.Fatalf("malformed json should yield empty payload, got %+v", ev[0].Payload)
	}

	// An empty body is also fine and yields an empty payload.
	w = post(a, "/h", "", map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("empty body: code=%d", w.Code)
	}
}

// TestInbound_DedupKeyHeaderPrecedence: the delivery-id header is the idempotency
// key irrespective of body, and a replay with a DIFFERENT body but the SAME id
// keeps that id (so the durable intake can dedup it), while a body-only delivery
// falls back to a stable content hash.
func TestInbound_DedupKeyHeaderPrecedence(t *testing.T) {
	a, events := collect([]Provider{{Name: "p", Path: "/h"}})

	// Header precedence order is X-Delivery-Id > X-GitHub-Delivery > X-Request-Id.
	post(a, "/h", `{"v":1}`, map[string]string{"Content-Type": "application/json", "X-GitHub-Delivery": "gh-1", "X-Delivery-Id": "deliver-1"})
	post(a, "/h", `{"v":2}`, map[string]string{"Content-Type": "application/json", "X-Delivery-Id": "deliver-1"})
	post(a, "/h", `{"v":3}`, map[string]string{"Content-Type": "application/json", "X-Request-Id": "req-9"})

	ev := events()
	if len(ev) != 3 {
		t.Fatalf("expected 3 deliveries, got %d", len(ev))
	}
	if ev[0].DedupKey != "deliver-1" {
		t.Fatalf("X-Delivery-Id should win over X-GitHub-Delivery: %q", ev[0].DedupKey)
	}
	if ev[1].DedupKey != "deliver-1" {
		t.Fatalf("same delivery id across different bodies must yield same dedup key: %q", ev[1].DedupKey)
	}
	if ev[2].DedupKey != "req-9" {
		t.Fatalf("X-Request-Id fallback: %q", ev[2].DedupKey)
	}

	// No delivery header → content hash. Identical bodies hash identically;
	// different bodies differ.
	b, events2 := collect([]Provider{{Name: "p", Path: "/h"}})
	post(b, "/h", `same`, map[string]string{"Content-Type": "text/plain"})
	post(b, "/h", `same`, map[string]string{"Content-Type": "text/plain"})
	post(b, "/h", `different`, map[string]string{"Content-Type": "text/plain"})
	ev2 := events2()
	if ev2[0].DedupKey == "" || ev2[0].DedupKey != ev2[1].DedupKey {
		t.Fatalf("identical bodies must share a content-hash dedup key: %q vs %q", ev2[0].DedupKey, ev2[1].DedupKey)
	}
	if ev2[2].DedupKey == ev2[0].DedupKey {
		t.Fatalf("different bodies must not collide on dedup key")
	}
}

// TestInbound_MethodAndReadiness: non-POST is 405, and an adapter with no sink
// (not started) returns 503 rather than dropping the delivery silently.
func TestInbound_MethodAndReadiness(t *testing.T) {
	a := New([]Provider{{Name: "p", Path: "/h"}})
	// No sink installed → not ready.
	r := httptest.NewRequest(http.MethodPost, "/h", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no sink should be 503, got %d", w.Code)
	}

	a.sink = func(context.Context, adapter.Event) error { return nil }
	r2 := httptest.NewRequest(http.MethodGet, "/h", nil)
	w2 := httptest.NewRecorder()
	a.Handler().ServeHTTP(w2, r2)
	if w2.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET should be 405, got %d", w2.Code)
	}
}

// TestInbound_SinkFailurePropagates: when the durable intake fails, the source
// gets 503 (so it retries) and not a false 202.
func TestInbound_SinkFailurePropagates(t *testing.T) {
	a := New([]Provider{{Name: "p", Path: "/h"}})
	a.sink = func(context.Context, adapter.Event) error { return context.DeadlineExceeded }
	w := post(a, "/h", `{}`, map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("sink failure must surface as 503, got %d", w.Code)
	}
}

// TestOutbound_SSRFGuard is the security core: outbound Send must refuse every
// private / loopback / link-local / unspecified / metadata destination, by IP
// literal AND by hostnames that resolve to them, while still allowing a genuine
// public (here: an httptest loopback we explicitly opt into) target.
func TestOutbound_SSRFGuard(t *testing.T) {
	a := New(nil)

	blocked := []struct {
		name string
		url  string
	}{
		{"ipv4 loopback", "http://127.0.0.1/x"},
		{"ipv4 loopback alt", "http://127.0.0.2/x"},
		{"ipv6 loopback literal", "http://[::1]/x"},
		{"private 10/8", "http://10.0.0.1/x"},
		{"private 172.16/12", "http://172.16.0.1/x"},
		{"private 192.168/16", "http://192.168.1.1/x"},
		{"link-local 169.254/16", "http://169.254.1.1/x"},
		{"cloud metadata", "http://169.254.169.254/latest/meta-data/"},
		{"unspecified v4", "http://0.0.0.0/x"},
		{"unspecified v6", "http://[::]/x"},
		{"localhost name", "http://localhost/x"},
		{"localhost with port", "http://localhost:9999/x"},
		{"https loopback", "https://127.0.0.1/x"},
	}
	for _, c := range blocked {
		if err := a.safeURL(c.url); err == nil {
			t.Errorf("SSRF BYPASS: %s (%s) was allowed", c.name, c.url)
		}
	}

	// Non-HTTP schemes are rejected before any DNS work.
	for _, raw := range []string{"file:///etc/passwd", "gopher://127.0.0.1/", "ftp://10.0.0.1/", "ssh://127.0.0.1/", "//127.0.0.1/x"} {
		if err := a.safeURL(raw); err == nil {
			t.Errorf("scheme bypass: %q was allowed", raw)
		}
	}

	// Send itself (not just safeURL) must refuse a blocked target and an empty url.
	if err := a.Send(context.Background(), map[string]any{"url": "http://169.254.169.254/"}, "x"); err == nil {
		t.Error("Send must refuse the metadata endpoint")
	}
	if err := a.Send(context.Background(), map[string]any{"url": ""}, "x"); err == nil {
		t.Error("Send must refuse an empty url")
	}
	if err := a.Send(context.Background(), map[string]any{}, "x"); err == nil {
		t.Error("Send must refuse a missing url")
	}
}

// TestOutbound_AllowsPublicTarget proves the guard is not blanket-deny: a real
// reachable server (opted-in via AllowPrivate because httptest is loopback)
// receives the reply body, and a >=400 response surfaces as an error.
func TestOutbound_AllowsPublicTarget(t *testing.T) {
	var mu sync.Mutex
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := New(nil)
	a.AllowPrivate = true
	if err := a.Send(context.Background(), map[string]any{"url": srv.URL}, "hello reply"); err != nil {
		t.Fatalf("Send to allowed target: %v", err)
	}
	mu.Lock()
	body := got
	mu.Unlock()
	if !strings.Contains(body, "hello reply") {
		t.Fatalf("target did not receive reply: %q", body)
	}

	// A 4xx/5xx upstream is reported as an error, not swallowed.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer bad.Close()
	if err := a.Send(context.Background(), map[string]any{"url": bad.URL}, "x"); err == nil {
		t.Fatal("a 502 upstream must surface as an error")
	}
}
