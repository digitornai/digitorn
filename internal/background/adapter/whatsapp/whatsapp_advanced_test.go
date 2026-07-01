package whatsapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/digitornai/digitorn/internal/background/adapter"
)

// collect builds an adapter whose sink records every delivered event.
func collect(providers []Provider) (*Adapter, func() []adapter.Event) {
	a := New(providers, nil)
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

func postBody(a *Adapter, path, body, sig string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if sig != "" {
		r.Header.Set("X-Hub-Signature-256", sig)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}

// TestVerification_EdgeCases hardens the GET subscription handshake: a missing
// hub.mode, a mismatched token, and a provider with an empty configured token
// (which must never auto-pass) are all 403; the happy path echoes the challenge.
func TestVerification_EdgeCases(t *testing.T) {
	a := New([]Provider{
		{Name: "wa", Path: "/wa", VerifyToken: "vtok"},
		{Name: "open", Path: "/open", VerifyToken: ""}, // misconfigured: no token
	}, nil)

	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}

	if w := get("/wa?hub.mode=subscribe&hub.verify_token=vtok&hub.challenge=42"); w.Code != http.StatusOK || w.Body.String() != "42" {
		t.Fatalf("happy challenge: code=%d body=%q", w.Code, w.Body.String())
	}
	// Right token but wrong mode → 403 (don't echo).
	if w := get("/wa?hub.mode=unsubscribe&hub.verify_token=vtok&hub.challenge=42"); w.Code != http.StatusForbidden {
		t.Fatalf("wrong mode should 403, got %d", w.Code)
	}
	// Wrong token → 403.
	if w := get("/wa?hub.mode=subscribe&hub.verify_token=NOPE&hub.challenge=42"); w.Code != http.StatusForbidden {
		t.Fatalf("wrong token should 403, got %d", w.Code)
	}
	// Missing token param → 403 (must not match an empty configured token).
	if w := get("/wa?hub.mode=subscribe&hub.challenge=42"); w.Code != http.StatusForbidden {
		t.Fatalf("missing token should 403, got %d", w.Code)
	}
	// Provider with empty configured token: an empty supplied token must NOT pass.
	if w := get("/open?hub.mode=subscribe&hub.verify_token=&hub.challenge=42"); w.Code != http.StatusForbidden {
		t.Fatalf("empty configured token must never verify, got %d", w.Code)
	}
	// Unknown path → 404.
	if w := get("/ghost?hub.mode=subscribe&hub.verify_token=vtok&hub.challenge=42"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown path should 404, got %d", w.Code)
	}
}

// TestInbound_HMACRejections: every malformed/forged/missing signature is 401
// and delivers nothing (security). A genuine signature still passes.
func TestInbound_HMACRejections(t *testing.T) {
	const secret = "appsecret"
	cases := []struct {
		name   string
		setSig bool
		sig    string
	}{
		{"missing header", false, ""},
		{"empty value", true, ""},
		{"wrong digest", true, "sha256=deadbeef"},
		{"valid-for-other-secret", true, sign("OTHER", inbound)},
		{"valid-for-other-body", true, sign(secret, `{"entry":[]}`)},
		{"truncated digest", true, strings.TrimPrefix(sign(secret, inbound), "sha256=")[:12]},
		{"uppercased digest", true, strings.ToUpper(sign(secret, inbound))},
		{"trailing garbage", true, sign(secret, inbound) + "00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, events := collect([]Provider{{Name: "wa", Path: "/wa", AppSecret: secret}})
			w := postBody(a, "/wa", inbound, "")
			if c.setSig {
				w = postBody(a, "/wa", inbound, c.sig)
			}
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d", w.Code)
			}
			if len(events()) != 0 {
				t.Fatalf("forged delivery leaked an event")
			}
		})
	}

	a, events := collect([]Provider{{Name: "wa", Path: "/wa", AppSecret: secret}})
	if w := postBody(a, "/wa", inbound, sign(secret, inbound)); w.Code != http.StatusOK || len(events()) != 1 {
		t.Fatalf("genuine signature must pass: code=%d events=%d", w.Code, len(events()))
	}
}

// TestInbound_NoAppSecret documents the by-design behaviour when AppSecret is
// empty: HMAC is skipped and inbound is accepted unauthenticated. (Recorded so a
// regression that silently flips this is caught.)
func TestInbound_NoAppSecret(t *testing.T) {
	a, events := collect([]Provider{{Name: "wa", Path: "/wa"}}) // no AppSecret
	if w := postBody(a, "/wa", inbound, ""); w.Code != http.StatusOK {
		t.Fatalf("no-secret provider should accept inbound, got %d", w.Code)
	}
	if len(events()) != 1 {
		t.Fatalf("expected the message to be parsed, got %d events", len(events()))
	}
}

// TestInbound_StatusesOnly: a status-update payload (delivery receipts, no
// messages) must produce NO event — only real inbound messages launch a turn.
func TestInbound_StatusesOnly(t *testing.T) {
	const statuses = `{"entry":[{"changes":[{"value":{
	  "metadata":{"phone_number_id":"PNID1"},
	  "statuses":[{"id":"wamid.S","status":"delivered","recipient_id":"33600000000"}]
	}}]}]}`
	a, events := collect([]Provider{{Name: "wa", Path: "/wa"}})
	if w := postBody(a, "/wa", statuses, ""); w.Code != http.StatusOK {
		t.Fatalf("statuses payload should still 200, got %d", w.Code)
	}
	if len(events()) != 0 {
		t.Fatalf("statuses-only payload must not emit an event, got %d", len(events()))
	}
}

// TestInbound_MultipleMessages: several messages across multiple entries/changes
// in one POST each become a distinct event with its own reply handle and dedup
// key.
func TestInbound_MultipleMessages(t *testing.T) {
	const multi = `{"entry":[
	  {"changes":[{"value":{
	    "metadata":{"phone_number_id":"PNID1"},
	    "messages":[
	      {"from":"331","id":"wamid.A","type":"text","text":{"body":"first"}},
	      {"from":"332","id":"wamid.B","type":"text","text":{"body":"second"}}
	    ]}}]},
	  {"changes":[{"value":{
	    "metadata":{"phone_number_id":"PNID2"},
	    "messages":[
	      {"from":"333","id":"wamid.C","type":"text","text":{"body":"third"}}
	    ]}}]}
	]}`
	a, events := collect([]Provider{{Name: "wa", Path: "/wa"}})
	if w := postBody(a, "/wa", multi, ""); w.Code != http.StatusOK {
		t.Fatalf("multi payload: code=%d", w.Code)
	}
	ev := events()
	if len(ev) != 3 {
		t.Fatalf("expected 3 events, got %d", len(ev))
	}
	want := []struct{ dedup, src, msg, pnid string }{
		{"wa:wamid.A", "331", "first", "PNID1"},
		{"wa:wamid.B", "332", "second", "PNID1"},
		{"wa:wamid.C", "333", "third", "PNID2"},
	}
	for i, w := range want {
		if ev[i].DedupKey != w.dedup || ev[i].Source != w.src || ev[i].Message != w.msg {
			t.Fatalf("event %d wrong: %+v", i, ev[i])
		}
		if ev[i].ReplyRef["to"] != w.src || ev[i].ReplyRef["phone_number_id"] != w.pnid || ev[i].ReplyRef["provider"] != "wa" {
			t.Fatalf("event %d reply handle wrong: %+v", i, ev[i].ReplyRef)
		}
	}
}

// TestInbound_MissingFieldsGraceful: empty/partial bodies and a non-text message
// type don't panic and don't fabricate junk events; a text message with empty
// body is still delivered (with empty Message).
func TestInbound_MissingFieldsGraceful(t *testing.T) {
	a, events := collect([]Provider{{Name: "wa", Path: "/wa"}})

	// Totally empty JSON object → no event, 200.
	if w := postBody(a, "/wa", `{}`, ""); w.Code != http.StatusOK || len(events()) != 0 {
		t.Fatalf("empty object: code=%d events=%d", w.Code, len(events()))
	}
	// Malformed JSON → parseInbound returns nil → no event, 200.
	if w := postBody(a, "/wa", `{not json`, ""); w.Code != http.StatusOK || len(events()) != 0 {
		t.Fatalf("malformed json: code=%d events=%d", w.Code, len(events()))
	}
	// Empty entry/changes arrays → no event.
	if w := postBody(a, "/wa", `{"entry":[{"changes":[]}]}`, ""); w.Code != http.StatusOK || len(events()) != 0 {
		t.Fatalf("empty changes: code=%d events=%d", w.Code, len(events()))
	}

	// A non-text message (image) with no text body: parses, emits one event with
	// empty Message (the processor handles media separately) — must not panic.
	const imageMsg = `{"entry":[{"changes":[{"value":{
	  "metadata":{"phone_number_id":"PNID1"},
	  "messages":[{"from":"331","id":"wamid.IMG","type":"image"}]
	}}]}]}`
	if w := postBody(a, "/wa", imageMsg, ""); w.Code != http.StatusOK {
		t.Fatalf("image message: code=%d", w.Code)
	}
	ev := events()
	if len(ev) != 1 {
		t.Fatalf("image message should produce one event, got %d", len(ev))
	}
	if ev[0].DedupKey != "wa:wamid.IMG" || ev[0].Message != "" {
		t.Fatalf("image event wrong: %+v", ev[0])
	}
}

// TestInbound_OversizeBody: a body over the 1 MiB cap is rejected with 413 and
// never reaches the parser.
func TestInbound_OversizeBody(t *testing.T) {
	a, events := collect([]Provider{{Name: "wa", Path: "/wa"}})
	big := strings.Repeat("a", maxBodyBytes+1)
	if w := postBody(a, "/wa", big, ""); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body should 413, got %d", w.Code)
	}
	if len(events()) != 0 {
		t.Fatalf("oversize body must not deliver, got %d", len(events()))
	}
}

// TestInbound_MethodAndReadiness: non POST/GET → 405; POST before Start (no sink)
// → 503 so Meta retries instead of us silently dropping.
func TestInbound_MethodAndReadiness(t *testing.T) {
	a := New([]Provider{{Name: "wa", Path: "/wa", AppSecret: "s"}}, nil)
	// No sink: POST → 503. (HMAC is checked after the sink readiness gate, so an
	// unsigned POST still surfaces as not-ready, which is the intended ordering.)
	if w := postBody(a, "/wa", inbound, ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no sink should 503, got %d", w.Code)
	}
	a.sink = func(context.Context, adapter.Event) error { return nil }
	r := httptest.NewRequest(http.MethodPut, "/wa", strings.NewReader(inbound))
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT should be 405, got %d", w.Code)
	}
}

// TestOutbound_RecipientAndTokenRedaction: Send requires a recipient, honours a
// per-reply phone_number_id override, and never leaks the bearer token into the
// error returned on an upstream failure.
func TestOutbound_RecipientAndTokenRedaction(t *testing.T) {
	var mu sync.Mutex
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"messages":[{"id":"x"}]}`))
	}))
	defer srv.Close()

	a := New([]Provider{{Name: "wa", AccessToken: "SECRET-TOKEN", PhoneNumberID: "DEFAULT", APIBase: srv.URL, APIVersion: "v19.0"}}, nil)

	// Missing recipient → error, no request.
	if err := a.Send(context.Background(), map[string]any{"provider": "wa"}, "x"); err == nil {
		t.Fatal("missing recipient must error")
	}
	// Per-reply phone_number_id override is used in the path.
	if err := a.Send(context.Background(), map[string]any{"provider": "wa", "to": "331", "phone_number_id": "OVERRIDE"}, "hi"); err != nil {
		t.Fatalf("send with override: %v", err)
	}
	mu.Lock()
	path := gotPath
	mu.Unlock()
	if path != "/v19.0/OVERRIDE/messages" {
		t.Fatalf("override phone id not used: %q", path)
	}

	// Upstream error: the bearer token must NOT appear in the returned error.
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad"}`))
	}))
	defer errSrv.Close()
	b := New([]Provider{{Name: "wa", AccessToken: "SECRET-TOKEN", PhoneNumberID: "P", APIBase: errSrv.URL, APIVersion: "v19.0"}}, nil)
	err := b.Send(context.Background(), map[string]any{"provider": "wa", "to": "331"}, "x")
	if err == nil {
		t.Fatal("400 upstream must surface as error")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("LEAK: access token present in error: %v", err)
	}
}
