package bash

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

// cat reads stdin until EOF. With input="Paul", the stdin pipe MUST be closed
// after the input so cat sees EOF and exits — otherwise it blocks until timeout.
func TestModule_InputClosesStdinEOF(t *testing.T) {
	m := testModule(t)
	ctx := tool.WithIdentity(context.Background(), tool.Identity{AppID: "app", SessionID: "s"})
	raw, _ := json.Marshal(runParams{Command: "cat", Input: "Paul", TimeoutSeconds: 4})
	start := time.Now()
	res, err := m.run(ctx, raw)
	el := time.Since(start)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	rr, _ := res.Data.(runResult)
	t.Logf("elapsed=%v stdout=%q timedOut=%v", el, rr.Stdout, rr.TimedOut)
	if el > 2*time.Second || rr.TimedOut {
		t.Fatalf("cat with input BLOCKED (%v, timedOut=%v) — stdin not EOF'd", el, rr.TimedOut)
	}
	if !strings.Contains(rr.Stdout, "Paul") {
		t.Fatalf("input not delivered: stdout=%q", rr.Stdout)
	}
}
