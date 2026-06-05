//go:build windows

package worker

import "testing"

// TestKillOnCloseJob_CreatesWithKillFlag proves the Windows Job Object wiring is
// correct on this machine : CreateJobObject + SetInformationJobObject with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE succeed and return a usable handle. Once
// the flag is set, the OS guarantees every assigned process dies when the job's
// last handle (held by the daemon for its whole life) closes — so a worker can
// never outlive the daemon. The end-to-end kill-on-crash is an OS guarantee
// exercised live; this pins that the API calls don't silently fail.
func TestKillOnCloseJob_CreatesWithKillFlag(t *testing.T) {
	h, err := killOnCloseJob()
	if err != nil {
		t.Fatalf("killOnCloseJob: %v", err)
	}
	if h == 0 {
		t.Fatal("killOnCloseJob returned a nil handle")
	}
	// Idempotent : the sync.Once means repeated calls hand back the same job.
	h2, err := killOnCloseJob()
	if err != nil || h2 != h {
		t.Fatalf("job not stable across calls: h=%v h2=%v err=%v", h, h2, err)
	}
}

// TestTrackChild_NilSafe : tracking a nil / not-started command is a safe no-op
// (the spawn path calls it unconditionally after Start).
func TestTrackChild_NilSafe(t *testing.T) {
	trackChild(nil) // must not panic
}
