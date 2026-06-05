package sessionstore

import (
	"fmt"
	"testing"
)

func TestProjection_FactDedup(t *testing.T) {
	s := NewSessionState("s1")
	add := func(fact string) {
		Apply(s, &Event{Type: EventMemoryFactAdded, SessionID: "s1", Memory: &MemoryPayload{Fact: fact}})
	}
	add("Test command: go test ./...")
	add("test command: GO TEST ./...") // case/space variant → dedup
	add("  Test command: go test ./...  ")
	add("Bug is in auth/validate.go:42")

	snap := s.Snapshot()
	if len(snap.Facts) != 2 {
		t.Fatalf("dedup failed: got %d facts, want 2 : %v", len(snap.Facts), snap.Facts)
	}
}

func TestProjection_FactCap(t *testing.T) {
	s := NewSessionState("s1")
	for i := 0; i < maxKeyFacts+50; i++ {
		Apply(s, &Event{Type: EventMemoryFactAdded, SessionID: "s1",
			Memory: &MemoryPayload{Fact: fmt.Sprintf("fact number %d", i)}})
	}
	snap := s.Snapshot()
	if len(snap.Facts) != maxKeyFacts {
		t.Fatalf("cap failed: got %d facts, want %d", len(snap.Facts), maxKeyFacts)
	}
	// Oldest evicted, newest kept (FIFO).
	if snap.Facts[0] != fmt.Sprintf("fact number %d", 50) {
		t.Errorf("oldest fact not evicted: first=%q", snap.Facts[0])
	}
	if snap.Facts[len(snap.Facts)-1] != fmt.Sprintf("fact number %d", maxKeyFacts+49) {
		t.Errorf("newest fact missing: last=%q", snap.Facts[len(snap.Facts)-1])
	}
}
