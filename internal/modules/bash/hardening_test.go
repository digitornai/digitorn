package bash

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHardening_HugeSingleLine_NoWedge : a command emitting a single line larger
// than the scanner's max token (no newline) must NOT wedge the shell until the
// timeout — the completion marker (a later line) must still be read. Probes the
// bufio.Scanner ErrTooLong failure mode.
func TestHardening_HugeSingleLine_NoWedge(t *testing.T) {
	sh := testShell(t, 1<<20)
	// ~40 MB of 'x' with NO trailing newline -> one oversized line.
	res, err := sh.run(context.Background(), `head -c 40000000 /dev/zero | tr '\0' x`, 30*time.Second)
	if err != nil {
		t.Fatalf("huge single line wedged/failed: err=%v timedOut=%v", err, res.TimedOut)
	}
	if res.TimedOut {
		t.Fatalf("huge single line wedged until timeout (marker lost) — exit=%d", res.ExitCode)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d, want 0 (stdout len=%d)", res.ExitCode, len(res.Stdout))
	}
}

// TestHardening_ExitCodeVariety : exotic exit codes from a CHILD process
// propagate verbatim while the persistent shell survives (a subshell exit must
// not kill the session — only a bare top-level `exit` does, by design).
func TestHardening_ExitCodeVariety(t *testing.T) {
	sh := testShell(t, 1<<20)
	for _, code := range []int{2, 42, 127, 255} {
		res, err := sh.run(context.Background(), "(exit "+strconv.Itoa(code)+")", 10*time.Second)
		if err != nil {
			t.Fatalf("exit %d: err=%v", code, err)
		}
		if res.ExitCode != code {
			t.Fatalf("exit code = %d, want %d", res.ExitCode, code)
		}
	}
}

// TestHardening_RapidSequential : many quick commands on one persistent shell
// keep correct, non-bleeding output (each result is its own).
func TestHardening_RapidSequential(t *testing.T) {
	sh := testShell(t, 1<<20)
	for i := 0; i < 40; i++ {
		want := "tok" + strconv.Itoa(i)
		res, err := sh.run(context.Background(), "echo "+want, 10*time.Second)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if strings.TrimSpace(res.Stdout) != want {
			t.Fatalf("iter %d: stdout=%q want %q (output bleed?)", i, res.Stdout, want)
		}
	}
}
