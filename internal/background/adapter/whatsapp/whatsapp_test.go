package whatsapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

const inbound = `{"entry":[{"changes":[{"value":{
  "metadata":{"phone_number_id":"PNID1"},
  "messages":[{"from":"33600000000","id":"wamid.ABC","type":"text","text":{"body":"bonjour"}}]
}}]}]}`

func TestVerificationChallenge(t *testing.T) {
	a := New([]Provider{{Name: "wa", Path: "/wa", VerifyToken: "vtok"}}, nil)
	r := httptest.NewRequest(http.MethodGet, "/wa?hub.mode=subscribe&hub.verify_token=vtok&hub.challenge=12345", nil)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "12345" {
		t.Fatalf("challenge echo failed: code=%d body=%q", w.Code, w.Body.String())
	}
	// Wrong token → 403.
	r2 := httptest.NewRequest(http.MethodGet, "/wa?hub.mode=subscribe&hub.verify_token=nope&hub.challenge=x", nil)
	w2 := httptest.NewRecorder()
	a.Handler().ServeHTTP(w2, r2)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("bad verify token should 403, got %d", w2.Code)
	}
}

func TestInbound_HMACAndParse(t *testing.T) {
	a := New([]Provider{{Name: "wa", Path: "/wa", AppSecret: "s3cret"}}, nil)
	var got []adapter.Event
	a.sink = func(_ context.Context, ev adapter.Event) error { got = append(got, ev); return nil }

	post := func(sig string) int {
		r := httptest.NewRequest(http.MethodPost, "/wa", strings.NewReader(inbound))
		if sig != "" {
			r.Header.Set("X-Hub-Signature-256", sig)
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w.Code
	}

	// Bad signature → 401, no event.
	if code := post("sha256=deadbeef"); code != http.StatusUnauthorized {
		t.Fatalf("bad sig: %d", code)
	}
	if len(got) != 0 {
		t.Fatalf("bad-sig event leaked")
	}
	// Valid signature → 200 + parsed event with reply handle.
	if code := post(sign("s3cret", inbound)); code != 200 {
		t.Fatalf("valid sig: %d", code)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	e := got[0]
	if e.Adapter != "whatsapp" || e.Message != "bonjour" || e.Source != "33600000000" || e.DedupKey != "wa:wamid.ABC" {
		t.Fatalf("parsed event wrong: %+v", e)
	}
	if e.ReplyRef["to"] != "33600000000" || e.ReplyRef["phone_number_id"] != "PNID1" || e.ReplyRef["provider"] != "wa" {
		t.Fatalf("reply handle wrong: %+v", e.ReplyRef)
	}
}

func TestOutbound_GraphSend(t *testing.T) {
	var mu sync.Mutex
	var gotAuth, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"messages":[{"id":"x"}]}`))
	}))
	defer srv.Close()

	a := New([]Provider{{Name: "wa", AccessToken: "TOKEN123", PhoneNumberID: "PNID1", APIBase: srv.URL, APIVersion: "v19.0"}}, nil)
	err := a.Send(context.Background(), map[string]any{"to": "3360", "provider": "wa"}, "salut")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotAuth != "Bearer TOKEN123" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotPath != "/v19.0/PNID1/messages" {
		t.Fatalf("graph path = %q", gotPath)
	}
	if gotBody["to"] != "3360" || gotBody["messaging_product"] != "whatsapp" {
		t.Fatalf("graph body wrong: %+v", gotBody)
	}
	txt, _ := gotBody["text"].(map[string]any)
	if txt["body"] != "salut" {
		t.Fatalf("text body = %v", txt)
	}

	// Unknown provider → error.
	if err := a.Send(context.Background(), map[string]any{"to": "1", "provider": "ghost"}, "x"); err == nil {
		t.Fatal("send to unknown provider should error")
	}
}
