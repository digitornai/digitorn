package runtime

import (
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/llm"
)

func TestSnipOversizedMessages(t *testing.T) {
	big := strings.Repeat("x", 300*1024)
	msgs := []llm.ChatMessage{
		{Role: "user", Content: "small"},
		{Role: "tool", Content: big},
	}
	snipOversizedMessages(msgs, maxMessageBytes)

	if msgs[0].Content != "small" {
		t.Errorf("small message must be untouched, got %d bytes", len(msgs[0].Content))
	}
	c := msgs[1].Content
	if len(c) >= len(big) {
		t.Fatalf("oversized message not clipped: %d bytes", len(c))
	}
	if len(c) > maxMessageBytes {
		t.Errorf("clipped message exceeds cap: %d > %d", len(c), maxMessageBytes)
	}
	if !strings.Contains(c, "truncated") {
		t.Errorf("clipped message missing the truncation marker")
	}
	if !strings.HasPrefix(c, "xxx") || !strings.HasSuffix(c, "xxx") {
		t.Errorf("head and tail of the payload must be preserved")
	}
}
