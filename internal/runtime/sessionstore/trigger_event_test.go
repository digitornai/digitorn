package sessionstore

import "testing"

// TestTriggerEventPassthrough proves a structured inbound event attached to a
// user message survives projection → snapshot → cold restore (the webhook →
// flow path).
func TestTriggerEventPassthrough(t *testing.T) {
	trigger := map[string]any{
		"provider": "glpi",
		"adapter":  "webhook",
		"payload": map[string]any{
			"id":     float64(4242),
			"status": "new",
			"name":   "VPN down",
		},
	}
	s := &SessionState{SessionID: "ticket-4242"}
	Apply(s, &Event{
		Type:      EventUserMessage,
		SessionID: "ticket-4242",
		Message: &MessagePayload{
			Role:         "user",
			Content:      "GLPI ticket #4242 — VPN down",
			TriggerEvent: trigger,
		},
	})
	if s.Messages[len(s.Messages)-1].TriggerEvent == nil {
		t.Fatal("projection dropped TriggerEvent")
	}
	gotID := s.Messages[len(s.Messages)-1].TriggerEvent["payload"].(map[string]any)["id"]
	if gotID != float64(4242) {
		t.Fatalf("payload.id = %v, want 4242", gotID)
	}

	snap := s.Snapshot()
	if snap.Messages[len(snap.Messages)-1].TriggerEvent == nil {
		t.Fatal("snapshot dropped TriggerEvent")
	}

	fresh := &SessionState{}
	hydrateFromSnapshot(fresh, &snap)
	if fresh.Messages[len(fresh.Messages)-1].TriggerEvent == nil {
		t.Fatal("restore dropped TriggerEvent")
	}
}
