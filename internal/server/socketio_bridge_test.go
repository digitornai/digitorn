package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/ports"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// fakeRealtime is a minimal in-memory ports.RealtimeServer used to verify
// that the bridge routes events with strict per-session isolation.
type fakeRealtime struct {
	mu             sync.Mutex
	emits          []emitRecord
	authHandler    ports.AuthHandler
	connHandler    ports.ConnectionHandler
	disconnHandler ports.DisconnectHandler
	events         map[string][]ports.EventHandler
}

type emitRecord struct {
	Namespace string
	Room      string
	Event     string
	Data      any
}

func newFakeRealtime() *fakeRealtime {
	return &fakeRealtime{events: map[string][]ports.EventHandler{}}
}

func (f *fakeRealtime) SetAuthHandler(h ports.AuthHandler) {
	f.mu.Lock()
	f.authHandler = h
	f.mu.Unlock()
}

func (f *fakeRealtime) OnConnection(h ports.ConnectionHandler) {
	f.mu.Lock()
	f.connHandler = h
	f.mu.Unlock()
}

func (f *fakeRealtime) OnDisconnection(h ports.DisconnectHandler) {
	f.mu.Lock()
	f.disconnHandler = h
	f.mu.Unlock()
}

func (f *fakeRealtime) OnEvent(event string, h ports.EventHandler) {
	f.mu.Lock()
	f.events[event] = append(f.events[event], h)
	f.mu.Unlock()
}

func (f *fakeRealtime) Emit(_ context.Context, namespace, room, event string, data any) error {
	f.mu.Lock()
	f.emits = append(f.emits, emitRecord{Namespace: namespace, Room: room, Event: event, Data: data})
	f.mu.Unlock()
	return nil
}

func (f *fakeRealtime) Broadcast(_ context.Context, _, _ string, _ any) error { return nil }
func (f *fakeRealtime) Close(_ context.Context) error                         { return nil }

func (f *fakeRealtime) recordedEmits() []emitRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]emitRecord, len(f.emits))
	copy(out, f.emits)
	return out
}

func (f *fakeRealtime) connect(ctx context.Context, c ports.RealtimeClient) error {
	f.mu.Lock()
	h := f.connHandler
	f.mu.Unlock()
	if h == nil {
		return nil
	}
	return h(ctx, c)
}

func (f *fakeRealtime) trigger(ctx context.Context, event string, c ports.RealtimeClient, data any) error {
	f.mu.Lock()
	handlers := append([]ports.EventHandler(nil), f.events[event]...)
	f.mu.Unlock()
	for _, h := range handlers {
		if err := h(ctx, c, data); err != nil {
			return err
		}
	}
	return nil
}

// fakeClient implements ports.RealtimeClient in memory.
type fakeClient struct {
	id      string
	auth    map[string]any
	rooms   sync.Map
	emits   []emitRecord
	mu      sync.Mutex
	dropped bool
}

func newFakeClient(id string, auth map[string]any) *fakeClient {
	return &fakeClient{id: id, auth: auth}
}

func (c *fakeClient) ID() string             { return c.id }
func (c *fakeClient) Auth() map[string]any   { return c.auth }
func (c *fakeClient) Disconnect()            { c.dropped = true }
func (c *fakeClient) Join(room string) error { c.rooms.Store(room, true); return nil }
func (c *fakeClient) Leave(room string) error {
	c.rooms.Delete(room)
	return nil
}
func (c *fakeClient) Rooms() []string {
	var out []string
	c.rooms.Range(func(k, _ any) bool { out = append(out, k.(string)); return true })
	return out
}
func (c *fakeClient) Emit(event string, data any) error {
	c.mu.Lock()
	c.emits = append(c.emits, emitRecord{Event: event, Data: data})
	c.mu.Unlock()
	return nil
}

func (c *fakeClient) hasRoom(name string) bool {
	_, ok := c.rooms.Load(name)
	return ok
}

func (c *fakeClient) recordedEmits() []emitRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]emitRecord, len(c.emits))
	copy(out, c.emits)
	return out
}

func setupBridge(t *testing.T) (*SocketIOBridge, *sessionstore.Bus, *fakeRealtime, sessionstore.Paths, func()) {
	t.Helper()
	paths := sessionstore.NewPaths(t.TempDir())
	flusher, err := sessionstore.NewDiskFlusher(sessionstore.DiskFlusherConfig{
		Paths: paths, NumShards: 2, QueueCapPerShard: 4096,
		BatchMax: 100, FlushInterval: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := flusher.Start(); err != nil {
		t.Fatal(err)
	}
	bus, err := sessionstore.NewBus(sessionstore.BusConfig{
		Paths:               paths,
		Flusher:             flusher,
		EvictionInterval:    1 * time.Hour,
		StateIdleEvictAfter: 1 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	rt := newFakeRealtime()
	builder := sessionstore.NewEnvelopeBuilder("inst-test", []string{"chat"})
	bridge := NewSocketIOBridge(rt, bus, builder, paths, NullAuth{}, slog.New(slog.NewTextHandler(testWriter{t: t}, nil)))
	if err := bridge.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bridge.Stop(ctx)
		bus.Stop(ctx)
		flusher.Stop(ctx)
	}
	return bridge, bus, rt, paths, cleanup
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func authedClient(rt *fakeRealtime, id, userID string) (*fakeClient, error) {
	auth := map[string]any{"token": userID, "user_id": userID}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if rt.authHandler != nil {
		if err := rt.authHandler(ctx, userID, auth); err != nil {
			return nil, err
		}
	}
	c := newFakeClient(id, auth)
	if err := rt.connect(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// TestBridge_EventWithSessionID_RoutesOnlyToSessionRoom is THE critical
// isolation test : an event with a session_id must NEVER be delivered to
// the app room or user room, only to session:<sid>.
func TestBridge_EventWithSessionID_RoutesOnlyToSessionRoom(t *testing.T) {
	_, bus, rt, _, cleanup := setupBridge(t)
	defer cleanup()

	ev := sessionstore.Event{
		Type:      sessionstore.EventAssistantMessage,
		SessionID: "sess-A",
		AppID:     "app-1",
		UserID:    "user-X",
		Message:   &sessionstore.MessagePayload{Role: "assistant", Content: "hi"},
	}
	if _, err := bus.Append(context.Background(), ev); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 2*time.Second, func() bool {
		return len(rt.recordedEmits()) >= 1
	})

	emits := rt.recordedEmits()
	if len(emits) != 1 {
		t.Fatalf("expected exactly 1 emit, got %d", len(emits))
	}
	e := emits[0]
	if e.Room != "session:sess-A" {
		t.Errorf("ISOLATION LEAK : routed to %q, must be session:sess-A only", e.Room)
	}
	if e.Namespace != "/events" {
		t.Errorf("namespace = %q, want /events", e.Namespace)
	}
	if e.Event != "event" {
		t.Errorf("event name = %q, want event", e.Event)
	}
}

func TestBridge_JoinSession_LeavesAllOtherSessionRooms(t *testing.T) {
	_, bus, rt, _, cleanup := setupBridge(t)
	defer cleanup()

	// Seed sessions owned by user-X.
	for _, sid := range []string{"sess-A", "sess-B", "sess-C"} {
		ev := sessionstore.Event{
			Type: sessionstore.EventSessionStarted, SessionID: sid, UserID: "user-X",
			Meta: &sessionstore.MetaPayload{},
		}
		bus.Append(context.Background(), ev)
	}

	c, err := authedClient(rt, "client-1", "user-X")
	if err != nil {
		t.Fatal(err)
	}

	// Join A, then B, then C.
	for _, sid := range []string{"sess-A", "sess-B", "sess-C"} {
		if err := rt.trigger(context.Background(), "join_session", c,
			map[string]any{"session_id": sid, "app_id": "app-1"}); err != nil {
			t.Fatalf("join_session %s: %v", sid, err)
		}
	}

	rooms := c.Rooms()
	hasA := false
	hasB := false
	hasC := false
	for _, r := range rooms {
		switch r {
		case "session:sess-A":
			hasA = true
		case "session:sess-B":
			hasB = true
		case "session:sess-C":
			hasC = true
		}
	}
	if hasA || hasB {
		t.Errorf("LEAK : after joining C, still in %v (must only be session:sess-C)", rooms)
	}
	if !hasC {
		t.Errorf("missing session:sess-C in rooms %v", rooms)
	}
}

func TestBridge_JoinSession_RejectsForeignSession(t *testing.T) {
	_, bus, rt, _, cleanup := setupBridge(t)
	defer cleanup()

	// Session owned by user-A.
	bus.Append(context.Background(), sessionstore.Event{
		Type: sessionstore.EventSessionStarted, SessionID: "sess-A", UserID: "user-A",
		Meta: &sessionstore.MetaPayload{},
	})

	// user-B tries to join sess-A.
	c, err := authedClient(rt, "client-attacker", "user-B")
	if err != nil {
		t.Fatal(err)
	}
	err = rt.trigger(context.Background(), "join_session", c,
		map[string]any{"session_id": "sess-A"})
	if err == nil {
		t.Fatal("expected unauthorized error, got nil")
	}
	for _, r := range c.Rooms() {
		if r == "session:sess-A" {
			t.Fatalf("ATTACK SUCCEEDED : user-B joined session of user-A; rooms=%v", c.Rooms())
		}
	}
	// The refusal must NOT be silent : the client gets an error event so the UI
	// shows why, instead of a dead session with no spinner / no error.
	gotErr := false
	for _, e := range c.recordedEmits() {
		if e.Event != "event" {
			continue
		}
		if raw, err := json.Marshal(e.Data); err == nil && strings.Contains(string(raw), "join_refused") {
			gotErr = true
		}
	}
	if !gotErr {
		t.Errorf("a refused join must emit a join_refused error to the client (silent refusal regression)")
	}
}

// TestBridge_JoinSession_RejectsNonexistentSession is the regression for the
// session-squatting hole : a client must NOT be able to join a session id that
// has never received an event (it would join the room and then receive the
// real owner's events once the session is created). Mirrors the HTTP 404 rule.
func TestBridge_JoinSession_RejectsNonexistentSession(t *testing.T) {
	_, _, rt, _, cleanup := setupBridge(t)
	defer cleanup()

	c, err := authedClient(rt, "client-squatter", "user-Z")
	if err != nil {
		t.Fatal(err)
	}
	// "victim-future-session" was never created — joining it must be rejected.
	if err := rt.trigger(context.Background(), "join_session", c,
		map[string]any{"session_id": "victim-future-session"}); err == nil {
		t.Fatal("expected rejection joining a non-existent session, got nil")
	}
	for _, r := range c.Rooms() {
		if r == "session:victim-future-session" {
			t.Fatalf("SQUAT SUCCEEDED : joined a not-yet-created session; rooms=%v", c.Rooms())
		}
	}
}

func TestBridge_TwoUsersDifferentSessions_NoCrossLeak(t *testing.T) {
	_, bus, rt, _, cleanup := setupBridge(t)
	defer cleanup()

	for _, kv := range []struct{ sid, uid string }{
		{"sess-A", "user-A"}, {"sess-B", "user-B"},
	} {
		bus.Append(context.Background(), sessionstore.Event{
			Type: sessionstore.EventSessionStarted, SessionID: kv.sid, UserID: kv.uid,
			Meta: &sessionstore.MetaPayload{},
		})
	}

	cA, _ := authedClient(rt, "cA", "user-A")
	cB, _ := authedClient(rt, "cB", "user-B")

	rt.trigger(context.Background(), "join_session", cA, map[string]any{"session_id": "sess-A"})
	rt.trigger(context.Background(), "join_session", cB, map[string]any{"session_id": "sess-B"})

	if !cA.hasRoom("session:sess-A") || cA.hasRoom("session:sess-B") {
		t.Fatalf("user-A rooms wrong: %v", cA.Rooms())
	}
	if !cB.hasRoom("session:sess-B") || cB.hasRoom("session:sess-A") {
		t.Fatalf("user-B rooms wrong: %v", cB.Rooms())
	}
}

func TestBridge_SendMessage_AppendsToBusWithCorrectUser(t *testing.T) {
	_, bus, rt, _, cleanup := setupBridge(t)
	defer cleanup()

	bus.Append(context.Background(), sessionstore.Event{
		Type: sessionstore.EventSessionStarted, SessionID: "sess-msg", UserID: "user-X",
		Meta: &sessionstore.MetaPayload{},
	})

	c, _ := authedClient(rt, "cm", "user-X")

	if err := rt.trigger(context.Background(), "send_message", c, map[string]any{
		"session_id": "sess-msg",
		"app_id":     "app-1",
		"text":       "hello",
	}); err != nil {
		t.Fatal(err)
	}

	state, err := bus.State("sess-msg")
	if err != nil {
		t.Fatal(err)
	}
	state.RLock()
	msgCount := len(state.Messages)
	var lastMsg sessionstore.Message
	if msgCount > 0 {
		lastMsg = state.Messages[msgCount-1]
	}
	state.RUnlock()

	if msgCount == 0 {
		t.Fatal("send_message did not append")
	}
	if lastMsg.Role != "user" || lastMsg.Content != "hello" {
		t.Fatalf("last msg wrong: %+v", lastMsg)
	}
}

func TestBridge_Replay_ReturnsEventsForRequestorOnly(t *testing.T) {
	_, bus, rt, _, cleanup := setupBridge(t)
	defer cleanup()

	// Two sessions belonging to two different users.
	bus.Append(context.Background(), sessionstore.Event{
		Type: sessionstore.EventSessionStarted, SessionID: "ses-mine", UserID: "user-A",
		Meta: &sessionstore.MetaPayload{},
	})
	bus.Append(context.Background(), sessionstore.Event{
		Type: sessionstore.EventUserMessage, SessionID: "ses-mine", UserID: "user-A",
		Message: &sessionstore.MessagePayload{Role: "user", Content: "mine 1"},
	})
	bus.Append(context.Background(), sessionstore.Event{
		Type: sessionstore.EventSessionStarted, SessionID: "ses-foreign", UserID: "user-B",
		Meta: &sessionstore.MetaPayload{},
	})

	// Flush to disk so ReadJSONL picks up the events.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Get flusher from bridge's bus
	c, _ := authedClient(rt, "cr", "user-A")

	// Wait briefly for the flusher to commit events to disk before replay.
	time.Sleep(100 * time.Millisecond)

	// user-A replays his own session — OK.
	if err := rt.trigger(context.Background(), "replay", c, map[string]any{
		"session_id": "ses-mine",
		"since":      0,
	}); err != nil {
		t.Fatalf("replay own: %v", err)
	}
	emitted := c.recordedEmits()
	if len(emitted) == 0 {
		t.Fatal("expected replay events on own session")
	}

	// user-A tries to replay user-B's session — must fail.
	err := rt.trigger(context.Background(), "replay", c, map[string]any{
		"session_id": "ses-foreign",
		"since":      0,
	})
	if err == nil {
		t.Fatal("replay of foreign session must reject")
	}
	_ = ctx
}

func TestBridge_AuthRejected_ConnectionRefused(t *testing.T) {
	paths := sessionstore.NewPaths(t.TempDir())
	flusher, _ := sessionstore.NewDiskFlusher(sessionstore.DiskFlusherConfig{
		Paths: paths, NumShards: 1, QueueCapPerShard: 1024,
		BatchMax: 100, FlushInterval: 2 * time.Millisecond,
	})
	flusher.Start()
	defer flusher.Stop(context.Background())
	bus, _ := sessionstore.NewBus(sessionstore.BusConfig{Paths: paths, Flusher: flusher})
	bus.Start(context.Background())
	defer bus.Stop(context.Background())
	rt := newFakeRealtime()
	builder := sessionstore.NewEnvelopeBuilder("inst-1", nil)
	bridge := NewSocketIOBridge(rt, bus, builder, paths, rejectingAuth{}, slog.Default())
	bridge.Start(context.Background())
	defer bridge.Stop(context.Background())

	ctx := context.Background()
	err := rt.authHandler(ctx, "bad-token", map[string]any{})
	if err == nil {
		t.Fatal("rejectingAuth must reject")
	}
}

type rejectingAuth struct{}

func (rejectingAuth) Validate(_ context.Context, _ string, _ map[string]any) (*AuthIdentity, error) {
	return nil, errors.New("denied")
}

func TestBridge_ConnectAutoJoinsUserRoom(t *testing.T) {
	_, _, rt, _, cleanup := setupBridge(t)
	defer cleanup()

	c, err := authedClient(rt, "c-1", "user-Z")
	if err != nil {
		t.Fatal(err)
	}
	if !c.hasRoom("user:user-Z") {
		t.Fatalf("user room not auto-joined: %v", c.Rooms())
	}
	// `connected` event echoed.
	emits := c.recordedEmits()
	if len(emits) == 0 {
		t.Fatal("expected connected event")
	}
	env, ok := emits[0].Data.(sessionstore.SocketEnvelope)
	if !ok {
		t.Fatalf("emit data type: %T", emits[0].Data)
	}
	if env.Type != "connected" {
		t.Errorf("first emit type = %q, want connected", env.Type)
	}
}

func TestBridge_AbortTurn_AppendsInterruptEvent(t *testing.T) {
	_, bus, rt, _, cleanup := setupBridge(t)
	defer cleanup()

	bus.Append(context.Background(), sessionstore.Event{
		Type: sessionstore.EventSessionStarted, SessionID: "sess-abort", UserID: "user-X",
		Meta: &sessionstore.MetaPayload{},
	})
	c, _ := authedClient(rt, "ca", "user-X")
	if err := rt.trigger(context.Background(), "abort_turn", c, map[string]any{
		"session_id": "sess-abort",
	}); err != nil {
		t.Fatal(err)
	}

	state, _ := bus.State("sess-abort")
	state.RLock()
	interrupted := state.Interrupted
	state.RUnlock()
	if !interrupted {
		t.Fatal("session not marked interrupted")
	}
}

func TestBridge_StrictRoomRouting_NoLeakToParentApp(t *testing.T) {
	_, bus, rt, _, cleanup := setupBridge(t)
	defer cleanup()

	// 100 events on session A within app-1. After dispatch, NONE should be
	// in room "app:app-1" — only "session:sess-A".
	for i := 0; i < 100; i++ {
		bus.Append(context.Background(), sessionstore.Event{
			Type: sessionstore.EventUserMessage, SessionID: "sess-A",
			AppID: "app-1", UserID: "user-X",
			Message: &sessionstore.MessagePayload{Role: "user", Content: fmt.Sprintf("%d", i)},
		})
	}

	waitFor(t, 2*time.Second, func() bool {
		return len(rt.recordedEmits()) >= 100
	})

	emits := rt.recordedEmits()
	if len(emits) < 100 {
		t.Fatalf("expected 100 emits, got %d", len(emits))
	}
	var leakedToApp int
	var leakedToUser int
	for _, e := range emits {
		if e.Room == "app:app-1" {
			leakedToApp++
		}
		if e.Room == "user:user-X" {
			leakedToUser++
		}
	}
	if leakedToApp > 0 || leakedToUser > 0 {
		t.Fatalf("LEAK : %d events to app room, %d to user room (should be 0)", leakedToApp, leakedToUser)
	}
}

func TestBridge_BusEventDelivery_StatsCount(t *testing.T) {
	bridge, bus, _, _, cleanup := setupBridge(t)
	defer cleanup()

	for i := 0; i < 10; i++ {
		bus.Append(context.Background(), sessionstore.Event{
			Type: sessionstore.EventUserMessage, SessionID: "s",
			Message: &sessionstore.MessagePayload{Role: "user", Content: "x"},
		})
	}
	waitFor(t, 1*time.Second, func() bool {
		return bridge.Stats().Emits >= 10
	})
	if got := bridge.Stats().Emits; got < 10 {
		t.Fatalf("emits = %d, want >= 10", got)
	}
}

func waitFor(t *testing.T, max time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitFor timeout (%v) — condition never true", max)
}

// Sanity : verify fakeClient implements ports.RealtimeClient.
var _ ports.RealtimeClient = (*fakeClient)(nil)
var _ ports.RealtimeServer = (*fakeRealtime)(nil)

// Hush unused warnings.
var _ = atomic.LoadUint64
