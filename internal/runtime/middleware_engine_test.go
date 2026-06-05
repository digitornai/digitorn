package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/ports"
	dgruntime "github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// upperMW is a trivial AppMiddleware that uppercases the response, standing in
// for a resolved `custom` plugin.
type upperMW struct{}

func (upperMW) Name() string { return "custom:upper" }
func (upperMW) Before(context.Context, *ports.MiddlewareContext) (string, bool, error) {
	return "", false, nil
}
func (upperMW) After(_ context.Context, _ *ports.MiddlewareContext, resp string, _ []ports.LLMToolCall) (string, error) {
	return strings.ToUpper(resp), nil
}

func mwApp(appID string, entries []schema.MiddlewareEntry) *appmgr.RuntimeApp {
	app := secApp(appID, &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto}, nil)
	app.Definition.Runtime.Middleware = entries
	return app
}

func seedUser(t *testing.T, sess *projectingSessions, appID, sid, text string) {
	t.Helper()
	if _, err := sess.AppendDurable(context.Background(), sessionstore.Event{
		Type: sessionstore.EventUserMessage, SessionID: sid, AppID: appID, UserID: "u",
		Message: &sessionstore.MessagePayload{Role: "user", Parts: []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeText, Text: text},
		}},
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

func finalAssistant(sess *projectingSessions) string {
	out := ""
	for i := range sess.events {
		ev := sess.events[i]
		if ev.Type == sessionstore.EventAssistantMessage && ev.Message != nil {
			if ev.Message.Content != "" {
				out = ev.Message.Content
			}
			for _, p := range ev.Message.Parts {
				if p.Type == sessionstore.PartTypeText && p.Text != "" {
					out = p.Text
				}
			}
		}
	}
	return out
}

// TestMiddleware_ContentFilterShortCircuits : content_filter blocks the turn
// before the LLM is ever called ; the rejection becomes the response.
func TestMiddleware_ContentFilterShortCircuits(t *testing.T) {
	app := mwApp("mw-cf", []schema.MiddlewareEntry{
		{Name: "content_filter", Config: map[string]any{
			"block_patterns":    []any{"forbidden"},
			"rejection_message": "BLOCKED BY MW",
		}},
	})
	sess := newProjectingSessions("mw-cf-sess")
	seedUser(t, sess, "mw-cf", "mw-cf-sess", "please do the forbidden thing")
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "should never run"}}
	e := newEngine(t, &stubApps{app: app}, sess, lc)

	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "mw-cf", SessionID: "mw-cf-sess", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lc.calls != 0 {
		t.Errorf("LLM must NOT be called on a content_filter short-circuit, got %d calls", lc.calls)
	}
	if got := finalAssistant(sess); !strings.Contains(got, "BLOCKED BY MW") {
		t.Errorf("response must be the rejection, got %q", got)
	}
}

// TestMiddleware_MaskSecretsBeforeLLM : mask_secrets redacts the user message
// BEFORE it reaches the LLM.
func TestMiddleware_MaskSecretsBeforeLLM(t *testing.T) {
	app := mwApp("mw-ms", []schema.MiddlewareEntry{{Name: "mask_secrets"}})
	sess := newProjectingSessions("mw-ms-sess")
	seedUser(t, sess, "mw-ms", "mw-ms-sess", "my password=hunter2 please help")
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "ok"}}
	e := newEngine(t, &stubApps{app: app}, sess, lc)

	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "mw-ms", SessionID: "mw-ms-sess", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lc.got == nil {
		t.Fatal("LLM not called")
	}
	var sawMasked bool
	for _, m := range lc.got.Messages {
		if m.Role == "user" {
			if strings.Contains(m.Content, "hunter2") {
				t.Errorf("secret leaked to the LLM: %q", m.Content)
			}
			if strings.Contains(m.Content, "[MASKED]") {
				sawMasked = true
			}
		}
	}
	if !sawMasked {
		t.Error("user message must be masked before the LLM call")
	}
}

// TestMiddleware_ResponseFilterTransforms : response_filter truncates the
// LLM response that the agent ultimately records.
func TestMiddleware_ResponseFilterTransforms(t *testing.T) {
	app := mwApp("mw-rf", []schema.MiddlewareEntry{
		{Name: "response_filter", Config: map[string]any{"max_length": 5}},
	})
	sess := newProjectingSessions("mw-rf-sess")
	seedUser(t, sess, "mw-rf", "mw-rf-sess", "hello")
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "this is a very long answer"}}
	e := newEngine(t, &stubApps{app: app}, sess, lc)

	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "mw-rf", SessionID: "mw-rf-sess", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := finalAssistant(sess)
	if !strings.Contains(got, "[Response truncated]") || strings.Contains(got, "very long answer") {
		t.Errorf("response_filter must truncate the recorded response, got %q", got)
	}
}

// TestMiddleware_CustomResolvesViaFactory : a `custom` entry is resolved
// through the engine's MiddlewareCustomFactory seam (the gRPC plugin transport
// in production) and runs in the pipeline.
func TestMiddleware_CustomResolvesViaFactory(t *testing.T) {
	app := mwApp("mw-custom", []schema.MiddlewareEntry{
		{Name: "custom", Config: map[string]any{"module": "upper_mw", "kind": "mw-pool"}},
	})
	sess := newProjectingSessions("mw-custom-sess")
	seedUser(t, sess, "mw-custom", "mw-custom-sess", "hi")
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "quiet answer"}}
	e := newEngine(t, &stubApps{app: app}, sess, lc)

	var gotCfg map[string]any
	e.MiddlewareCustomFactory = func(name string, cfg map[string]any) (ports.AppMiddleware, error) {
		gotCfg = cfg
		return upperMW{}, nil
	}

	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "mw-custom", SessionID: "mw-custom-sess", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotCfg["module"] != "upper_mw" {
		t.Errorf("factory must receive the custom entry config, got %v", gotCfg)
	}
	if got := finalAssistant(sess); got != "QUIET ANSWER" {
		t.Errorf("custom middleware must transform the response, got %q", got)
	}
}
