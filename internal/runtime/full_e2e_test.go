package runtime_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/llm"
	dgruntime "github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/hooks"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// UT-E1 — Full chat exercising the combined feature surface :
// real filesystem tool + hook with templating + per-agent tool
// index + system prompt assembled + streaming-mode fallback.
// =====================================================================

// e2eLogger records hook log messages so the test can assert
// templated values made it through.
type e2eLogger struct {
	mu   sync.Mutex
	msgs []string
}

func (l *e2eLogger) Info(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, msg)
}
func (l *e2eLogger) Warn(string, ...any)  {}
func (l *e2eLogger) Error(string, ...any) {}

func TestE2E_ToolPlusHookWithTemplating(t *testing.T) {
	tmp := t.TempDir()
	contents := "hello from E2E test"
	target := filepath.Join(tmp, "hello.txt")
	if err := os.WriteFile(target, []byte(contents), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-e2e")

	// Hook : log every tool_end with the tool name and its path arg.
	logger := &e2eLogger{}
	hk := schema.Hook{
		ID: "audit_writes",
		On: schema.HookEventToolEnd,
		Condition: schema.HookCondition{
			Type: "tool_name", Params: map[string]any{"match": "filesystem.read"},
		},
		Action: schema.HookAction{
			Type: "log",
			Params: map[string]any{
				"message": "tool={{tool.name}} path={{tool.params.path}}",
			},
		},
	}
	eng := hooks.New([]schema.Hook{hk}, hooks.ActionDeps{Logger: logger})
	eng.Async = false

	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "filesystem.read",
			Arguments: map[string]any{"path": "hello.txt"},
		}}},
		{Content: "Read the file."},
	}}

	cb, disp := buildRealBus(t, tmp)

	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = disp
	e.Hooks = &hookSourceWith{eng: eng}

	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-e2e", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 1. Hook fired with templated values.
	logger.mu.Lock()
	defer logger.mu.Unlock()
	found := false
	for _, m := range logger.msgs {
		if strings.Contains(m, "tool=filesystem.read") && strings.Contains(m, "path=hello.txt") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("templated log message missing : %v", logger.msgs)
	}

	// 2. File contents reached the LLM.
	round2 := lc.allGots[1]
	var sawContent bool
	for _, m := range round2.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, contents) {
			sawContent = true
		}
	}
	if !sawContent {
		t.Errorf("file content didn't surface in tool message")
	}

	// 3. Turn ended cleanly.
	ev := sess.find(sessionstore.EventTurnEnded)
	if ev == nil || ev.Turn == nil || ev.Turn.Status != "done" {
		t.Errorf("turn must end as done, got %+v", ev)
	}
}

// =====================================================================
// UT-E2 — Multi-app multi-user isolation under load. Verifies the
// per-(app, agent) ToolIndex cache and per-session state remain
// isolated when many sessions run concurrently.
// =====================================================================

func makeApp(appID string) *appmgr.RuntimeApp {
	return &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: appID, Enabled: true},
		Definition: &schema.AppDefinition{
			App: schema.AppMeta{
				AppID: appID, Name: appID, Version: "1.0",
			},
			Agents: []schema.Agent{{
				ID:           "main",
				Role:         "assistant",
				Brain:        schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
				SystemPrompt: "Identity-" + appID,
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

func TestE2E_MultiAppMultiUserIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("isolation stress slow under -short")
	}
	const (
		nApps        = 5
		nUsers       = 20
		nSessPerUser = 10
	)

	appsMap := make(map[string]*appmgr.RuntimeApp, nApps)
	for a := 0; a < nApps; a++ {
		appID := fmt.Sprintf("app-%d", a)
		appsMap[appID] = makeApp(appID)
	}

	// All sessions share the same in-memory app registry but each
	// gets its own session state.
	var wg sync.WaitGroup
	wg.Add(nApps * nUsers * nSessPerUser)
	var totalRuns atomic.Int64
	var totalErrs atomic.Int64

	// Lock-free per-(app, user, session) state validation.
	stateCheck := sync.Map{} // key="appID|userID|sessID" → bool

	for a := 0; a < nApps; a++ {
		appID := fmt.Sprintf("app-%d", a)
		for u := 0; u < nUsers; u++ {
			userID := fmt.Sprintf("u-%d-%d", a, u)
			for s := 0; s < nSessPerUser; s++ {
				sessID := fmt.Sprintf("sess-%d-%d-%d", a, u, s)
				go func(appID, userID, sessID string) {
					defer wg.Done()

					apps := &stubApps{app: appsMap[appID]}
					sess := newProjectingSessions(sessID)
					lc := &stubLLM{resp: &llm.ChatResponse{
						Content: "done-" + sessID,
					}}

					e := newEngine(t, apps, sess, lc)
					_, err := e.Run(context.Background(), dgruntime.TurnInput{
						AppID: appID, SessionID: sessID, UserID: userID,
					})
					totalRuns.Add(1)
					if err != nil {
						totalErrs.Add(1)
						return
					}

					// Verify the LLM saw the correct system prompt
					// for THIS app (no cross-talk).
					if lc.got == nil {
						totalErrs.Add(1)
						return
					}
					var sysContent string
					for _, m := range lc.got.Messages {
						if m.Role == "system" {
							sysContent = m.Content
							break
						}
					}
					if !strings.Contains(sysContent, "Identity-"+appID) {
						t.Errorf("[%s] system prompt cross-leak : %q",
							sessID, sysContent)
					}

					key := appID + "|" + userID + "|" + sessID
					stateCheck.Store(key, true)
				}(appID, userID, sessID)
			}
		}
	}
	wg.Wait()

	if totalErrs.Load() > 0 {
		t.Errorf("isolation test had %d errors out of %d runs",
			totalErrs.Load(), totalRuns.Load())
	}
	t.Logf("ran %d isolated turns across %d apps × %d users × %d sessions, errors=%d",
		totalRuns.Load(), nApps, nUsers, nSessPerUser, totalErrs.Load())

	// Count of stored keys must equal expected total.
	gotKeys := 0
	stateCheck.Range(func(_, _ any) bool {
		gotKeys++
		return true
	})
	expected := nApps * nUsers * nSessPerUser
	if gotKeys != expected {
		t.Errorf("state check : got %d keys, want %d", gotKeys, expected)
	}
}

// =====================================================================
// UT-E2 (bis) — Per-agent ToolIndex isolation : two agents in the
// same app see DIFFERENT tool sets, and the cache doesn't leak
// between them.
// =====================================================================

func TestE2E_PerAgentToolIndexIsolation(t *testing.T) {
	// App with two agents, each restricted to different modules.
	app := &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "two-agent-app", Enabled: true},
		Definition: &schema.AppDefinition{
			App: schema.AppMeta{AppID: "two-agent-app", Version: "1.0"},
			Agents: []schema.Agent{
				{
					ID:           "reader",
					Role:         "specialist",
					Brain:        schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
					SystemPrompt: "I only read",
					Modules: schema.AgentModules{
						{ID: "filesystem"},
					},
				},
				{
					ID:           "shell",
					Role:         "specialist",
					Brain:        schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
					SystemPrompt: "I only shell",
					Modules: schema.AgentModules{
						{ID: "shell"},
					},
				},
			},
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

	universe := []policy.AvailableAction{
		{Module: "filesystem", Action: "read",
			Spec: &tool.Spec{Name: "filesystem.read", RiskLevel: tool.RiskLow}},
		{Module: "shell", Action: "bash",
			Spec: &tool.Spec{Name: "shell.bash", RiskLevel: tool.RiskLow}},
	}
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "ok"}}

	cb := buildContextOnly(universe)
	e := newEngine(t, apps, sess, lc)
	e.Context = cb

	// First agent (reader) — should only see filesystem.read.
	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "two-agent-app", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The first agent of the app is "reader" by index, so the
	// ToolIndex for ("two-agent-app", "reader") was just built.
	idx := cb.IndexFor("two-agent-app", "reader")
	if idx == nil {
		t.Fatal("reader index missing")
	}
	if idx.Get("filesystem.read") == nil {
		t.Errorf("reader should see filesystem.read")
	}
	if idx.Get("shell.bash") != nil {
		t.Errorf("reader must NOT see shell.bash (cross-leak)")
	}
}

// =====================================================================
// UT-E2 (ter) — Two sessions of the same app run concurrently :
// state must remain isolated even though they share the engine.
// =====================================================================

func TestE2E_TwoConcurrentSessionsDoNotCrossTalk(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}

	sessA := newProjectingSessions("sess-A")
	sessB := newProjectingSessions("sess-B")

	lcA := &stubLLM{resp: &llm.ChatResponse{Content: "reply A"}}
	lcB := &stubLLM{resp: &llm.ChatResponse{Content: "reply B"}}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		e := newEngine(t, apps, sessA, lcA)
		_, _ = e.Run(context.Background(), dgruntime.TurnInput{
			AppID: "rt3-app", SessionID: "sess-A", UserID: "uA",
		})
	}()
	go func() {
		defer wg.Done()
		e := newEngine(t, apps, sessB, lcB)
		_, _ = e.Run(context.Background(), dgruntime.TurnInput{
			AppID: "rt3-app", SessionID: "sess-B", UserID: "uB",
		})
	}()
	wg.Wait()

	// State of session A must only contain A's events.
	for _, ev := range sessA.events {
		if ev.SessionID != "sess-A" {
			t.Errorf("session A leaked event from %q", ev.SessionID)
		}
	}
	for _, ev := range sessB.events {
		if ev.SessionID != "sess-B" {
			t.Errorf("session B leaked event from %q", ev.SessionID)
		}
	}
}

// =====================================================================
// UT-E3 — Recovery semantics : when the runtime starts after a
// "crash" (we simulate by creating a session state with a stale
// in-flight turn marker), the next turn must recover cleanly
// without losing events.
// =====================================================================

func TestE2E_StaleInFlightTurnRecovers(t *testing.T) {
	// Create a session state with a fake in-flight turn marker —
	// simulates the daemon being killed mid-turn.
	state := sessionstore.NewSessionState("sess-stale")
	state.CurrentTurnID = "stale-turn-id"
	state.CurrentTurnPhase = "running"
	state.CurrentTurnStartedAtNano = 12345
	sess := &projectingSessions{state: state}

	app := realDispatchApp()
	apps := &stubApps{app: app}
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "after recovery"}}

	e := newEngine(t, apps, sess, lc)
	_, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-stale", UserID: "u",
	})
	if err != nil {
		t.Fatalf("Run after stale state: %v", err)
	}

	// After recovery, TWO TurnEnded events should land : the
	// first marks the stale turn as errored with reason
	// "daemon_restarted" (turn.RecoveryReason), the second is
	// the fresh turn we just ran with status=done.
	endedCount := 0
	sawStaleRecovered := false
	sawFreshDone := false
	for _, ev := range sess.events {
		if ev.Type != sessionstore.EventTurnEnded || ev.Turn == nil {
			continue
		}
		endedCount++
		if ev.Turn.TurnID == "stale-turn-id" &&
			ev.Turn.Status == "errored" &&
			ev.Turn.Reason == "daemon_restarted" {
			sawStaleRecovered = true
		}
		if ev.Turn.TurnID != "stale-turn-id" && ev.Turn.Status == "done" {
			sawFreshDone = true
		}
	}
	if endedCount != 2 {
		t.Errorf("expected 2 TurnEnded events (recovery + fresh), got %d", endedCount)
	}
	if !sawStaleRecovered {
		t.Errorf("stale turn must be marked errored with reason=daemon_restarted")
	}
	if !sawFreshDone {
		t.Errorf("fresh turn must end as done")
	}

	// And an EventError must have been written as the audit trail.
	errEv := sess.find(sessionstore.EventError)
	if errEv == nil || errEv.Error == nil {
		t.Error("recovery must emit an EventError audit row")
	} else if errEv.Error.Code != "daemon_restarted" {
		t.Errorf("recovery EventError code = %q, want daemon_restarted", errEv.Error.Code)
	}
}
