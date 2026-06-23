package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/agent"
	"github.com/mbathepaul/digitorn/internal/runtime/background"
	"github.com/mbathepaul/digitorn/internal/runtime/context/meta"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// blockingBGDispatcher blocks until its context is cancelled — a stand-in for a
// long-running background tool, so we can prove abort cancels it.
type blockingBGDispatcher struct{}

func (blockingBGDispatcher) Dispatch(ctx context.Context, _ runtime.ToolInvocation) runtime.ToolOutcome {
	<-ctx.Done()
	return runtime.ToolOutcome{Status: "errored", Error: ctx.Err().Error()}
}

// TestAPI_AbortStopsBackgroundTasks : the total abort primitive must also cancel
// running background_run tasks in the session, not only the turn + agents.
func TestAPI_AbortStopsBackgroundTasks(t *testing.T) {
	h := newAPIHarness(t)
	bg := background.New()
	bg.AttachDispatcher(blockingBGDispatcher{})
	h.daemon.background = bg

	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	taskID, err := bg.Launch(context.Background(), meta.LaunchRequest{
		SessionID: sid, AppID: "app-1", UserID: "user-A", Tool: "database.sql",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	time.Sleep(10 * time.Millisecond) // let the task goroutine register

	code, body := h.do(t, "POST", "/api/apps/app-1/sessions/"+sid+"/abort", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("abort: %d %s", code, body)
	}
	var resp struct {
		Stopped struct {
			Background int `json:"background"`
		} `json:"stopped"`
	}
	decodeBody(t, body, &resp)
	if resp.Stopped.Background != 1 {
		t.Errorf("abort response should report 1 background task stopped, got %d", resp.Stopped.Background)
	}

	waitUntil(t, func() bool {
		st, _ := bg.Status(context.Background(), sid, taskID)
		return st.State == "cancelled"
	}, "background task cancelled by abort")
}

// resyncStubRunner is a deterministic sub-agent runner. It drives the real
// telemetry Recorder the manager injected into the sub-turn (proving the live
// counters fill exactly as they would under a real engine), then returns a
// result. No LLM — what's under test is the resync mechanism (durable agent
// tree + live telemetry + per-session isolation), not the model.
type resyncStubRunner struct {
	toolCalls int
	tokensIn  int
	tokensOut int
}

func (r resyncStubRunner) RunSubAgent(ctx context.Context, spec runtime.SubAgentSpec) (runtime.AgentResult, error) {
	if rec := runtime.RecorderFromContext(ctx); rec != nil {
		for i := 0; i < r.toolCalls; i++ {
			rec.AddToolCall("filesystem.read")
		}
		rec.AddLLMCall(r.tokensIn, r.tokensOut)
	}
	return runtime.AgentResult{
		RunID:   spec.RunID,
		AgentID: spec.AgentID,
		Session: spec.ParentSession + "::agent::" + spec.RunID,
		Status:  "completed",
		Content: "result of " + spec.AgentID,
	}, nil
}

// blockingRunner blocks until its context is cancelled, then reports the
// terminal status the manager derives from the cancelled ctx.
type blockingRunner struct{ started chan struct{} }

func (b blockingRunner) RunSubAgent(ctx context.Context, spec runtime.SubAgentSpec) (runtime.AgentResult, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return runtime.AgentResult{RunID: spec.RunID, AgentID: spec.AgentID, Status: "cancelled"}, ctx.Err()
}

// TestAPI_AbortCancelsAgentTree : POST /abort is a HARD stop — it must cancel
// the whole delegated tree, not just leave a durable marker. Sub-agents run on
// independent contexts, so only CancelAll reaches them.
func TestAPI_AbortCancelsAgentTree(t *testing.T) {
	h := newAPIHarness(t)
	started := make(chan struct{}, 2)
	am := agent.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	am.AttachSink(h.daemon.sessionStore)
	am.AttachRunner(blockingRunner{started: started})
	h.daemon.agents = am

	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	root, err := am.Spawn(context.Background(), agent.SpawnRequest{
		AppID: "app-1", RootSession: sid, UserID: "user-A", AgentID: "researcher", Task: "long job",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	child, err := am.Spawn(context.Background(), agent.SpawnRequest{
		AppID: "app-1", RootSession: sid, UserID: "user-A", AgentID: "writer", Task: "nested", ParentRunID: root,
	})
	if err != nil {
		t.Fatalf("spawn child: %v", err)
	}
	<-started // at least one is actually running before we abort

	code, body := h.do(t, "POST", "/api/apps/app-1/sessions/"+sid+"/abort", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("abort: %d %s", code, body)
	}

	// Both agents must unwind to "cancelled" — the whole tree, not just the root.
	for _, id := range []string{root, child} {
		s, werr := am.Wait(context.Background(), sid, id, 2*time.Second)
		if werr != nil {
			t.Fatalf("wait %s: %v", id, werr)
		}
		if s.Status != "cancelled" {
			t.Errorf("agent %s status = %q, want cancelled after abort", id, s.Status)
		}
	}

	// The durable interrupt marker is recorded too.
	st, _ := h.bus.State(sid)
	st.RLock()
	interrupted := st.Interrupted
	st.RUnlock()
	if !interrupted {
		t.Error("session must be marked interrupted after abort")
	}
}

// TestAPI_MultiAgentResync proves a client can resynchronise the multi-agent
// state after a disconnect — the headline requirement. It exercises the real
// HTTP endpoints over a real session store with the production AgentManager :
//
//  1. CONNECTED      : GET /agents returns the durable tree (children, with
//     parent linkage + depth) AND the live registry telemetry (tool_calls,
//     tokens) in real time.
//  2. AFTER RESTART  : the session state rebuilt from the on-disk event log
//     ALONE — no live registry, as on a cold daemon start — reconstructs the
//     full tree with each agent's final telemetry. This is the "reconnect after
//     the daemon restarted" path.
//  3. ISOLATION      : a different session sees none of these agents.
func TestAPI_MultiAgentResync(t *testing.T) {
	h := newAPIHarness(t)

	// Wire the orchestrator exactly like bootstrap : sink = the real session
	// store (durable agent_spawn / agent_result events), runner = the
	// deterministic telemetry stub.
	am := agent.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	am.AttachSink(h.daemon.sessionStore)
	am.AttachRunner(resyncStubRunner{toolCalls: 3, tokensIn: 100, tokensOut: 20})
	h.daemon.agents = am

	_, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A coordinator delegates to a researcher ; the researcher then spawns a
	// nested writer — a two-level tree.
	rResearcher, err := am.Spawn(ctx, agent.SpawnRequest{
		AppID: "app-1", RootSession: sid, UserID: "user-A", AgentID: "researcher", Task: "dig",
	})
	if err != nil {
		t.Fatalf("spawn researcher: %v", err)
	}
	if _, err := am.Wait(ctx, sid, rResearcher, 5*time.Second); err != nil {
		t.Fatalf("wait researcher: %v", err)
	}
	rWriter, err := am.Spawn(ctx, agent.SpawnRequest{
		AppID: "app-1", RootSession: sid, UserID: "user-A", AgentID: "writer", Task: "write",
		ParentRunID: rResearcher,
	})
	if err != nil {
		t.Fatalf("spawn writer: %v", err)
	}
	if _, err := am.Wait(ctx, sid, rWriter, 5*time.Second); err != nil {
		t.Fatalf("wait writer: %v", err)
	}

	// ===== PHASE 1 : connected — GET /agents : durable tree + live telemetry. =====
	code, body := h.do(t, "GET", "/api/apps/app-1/sessions/"+sid+"/agents", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("get agents: %d %s", code, body)
	}
	var ag struct {
		Children []sessionstore.ChildAgent `json:"children"`
		Live     []agent.Snapshot          `json:"live"`
		Count    int                       `json:"count"`
	}
	decodeBody(t, body, &ag)
	if ag.Count != 2 || len(ag.Children) != 2 {
		t.Fatalf("expected 2 children, got %d : %s", ag.Count, body)
	}

	byRun := map[string]sessionstore.ChildAgent{}
	for _, c := range ag.Children {
		byRun[c.RunID] = c
	}
	res, wri := byRun[rResearcher], byRun[rWriter]
	if res.Status != "completed" || res.Depth != 0 || res.ParentRunID != "" {
		t.Errorf("researcher durable shape wrong: %+v", res)
	}
	if wri.Status != "completed" || wri.Depth != 1 || wri.ParentRunID != rResearcher {
		t.Errorf("writer must be nested under researcher (depth 1): %+v", wri)
	}
	if res.ToolCalls != 3 || res.TokensIn != 100 || res.TokensOut != 20 {
		t.Errorf("researcher final telemetry not durable: %+v", res)
	}
	if len(ag.Live) != 2 {
		t.Fatalf("expected 2 live agents, got %d", len(ag.Live))
	}
	var liveTokens int64
	for _, s := range ag.Live {
		liveTokens += s.TokensIn
	}
	if liveTokens != 200 {
		t.Errorf("live registry telemetry wrong: total tokens_in = %d, want 200", liveTokens)
	}

	// ===== PHASE 2 : reconnect after a daemon restart — rebuild from disk ALONE. =====
	h.flusher.Flush(ctx)
	jres, err := sessionstore.ReadJSONL(h.paths.EventsFile(sid), sessionstore.JSONLBestEffort, "")
	if err != nil {
		t.Fatalf("read events file: %v", err)
	}
	cold := sessionstore.NewSessionState(sid)
	var spawns, results int
	for i := range jres.Events {
		switch jres.Events[i].Type {
		case sessionstore.EventAgentSpawn:
			spawns++
		case sessionstore.EventAgentResult:
			results++
		}
		sessionstore.Apply(cold, &jres.Events[i])
	}
	if spawns != 2 || results != 2 {
		t.Fatalf("on-disk log must carry 2 spawn + 2 result events, got %d/%d", spawns, results)
	}
	coldChildren := cold.Snapshot().Children
	if len(coldChildren) != 2 {
		t.Fatalf("cold rebuild lost the tree: %d children", len(coldChildren))
	}
	coldByRun := map[string]sessionstore.ChildAgent{}
	for _, c := range coldChildren {
		coldByRun[c.RunID] = c
	}
	cr, cw := coldByRun[rResearcher], coldByRun[rWriter]
	if cr.Status != "completed" || cr.ToolCalls != 3 || cr.TokensIn != 100 {
		t.Errorf("cold-loaded researcher telemetry lost: %+v", cr)
	}
	if cw.ParentRunID != rResearcher || cw.Depth != 1 {
		t.Errorf("cold-loaded tree shape lost: %+v", cw)
	}

	// ===== PHASE 3 : isolation — a different session sees no agents. =====
	_, body = h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{}`)
	var other map[string]any
	decodeBody(t, body, &other)
	sid2 := other["session_id"].(string)
	code, body = h.do(t, "GET", "/api/apps/app-1/sessions/"+sid2+"/agents", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("get agents sid2: %d %s", code, body)
	}
	var ag2 struct {
		Live  []agent.Snapshot `json:"live"`
		Count int              `json:"count"`
	}
	decodeBody(t, body, &ag2)
	if ag2.Count != 0 || len(ag2.Live) != 0 {
		t.Fatalf("isolation breach: session 2 sees %d durable / %d live agents", ag2.Count, len(ag2.Live))
	}
}
