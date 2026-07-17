package sessionstore

import "testing"

func TestProjection_ContextTokensBreakdown(t *testing.T) {
	s := NewSessionState("s1")
	Apply(s, &Event{
		Seq: 1, Type: EventContextTokens, SessionID: "s1",
		CtxTokens: &ContextTokensPayload{Total: 1000, System: 300, Tools: 500, Messages: 200},
	})
	if s.ContextTokens != 1000 {
		t.Errorf("gauge total = %d, want 1000", s.ContextTokens)
	}
	if s.ContextSystemTokens != 300 || s.ContextToolsTokens != 500 || s.ContextMessageTokens != 200 {
		t.Errorf("breakdown = sys:%d tools:%d msgs:%d, want 300/500/200",
			s.ContextSystemTokens, s.ContextToolsTokens, s.ContextMessageTokens)
	}
	snap := s.Snapshot()
	if snap.ContextToolsTokens != 500 {
		t.Errorf("snapshot lost the tools bucket: %d", snap.ContextToolsTokens)
	}
}
