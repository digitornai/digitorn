package sessionstore

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEnvelope_ShapeHasAllLegacyFields(t *testing.T) {
	ev := Event{
		Seq:        42,
		Type:       EventAssistantMessage,
		TsUnixNano: time.Date(2026, 5, 27, 12, 30, 0, 123_000_000, time.UTC).UnixNano(),
		SessionID:  "sess-abc",
		AppID:      "app-x",
		UserID:     "user-1",
		Message:    &MessagePayload{Role: "assistant", Content: "hello"},
	}
	builder := NewEnvelopeBuilder("inst-test-1", []string{"chat", "tools"})
	env := builder.Build(&ev)

	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)

	required := []string{
		`"event_id":"sess-abc:42"`,
		`"type":"assistant_message"`,
		`"kind":"session"`,
		`"seq":42`,
		`"app_id":"app-x"`,
		`"session_id":"sess-abc"`,
		`"ts":"2026-05-27T12:30:00.123Z"`,
		`"control":false`,
		`"capabilities":["chat","tools"]`,
		`"user_id":"user-1"`,
		`"instance_id":"inst-test-1"`,
		`"payload":`,
	}
	for _, key := range required {
		if !strings.Contains(got, key) {
			t.Errorf("envelope JSON missing %q\nfull: %s", key, got)
		}
	}
	// _dropped_pre_bootstrap should be omitted when false.
	if strings.Contains(got, "_dropped_pre_bootstrap") {
		t.Errorf("envelope must omit _dropped_pre_bootstrap when false; got: %s", got)
	}
}

// TestEnvelope_CarriesCorrelationID : a run_parallel child's tool_progress
// event must surface its parent call_id as correlation_id on the socket wire
// (so the client can advance the parent chip), while a plain event omits it.
func TestEnvelope_CarriesCorrelationID(t *testing.T) {
	prog := Event{
		Seq: 7, Type: EventToolProgress, SessionID: "s", TsUnixNano: time.Now().UnixNano(),
		CorrelationID: "parent-call",
		Tool:          &ToolPayload{CallID: "parent-call:0", Name: "filesystem.read", Status: "completed"},
	}
	got := string(mustJSON(t, NewEnvelopeBuilder("i", nil).Build(&prog)))
	if !strings.Contains(got, `"correlation_id":"parent-call"`) {
		t.Errorf("tool_progress envelope missing correlation_id on the wire\nfull: %s", got)
	}

	plain := Event{Seq: 1, Type: EventAssistantMessage, SessionID: "s", TsUnixNano: time.Now().UnixNano(),
		Message: &MessagePayload{Role: "assistant", Content: "hi"}}
	if strings.Contains(string(mustJSON(t, NewEnvelopeBuilder("i", nil).Build(&plain))), "correlation_id") {
		t.Errorf("plain event must omit correlation_id")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEnvelope_ControlEventsMarkedControl(t *testing.T) {
	controlTypes := []EventType{
		EventSessionStarted, EventSessionEnded, EventSessionInterrupt,
		EventCompactDone, EventQuarantine,
	}
	for _, typ := range controlTypes {
		ev := Event{Seq: 1, Type: typ, SessionID: "s", TsUnixNano: time.Now().UnixNano()}
		env := BuildEnvelope(&ev)
		if !env.Control {
			t.Errorf("%s: control should be true", typ)
		}
	}
}

func TestEnvelope_ErrorEventGetsErrorKind(t *testing.T) {
	ev := Event{
		Seq:        1,
		Type:       EventError,
		SessionID:  "s",
		TsUnixNano: time.Now().UnixNano(),
		Error:      &ErrorPayload{Code: "X", Message: "broken"},
	}
	env := BuildEnvelope(&ev)
	if env.Kind != "error" {
		t.Errorf("error event kind = %q, want error", env.Kind)
	}
}

func TestEnvelope_SessionEventsGetSessionKind(t *testing.T) {
	sessionTypes := []EventType{
		EventUserMessage, EventAssistantMessage,
		EventToolCall, EventToolResult,
		EventMemoryRemember, EventWorkspaceWrite,
		EventApprovalRequest, EventWidget,
	}
	for _, typ := range sessionTypes {
		ev := Event{Seq: 1, Type: typ, SessionID: "s", TsUnixNano: time.Now().UnixNano()}
		env := BuildEnvelope(&ev)
		if env.Kind != "session" {
			t.Errorf("%s: kind = %q, want session", typ, env.Kind)
		}
	}
}

func TestEnvelope_TsIsISO8601(t *testing.T) {
	ev := Event{
		Seq:        1,
		Type:       EventUserMessage,
		TsUnixNano: time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC).UnixNano(),
		SessionID:  "s",
	}
	env := BuildEnvelope(&ev)
	if env.Ts != "2026-01-15T09:00:00Z" {
		t.Errorf("ts = %q, want 2026-01-15T09:00:00Z", env.Ts)
	}
	// Parse roundtrip
	if _, err := time.Parse(time.RFC3339Nano, env.Ts); err != nil {
		t.Errorf("ts not parseable as RFC3339Nano: %v", err)
	}
}

func TestPrimaryRoomFor_PrefersSessionStrictIsolation(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
		want string
	}{
		{"session+app+user", Event{SessionID: "s", AppID: "a", UserID: "u"}, "session:s"},
		{"app+user only", Event{AppID: "a", UserID: "u"}, "app:a"},
		{"user only", Event{UserID: "u"}, "user:u"},
		{"empty", Event{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PrimaryRoomFor(&tc.ev)
			if got != tc.want {
				t.Errorf("PrimaryRoomFor(%+v) = %q, want %q", tc.ev, got, tc.want)
			}
		})
	}
}

func TestHistoryMessage_TsIsISO8601(t *testing.T) {
	tsNano := time.Date(2026, 3, 10, 14, 22, 33, 456_000_000, time.UTC).UnixNano()
	state := NewSessionState("s")
	state.Messages = []Message{
		{Seq: 1, Role: "user", Content: "hi", TsUnixNano: tsNano},
	}
	resp := BuildHistory(state, state.Messages, nil, ViewOptions{InstanceID: "inst-test"})
	if len(resp.Messages) != 1 {
		t.Fatalf("messages: %d", len(resp.Messages))
	}
	if resp.Messages[0].Ts != "2026-03-10T14:22:33.456Z" {
		t.Errorf("message ts = %q", resp.Messages[0].Ts)
	}
}

func TestHistoryResponse_TurnActiveAndPendingQueuePresent(t *testing.T) {
	state := NewSessionState("s")
	resp := BuildHistory(state, nil, nil, ViewOptions{InstanceID: "inst"})
	data, _ := json.Marshal(resp)
	got := string(data)
	if !strings.Contains(got, `"turn_active":`) {
		t.Errorf("missing turn_active in response: %s", got)
	}
	if !strings.Contains(got, `"pending_queue":[]`) {
		t.Errorf("pending_queue must be empty array, got: %s", got)
	}
	if !strings.Contains(got, `"instance_id":"inst"`) {
		t.Errorf("missing instance_id: %s", got)
	}
}
