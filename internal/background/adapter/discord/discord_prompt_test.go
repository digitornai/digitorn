package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/background/adapter"
)

// promptHarness stands in for the Discord REST API: it captures the nonce from the
// posted prompt's component custom_id (so a test can deliver a matching interaction)
// and returns 200 for interaction callbacks.
func promptHarness(t *testing.T) (*Adapter, Provider, func() string, func()) {
	t.Helper()
	var (
		mu    sync.Mutex
		nonce string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			body, _ := io.ReadAll(r.Body)
			var m struct {
				Components []struct {
					Components []struct {
						CustomID string `json:"custom_id"`
					} `json:"components"`
				} `json:"components"`
			}
			_ = json.Unmarshal(body, &m)
			mu.Lock()
			for _, row := range m.Components {
				for _, c := range row.Components {
					if p, _, ok := parseCustomID(c.CustomID, "a"); ok {
						nonce = p
					}
				}
			}
			mu.Unlock()
			_, _ = w.Write([]byte(`{"id":"msg1"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	a := New([]Provider{{Name: "dc", Token: "x", APIBase: srv.URL}}, nil)
	getNonce := func() string {
		for range 200 {
			mu.Lock()
			n := nonce
			mu.Unlock()
			if n != "" {
				return n
			}
			time.Sleep(2 * time.Millisecond)
		}
		return ""
	}
	return a, a.byName["dc"], getNonce, srv.Close
}

// TestPromptButtonResolves : a button click routes back to the blocked Prompt with the
// chosen option id and the clicking user — the core approval round-trip.
func TestPromptButtonResolves(t *testing.T) {
	a, p, getNonce, _ := promptHarness(t)
	req := adapter.PromptRequest{
		ReplyRef: map[string]any{"provider": "dc", "channel_id": "c1"},
		Title:    "Approbation",
		Body:     "shell.bash",
		Options: []adapter.PromptOption{
			{ID: "grant", Label: "✅ Approuver", Style: "primary"},
			{ID: "deny", Label: "❌ Refuser", Style: "danger"},
		},
	}
	type result struct {
		r adapter.PromptResponse
		e error
	}
	ch := make(chan result, 1)
	go func() {
		r, e := a.Prompt(context.Background(), req)
		ch <- result{r, e}
	}()

	nonce := getNonce()
	if nonce == "" {
		t.Fatal("prompt was never posted")
	}
	raw := json.RawMessage(`{"id":"i1","token":"tok","type":3,"data":{"custom_id":"a:` + nonce + `:0"},"member":{"user":{"id":"u7"}}}`)
	a.onInteraction(context.Background(), p, raw)

	select {
	case got := <-ch:
		if got.e != nil {
			t.Fatalf("prompt returned error: %v", got.e)
		}
		if got.r.OptionID != "grant" || got.r.UserID != "u7" {
			t.Fatalf("wrong response: %+v", got.r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prompt did not resolve after click")
	}
}

// TestPromptTextModalResolves : clicking the free-text button opens a modal (no
// resolution yet), and the modal submit resolves Prompt with the typed answer.
func TestPromptTextModalResolves(t *testing.T) {
	a, p, getNonce, _ := promptHarness(t)
	req := adapter.PromptRequest{
		ReplyRef:  map[string]any{"provider": "dc", "channel_id": "c1"},
		Title:     "Question",
		Body:      "Quelle couleur ?",
		AllowText: true,
		TextLabel: "Ta réponse",
	}
	type result struct {
		r adapter.PromptResponse
		e error
	}
	ch := make(chan result, 1)
	go func() {
		r, e := a.Prompt(context.Background(), req)
		ch <- result{r, e}
	}()

	nonce := getNonce()
	if nonce == "" {
		t.Fatal("prompt was never posted")
	}
	// Open the modal — must NOT resolve the prompt.
	a.onInteraction(context.Background(), p, json.RawMessage(`{"id":"i1","token":"t","type":3,"data":{"custom_id":"a:`+nonce+`:t"},"member":{"user":{"id":"u7"}}}`))
	select {
	case got := <-ch:
		t.Fatalf("prompt resolved on modal-open, must wait for submit: %+v", got.r)
	case <-time.After(80 * time.Millisecond):
	}
	// Submit the modal.
	a.onInteraction(context.Background(), p, json.RawMessage(`{"id":"i2","token":"t","type":5,"data":{"custom_id":"m:`+nonce+`","components":[{"components":[{"custom_id":"answer","value":"bleu"}]}]},"member":{"user":{"id":"u7"}}}`))
	select {
	case got := <-ch:
		if got.e != nil {
			t.Fatalf("prompt returned error: %v", got.e)
		}
		if got.r.Text != "bleu" || got.r.UserID != "u7" {
			t.Fatalf("wrong text response: %+v", got.r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prompt did not resolve after modal submit")
	}
}

// TestPromptCtxCancel : a cancelled context ends the wait (the parked turn timed out
// or was aborted) instead of blocking forever.
func TestPromptCtxCancel(t *testing.T) {
	a, _, getNonce, _ := promptHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	req := adapter.PromptRequest{
		ReplyRef: map[string]any{"provider": "dc", "channel_id": "c1"},
		Title:    "Approbation",
		Options:  []adapter.PromptOption{{ID: "grant", Label: "OK"}},
	}
	errCh := make(chan error, 1)
	go func() {
		_, e := a.Prompt(ctx, req)
		errCh <- e
	}()
	if getNonce() == "" {
		t.Fatal("prompt was never posted")
	}
	cancel()
	select {
	case e := <-errCh:
		if e == nil {
			t.Fatal("expected an error on ctx cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prompt did not return after ctx cancel")
	}
}

// TestPromptUnknownNonce : a click for an already-resolved/expired prompt is acked
// (no panic, no goroutine wedged) — Discord must never show "interaction failed".
func TestPromptUnknownNonce(t *testing.T) {
	a, p, _, _ := promptHarness(t)
	a.onInteraction(context.Background(), p, json.RawMessage(`{"id":"i9","token":"t","type":3,"data":{"custom_id":"a:deadbeef:0"},"member":{"user":{"id":"u1"}}}`))
}
