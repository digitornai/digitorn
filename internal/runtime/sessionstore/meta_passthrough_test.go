package sessionstore

import "testing"

func TestMetaPassthrough(t *testing.T) {
	s := &SessionState{SessionID: "sess1"}
	Apply(s, &Event{
		Type:      EventSessionStarted,
		SessionID: "sess1",
		Meta:      &MetaPayload{EntryAgent: "vip", ContextExtra: "Answer tersely."},
	})
	if s.EntryAgent != "vip" || s.ContextExtra != "Answer tersely." {
		t.Fatalf("projection lost fields: %q / %q", s.EntryAgent, s.ContextExtra)
	}

	snap := s.Snapshot()
	if snap.EntryAgent != "vip" || snap.ContextExtra != "Answer tersely." {
		t.Fatalf("snapshot lost fields: %+v", snap)
	}

	fresh := &SessionState{}
	hydrateFromSnapshot(fresh, &snap)
	if fresh.EntryAgent != "vip" || fresh.ContextExtra != "Answer tersely." {
		t.Fatalf("restore lost fields: %q / %q", fresh.EntryAgent, fresh.ContextExtra)
	}

	for _, format := range []SnapshotFormat{SnapshotJSON, SnapshotBinary} {
		dir := t.TempDir()
		if _, err := WriteSnapshotAtomic(dir, snap, format, false); err != nil {
			t.Fatalf("write (%d): %v", format, err)
		}
		got, _, err := ReadSnapshot(dir)
		if err != nil || got == nil {
			t.Fatalf("read (%d): %v", format, err)
		}
		if got.EntryAgent != "vip" || got.ContextExtra != "Answer tersely." {
			t.Fatalf("disk round-trip (%d) lost fields: %+v", format, got)
		}
	}
}

func TestMetaPassthrough_AbsentByDefault(t *testing.T) {
	s := &SessionState{SessionID: "s2"}
	Apply(s, &Event{Type: EventSessionStarted, SessionID: "s2", Meta: &MetaPayload{Title: "Chat"}})
	if s.EntryAgent != "" || s.ContextExtra != "" {
		t.Fatalf("ordinary session must have empty passthrough: %q / %q", s.EntryAgent, s.ContextExtra)
	}
}
