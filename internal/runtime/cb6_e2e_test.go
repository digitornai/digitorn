package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/context/index"
	"github.com/mbathepaul/digitorn/internal/runtime/context/meta"
	"github.com/mbathepaul/digitorn/internal/runtime/context/wiring"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// CB-6 — E2E proof that the wired Engine consumes the
// context_builder pipeline end-to-end :
//
//   - Tools list shipped to the LLM includes the 10 builtins +
//     the agent's domain tools, filtered by the gates.
//   - System prompt contains the identity + tool instructions +
//     user prompt last (the 9-section assembly).
//   - A meta-tool call (e.g. search_tools) is handled by the
//     MetaDispatcher (CB-3) without ever reaching the inner
//     dispatcher.
//   - A domain tool call (filesystem.read) traverses
//     MetaDispatcher → SG-4 → DefaultPolicyEvaluator → inner.

// cb6Actions wraps a static slice as wiring.AvailableActions.
type cb6Actions struct {
	all []policy.AvailableAction
}

func (a *cb6Actions) ForApp(string) []policy.AvailableAction {
	return a.all
}

// cb6App returns a fully-populated approval-bot-style app with one
// filesystem tool universe.
func cb6App() *appmgr.RuntimeApp {
	return &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "cb6-app", Enabled: true},
		Definition: &schema.AppDefinition{
			App: schema.AppMeta{
				AppID: "cb6-app", Name: "CB6 Test App", Version: "1.0",
			},
			Agents: []schema.Agent{{
				ID:           "main",
				Role:         "assistant",
				Brain:        schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
				SystemPrompt: "USER_PROMPT_MARKER",
			}},
			Tools: &schema.ToolsBlock{
				Capabilities: &schema.CapabilitiesConfig{
					DefaultPolicy: schema.CapAuto,
					MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
				},
			},
			Runtime: &schema.RuntimeBlock{
				ToolInjection: schema.ToolInjectionDirect, // force direct
			},
		},
	}
}

func cb6Universe() []policy.AvailableAction {
	return []policy.AvailableAction{
		{Module: "filesystem", Action: "read",
			Spec: &tool.Spec{
				Name:        "filesystem.read",
				Description: "Read the contents of a file from disk",
				RiskLevel:   tool.RiskLow,
			}},
	}
}

// TestCB6_Engine_ShipsContextBuilderToolList : the Engine actually
// sends the context_builder's tools (builtins + filesystem.read)
// to the LLM.
func TestCB6_Engine_ShipsContextBuilderToolList(t *testing.T) {
	app := cb6App()
	apps := &stubApps{app: app}
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{Content: "done"},
	}}

	e := newEngine(t, apps, sess, lc)
	e.Context = wiring.New(&cb6Actions{all: cb6Universe()})

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "cb6-app", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if lc.got == nil {
		t.Fatal("LLM not called")
	}
	names := make(map[string]bool, len(lc.got.Tools))
	for _, ts := range lc.got.Tools {
		names[ts.Name] = true
	}
	// Names are sanitized to the OpenAI-compatible underscored form
	// at the planner output (filesystem.read → filesystem__read)
	// per docs-site/language/04-tools.md "Tool name sanitization".
	// cb6App has a few filesystem tools → direct mode : only the execution
	// primitives are injected (the discovery meta-tools would be pollution).
	for _, want := range []string{
		"context_builder__run_parallel",
		"context_builder__background_run",
		"filesystem__read",
	} {
		if !names[want] {
			t.Errorf("missing %s in LLM tools list", want)
		}
	}
}

// TestCB6_Engine_AssemblesSystemPrompt_WithUserLast : the system
// prompt the Engine sends to the LLM is the 9-section assembly,
// and the user_prompt block is the LAST line.
func TestCB6_Engine_AssemblesSystemPrompt_WithUserLast(t *testing.T) {
	app := cb6App()
	apps := &stubApps{app: app}
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{Content: "done"},
	}}

	e := newEngine(t, apps, sess, lc)
	e.Context = wiring.New(&cb6Actions{all: cb6Universe()})

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "cb6-app", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Find the system message in the chat request.
	var sysContent string
	for _, m := range lc.got.Messages {
		if m.Role == "system" {
			sysContent = m.Content
			break
		}
	}
	if sysContent == "" {
		t.Fatal("no system message in LLM request")
	}
	if !strings.Contains(sysContent, `You are agent "main"`) {
		t.Errorf("identity missing : %q", sysContent)
	}
	if !strings.Contains(sysContent, "CB6 Test App") {
		t.Errorf("app name missing : %q", sysContent)
	}
	if !strings.HasSuffix(sysContent, "USER_PROMPT_MARKER") {
		tail := sysContent
		if len(tail) > 80 {
			tail = tail[len(tail)-80:]
		}
		t.Errorf("user prompt not last : tail = %q", tail)
	}
}

// recordingInner stores every Dispatch call it receives, so the
// CB-6 E2E tests can verify the meta-dispatcher forwarded the
// right argument shape.
type recordingInner struct {
	count int
	calls []runtime.ToolInvocation
}

func (r *recordingInner) Dispatch(_ context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	r.count++
	r.calls = append(r.calls, call)
	return runtime.ToolOutcome{
		Status: "completed",
		Parts: []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeText, Text: "inner ok"},
		},
	}
}

// TestCB6_MetaToolCall_HandledLocally : when the LLM calls
// context_builder.list_categories, the MetaDispatcher handles it
// and the inner dispatcher is NEVER reached.
func TestCB6_MetaToolCall_HandledLocally(t *testing.T) {
	app := cb6App()
	apps := &stubApps{app: app}
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "context_builder.list_categories",
		}}},
		{Content: "I see filesystem."},
	}}

	inner := &recordingInner{}
	idx := index.NewBuilder().Build(true, app.Definition.Tools.Capabilities,
		&app.Definition.Agents[0], cb6Universe())
	disp := &meta.MetaDispatcher{
		IndexLookup: func(_, _ string) *index.ToolIndex { return idx },
		Inner:       inner,
	}

	e := newEngine(t, apps, sess, lc)
	e.Context = wiring.New(&cb6Actions{all: cb6Universe()})
	e.Dispatcher = disp

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "cb6-app", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if inner.count != 0 {
		t.Errorf("inner called %d times for meta-tool, want 0", inner.count)
	}
	if got := sess.countAppend(sessionstore.EventToolResult); got != 1 {
		t.Errorf("tool_result events = %d, want 1", got)
	}
}

// TestCB6_DomainToolCall_RoutesViaInner : LLM calls filesystem.read
// → MetaDispatcher forwards to inner.
func TestCB6_DomainToolCall_RoutesViaInner(t *testing.T) {
	app := cb6App()
	apps := &stubApps{app: app}
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID:        "c1",
			Name:      "filesystem.read",
			Arguments: map[string]any{"path": "/etc/hosts"},
		}}},
		{Content: "OK"},
	}}

	inner := &recordingInner{}
	idx := index.NewBuilder().Build(true, app.Definition.Tools.Capabilities,
		&app.Definition.Agents[0], cb6Universe())
	disp := &meta.MetaDispatcher{
		IndexLookup: func(_, _ string) *index.ToolIndex { return idx },
		Inner:       inner,
	}

	e := newEngine(t, apps, sess, lc)
	e.Context = wiring.New(&cb6Actions{all: cb6Universe()})
	e.Dispatcher = disp

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "cb6-app", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if inner.count != 1 {
		t.Fatalf("inner count = %d, want 1", inner.count)
	}
	if inner.calls[0].Name != "filesystem.read" {
		t.Errorf("inner saw %q, want filesystem.read", inner.calls[0].Name)
	}
	if inner.calls[0].Args["path"] != "/etc/hosts" {
		t.Errorf("path lost : %+v", inner.calls[0].Args)
	}
}
