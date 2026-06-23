//go:build live

package runtime_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/core/servicebus"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	fsmod "github.com/mbathepaul/digitorn/internal/modules/filesystem"
	dgruntime "github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/context/index"
	"github.com/mbathepaul/digitorn/internal/runtime/context/meta"
	"github.com/mbathepaul/digitorn/internal/runtime/context/wiring"
	"github.com/mbathepaul/digitorn/internal/runtime/dispatch"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// multiSession is a concurrency-safe, multi-session SessionAccess so several
// sessions can share ONE engine (and thus ONE per-app behavior engine) — the
// setup needed to exercise per-session isolation under real concurrent load.
type multiSession struct {
	mu     sync.Mutex
	states map[string]*sessionstore.SessionState
	events map[string][]sessionstore.Event
	seq    uint64
}

func newMultiSession() *multiSession {
	return &multiSession{
		states: map[string]*sessionstore.SessionState{},
		events: map[string][]sessionstore.Event{},
	}
}

func (m *multiSession) State(sid string) (*sessionstore.SessionState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.states[sid]
	if st == nil {
		st = sessionstore.NewSessionState(sid)
		m.states[sid] = st
	}
	return st, nil
}

func (m *multiSession) AppendDurable(_ context.Context, ev sessionstore.Event) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	ev.Seq = m.seq
	if ev.TsUnixNano == 0 {
		ev.TsUnixNano = time.Now().UnixNano()
	}
	st := m.states[ev.SessionID]
	if st == nil {
		st = sessionstore.NewSessionState(ev.SessionID)
		m.states[ev.SessionID] = st
	}
	sessionstore.Apply(st, &ev)
	m.events[ev.SessionID] = append(m.events[ev.SessionID], ev)
	return m.seq, nil
}

func (m *multiSession) Append(_ context.Context, ev sessionstore.Event) (uint64, error) {
	return m.AppendDurable(context.Background(), ev)
}

func (m *multiSession) eventsFor(sid string) []sessionstore.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sessionstore.Event, len(m.events[sid]))
	copy(out, m.events[sid])
	return out
}

// TestLiveConcurrent_ModeBehaviorIsolation runs N sessions in parallel on ONE
// engine against the REAL LLM. Half are in read-only Ask mode (must NOT write),
// half in Build mode (must create their own file). A behavior warn rule fires
// per write. Proves, under real concurrency : per-session mode application,
// per-session behavior state, and zero cross-session file contamination.
func TestLiveConcurrent_ModeBehaviorIsolation(t *testing.T) {
	provider, model, jwt, _ := liveProvider(t)
	workspace := t.TempDir()

	bus := servicebus.New()
	fs := fsmod.New()
	if err := fs.Init(context.Background(), map[string]any{"workspace": workspace}); err != nil {
		t.Fatalf("fs init: %v", err)
	}
	if err := bus.Register(fs); err != nil {
		t.Fatalf("bus register: %v", err)
	}

	universe := []policy.AvailableAction{
		{Module: "filesystem", Action: "read", Spec: &tool.Spec{Name: "filesystem.read", Description: "Read a file.", RiskLevel: tool.RiskLow, Params: []tool.ParamSpec{{Name: "path", Type: "string", Required: true}}}},
		{Module: "filesystem", Action: "write", Spec: &tool.Spec{Name: "filesystem.write", Description: "Write a file.", RiskLevel: tool.RiskMedium, Params: []tool.ParamSpec{{Name: "path", Type: "string", Required: true}, {Name: "content", Type: "string", Required: true}}}},
		{Module: "filesystem", Action: "ls", Spec: &tool.Spec{Name: "filesystem.ls", Description: "List a directory.", RiskLevel: tool.RiskLow, Params: []tool.ParamSpec{{Name: "path", Type: "string", Required: true}}}},
		{Module: "filesystem", Action: "grep", Spec: &tool.Spec{Name: "filesystem.grep", Description: "Search files.", RiskLevel: tool.RiskLow, Params: []tool.ParamSpec{{Name: "pattern", Type: "string", Required: true}}}},
	}

	app := &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "conc-app", Enabled: true, BYOK: false},
		Definition: &schema.AppDefinition{
			App: schema.AppMeta{AppID: "conc-app", Name: "Concurrent", Version: "1.0"},
			Agents: []schema.Agent{{
				ID: "main", Role: "assistant",
				Brain:        schema.Brain{Provider: provider, Model: model},
				SystemPrompt: "You are a helpful assistant with filesystem tools. When asked to create a file you MUST call filesystem.write. Be concise.",
			}},
			Tools: &schema.ToolsBlock{Capabilities: &schema.CapabilitiesConfig{
				DefaultPolicy: schema.CapAuto, MaxRiskLevel: schema.RiskLevel(tool.RiskHigh),
			}},
			Runtime: &schema.RuntimeBlock{
				ToolInjection: schema.ToolInjectionDirect,
				Modes: map[string]schema.ModeDef{
					"ask":   {Label: "Ask", ToolGrants: []schema.CapabilityGrant{{Module: "filesystem", Tools: []string{"read", "ls", "grep"}}}},
					"build": {Label: "Build"},
				},
				ModesOrder: []string{"ask", "build"},
			},
			Security: &schema.SecurityBlock{Behavior: &schema.BehaviorConfig{
				RuleDefinitions: []schema.BehaviorRuleDefinition{{
					ID: "audit_writes", When: schema.RuleWhenPreTool, Action: schema.RuleActionWarn,
					Trigger: []string{"filesystem.write"}, Message: "Writes are audited.",
				}},
			}},
		},
		BundleDir: workspace,
	}

	store := newMultiSession()
	client := liveLLMClient(t)
	cb := wiring.New(staticActionsSource{all: universe})
	disp := &meta.MetaDispatcher{
		IndexLookup: func(appID, agentID string) *index.ToolIndex { return cb.IndexFor(appID, agentID) },
		Inner:       dispatch.NewBusAdapter(bus),
	}
	e, err := dgruntime.New(&stubApps{app: app}, store, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	e.Context = cb
	disp.Gate = e // match production : gate meta sub-tools by security + mode
	e.Dispatcher = disp

	const n = 8
	type result struct {
		sid     string
		mode    string
		wrote   bool
		audited bool
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sid := fmt.Sprintf("conc-%d", i)
			mode := "ask"
			if i%2 == 0 {
				mode = "build"
			}
			fname := fmt.Sprintf("report_%d.txt", i)
			// Seed the user message.
			store.AppendDurable(context.Background(), sessionstore.Event{
				Type: sessionstore.EventUserMessage, SessionID: sid, AppID: "conc-app", UserID: "u",
				Message: &sessionstore.MessagePayload{Role: "user", Parts: []sessionstore.MessagePart{
					{Type: sessionstore.PartTypeText, Text: fmt.Sprintf("Create a file named %s containing exactly: SESSION %d", fname, i)},
				}},
			})
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			if _, err := e.Run(ctx, dgruntime.TurnInput{AppID: "conc-app", SessionID: sid, UserID: "u", UserJWT: jwt, Mode: mode}); err != nil {
				t.Errorf("session %s run: %v", sid, err)
				return
			}
			r := result{sid: sid, mode: mode}
			for _, ev := range store.eventsFor(sid) {
				if ev.Type == sessionstore.EventToolCall && ev.Tool != nil &&
					(ev.Tool.Name == "filesystem.write" || ev.Tool.Name == "filesystem__write") {
					r.wrote = true
				}
				if ev.Type == sessionstore.EventSystemMessage && ev.Message != nil && ev.Message.Extra != nil {
					if s, _ := ev.Message.Extra["source"].(string); s == "behavior_enforcement" &&
						strings.Contains(ev.Message.Content, "audit_writes") {
						r.audited = true
					}
				}
			}
			results[i] = r
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)
	t.Logf("%d concurrent live sessions finished in %s", n, elapsed)

	for i, r := range results {
		fname := filepath.Join(workspace, fmt.Sprintf("report_%d.txt", i))
		_, statErr := os.Stat(fname)
		exists := statErr == nil
		if r.mode == "ask" {
			if exists {
				t.Errorf("session %d (ask): report_%d.txt must NOT exist (read-only mode)", i, i)
			}
			continue
		}
		// build mode : must have created ITS OWN file with ITS OWN content.
		if !exists {
			t.Errorf("session %d (build): report_%d.txt must exist", i, i)
			continue
		}
		data, _ := os.ReadFile(fname)
		if !strings.Contains(strings.ToUpper(string(data)), fmt.Sprintf("SESSION %d", i)) {
			t.Errorf("session %d (build): content cross-contaminated, got %q", i, string(data))
		}
		if r.wrote && !r.audited {
			t.Errorf("session %d (build): write happened but no per-session audit_writes warning", i)
		}
	}

	// No stray files from cross-session writes : exactly the build sessions'
	// files (indices 0,2,4,6) must exist, nothing else.
	entries, _ := os.ReadDir(workspace)
	got := map[string]bool{}
	for _, en := range entries {
		got[en.Name()] = true
	}
	for i := 0; i < n; i++ {
		want := fmt.Sprintf("report_%d.txt", i)
		if i%2 == 0 { // build
			if !got[want] {
				t.Errorf("missing %s", want)
			}
		} else { // ask
			if got[want] {
				t.Errorf("ask session %d leaked a file %s", i, want)
			}
		}
	}
}
