package bash

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

// Reproduce the real bug: a stdin pipe is attached (background/interactive
// dispatch) AND `input` is provided. Today detached.go prefers the pipe and
// IGNORES input; the pipe only closes at task end, so a read-until-EOF command
// deadlocks. Expected AFTER fix: input is delivered + EOF, command exits fast.
func TestDetached_InputWinsOverIdlePipe(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	pr, pw := io.Pipe() // an idle interactive pipe (never fed, never closed by us)
	defer pw.Close()
	ctx := tool.WithStdinPipe(context.Background(), pr)
	start := time.Now()
	res, rerr := runDetached(ctx, "bash", bash, "cat", "", os.Environ(), 65536, "Paul", 3*time.Second, 0)
	el := time.Since(start)
	t.Logf("elapsed=%v stdout=%q timedOut=%v err=%v", el, res.Stdout, res.TimedOut, rerr)
	if el > 2*time.Second || res.TimedOut {
		t.Fatalf("DEADLOCK: input ignored / pipe not EOF'd (%v)", el)
	}
	if !strings.Contains(res.Stdout, "Paul") {
		t.Fatalf("input not delivered when a pipe is present: %q", res.Stdout)
	}
}
