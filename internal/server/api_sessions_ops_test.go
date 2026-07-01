package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// seedConversation appends N user/assistant pairs to a session via the bus, then
// flushes so the JSONL is on disk (fork/export read from disk).
func seedConversation(t *testing.T, h *apiHarness, sid string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := h.bus.AppendDurable(context.Background(), sessionstore.Event{
			Type: sessionstore.EventUserMessage, SessionID: sid, AppID: "app-1", UserID: "user-A",
			Message: &sessionstore.MessagePayload{Role: "user", Content: fmt.Sprintf("q%d", i)},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.bus.AppendDurable(context.Background(), sessionstore.Event{
			Type: sessionstore.EventAssistantMessage, SessionID: sid, AppID: "app-1", UserID: "user-A",
			Message: &sessionstore.MessagePayload{Role: "assistant", Content: fmt.Sprintf("a%d", i)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.flusher.Flush(ctx)
}

func TestAPI_ForkSession(t *testing.T) {
	h := newAPIHarness(t)

	code, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{"title":"Original"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	seedConversation(t, h, sid, 3)

	code, body = h.do(t, "POST", "/api/apps/app-1/sessions/"+sid+"/fork", "user-A", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("fork: %d %s", code, body)
	}
	var fr map[string]any
	decodeBody(t, body, &fr)

	newSid, _ := fr["new_session_id"].(string)
	if newSid == "" || newSid == sid {
		t.Fatalf("bad new_session_id: %v", fr)
	}
	if fr["forked_from"] != sid {
		t.Fatalf("forked_from=%v want %s", fr["forked_from"], sid)
	}
	if fr["forked"] != true {
		t.Fatalf("forked flag=%v", fr["forked"])
	}
	if mc, _ := fr["message_count"].(float64); mc != 6 {
		t.Fatalf("message_count=%v want 6", fr["message_count"])
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.flusher.Flush(ctx)

	// The fork owns a full copy of the conversation under a "Fork of …" title.
	st, err := h.bus.State(newSid)
	if err != nil {
		t.Fatal(err)
	}
	st.RLock()
	title, owner := st.Title, st.UserID
	st.RUnlock()
	if !strings.HasPrefix(title, "Fork of ") {
		t.Fatalf("fork title=%q", title)
	}
	if owner != "user-A" {
		t.Fatalf("fork owner=%q", owner)
	}
	tr, err := h.bus.Transcript(newSid)
	if err != nil {
		t.Fatal(err)
	}
	var users, assts int
	for i := range tr {
		switch tr[i].Role {
		case "user":
			users++
		case "assistant":
			assts++
		}
	}
	if users != 3 || assts != 3 {
		t.Fatalf("fork transcript users=%d assts=%d want 3/3", users, assts)
	}

	// The source session is untouched by the fork.
	src, _ := h.bus.State(sid)
	src.RLock()
	srcTitle := src.Title
	src.RUnlock()
	if srcTitle != "Original" {
		t.Fatalf("source title mutated: %q", srcTitle)
	}
}

func TestAPI_ForkSession_CrossUserDenied(t *testing.T) {
	h := newAPIHarness(t)
	code, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{"title":"Mine"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)
	seedConversation(t, h, sid, 1)

	// user-B must not be able to fork user-A's session.
	code, _ = h.do(t, "POST", "/api/apps/app-1/sessions/"+sid+"/fork", "user-B", `{}`)
	if code != http.StatusForbidden {
		t.Fatalf("cross-user fork status=%d want 403", code)
	}
}

func TestAPI_ForkSession_NotFound(t *testing.T) {
	h := newAPIHarness(t)
	code, _ := h.do(t, "POST", "/api/apps/app-1/sessions/does-not-exist/fork", "user-A", `{}`)
	if code != http.StatusNotFound {
		t.Fatalf("fork missing session status=%d want 404", code)
	}
}

func TestAPI_ExportSession_Markdown(t *testing.T) {
	h := newAPIHarness(t)
	code, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{"title":"Demo"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)
	seedConversation(t, h, sid, 2)

	code, body = h.do(t, "GET", "/api/apps/app-1/sessions/"+sid+"/export", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("export: %d %s", code, body)
	}
	var md map[string]any
	decodeBody(t, body, &md)
	if md["format"] != "markdown" {
		t.Fatalf("format=%v", md["format"])
	}
	content, _ := md["content"].(string)
	for _, want := range []string{"# Demo", "## Turn 1", "q0", "a0", "q1", "a1"} {
		if !strings.Contains(content, want) {
			t.Fatalf("markdown missing %q in:\n%s", want, content)
		}
	}
	if turns, _ := md["turns"].(float64); turns != 2 {
		t.Fatalf("turns=%v want 2", md["turns"])
	}
}

func TestAPI_ExportSession_JSON(t *testing.T) {
	h := newAPIHarness(t)
	code, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{"title":"Demo"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)
	seedConversation(t, h, sid, 2)

	code, body = h.do(t, "GET", "/api/apps/app-1/sessions/"+sid+"/export?format=json", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("export json: %d %s", code, body)
	}
	var js map[string]any
	decodeBody(t, body, &js)
	if js["format"] != "json" {
		t.Fatalf("format=%v", js["format"])
	}
	msgs, _ := js["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages=%d want 4", len(msgs))
	}
	if mc, _ := js["message_count"].(float64); mc != 4 {
		t.Fatalf("message_count=%v want 4", js["message_count"])
	}
}

func TestAPI_ExportSession_Errors(t *testing.T) {
	h := newAPIHarness(t)
	code, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{"title":"Demo"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)
	seedConversation(t, h, sid, 1)

	if code, _ := h.do(t, "GET", "/api/apps/app-1/sessions/"+sid+"/export?format=pdf", "user-A", ""); code != http.StatusBadRequest {
		t.Fatalf("bad format status=%d want 400", code)
	}
	if code, _ := h.do(t, "GET", "/api/apps/app-1/sessions/missing/export", "user-A", ""); code != http.StatusNotFound {
		t.Fatalf("missing session status=%d want 404", code)
	}
	if code, _ := h.do(t, "GET", "/api/apps/app-1/sessions/"+sid+"/export", "user-B", ""); code != http.StatusForbidden {
		t.Fatalf("cross-user status=%d want 403", code)
	}
}
