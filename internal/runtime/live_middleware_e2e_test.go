//go:build live

package runtime_test

import (
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

// TestLiveMiddleware_ContentFilterBlocks : content_filter short-circuits the
// real turn — the LLM is never reached and the rejection is the response.
func TestLiveMiddleware_ContentFilterBlocks(t *testing.T) {
	f := liveSetup(t)
	f.app.Definition.Runtime.Middleware = []schema.MiddlewareEntry{
		{Name: "content_filter", Config: map[string]any{
			"block_patterns":    []any{"forbidden"},
			"rejection_message": "BLOCKED BY MIDDLEWARE",
		}},
	}
	f.runLive(t, "Please do the forbidden thing right now.")
	if got := finalAssistantText(f); !strings.Contains(got, "BLOCKED BY MIDDLEWARE") {
		t.Errorf("content_filter must short-circuit with the rejection, got %q", got)
	}
}

// TestLiveMiddleware_ResponseFilterTruncates : response_filter caps the real
// model response that the agent records.
func TestLiveMiddleware_ResponseFilterTruncates(t *testing.T) {
	f := liveSetup(t)
	f.app.Definition.Runtime.Middleware = []schema.MiddlewareEntry{
		{Name: "response_filter", Config: map[string]any{"max_length": 20}},
	}
	f.runLive(t, "Explain in detail how photosynthesis works, in several sentences.")
	got := finalAssistantText(f)
	if !strings.Contains(got, "[Response truncated]") {
		t.Errorf("response_filter must truncate the real response, got %q", got)
	}
}
