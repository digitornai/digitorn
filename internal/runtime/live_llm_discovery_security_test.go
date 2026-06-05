//go:build live

package runtime_test

import (
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime/hooks"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// LL-3 DISCOVERY — force discovery mode and verify the LLM uses
// meta-tools to find + invoke domain actions.
// =====================================================================

// TestLive_DiscoverySearchTools : in discovery mode, the LLM sees
// only meta-tools. To read a file it must search_tools then
// execute_tool. Verify search_tools is called.
func TestLive_DiscoverySearchTools(t *testing.T) {
	f := liveSetup(t)
	f.app.Definition.Runtime.ToolInjection = schema.ToolInjectionDiscovery
	f.writeWorkspaceFile(t, "readme.txt", "rendezvous at noon")

	f.runLive(t, "Find a tool that reads files, then use it to read readme.txt.")

	if !toolWasCalled(f, "context_builder.search_tools") &&
		!toolWasCalled(f, "context_builder.browse_category") &&
		!toolWasCalled(f, "context_builder.get_tool") {
		t.Errorf("expected at least one discovery meta-tool to be called")
	}
	// The actual read must have happened (via execute_tool or
	// auto-routing).
	if !toolWasCalled(f, "filesystem.read") &&
		!toolWasCalled(f, "context_builder.execute_tool") {
		t.Errorf("the file was never actually read")
	}
	assertSemantic(t, f, "rendezvous", "noon")
}

// TestLive_DiscoveryListCategories : the LLM should be able to
// enumerate top-level tool domains.
func TestLive_DiscoveryListCategories(t *testing.T) {
	f := liveSetup(t)
	f.app.Definition.Runtime.ToolInjection = schema.ToolInjectionDiscovery

	f.runLive(t, "What tool domains are available to you ? List them by calling the appropriate discovery tool.")

	if !toolWasCalled(f, "context_builder.list_categories") {
		t.Logf("note : LLM didn't call list_categories ; may have inferred from system prompt. Got tools called : %v", toolNamesCalled(f))
	}
	assertSemantic(t, f, "filesystem")
}

// TestLive_DiscoveryGetToolSchema : when the LLM uses get_tool
// before calling, it must use the returned schema correctly.
func TestLive_DiscoveryGetToolSchema(t *testing.T) {
	f := liveSetup(t)
	f.app.Definition.Runtime.ToolInjection = schema.ToolInjectionDiscovery
	f.writeWorkspaceFile(t, "x.txt", "the answer is 42")

	f.runLive(t, "I need to read the file x.txt. First check what params the file-read tool needs, then read it.")

	// Either get_tool was called, or the LLM went straight to
	// execute_tool — both are valid.
	if !toolWasCalled(f, "context_builder.get_tool") &&
		!toolWasCalled(f, "context_builder.execute_tool") &&
		!toolWasCalled(f, "filesystem.read") {
		t.Error("no meta-tool path was used to read the file")
	}
	assertSemantic(t, f, "42", "answer")
}

// TestLive_DiscoveryBrowseCategory : alternative discovery via
// browse_category. Verify the LLM can list tools in 'filesystem'.
func TestLive_DiscoveryBrowseCategory(t *testing.T) {
	f := liveSetup(t)
	f.app.Definition.Runtime.ToolInjection = schema.ToolInjectionDiscovery

	f.runLive(t, "Browse the filesystem tool category. Tell me what's available.")

	if !toolWasCalled(f, "context_builder.browse_category") &&
		!toolWasCalled(f, "context_builder.list_categories") {
		t.Logf("note : LLM didn't browse_category. Tools called : %v", toolNamesCalled(f))
	}
	assertSemantic(t, f, "read", "write", "ls", "grep", "filesystem")
}

// TestLive_DiscoveryExecuteTool : the LLM uses execute_tool to
// invoke filesystem.read. Verifies the meta-tool execute path
// works end-to-end.
func TestLive_DiscoveryExecuteTool(t *testing.T) {
	f := liveSetup(t)
	f.app.Definition.Runtime.ToolInjection = schema.ToolInjectionDiscovery
	f.writeWorkspaceFile(t, "magic.txt", "presto chango")

	f.runLive(t, "Use the execute_tool action to call filesystem.read on magic.txt.")

	if !toolWasCalled(f, "context_builder.execute_tool") &&
		!toolWasCalled(f, "filesystem.read") {
		t.Errorf("expected execute_tool or auto-routed filesystem.read")
	}
	assertSemantic(t, f, "presto", "chango")
}

// =====================================================================
// LL-4 HOOKS + SECURITY — verify hook gates and YAML security
// against a real LLM that may try to bypass them.
// =====================================================================

// liveLoggingHook returns a recording logger that captures all
// log lines from hooks for assertion.
type liveLoggingHook struct {
	mu   sync_dummy
	msgs []string
}

type sync_dummy struct{}

func (l *liveLoggingHook) Info(msg string, _ ...any) { l.msgs = append(l.msgs, msg) }
func (l *liveLoggingHook) Warn(string, ...any)       {}
func (l *liveLoggingHook) Error(string, ...any)      {}

// TestLive_HookGateVetoOnWrite : a hook with gate allow=false on
// filesystem.write tool_start prevents the call. The LLM tries to
// write, sees the error, and must apologise.
func TestLive_HookGateVetoOnWrite(t *testing.T) {
	f := liveSetup(t)

	hk := schema.Hook{
		ID: "block_write",
		On: schema.HookEventToolStart,
		Condition: schema.HookCondition{
			Type:   "tool_name",
			Params: map[string]any{"match": "filesystem.write"},
		},
		Action: schema.HookAction{
			Type:   "gate",
			Params: map[string]any{"allow": false, "reason": "writes blocked in test mode"},
		},
	}
	eng := hooks.New([]schema.Hook{hk}, hooks.ActionDeps{})
	eng.Async = false
	f.engine.Hooks = &hookSourceWith{eng: eng}

	f.runLive(t, "Write the word 'hello' to test.txt.")

	// Tool result for filesystem.write must be errored with the
	// gate reason.
	for _, ev := range f.session.events {
		if ev.Tool != nil && ev.Tool.Name == "filesystem.write" &&
			ev.Type == sessionToolResultType() {
			if ev.Tool.Status != "errored" {
				t.Errorf("write status = %q, want errored", ev.Tool.Status)
			}
			if !strings.Contains(strings.ToLower(ev.Tool.Error), "blocked") &&
				!strings.Contains(strings.ToLower(ev.Tool.Error), "gate") {
				t.Errorf("error doesn't mention block/gate : %q", ev.Tool.Error)
			}
		}
	}
}

// TestLive_HookTransformParams : a hook injects an extra param
// before dispatch. The LLM doesn't know but the underlying tool
// receives the modified args.
func TestLive_HookTransformParams(t *testing.T) {
	f := liveSetup(t)
	// Pre-populate two files to verify transform redirected the
	// path.
	f.writeWorkspaceFile(t, "expected.txt", "this is the expected file")
	f.writeWorkspaceFile(t, "decoy.txt", "this is the decoy")

	hk := schema.Hook{
		ID: "redirect",
		On: schema.HookEventToolStart,
		Condition: schema.HookCondition{
			Type:   "tool_name",
			Params: map[string]any{"match": "filesystem.read"},
		},
		Action: schema.HookAction{
			Type: "transform_params",
			Params: map[string]any{
				"transformation": map[string]any{
					"path": "expected.txt", // override whatever LLM passed
				},
			},
		},
	}
	eng := hooks.New([]schema.Hook{hk}, hooks.ActionDeps{})
	eng.Async = false
	f.engine.Hooks = &hookSourceWith{eng: eng}

	// The LLM thinks it's reading decoy.txt, but the hook
	// redirects to expected.txt.
	f.runLive(t, "Read the file decoy.txt and tell me exactly what it says.")

	// The LLM should report the expected.txt content (because
	// that's what the runtime gave it).
	assertSemantic(t, f, "expected")
}

// TestLive_YAMLDenyBlocksWrite : add a YAML deny entry for
// filesystem.write. The LLM tries to write ; the runtime gate
// rejects ; the LLM explains the failure.
func TestLive_YAMLDenyBlocksWrite(t *testing.T) {
	f := liveSetup(t)
	f.caps.Deny = []schema.CapabilityGrant{
		{Module: "filesystem", Tools: []string{"write"}},
	}

	f.runLive(t, "Write 'hello' to a file called test.txt.")

	// Either filesystem.write was NEVER called (SG-3 filtered it
	// from the LLM's view) — which is the expected primary
	// outcome — OR if the LLM somehow called it via FQN, the
	// result is errored.
	if toolWasCalled(f, "filesystem.write") {
		for _, ev := range f.session.events {
			if ev.Tool != nil && ev.Tool.Name == "filesystem.write" &&
				ev.Type == sessionToolResultType() {
				if ev.Tool.Status != "errored" {
					t.Errorf("denied write status = %q", ev.Tool.Status)
				}
			}
		}
	}
}

// TestLive_YAMLMaxRiskHidesHighRiskTool : with max_risk_level=low,
// filesystem.write (medium) is hidden. The LLM cannot use it
// even if asked.
func TestLive_YAMLMaxRiskHidesHighRiskTool(t *testing.T) {
	f := liveSetup(t)
	f.caps.MaxRiskLevel = schema.RiskLevel(tool.RiskLow)

	f.runLive(t, "Write a file called note.txt with the word 'noted'.")

	// filesystem.write is medium-risk. With max_risk_level=low,
	// it must never be called.
	assertToolNotCalled(t, f, "filesystem.write")
}

// TestLive_HookGateAuditTrail : even when the gate vetoes a call,
// the runtime must persist an EventToolResult (errored). Audit
// trail is non-negotiable.
func TestLive_HookGateAuditTrail(t *testing.T) {
	f := liveSetup(t)

	hk := schema.Hook{
		ID:        "block_all_writes",
		On:        schema.HookEventToolStart,
		Condition: schema.HookCondition{Type: "tool_name", Params: map[string]any{"match": "filesystem.write"}},
		Action:    schema.HookAction{Type: "gate", Params: map[string]any{"allow": false, "reason": "audit-trail-test"}},
	}
	eng := hooks.New([]schema.Hook{hk}, hooks.ActionDeps{})
	eng.Async = false
	f.engine.Hooks = &hookSourceWith{eng: eng}

	f.runLive(t, "Write the word 'audit' to audit.txt.")

	// Even if the gate vetoed, a tool_result must be persisted.
	if toolWasCalled(f, "filesystem.write") {
		// Verify the corresponding result event landed.
		var seenResult bool
		for _, ev := range f.session.events {
			if ev.Type == sessionToolResultType() && ev.Tool != nil &&
				ev.Tool.Name == "filesystem.write" {
				seenResult = true
			}
		}
		if !seenResult {
			t.Error("EventToolResult missing for vetoed call (audit trail broken)")
		}
	}
}

// =====================================================================
// Helpers
// =====================================================================

func toolNamesCalled(f *liveEngineFixture) []string {
	seen := map[string]bool{}
	var out []string
	for _, ev := range f.session.events {
		if ev.Type == sessionToolCallType() && ev.Tool != nil {
			if !seen[ev.Tool.Name] {
				seen[ev.Tool.Name] = true
				out = append(out, ev.Tool.Name)
			}
		}
	}
	return out
}

func sessionToolCallType() sessionstore.EventType   { return sessionstore.EventToolCall }
func sessionToolResultType() sessionstore.EventType { return sessionstore.EventToolResult }
