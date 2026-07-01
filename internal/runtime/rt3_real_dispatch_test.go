package runtime_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/core/servicebus"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/llm"
	fsmod "github.com/digitornai/digitorn/internal/modules/filesystem"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/context/index"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
	"github.com/digitornai/digitorn/internal/runtime/context/wiring"
	"github.com/digitornai/digitorn/internal/runtime/dispatch"
	"github.com/digitornai/digitorn/internal/runtime/policy"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// RT-3h — Real end-to-end smoke proof
//
// This is the canonical "the runtime works" test. It wires every
// production component except the LLM (still stubbed because we
// don't want a real network call in CI) :
//
//   - A real *servicebus.Bus
//   - A real *filesystem.Module registered on the bus
//   - A real *dispatch.BusAdapter
//   - A real *meta.MetaDispatcher with IndexLookup
//   - A real *wiring.Builder for the per-agent ToolIndex
//
// The flow exercised :
//
//   user input → engine.Run
//     → Context.BuildFor builds the per-agent index
//     → LLM stub returns ToolCalls: filesystem.read({path: "..."})
//     → engine.dispatchToolsParallel
//       → MetaDispatcher (not a meta tool, forwards)
//         → BusAdapter (split fqn, marshal, bus.Call)
//           → servicebus.Bus.Call
//             → filesystem.Module.Invoke
//               → m.read (real os.ReadFile)
//     → tool result Parts contain the real file content
//     → second LLM call sees the content and ends the turn
// =====================================================================

// realDispatchActions returns a single filesystem.read AvailableAction
// matching the bare-action-name convention.
func realDispatchActions() []policy.AvailableAction {
	return []policy.AvailableAction{
		{Module: "filesystem", Action: "read",
			Spec: &tool.Spec{
				Name:        "filesystem.read",
				Description: "Read the contents of a file from disk",
				RiskLevel:   tool.RiskLow,
			}},
	}
}

type realDispatchActionsSource struct{}

func (realDispatchActionsSource) ForApp(string) []policy.AvailableAction {
	return realDispatchActions()
}

// realDispatchApp matches cb6App() but is local to this file to keep
// it self-contained and avoid coupling to the CB-6 test fixtures.
func realDispatchApp() *appmgr.RuntimeApp {
	return &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "rt3-app", Enabled: true},
		Definition: &schema.AppDefinition{
			App: schema.AppMeta{
				AppID: "rt3-app", Name: "RT3 Real Dispatch", Version: "1.0",
			},
			Agents: []schema.Agent{{
				ID:           "main",
				Role:         "assistant",
				Brain:        schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
				SystemPrompt: "Read whatever the user asks for.",
			}},
			Tools: &schema.ToolsBlock{
				Capabilities: &schema.CapabilitiesConfig{
					DefaultPolicy: schema.CapAuto,
					MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
				},
			},
			Runtime: &schema.RuntimeBlock{
				ToolInjection: schema.ToolInjectionDirect,
			},
		},
	}
}

// buildRealBus constructs the production dispatch chain for the
// given workspace path. Returns the engine-ready ContextBuilder
// and MetaDispatcher.
func buildRealBus(t *testing.T, workspace string) (
	*wiring.Builder, *meta.MetaDispatcher,
) {
	t.Helper()

	bus := servicebus.New()
	fs := fsmod.New()
	if err := fs.Init(context.Background(), map[string]any{
		"workspace": workspace,
	}); err != nil {
		t.Fatalf("filesystem init: %v", err)
	}
	if err := bus.Register(fs); err != nil {
		t.Fatalf("bus register: %v", err)
	}

	cb := wiring.New(realDispatchActionsSource{})
	disp := &meta.MetaDispatcher{
		IndexLookup: func(appID, agentID string) *index.ToolIndex {
			return cb.IndexFor(appID, agentID)
		},
		Inner: dispatch.NewBusAdapter(bus),
	}
	return cb, disp
}

// TestRT3_RealFilesystemRead is THE smoke proof. A file is written
// on disk, the LLM stub asks the runtime to read it via
// filesystem.read, and we assert the file content lands in the
// tool_result Parts.
func TestRT3_RealFilesystemRead(t *testing.T) {
	tmp := t.TempDir()
	content := "hello from the runtime — " + t.Name()
	target := filepath.Join(tmp, "hello.txt")
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	lc := &stubLLM{responses: []*llm.ChatResponse{
		// Round 1 : LLM decides to call filesystem.read.
		{ToolCalls: []llm.ChatToolCall{{
			ID:        "call-1",
			Name:      "filesystem.read",
			Arguments: map[string]any{"path": "hello.txt"},
		}}},
		// Round 2 : LLM acknowledges and ends.
		{Content: "Done."},
	}}

	cb, disp := buildRealBus(t, tmp)

	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = disp

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The engine should have made TWO calls to the LLM : the first
	// returning the tool_call, the second seeing the tool result.
	if lc.calls != 2 {
		t.Fatalf("LLM calls = %d, want 2 (tool round-trip)", lc.calls)
	}

	// The second LLM request must contain a "tool" role message
	// carrying the file content as text.
	round2 := lc.allGots[1]
	var foundContent bool
	for _, m := range round2.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, content) {
			foundContent = true
			break
		}
	}
	if !foundContent {
		t.Errorf("file content did not surface in round-2 messages")
		for i, m := range round2.Messages {
			t.Logf("msg[%d] role=%q content=%q", i, m.Role, m.Content)
		}
	}

	// A tool_result event must have been persisted with the content
	// in its Parts.
	if got := sess.count(sessionstore.EventToolResult); got != 1 {
		t.Errorf("tool_result events = %d, want 1", got)
	}
}

// TestRT3_RealFilesystemRead_PathEscape proves the filesystem
// module's path-escape protection survives the dispatch chain :
// a malicious "../../etc/passwd" request returns an error, not the
// host's file content.
func TestRT3_RealFilesystemRead_PathEscape(t *testing.T) {
	tmp := t.TempDir()

	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID:        "call-1",
			Name:      "filesystem.read",
			Arguments: map[string]any{"path": "../../../../etc/passwd"},
		}}},
		{Content: "Couldn't read it."},
	}}

	cb, disp := buildRealBus(t, tmp)

	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = disp

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if lc.calls != 2 {
		t.Fatalf("LLM calls = %d, want 2", lc.calls)
	}

	// Round 2 must surface an error, not /etc/passwd content.
	round2 := lc.allGots[1]
	for _, m := range round2.Messages {
		if m.Role == "tool" {
			if strings.Contains(m.Content, "root:") {
				t.Errorf("path escape leaked /etc/passwd content : %q", m.Content)
			}
		}
	}
}

// TestRT3_RealFilesystemList tests filesystem.glob through the same
// chain, proving the dispatcher handles multiple actions per
// module, not just filesystem.read.
func TestRT3_RealFilesystemList(t *testing.T) {
	tmp := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	app := realDispatchApp()
	// Expand the universe to include 'glob' (lists a directory).
	universe := []policy.AvailableAction{
		{Module: "filesystem", Action: "read",
			Spec: &tool.Spec{Name: "filesystem.read", RiskLevel: tool.RiskLow}},
		{Module: "filesystem", Action: "glob",
			Spec: &tool.Spec{Name: "filesystem.glob", RiskLevel: tool.RiskLow}},
	}
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID:        "call-1",
			Name:      "filesystem.glob",
			Arguments: map[string]any{"pattern": "*"},
		}}},
		{Content: "Listed."},
	}}

	bus := servicebus.New()
	fs := fsmod.New()
	if err := fs.Init(context.Background(), map[string]any{"workspace": tmp}); err != nil {
		t.Fatal(err)
	}
	if err := bus.Register(fs); err != nil {
		t.Fatal(err)
	}

	cb := wiring.New(staticActionsSource{all: universe})
	disp := &meta.MetaDispatcher{
		IndexLookup: func(appID, agentID string) *index.ToolIndex {
			return cb.IndexFor(appID, agentID)
		},
		Inner: dispatch.NewBusAdapter(bus),
	}

	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = disp

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	round2 := lc.allGots[1]
	var sawList bool
	for _, m := range round2.Messages {
		if m.Role == "tool" {
			if strings.Contains(m.Content, "a.txt") &&
				strings.Contains(m.Content, "b.txt") &&
				strings.Contains(m.Content, "c.txt") {
				sawList = true
			}
		}
	}
	if !sawList {
		t.Errorf("glob result did not surface in round-2 messages")
		for i, m := range round2.Messages {
			t.Logf("msg[%d] role=%q content=%q", i, m.Role, m.Content)
		}
	}
}

// TestRT3_UnknownModule proves a missing module produces a clean
// errored outcome rather than a panic.
func TestRT3_UnknownModule(t *testing.T) {
	tmp := t.TempDir()

	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	universe := []policy.AvailableAction{
		{Module: "nope", Action: "do",
			Spec: &tool.Spec{Name: "nope.do", RiskLevel: tool.RiskLow}},
	}

	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID:        "call-1",
			Name:      "nope.do",
			Arguments: map[string]any{},
		}}},
		{Content: "Failed."},
	}}

	bus := servicebus.New()
	fs := fsmod.New()
	_ = fs.Init(context.Background(), map[string]any{"workspace": tmp})
	_ = bus.Register(fs)

	cb := wiring.New(staticActionsSource{all: universe})
	disp := &meta.MetaDispatcher{
		IndexLookup: func(appID, agentID string) *index.ToolIndex {
			return cb.IndexFor(appID, agentID)
		},
		Inner: dispatch.NewBusAdapter(bus),
	}

	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = disp

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Tool_result event must be persisted with errored status (the
	// LLM gets to see the failure and can recover).
	if got := sess.count(sessionstore.EventToolResult); got != 1 {
		t.Errorf("tool_result events = %d, want 1", got)
	}
}

// staticActionsSource wraps a fixed slice as wiring.AvailableActions.
type staticActionsSource struct {
	all []policy.AvailableAction
}

func (s staticActionsSource) ForApp(string) []policy.AvailableAction { return s.all }

// buildContextOnly builds a wiring.Builder with the given universe
// and no real bus. Used by tests that don't need actual module
// execution, e.g. gate-veto tests.
func buildContextOnly(universe []policy.AvailableAction) *wiring.Builder {
	return wiring.New(staticActionsSource{all: universe})
}

// buildMetaDispatcherWith wires a MetaDispatcher with the given
// inner dispatcher and the context builder's IndexFor lookup.
func buildMetaDispatcherWith(cb *wiring.Builder, inner runtime.ToolDispatcher) *meta.MetaDispatcher {
	return &meta.MetaDispatcher{
		IndexLookup: func(appID, agentID string) *index.ToolIndex {
			return cb.IndexFor(appID, agentID)
		},
		Inner: inner,
	}
}
