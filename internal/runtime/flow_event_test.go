package runtime

import (
	"context"
	"testing"

	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

type flowEventSessions struct {
	st *sessionstore.SessionState
}

func (f *flowEventSessions) State(sid string) (*sessionstore.SessionState, error) {
	return f.st, nil
}

func (f *flowEventSessions) AppendDurable(context.Context, sessionstore.Event) (uint64, error) {
	return 0, nil
}

func (f *flowEventSessions) Append(context.Context, sessionstore.Event) (uint64, error) {
	return 0, nil
}

func TestEngine_flowEvent_StructuredTrigger(t *testing.T) {
	s := &sessionstore.SessionState{SessionID: "s1"}
	sessionstore.Apply(s, &sessionstore.Event{
		Type:      sessionstore.EventUserMessage,
		SessionID: "s1",
		Message: &sessionstore.MessagePayload{
			Role:    "user",
			Content: "rendered activation message",
			TriggerEvent: map[string]any{
				"provider": "glpi",
				"payload": map[string]any{
					"id":     float64(99),
					"status": "new",
				},
			},
		},
	})
	e := &Engine{Sessions: &flowEventSessions{st: s}}
	ev := e.flowEvent("s1")
	payload, ok := ev["payload"].(map[string]any)
	if !ok {
		t.Fatalf("expected payload map, got %T", ev["payload"])
	}
	if payload["id"] != float64(99) {
		t.Fatalf("payload.id = %v, want 99", payload["id"])
	}
	if ev["provider"] != "glpi" {
		t.Fatalf("provider = %v", ev["provider"])
	}
}

func TestEngine_flowEvent_FallbackToMessageText(t *testing.T) {
	s := &sessionstore.SessionState{SessionID: "s2"}
	sessionstore.Apply(s, &sessionstore.Event{
		Type:      sessionstore.EventUserMessage,
		SessionID: "s2",
		Message:   &sessionstore.MessagePayload{Role: "user", Content: "hello from chat"},
	})
	e := &Engine{Sessions: &flowEventSessions{st: s}}
	ev := e.flowEvent("s2")
	payload := ev["payload"].(map[string]any)
	if payload["message"] != "hello from chat" {
		t.Fatalf("fallback message = %v", payload["message"])
	}
}
