package telegram

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

	"github.com/mbathepaul/digitorn/internal/background/adapter"
)

// TestPromptCallbackResolves : an inline-button click (callback_query) routes back to
// the blocked Prompt with the chosen option id and the clicking user.
func TestPromptCallbackResolves(t *testing.T) {
	var (
		mu    sync.Mutex
		nonce string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			body, _ := io.ReadAll(r.Body)
			var m struct {
				ReplyMarkup struct {
					InlineKeyboard [][]struct {
						CallbackData string `json:"callback_data"`
					} `json:"inline_keyboard"`
				} `json:"reply_markup"`
			}
			_ = json.Unmarshal(body, &m)
			mu.Lock()
			for _, row := range m.ReplyMarkup.InlineKeyboard {
				for _, b := range row {
					if n, _, ok := parseCB(b.CallbackData); ok {
						nonce = n
					}
				}
			}
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	a := New([]Provider{{Name: "tg", Token: "x", APIBase: srv.URL}}, nil, nil)
	p := a.byName["tg"]

	req := adapter.PromptRequest{
		ReplyRef: map[string]any{"provider": "tg", "chat_id": "555"},
		Title:    "Approbation",
		Body:     "filesystem.write",
		Options: []adapter.PromptOption{
			{ID: "grant", Label: "✅ Approuver"},
			{ID: "deny", Label: "❌ Refuser"},
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

	var n string
	for range 200 {
		mu.Lock()
		n = nonce
		mu.Unlock()
		if n != "" {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if n == "" {
		t.Fatal("prompt never posted its inline keyboard")
	}

	a.onCallback(context.Background(), p, &callbackQuery{
		ID:   "cb1",
		Data: "a:" + n + ":0",
		From: struct {
			ID int64 `json:"id"`
		}{ID: 7},
	})

	select {
	case got := <-ch:
		if got.e != nil {
			t.Fatalf("prompt error: %v", got.e)
		}
		if got.r.OptionID != "grant" || got.r.UserID != "7" {
			t.Fatalf("wrong response: %+v", got.r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prompt did not resolve after callback")
	}
}

// TestPromptFreeTextDegrades : a prompt with no options (free-text) is unsupported on
// Telegram and returns an error so the processor degrades it (web/CLI).
func TestPromptFreeTextDegrades(t *testing.T) {
	a := New([]Provider{{Name: "tg", Token: "x", APIBase: "http://127.0.0.1:0"}}, nil, nil)
	_, err := a.Prompt(context.Background(), adapter.PromptRequest{
		ReplyRef:  map[string]any{"provider": "tg", "chat_id": "1"},
		AllowText: true,
	})
	if err == nil {
		t.Fatal("free-text prompt must return an error (degrade)")
	}
}
