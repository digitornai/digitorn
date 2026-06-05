package sessionstore

import (
	"fmt"
	"runtime"
	"testing"
)

// TestScale_PerSessionMemoryFootprint measures the real heap cost of an
// in-memory SessionState at a few message counts, then extrapolates to
// 10,000,000 sessions. This is the physical ceiling for how many sessions
// one process can hold resident — independent of any logical cap.
func TestScale_PerSessionMemoryFootprint(t *testing.T) {
	const N = 20000
	for _, msgs := range []int{0, 5, 50} {
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)

		sessions := make([]*SessionState, N)
		for i := 0; i < N; i++ {
			s := &SessionState{
				SessionID: fmt.Sprintf("session-id-%012d", i),
				AppID:     "some-app-id",
				UserID:    fmt.Sprintf("user-%010d", i),
			}
			for j := 0; j < msgs; j++ {
				s.Messages = append(s.Messages, Message{
					Seq:  uint64(j + 1),
					Role: "assistant",
					// Unique content forces a real allocation per message,
					// mirroring real transcripts (no shared string literal).
					Content: fmt.Sprintf("assistant reply %d in session %d — a typical line of model output", j, i),
				})
			}
			sessions[i] = s
		}

		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)

		perSession := float64(after.HeapAlloc-before.HeapAlloc) / float64(N)
		t.Logf("msgs=%-3d → %8.0f bytes/session → 10M sessions = %6.1f GB (state only)",
			msgs, perSession, perSession*10_000_000/1e9)
		runtime.KeepAlive(sessions)
	}
}
