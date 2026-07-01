//go:build live

package runtime_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime/dispatch"
	"github.com/digitornai/digitorn/internal/toolmw"
)

// onionSource attaches the same pipeline to the filesystem module only.
type onionSource struct{ p dispatch.ToolPipeline }

func (s onionSource) PipelineFor(_, moduleID string) dispatch.ToolPipeline {
	if moduleID == "filesystem" {
		return s.p
	}
	return nil
}

// TestLiveToolMiddleware_OnionWrapsRealToolCall : with a real LLM driving a
// real filesystem tool call, the per-module tool-call onion (audit + dedup)
// runs around it daemon-side. We assert the model actually called the tool,
// the audit layer logged that call, and the tool result (the file's secret)
// flowed back through the onion untouched into the model's answer.
func TestLiveToolMiddleware_OnionWrapsRealToolCall(t *testing.T) {
	f := liveSetup(t)

	if err := os.WriteFile(filepath.Join(f.workspace, "notes.txt"),
		[]byte("the secret number is 4271"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logbuf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pipe := toolmw.Build([]map[string]any{
		{"audit": map[string]any{}},
		{"dedup": map[string]any{"window_seconds": 60.0}},
	}, toolmw.Deps{Logger: lg}, lg)
	if pipe == nil {
		t.Fatal("expected an onion pipeline")
	}
	f.busAdapter.Pipelines = onionSource{p: pipe}

	f.runLive(t, "Use the filesystem read tool to read notes.txt, then tell me the secret number.")

	assertToolCalled(t, f, "filesystem.read")

	log := logbuf.String()
	if !strings.Contains(log, "tool_audit") || !strings.Contains(log, "filesystem") {
		t.Errorf("the audit middleware must have wrapped the real tool call, log:\n%s", log)
	}
	// The result flowed back through the onion intact.
	assertSemantic(t, f, "4271")
}
