package sessionstore

import "testing"

// TestMetaPassthrough proves the channel passthrough fields (entry_agent +
// extra context) survive the full durable path: projection from the
// EventSessionStarted event → live state → snapshot → cold-load restore → and a
// disk round-trip in both snapshot formats. This is what lets a background
// channel trigger's chosen agent + context apply to EVERY turn of the session,
// including after a daemon restart.
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

	// Cold-load restore (the daemon-restart path).
	fresh := &SessionState{}
	hydrateFromSnapshot(fresh, &snap)
	if fresh.EntryAgent != "vip" || fresh.ContextExtra != "Answer tersely." {
		t.Fatalf("restore lost fields: %q / %q", fresh.EntryAgent, fresh.ContextExtra)
	}

	// Disk round-trip, both formats.
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

// TestMetaPassthrough_AbsentByDefault proves the fields are empty for ordinary
// sessions (no Meta override) — behaviour is byte-identical to before.
func TestMetaPassthrough_AbsentByDefault(t *testing.T) {
	s := &SessionState{SessionID: "s2"}
	Apply(s, &Event{Type: EventSessionStarted, SessionID: "s2", Meta: &MetaPayload{Title: "Chat"}})
	if s.EntryAgent != "" || s.ContextExtra != "" {
		t.Fatalf("ordinary session must have empty passthrough: %q / %q", s.EntryAgent, s.ContextExtra)
	}
}
