package meta_test

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"sync"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/background"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// mcpRoutingInner stands in for the production ModuleDispatcher: it routes a tool
// purely by FQN, so an MCP tool (mcp_<server>.<tool>) and a native tool
// (filesystem.read) travel the exact same code path. It records every FQN it is
// asked to run and echoes a tagged result.
type mcpRoutingInner struct {
	mu  sync.Mutex
	got []string
}

func (f *mcpRoutingInner) Dispatch(_ context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	f.mu.Lock()
	f.got = append(f.got, call.Name)
	f.mu.Unlock()
	msg, _ := call.Args["message"].(string)
	return runtime.ToolOutcome{
		Status: "completed",
		Parts:  []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: call.Name + ":" + msg}},
	}
}

func (f *mcpRoutingInner) seen(fqn string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, g := range f.got {
		if g == fqn {
			n++
		}
	}
	return n
}

// TestMCPToolThroughMetaPaths proves an MCP tool is executed by BOTH execute_tool
// and background_run exactly like a native tool — there is no MCP special-casing.
// Each meta path resolves the FQN, applies the SG-4 gate, and forwards to the
// SAME inner dispatcher; an MCP FQN routes identically to a native one. The
// background path uses the REAL background.Manager (dispatching through the same
// dispatcher foreground calls use).
func TestMCPToolThroughMetaPaths(t *testing.T) {
	inner := &mcpRoutingInner{}
	bg := background.New()
	md := &meta.MetaDispatcher{Inner: inner, Background: bg}
	bg.AttachDispatcher(md)

	const mcpFQN = "mcp_demo.search"
	const nativeFQN = "filesystem.read"

	call := func(metaTool, target string, extra map[string]any) runtime.ToolOutcome {
		args := map[string]any{"name": target, "params": map[string]any{"message": "hello"}}
		maps.Copy(args, extra)
		return md.Dispatch(context.Background(), runtime.ToolInvocation{
			Name: "context_builder." + metaTool, Args: args, SessionID: "s1", AppID: "app",
		})
	}

	// --- execute_tool: MCP runs, and identically to a native tool ---
	mcpExec := call("execute_tool", mcpFQN, nil)
	natExec := call("execute_tool", nativeFQN, nil)
	if mcpExec.Status != "completed" {
		t.Fatalf("execute_tool(MCP) status=%q err=%q", mcpExec.Status, mcpExec.Error)
	}
	if got := partsText(mcpExec); got != mcpFQN+":hello" {
		t.Errorf("execute_tool did not run the MCP tool: got %q", got)
	}
	if mcpExec.Status != natExec.Status {
		t.Errorf("execute_tool treats MCP and native differently: mcp=%q native=%q", mcpExec.Status, natExec.Status)
	}

	// --- background_run: MCP launches + runs (settles in the start-up window) ---
	mcpBg := call("background_run", mcpFQN, map[string]any{"settle_seconds": 3.0})
	if state := jsonField(mcpBg, "state"); state != "completed" {
		t.Fatalf("background_run(MCP) state=%q full=%q", state, partsText(mcpBg))
	}
	if result := jsonField(mcpBg, "result"); !strings.Contains(result, mcpFQN+":hello") {
		t.Errorf("background_run did not run the MCP tool; result=%q", result)
	}

	// The inner dispatcher received the MCP FQN, unchanged, from BOTH meta paths.
	if n := inner.seen(mcpFQN); n < 2 {
		t.Errorf("MCP FQN must reach inner from execute_tool AND background_run; saw %d (all=%v)", n, inner.got)
	}
}

func partsText(o runtime.ToolOutcome) string {
	var b strings.Builder
	for _, p := range o.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func jsonField(o runtime.ToolOutcome, key string) string {
	var m map[string]any
	if json.Unmarshal([]byte(partsText(o)), &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}
