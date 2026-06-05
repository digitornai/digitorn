package bash

import (
	"strings"
	"testing"
	"time"
)

// TestResult_FailureSurfacesStderr : a failed command must put its REASON in the
// error field, not just "exit code N". Without this a weak agent fixates on the
// opaque code and flails (the pytest / python -c case). stderr is the cause.
func TestResult_FailureSurfacesStderr(t *testing.T) {
	m := &Module{kind: "powershell"}
	res := cmdResult{ExitCode: 1, Stderr: "Traceback (most recent call last):\n  IndentationError: unexpected indent"}
	out := m.result("python -c ' x=1'", res, nil, time.Minute)

	if out.Success {
		t.Fatal("non-zero exit must be a failure")
	}
	if !strings.Contains(out.Error, "exit code 1") {
		t.Errorf("error should keep the exit code: %q", out.Error)
	}
	if !strings.Contains(out.Error, "IndentationError") {
		t.Errorf("error MUST surface stderr so the agent sees WHY: %q", out.Error)
	}
}

func TestErrorDetail(t *testing.T) {
	if got := errorDetail("boom-err", "ignored-stdout"); got != "boom-err" {
		t.Errorf("stderr must win: %q", got)
	}
	if got := errorDetail("   ", "fallback-out"); got != "fallback-out" {
		t.Errorf("stdout fallback when stderr blank: %q", got)
	}
	if got := errorDetail("", ""); got != "" {
		t.Errorf("empty when no output: %q", got)
	}
	// Tail kept + length-capped so a chatty failure can't flood context
	// (≤1500 content bytes + the 3-byte "…" ellipsis marker).
	if got := errorDetail(strings.Repeat("x", 5000), ""); len(got) > 1503 {
		t.Errorf("must cap the detail, got len=%d", len(got))
	}
}
