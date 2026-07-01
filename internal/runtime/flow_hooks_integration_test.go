package runtime_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/hooks"
)

func flowApp(tool string, params map[string]any) *appmgr.RuntimeApp {
	app := realDispatchApp()
	app.Definition.Flow = &schema.FlowConfig{
		ID:    "test_flow",
		Entry: "read_node",
		Nodes: []schema.FlowNode{
			{
				ID:     "read_node",
				Type:   "tool",
				Tool:   tool,
				Params: params,
				Routes: []schema.FlowRoute{{When: "default", To: "end"}},
			},
		},
	}
	return app
}

func TestFlow_ToolNodeFiresHooks(t *testing.T) {
	tmp := t.TempDir()
	content := "flow tool hook proof"
	if err := os.WriteFile(filepath.Join(tmp, "f.txt"), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	app := flowApp("filesystem.read", map[string]any{"path": "f.txt"})
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	logger := &rt4Logger{}
	eng := hooks.New(
		[]schema.Hook{
			makeLogHook("pre", schema.HookEventToolStart, "flow_tool_pre"),
			makeLogHook("post", schema.HookEventToolEnd, "flow_tool_post"),
		},
		hooks.ActionDeps{Logger: logger},
	)
	eng.Async = false

	cb, disp := buildRealBus(t, tmp)
	e := newEngine(t, apps, sess, &stubLLM{resp: &llm.ChatResponse{Content: "x"}})
	e.Context = cb
	e.Dispatcher = disp
	e.Hooks = &hookSourceWith{eng: eng}

	res, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := logger.count("flow_tool_pre"); got != 1 {
		t.Errorf("tool_start hook fired %d times around flow tool node, want 1", got)
	}
	if got := logger.count("flow_tool_post"); got != 1 {
		t.Errorf("tool_end hook fired %d times around flow tool node, want 1", got)
	}
	if res == nil || res.Content == "" {
		t.Fatal("expected flow tool node output as result content")
	}
}

func TestFlow_ToolNodeGateBlocks(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "f.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	app := flowApp("filesystem.read", map[string]any{"path": "f.txt"})
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	eng := hooks.New(
		[]schema.Hook{{
			ID:        "deny",
			On:        schema.HookEventToolStart,
			Condition: schema.HookCondition{Type: "always"},
			Action: schema.HookAction{
				Type:   "gate",
				Params: map[string]any{"allow": false, "reason": "flow gate denies read"},
			},
		}},
		hooks.ActionDeps{},
	)
	eng.Async = false

	cb, disp := buildRealBus(t, tmp)
	e := newEngine(t, apps, sess, &stubLLM{resp: &llm.ChatResponse{Content: "x"}})
	e.Context = cb
	e.Dispatcher = disp
	e.Hooks = &hookSourceWith{eng: eng}

	_, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})
	if err == nil {
		t.Fatal("expected the tool_start gate to block the flow tool node")
	}
	if !strings.Contains(err.Error(), "blocked by hook gate") {
		t.Fatalf("expected hook-gate block error, got %v", err)
	}
}
