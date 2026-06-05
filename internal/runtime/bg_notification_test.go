package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// CONF-C1 — Background auto-notification
// Doc reference : 04c-primitives.md "Auto-notification".
// =====================================================================

// fakeBgNotification matches the runtime.BackgroundNotification
// interface so tests can drive the injection without dragging in
// the real background.Manager.
type fakeBgNotification struct{ text string }

func (f fakeBgNotification) Message() string { return f.text }

// fakeBgNotifier serves pre-loaded notifications then drains.
type fakeBgNotifier struct {
	queues map[string][]runtime.BackgroundNotification
}

func (f *fakeBgNotifier) DrainNotifications(sessionID string) []runtime.BackgroundNotification {
	out := f.queues[sessionID]
	delete(f.queues, sessionID)
	return out
}

func TestBgNotification_InjectsAtTurnStart(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "noted"}}

	notif := &fakeBgNotifier{queues: map[string][]runtime.BackgroundNotification{
		"sess-1": {
			fakeBgNotification{text: "[BACKGROUND TASK COMPLETED] task_id=a1 tool=db.sql elapsed=2.5s"},
			fakeBgNotification{text: "[BACKGROUND TASK FAILED] task_id=b2 tool=shell.bash elapsed=0.3s"},
		},
	}}

	e := newEngine(t, apps, sess, lc)
	e.BackgroundNotifications = notif

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Both notifications must have been persisted as durable SYSTEM
	// directives (EventSystemMessage, Role="system") — authoritative status
	// the agent must heed, per the doc, not user input.
	gotSys := 0
	var combined string
	for _, ev := range sess.events {
		if ev.Type == sessionstore.EventSystemMessage && ev.Message != nil {
			if ev.Message.Role != "system" {
				t.Errorf("background directive role = %q, want system", ev.Message.Role)
			}
			combined += ev.Message.Content + "|"
			if src, _ := ev.Message.Extra["source"].(string); src != "background_notification" {
				t.Errorf("directive source = %q, want background_notification", src)
			}
			gotSys++
		}
	}
	if gotSys < 2 {
		t.Errorf("expected at least 2 system-directive events, got %d : %q", gotSys, combined)
	}
	if !strings.Contains(combined, "task_id=a1") {
		t.Errorf("first notification missing : %q", combined)
	}
	if !strings.Contains(combined, "task_id=b2") {
		t.Errorf("second notification missing : %q", combined)
	}

	// End-to-end : the directive must actually reach the LLM as a system
	// message in this turn's context (re-snapshot picks up the projection).
	if lc.got == nil {
		t.Fatal("LLM was not called")
	}
	var sysHit bool
	for _, m := range lc.got.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "task_id=a1") {
			sysHit = true
		}
	}
	if !sysHit {
		t.Error("background directive did not reach the LLM as a system message")
	}
}

func TestBgNotification_NilNotifierIsHarmless(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "ok"}}

	e := newEngine(t, apps, sess, lc)
	e.BackgroundNotifications = nil

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestBgNotification_EmptyQueueSkipsInjection(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "ok"}}

	notif := &fakeBgNotifier{queues: map[string][]runtime.BackgroundNotification{}}
	e := newEngine(t, apps, sess, lc)
	e.BackgroundNotifications = notif

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// No background directive should land on an empty queue.
	for _, ev := range sess.events {
		if ev.Type == sessionstore.EventSystemMessage && ev.Message != nil {
			if strings.HasPrefix(ev.Message.Content, "[BACKGROUND TASK") {
				t.Errorf("unexpected bg notif injected : %q", ev.Message.Content)
			}
		}
	}
}
