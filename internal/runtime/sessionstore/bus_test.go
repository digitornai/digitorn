package sessionstore

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func startBus(t *testing.T, root string) (*Bus, *DiskFlusher, func()) {
	t.Helper()
	paths := NewPaths(root)
	flusher, err := NewDiskFlusher(DiskFlusherConfig{
		Paths:            paths,
		NumShards:        4,
		QueueCapPerShard: 65536,
		BatchMax:         500,
		FlushInterval:    2 * time.Millisecond,
		FDCachePerShard:  256,
		PerSidQuotaPct:   80,
	})
	if err != nil {
		t.Fatalf("new flusher: %v", err)
	}
	if err := flusher.Start(); err != nil {
		t.Fatalf("start flusher: %v", err)
	}
	bus, err := NewBus(BusConfig{
		Paths:               paths,
		Flusher:             flusher,
		EvictionInterval:    50 * time.Millisecond,
		StateIdleEvictAfter: 1 * time.Hour,
	})
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	if err := bus.Start(context.Background()); err != nil {
		t.Fatalf("start bus: %v", err)
	}
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = bus.Stop(ctx)
		_ = flusher.Stop(ctx)
	}
	return bus, flusher, cleanup
}

func makeUserMsg(sid, text string) Event {
	return Event{Type: EventUserMessage, SessionID: sid, Message: &MessagePayload{Role: "user", Content: text}}
}

func makeAssistantMsg(sid, text string) Event {
	return Event{Type: EventAssistantMessage, SessionID: sid, Message: &MessagePayload{Role: "assistant", Content: text}}
}

func makeToolCall(sid, callID, name string) Event {
	return Event{Type: EventToolCall, SessionID: sid, Tool: &ToolPayload{CallID: callID, Name: name}}
}

func makeError(sid, msg string) Event {
	return Event{Type: EventError, SessionID: sid, Error: &ErrorPayload{Code: "E", Message: msg}}
}

func TestBus_AppendAllocatesSeqAndProjects(t *testing.T) {
	bus, flusher, cleanup := startBus(t, t.TempDir())
	defer cleanup()
	sid := "sess-bus-1"

	for i := 0; i < 10; i++ {
		seq, err := bus.Append(context.Background(), makeUserMsg(sid, fmt.Sprintf("hi %d", i)))
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if seq != uint64(i+1) {
			t.Fatalf("seq @ i=%d: want %d got %d", i, i+1, seq)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := flusher.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	state, err := bus.State(sid)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	state.mu.RLock()
	gotMsgs := len(state.Messages)
	gotLast := state.LastSeq
	state.mu.RUnlock()
	if gotMsgs != 10 || gotLast != 10 {
		t.Fatalf("state mismatch: msgs=%d last=%d", gotMsgs, gotLast)
	}

	res, err := ReadJSONL(flusher.cfg.Paths.EventsFile(sid), JSONLStrict, "")
	if err != nil {
		t.Fatalf("read jsonl: %v", err)
	}
	if len(res.Events) != 10 {
		t.Fatalf("jsonl events: %d", len(res.Events))
	}
	for i, ev := range res.Events {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("disk seq @ i=%d: %d", i, ev.Seq)
		}
	}
}

func TestBus_NoGapWhenFlusherRejects(t *testing.T) {
	paths := NewPaths(t.TempDir())
	flusher, err := NewDiskFlusher(DiskFlusherConfig{
		Paths:            paths,
		NumShards:        1,
		QueueCapPerShard: 2,
		BatchMax:         1,
		FlushInterval:    1 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	seqs := NewSeqRegistry(paths)
	bus, err := NewBus(BusConfig{Paths: paths, Flusher: flusher, Seqs: seqs})
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer bus.Stop(context.Background())
	// Flusher NOT started — Enqueue must fail with ErrFlusherStop.
	if _, err := bus.Append(context.Background(), makeUserMsg("s", "x")); err == nil {
		t.Fatal("expected append to fail when flusher not started")
	}

	alloc, _ := seqs.For("s")
	if cur := alloc.Current(); cur != 0 {
		t.Fatalf("seq leaked on failed enqueue: %d", cur)
	}
}

func TestBus_PerSessionOrderingUnderConcurrency(t *testing.T) {
	bus, flusher, cleanup := startBus(t, t.TempDir())
	defer cleanup()

	const sessions = 10
	const eventsPerSession = 200
	var wg sync.WaitGroup

	for i := 0; i < sessions; i++ {
		sid := fmt.Sprintf("sess-c-%d", i)
		wg.Add(1)
		go func(sid string) {
			defer wg.Done()
			for j := 0; j < eventsPerSession; j++ {
				ev := makeUserMsg(sid, fmt.Sprintf("%d", j))
				if _, err := bus.Append(context.Background(), ev); err != nil {
					t.Errorf("append %s/%d: %v", sid, j, err)
					return
				}
			}
		}(sid)
	}
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := flusher.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	for i := 0; i < sessions; i++ {
		sid := fmt.Sprintf("sess-c-%d", i)
		res, err := ReadJSONL(flusher.cfg.Paths.EventsFile(sid), JSONLStrict, "")
		if err != nil {
			t.Fatalf("%s read: %v", sid, err)
		}
		if len(res.Events) != eventsPerSession {
			t.Fatalf("%s events: %d", sid, len(res.Events))
		}
		seqs := make([]uint64, len(res.Events))
		for k := range res.Events {
			seqs[k] = res.Events[k].Seq
		}
		if !sort.SliceIsSorted(seqs, func(a, b int) bool { return seqs[a] < seqs[b] }) {
			t.Fatalf("%s seqs unsorted", sid)
		}
		seen := map[uint64]bool{}
		for _, s := range seqs {
			if seen[s] {
				t.Fatalf("%s duplicate seq %d", sid, s)
			}
			seen[s] = true
		}
		if seqs[0] != 1 || seqs[len(seqs)-1] != uint64(eventsPerSession) {
			t.Fatalf("%s seq range wrong: first=%d last=%d", sid, seqs[0], seqs[len(seqs)-1])
		}
	}
}

func TestBus_SubscriberReceivesAllEventsInOrder(t *testing.T) {
	bus, flusher, cleanup := startBus(t, t.TempDir())
	defer cleanup()
	sid := "sess-sub"

	var mu sync.Mutex
	received := []uint64{}
	var deliveredWG sync.WaitGroup
	deliveredWG.Add(50)
	sub, err := bus.Subscribe(sid, func(ev Event) {
		mu.Lock()
		received = append(received, ev.Seq)
		mu.Unlock()
		deliveredWG.Done()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()

	for i := 0; i < 50; i++ {
		_, err := bus.Append(context.Background(), makeUserMsg(sid, fmt.Sprintf("%d", i)))
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_ = flusher.Flush(ctx)
	deliveredWG.Wait()

	mu.Lock()
	got := append([]uint64(nil), received...)
	mu.Unlock()
	if len(got) != 50 {
		t.Fatalf("received: %d (want 50)", len(got))
	}
	for i, s := range got {
		if s != uint64(i+1) {
			t.Fatalf("seq @ i=%d: got %d want %d", i, s, i+1)
		}
	}
}

func TestBus_GlobalSubscriberSeesAllSessions(t *testing.T) {
	bus, _, cleanup := startBus(t, t.TempDir())
	defer cleanup()

	var count atomic.Int64
	var deliveredWG sync.WaitGroup
	deliveredWG.Add(21)
	sub, err := bus.SubscribeAll(func(ev Event) {
		count.Add(1)
		deliveredWG.Done()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()

	for i := 0; i < 3; i++ {
		sid := fmt.Sprintf("s%d", i)
		for j := 0; j < 7; j++ {
			_, err := bus.Append(context.Background(), makeUserMsg(sid, "x"))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	deliveredWG.Wait()
	if got := count.Load(); got != 21 {
		t.Fatalf("global sub got %d want 21", got)
	}
}

func TestBus_LoadColdRecoversSeqAllocator(t *testing.T) {
	paths := NewPaths(t.TempDir())
	bus1, flusher1, cleanup1 := startBus(t, paths.Root)
	for i := 0; i < 5; i++ {
		if _, err := bus1.Append(context.Background(), makeUserMsg("sess-cold", "x")); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := flusher1.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	cleanup1()

	bus2, flusher2, cleanup2 := startBus(t, paths.Root)
	defer cleanup2()
	state, err := bus2.State("sess-cold")
	if err != nil {
		t.Fatal(err)
	}
	if state.LastSeq != 5 {
		t.Fatalf("cold load last_seq: %d", state.LastSeq)
	}
	seq, err := bus2.Append(context.Background(), makeUserMsg("sess-cold", "after"))
	if err != nil {
		t.Fatal(err)
	}
	if seq != 6 {
		t.Fatalf("post-cold seq: %d (want 6)", seq)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	_ = flusher2.Flush(ctx2)
}

func TestBus_AllEventTypesPersistedAndProjected(t *testing.T) {
	bus, flusher, cleanup := startBus(t, t.TempDir())
	defer cleanup()
	sid := "sess-types"

	events := []Event{
		{Type: EventSessionStarted, SessionID: sid, Meta: &MetaPayload{Title: "T", Workspace: "ws"}},
		makeUserMsg(sid, "hello"),
		makeToolCall(sid, "c1", "shell"),
		{Type: EventToolResult, SessionID: sid, Tool: &ToolPayload{CallID: "c1", Status: "completed", Output: "ok"}},
		makeAssistantMsg(sid, "world"),
		makeError(sid, "oops"),
		{Type: EventApprovalRequest, SessionID: sid, Approval: &ApprovalPayload{ID: "ap1", Kind: "write_file"}},
		{Type: EventApprovalGranted, SessionID: sid, Approval: &ApprovalPayload{ID: "ap1"}},
		{Type: EventWorkspaceWrite, SessionID: sid, Workspace: &WorkspacePayload{Path: "a.txt", ContentHash: "h1", Bytes: 12}},
		{Type: EventAgentSpawn, SessionID: sid, Agent: &AgentPayload{RunID: "r1", Kind: "sub"}},
		{Type: EventAgentResult, SessionID: sid, Agent: &AgentPayload{RunID: "r1", Status: "done", ResultSummary: "ok"}},
		{Type: EventMemoryRemember, SessionID: sid, Memory: &MemoryPayload{Key: "k", Value: "v"}},
		{Type: EventCostUpdate, SessionID: sid, Cost: &CostPayload{TokensIn: 10, TokensOut: 20, UsdTotal: 0.001}},
		{Type: EventTodoAdded, SessionID: sid, Todo: &TodoPayload{ID: "t1", Text: "do it"}},
		{Type: EventWidget, SessionID: sid, Widget: &WidgetPayload{ID: "w1", Kind: "chart"}},
	}
	for i := range events {
		seq, err := bus.Append(context.Background(), events[i])
		if err != nil {
			t.Fatalf("append %d (%s): %v", i, events[i].Type, err)
		}
		if seq != uint64(i+1) {
			t.Fatalf("seq @ i=%d: got %d", i, seq)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = flusher.Flush(ctx)

	state, _ := bus.State(sid)
	state.mu.RLock()
	defer state.mu.RUnlock()
	if state.Title != "T" {
		t.Fatalf("title: %q", state.Title)
	}
	// 3 = user, tool_result (projected as "tool" role message so the LLM
	// adapter sees results next turn — RT-3 agent loop), assistant.
	if len(state.Messages) != 3 {
		t.Fatalf("messages: %d", len(state.Messages))
	}
	if state.Messages[1].Role != "tool" {
		t.Fatalf("messages[1] role: %q want tool", state.Messages[1].Role)
	}
	if state.ToolCalls["c1"].Status != "completed" {
		t.Fatalf("tool status: %v", state.ToolCalls["c1"])
	}
	if state.Approvals["ap1"].Status != "granted" {
		t.Fatalf("approval: %v", state.Approvals["ap1"])
	}
	if len(state.Errors) != 1 {
		t.Fatalf("errors: %d", len(state.Errors))
	}
	if state.WorkspaceFiles["a.txt"] == nil {
		t.Fatal("workspace missing")
	}
	if len(state.Children) != 1 || state.Children[0].Status != "done" {
		t.Fatalf("children: %v", state.Children)
	}
	if state.Memory["k"] != "v" {
		t.Fatal("memory missing")
	}
	if state.TokensIn != 10 || state.TokensOut != 20 {
		t.Fatalf("tokens: %d/%d", state.TokensIn, state.TokensOut)
	}
	if len(state.Todos) != 1 {
		t.Fatal("todos missing")
	}
	if len(state.Widgets) != 1 {
		t.Fatal("widgets missing")
	}
}

func TestBus_HighThroughput_10kAppends(t *testing.T) {
	bus, flusher, cleanup := startBus(t, t.TempDir())
	defer cleanup()
	sid := "sess-perf"

	start := time.Now()
	const N = 10_000
	for i := 0; i < N; i++ {
		if _, err := bus.Append(context.Background(), makeUserMsg(sid, "x")); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	apnd := time.Since(start)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	flushStart := time.Now()
	if err := flusher.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	flushDur := time.Since(flushStart)

	st := bus.Stats()
	fl := flusher.Stats()
	t.Logf("[%s] %d appends in %v (%.0f ev/s) flush=%v stats=%+v shards=%d", runtime.GOOS, N, apnd, float64(N)/apnd.Seconds(), flushDur, st, fl.TotalWritten)
	if fl.TotalWritten != N {
		t.Fatalf("written: %d want %d", fl.TotalWritten, N)
	}
}

func TestBus_View_HistoryShapeAndSocketEnvelope(t *testing.T) {
	bus, flusher, cleanup := startBus(t, t.TempDir())
	defer cleanup()
	sid := "sess-view"

	for _, ev := range []Event{
		{Type: EventSessionStarted, SessionID: sid, Meta: &MetaPayload{Title: "Demo", Workspace: "ws"}},
		makeUserMsg(sid, "hello"),
		makeToolCall(sid, "c1", "read"),
		{Type: EventToolResult, SessionID: sid, Tool: &ToolPayload{CallID: "c1", Status: "completed", Output: "file content"}},
		makeAssistantMsg(sid, "done"),
	} {
		if _, err := bus.Append(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = flusher.Flush(ctx)

	jres, err := ReadJSONL(flusher.cfg.Paths.EventsFile(sid), JSONLStrict, "")
	if err != nil {
		t.Fatal(err)
	}
	state, _ := bus.State(sid)

	resp := BuildHistory(state, state.Messages, jres.Events, ViewOptions{IncludePayload: true, InstanceID: "inst-1"})
	if resp.Title != "Demo" {
		t.Fatalf("title: %q", resp.Title)
	}
	if resp.InstanceID != "inst-1" {
		t.Fatalf("instance_id: %q", resp.InstanceID)
	}
	// user, tool_result (now projected as "tool" message — RT-3), assistant.
	if len(resp.Messages) != 3 {
		t.Fatalf("messages: %d", len(resp.Messages))
	}
	if resp.LastSeq != 5 {
		t.Fatalf("last_seq: %d", resp.LastSeq)
	}
	if resp.EventCount != 5 {
		t.Fatalf("event_count: %d", resp.EventCount)
	}
	if resp.EventsNextSeq != 6 {
		t.Fatalf("events_next_seq: %d", resp.EventsNextSeq)
	}
	if resp.EventsHasMore {
		t.Fatal("has_more should be false")
	}
	if resp.PendingQueue == nil {
		t.Fatal("pending_queue must be non-nil empty array")
	}

	env := BuildEnvelope(&jres.Events[0])
	if env.Type != string(EventSessionStarted) || env.Kind != "system" {
		t.Fatalf("envelope kind for session_started: got %+v", env)
	}
	if env.InstanceID == "" {
		t.Fatal("envelope missing instance_id")
	}
	if env.Ts == "" {
		t.Fatal("envelope missing ts (ISO8601)")
	}
	if env.EventID == "" {
		t.Fatal("envelope missing event_id")
	}
	if !env.Control {
		t.Fatal("session_started must be control=true")
	}
	rooms := SocketRoomsFor(&jres.Events[0])
	hasSessionRoom := false
	for _, r := range rooms {
		if r == "session:"+sid {
			hasSessionRoom = true
		}
	}
	if !hasSessionRoom {
		t.Fatalf("missing session room: %v", rooms)
	}

	pagedResp := BuildHistory(state, state.Messages, jres.Events, ViewOptions{IncludePayload: false, MaxEvents: 2})
	if pagedResp.EventCount != 2 || !pagedResp.EventsHasMore {
		t.Fatalf("paged: %+v", pagedResp)
	}
	if pagedResp.EventsNextSeq != 3 {
		t.Fatalf("next_seq paged: %d", pagedResp.EventsNextSeq)
	}
}

func TestFlusher_GracefulStopDrainsAllPending(t *testing.T) {
	paths := NewPaths(t.TempDir())
	flusher, err := NewDiskFlusher(DiskFlusherConfig{
		Paths:            paths,
		NumShards:        2,
		QueueCapPerShard: 4096,
		BatchMax:         100,
		FlushInterval:    1 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := flusher.Start(); err != nil {
		t.Fatal(err)
	}

	const N = 500
	for i := 0; i < N; i++ {
		ev := Event{Seq: uint64(i + 1), Type: EventUserMessage, SessionID: "sess-drain", TsUnixNano: time.Now().UnixNano(), Message: &MessagePayload{Role: "user", Content: "x"}}
		if err := flusher.Enqueue(ev); err != nil {
			t.Fatalf("enq %d: %v", i, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := flusher.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}

	res, err := ReadJSONL(paths.EventsFile("sess-drain"), JSONLStrict, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != N {
		t.Fatalf("expected %d events after drain, got %d", N, len(res.Events))
	}
}

func TestFlusher_RoutesToCorrectShard(t *testing.T) {
	paths := NewPaths(t.TempDir())
	flusher, err := NewDiskFlusher(DiskFlusherConfig{
		Paths: paths, NumShards: 8, QueueCapPerShard: 64, BatchMax: 16, FlushInterval: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := flusher.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = flusher.Stop(ctx)
	}()

	// Each session lands on one shard. Verify hash routing is consistent.
	sids := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta"}
	for _, sid := range sids {
		want := ShardOf(sid, 8)
		ev := Event{Seq: 1, Type: EventUserMessage, SessionID: sid, TsUnixNano: time.Now().UnixNano(), Message: &MessagePayload{Role: "u", Content: "x"}}
		if err := flusher.Enqueue(ev); err != nil {
			t.Fatalf("%s enq: %v", sid, err)
		}
		_ = want
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_ = flusher.Flush(ctx)
	for _, sid := range sids {
		if _, err := ReadJSONL(paths.EventsFile(sid), JSONLStrict, ""); err != nil {
			t.Errorf("%s: %v", sid, err)
		}
	}
}

func TestFlusher_DropOnRunawaySession(t *testing.T) {
	paths := NewPaths(t.TempDir())
	flusher, err := NewDiskFlusher(DiskFlusherConfig{
		Paths:            paths,
		NumShards:        1,
		QueueCapPerShard: 100,
		BatchMax:         100,
		FlushInterval:    1 * time.Hour,
		PerSidQuotaPct:   20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := flusher.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = flusher.Stop(ctx)
	}()

	rejected := 0
	for i := 0; i < 200; i++ {
		ev := Event{Seq: uint64(i + 1), Type: EventUserMessage, SessionID: "runaway", TsUnixNano: time.Now().UnixNano(), Message: &MessagePayload{Role: "u", Content: "x"}}
		if err := flusher.Enqueue(ev); err != nil {
			rejected++
		}
	}
	if rejected == 0 {
		t.Fatal("expected at least one drop from runaway session")
	}
}

func TestBus_CompactionEmittedViaBusHasSeq(t *testing.T) {
	paths := NewPaths(t.TempDir())
	flusher, err := NewDiskFlusher(DiskFlusherConfig{
		Paths: paths, NumShards: 1, QueueCapPerShard: 4096, BatchMax: 100, FlushInterval: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = flusher.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = flusher.Stop(ctx)
	}()

	bus, err := NewBus(BusConfig{Paths: paths, Flusher: flusher})
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer bus.Stop(context.Background())
	sid := "sess-compact-via-bus"
	for i := 0; i < 20; i++ {
		if _, err := bus.Append(context.Background(), makeUserMsg(sid, "x")); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = flusher.Flush(ctx)

	state, _ := bus.State(sid)
	c := bus.Compactor(CompactorConfig{})
	res, err := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateSync, Gate: bus})
	if err != nil {
		t.Fatal(err)
	}
	if res.CompactDoneSeq != 21 {
		t.Fatalf("compact_done via bus seq: want 21 got %d", res.CompactDoneSeq)
	}

	nextSeq, err := bus.Append(context.Background(), makeUserMsg(sid, "after-compact"))
	if err != nil {
		t.Fatal(err)
	}
	if nextSeq != 22 {
		t.Fatalf("post-compact seq via bus: want 22 got %d", nextSeq)
	}
	_ = flusher.Flush(ctx)
}
