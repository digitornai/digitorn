package sessionstore

import (
	"os"
	"testing"
	"time"
)

func txMsgEvent(seq uint64, sid, role, text string) Event {
	return Event{
		Seq:        seq,
		TsUnixNano: time.Now().UnixNano() + int64(seq),
		SessionID:  sid,
		Type:       map[string]EventType{"user": EventUserMessage, "assistant": EventAssistantMessage, "system": EventSystemMessage}[role],
		Message:    &MessagePayload{Role: role, Parts: []MessagePart{{Type: PartTypeText, Text: text}}},
	}
}

func writeEvents(t *testing.T, p Paths, sid string, evs ...Event) {
	t.Helper()
	if err := os.MkdirAll(p.SessionDir(sid), 0o700); err != nil {
		t.Fatal(err)
	}
	w, err := OpenJSONLAppend(p.EventsFile(sid), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(evs); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	w.Close()
}

// Events only, no snapshot : every message event is returned in seq order ;
// non-message events (turn lifecycle) are skipped.
func TestReadTranscript_EventsOnly(t *testing.T) {
	p := NewPaths(t.TempDir())
	sid := "s"
	writeEvents(t, p, sid,
		txMsgEvent(1, sid, "user", "hello"),
		Event{Seq: 2, SessionID: sid, Type: EventTurnStarted, Turn: &TurnPayload{TurnID: "t"}},
		txMsgEvent(3, sid, "assistant", "hi there"),
	)
	got, err := ReadTranscript(p, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 messages, got %d: %+v", len(got), got)
	}
	if got[0].Seq != 1 || got[0].Role != "user" || got[0].Content != "hello" {
		t.Errorf("msg[0] wrong: %+v", got[0])
	}
	if got[1].Seq != 3 || got[1].Role != "assistant" || got[1].Content != "hi there" {
		t.Errorf("msg[1] wrong: %+v", got[1])
	}
}

// Snapshot (full, up to its cutoff) + post-cutoff JSONL events. Pre-cutoff
// events still present in an untruncated JSONL must NOT be duplicated.
func TestReadTranscript_SnapshotPlusEvents_NoDup(t *testing.T) {
	p := NewPaths(t.TempDir())
	sid := "s"
	dir := p.SessionDir(sid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Snapshot carries the first two messages, cutoff at seq 2.
	snap := SessionSnapshot{
		Version:   snapshotVersion,
		SessionID: sid,
		FirstSeq:  1,
		LastSeq:   2,
		CutoffSeq: 2,
		Messages: []Message{
			{Seq: 1, Role: "user", Content: "first"},
			{Seq: 2, Role: "assistant", Content: "second"},
		},
	}
	if _, err := WriteSnapshotAtomic(dir, snap, SnapshotJSON, false); err != nil {
		t.Fatal(err)
	}
	// JSONL still holds seq 1,2 (deferred truncate) plus a fresh seq 3.
	writeEvents(t, p, sid,
		txMsgEvent(1, sid, "user", "first"),
		txMsgEvent(2, sid, "assistant", "second"),
		txMsgEvent(3, sid, "user", "third"),
	)
	got, err := ReadTranscript(p, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 messages (no dup of seq 1,2), got %d: %+v", len(got), got)
	}
	wantSeq := []uint64{1, 2, 3}
	for i, w := range wantSeq {
		if got[i].Seq != w {
			t.Errorf("msg[%d].Seq = %d, want %d", i, got[i].Seq, w)
		}
	}
	if got[2].Content != "third" {
		t.Errorf("post-cutoff message wrong: %+v", got[2])
	}
}

// Empty session : no snapshot, no events → no messages, no error.
func TestReadTranscript_Empty(t *testing.T) {
	p := NewPaths(t.TempDir())
	got, err := ReadTranscript(p, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 messages, got %d", len(got))
	}
}
