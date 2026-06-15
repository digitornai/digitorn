package sessionstore

import "testing"

// TestSeenClientMessage proves the idempotency key the append path relies on: a user
// message carrying a client_message_id is tracked (→ a re-delivery is detected), while
// keyless messages and unknown ids are not — so the handler skips only true duplicates.
func TestSeenClientMessage(t *testing.T) {
	s := &SessionState{SessionID: "s1"}
	Apply(s, &Event{
		Type: EventUserMessage, SessionID: "s1", Seq: 5,
		Message: &MessagePayload{Role: "user", Content: "hi", ClientMessageID: "k1"},
	})
	if seq, ok := s.SeenClientMessage("k1"); !ok || seq != 5 {
		t.Fatalf("k1 must be seen at seq 5, got seq=%d ok=%v", seq, ok)
	}
	if _, ok := s.SeenClientMessage("other"); ok {
		t.Fatal("an unknown id must not be seen")
	}
	if _, ok := s.SeenClientMessage(""); ok {
		t.Fatal("an empty id is never seen")
	}
	// A keyless message must not pollute the dedup set.
	Apply(s, &Event{
		Type: EventUserMessage, SessionID: "s1", Seq: 6,
		Message: &MessagePayload{Role: "user", Content: "no key"},
	})
	if len(s.SeenClientMsgIDs) != 1 {
		t.Fatalf("only keyed messages are tracked, got %d", len(s.SeenClientMsgIDs))
	}
}
