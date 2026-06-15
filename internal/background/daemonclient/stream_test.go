package daemonclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestStreamReplies : the event stream is relayed in seq order as assistant messages
// and compact tool lines, and the call returns when the turn settles.
func TestStreamReplies(t *testing.T) {
	body := `{"turn_active":false,"events":[
		{"seq":12,"type":"assistant_message","payload":{"content":"Je vais analyser le répertoire."}},
		{"seq":20,"type":"tool_call","payload":{"name":"filesystem__glob","status":"streaming"}},
		{"seq":21,"type":"tool_result","payload":{"name":"filesystem.glob","status":"completed","duration_ms":8}},
		{"seq":30,"type":"assistant_message","payload":{"content":"The workspace is empty."}},
		{"seq":31,"type":"assistant_message","payload":{"content":""}},
		{"seq":32,"type":"turn_ended"}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := New(srv.URL, "", WithPollInterval(time.Millisecond))
	var items []StreamItem
	err := c.StreamReplies(context.Background(), "app", "sid", 3, func(it StreamItem) {
		items = append(items, it)
	})
	if err != nil {
		t.Fatalf("StreamReplies: %v", err)
	}
	// Expect: preamble message, ONE tool line (from tool_result, not tool_call), final
	// message. The empty assistant_message (seq 31) is skipped.
	if len(items) != 3 {
		t.Fatalf("want 3 items, got %d: %+v", len(items), items)
	}
	if items[0].Kind != "message" || items[0].Text != "Je vais analyser le répertoire." {
		t.Fatalf("item0 wrong: %+v", items[0])
	}
	if items[1].Kind != "tool" || !strings.Contains(items[1].Text, "filesystem.glob") || !strings.Contains(items[1].Text, "✓") {
		t.Fatalf("item1 (tool) wrong: %+v", items[1])
	}
	if items[2].Kind != "message" || items[2].Text != "The workspace is empty." {
		t.Fatalf("item2 wrong: %+v", items[2])
	}
}

// TestToolLine : completed → ✓, anything else → ✗.
func TestToolLine(t *testing.T) {
	if l := toolLine("filesystem.read", "completed"); !strings.Contains(l, "✓") || !strings.Contains(l, "filesystem.read") {
		t.Fatalf("completed: %q", l)
	}
	if l := toolLine("shell.bash", "error"); !strings.Contains(l, "✗") {
		t.Fatalf("error: %q", l)
	}
}
