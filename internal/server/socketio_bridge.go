package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/ports"
	"github.com/mbathepaul/digitorn/internal/runtime/contextsvc"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// AuthValidator resolves a handshake token into a user identity. T6 will
// plug a JWKS-backed validator; V1 ships with NullAuth (accept-all dev mode).
type AuthValidator interface {
	Validate(ctx context.Context, token string, metadata map[string]any) (*AuthIdentity, error)
}

type AuthIdentity struct {
	UserID       string
	Capabilities []string
	Roles        []string
}

// NullAuth accepts every connection and derives the user_id from the token
// (or `metadata["user_id"]`). Suitable for development only.
type NullAuth struct{}

func (NullAuth) Validate(_ context.Context, token string, metadata map[string]any) (*AuthIdentity, error) {
	uid, _ := metadata["user_id"].(string)
	if uid == "" {
		uid = token
	}
	if uid == "" {
		uid = "anonymous"
	}
	return &AuthIdentity{UserID: uid}, nil
}

// SocketIOBridge wires the sessionstore.Bus to the Socket.IO realtime server.
// Every event a client sees on `/events` flows through this bridge, with
// strict per-session room routing via PrimaryRoomFor — no event can leak
// outside its session's room.
type SocketIOBridge struct {
	rt      ports.RealtimeServer
	bus     *sessionstore.Bus
	builder *sessionstore.EnvelopeBuilder
	auth    AuthValidator
	log     *slog.Logger
	paths   sessionstore.Paths

	// BrainFor resolves an app's entry-agent brain so the join handler can
	// compute the context window for the "last context on open" gauge. Set by
	// bootstrap after the daemon is assembled ; nil in tests (the join emit is
	// then a no-op, never fatal).
	BrainFor func(appID string) schema.Brain

	// Per-client state. Keyed by client.ID().
	clients sync.Map

	sub        *sessionstore.Subscription
	subStarted atomic.Bool

	// Metrics
	connectsTotal    atomic.Uint64
	disconnectsTotal atomic.Uint64
	authRejected     atomic.Uint64
	emitsTotal       atomic.Uint64
	emitErrors       atomic.Uint64
	dropsNoRouting   atomic.Uint64
	actionsTotal     atomic.Uint64
	actionsRejected  atomic.Uint64
}

// clientState holds per-socket context. Mutex protects appID + sessionID
// transitions which must be atomic with the underlying room changes.
type clientState struct {
	mu           sync.Mutex
	userID       string
	appID        string
	sessionID    string
	connectedAt  time.Time
	capabilities []string
}

const bridgeNamespace = "/events"

// NewSocketIOBridge constructs the bridge. Call Start to begin forwarding.
func NewSocketIOBridge(rt ports.RealtimeServer, bus *sessionstore.Bus, builder *sessionstore.EnvelopeBuilder, paths sessionstore.Paths, auth AuthValidator, log *slog.Logger) *SocketIOBridge {
	if auth == nil {
		auth = NullAuth{}
	}
	if log == nil {
		log = slog.Default()
	}
	b := &SocketIOBridge{
		rt:      rt,
		bus:     bus,
		builder: builder,
		auth:    auth,
		log:     log,
		paths:   paths,
	}
	rt.SetAuthHandler(b.handleAuth)
	rt.OnConnection(b.handleConnect)
	rt.OnDisconnection(b.onDisconnect)
	rt.OnEvent("join_app", b.handleJoinApp)
	rt.OnEvent("leave_app", b.handleLeaveApp)
	rt.OnEvent("join_session", b.handleJoinSession)
	rt.OnEvent("leave_session", b.handleLeaveSession)
	rt.OnEvent("send_message", b.handleSendMessage)
	rt.OnEvent("abort_turn", b.handleAbortTurn)
	rt.OnEvent("resolve_approval", b.handleResolveApproval)
	rt.OnEvent("replay", b.handleReplay)
	rt.OnEvent("latest_seq", b.handleLatestSeq)
	rt.OnEvent("ping", b.handlePing)
	return b
}

// Start subscribes the bridge to the bus. From now on, every event the bus
// emits is dispatched to the appropriate room via PrimaryRoomFor.
func (b *SocketIOBridge) Start(ctx context.Context) error {
	if !b.subStarted.CompareAndSwap(false, true) {
		return nil
	}
	sub, err := b.bus.SubscribeAll(b.dispatchToRealtime)
	if err != nil {
		b.subStarted.Store(false)
		return fmt.Errorf("bridge: subscribe bus: %w", err)
	}
	b.sub = sub
	return nil
}

// Stop cancels the bus subscription. Active connections remain open — call
// realtime.Close to disconnect them.
func (b *SocketIOBridge) Stop(ctx context.Context) error {
	if b.sub != nil {
		b.sub.Cancel()
		b.sub = nil
	}
	return nil
}

// dispatchToRealtime forwards a single bus event to its destination room.
// The strict-isolation invariant : if the event has a session_id, it is
// routed ONLY to "session:<sid>" — never to the app or user room (which
// would leak the event to all viewers of that app or that user's other
// sessions). This is THE rule that prevents cross-session leaks.
func (b *SocketIOBridge) dispatchToRealtime(ev sessionstore.Event) {
	room := sessionstore.PrimaryRoomFor(&ev)
	if room == "" {
		b.dropsNoRouting.Add(1)
		b.log.Warn("bridge: dropping event with no routing key",
			slog.String("type", string(ev.Type)),
			slog.Uint64("seq", ev.Seq))
		return
	}
	env := b.builder.Build(&ev)
	b.emitEnvelope(room, &ev, env)

	// Agent fan-out : a delegated sub-agent runs in an ISOLATED sub-session
	// (<root>::agent::<runID>) so its events never pollute the coordinator's
	// session state or its LLM context. But a client watching the TOP-LEVEL
	// session must still see the sub-agent work in real time. So we ALSO stream
	// every sub-session event to the root session's room, tagged with the
	// emitting agent's run id for client-side attribution. This is transport
	// only — the durable event still lives in the sub-session (isolation
	// intact) — and stays within the same session tree + same user, so it is
	// NOT a cross-session / cross-user leak (the rule above still holds : we
	// never route to an app or user room).
	if _, runID, isSub := sessionstore.SubAgentSession(ev.SessionID); isSub {
		// Fan to EVERY ancestor room (top root … immediate parent), each tagged
		// with that ancestor as RootSessionID so a client joined to it (the top
		// session, or one drilled into an intermediate sub-agent) matches its own
		// fan-out guard. Depth-1 yields exactly the old single top-root emit.
		for _, anc := range sessionstore.SubAgentAncestors(ev.SessionID) {
			ancRoom := "session:" + anc
			if ancRoom == room {
				continue
			}
			fan := env
			fan.AgentRunID = runID
			fan.RootSessionID = anc
			b.emitEnvelope(ancRoom, &ev, fan)
		}
	}
}

// emitEnvelope sends one already-built envelope to a room, counting the emit /
// error metrics. Factored out so the agent fan-out reuses the exact same
// emit-and-account path as the primary route.
func (b *SocketIOBridge) emitEnvelope(room string, ev *sessionstore.Event, env sessionstore.SocketEnvelope) {
	if err := b.rt.Emit(context.Background(), bridgeNamespace, room, "event", env); err != nil {
		b.emitErrors.Add(1)
		b.log.Error("bridge: emit failed",
			slog.String("room", room),
			slog.String("type", string(ev.Type)),
			slog.String("err", err.Error()))
		return
	}
	b.emitsTotal.Add(1)
}

// handleAuth validates the handshake token. On success it stashes the
// resolved AuthIdentity inside the handshake auth map so connection-time
// handlers can read it back.
func (b *SocketIOBridge) handleAuth(ctx context.Context, token string, metadata map[string]any) error {
	id, err := b.auth.Validate(ctx, token, metadata)
	if err != nil {
		b.authRejected.Add(1)
		return err
	}
	if id == nil || id.UserID == "" {
		b.authRejected.Add(1)
		return errors.New("auth: empty user_id")
	}
	if metadata != nil {
		metadata["__user_id"] = id.UserID
		if len(id.Capabilities) > 0 {
			metadata["__capabilities"] = id.Capabilities
		}
	}
	return nil
}

func (b *SocketIOBridge) handleConnect(ctx context.Context, c ports.RealtimeClient) error {
	auth := c.Auth()
	userID, _ := auth["__user_id"].(string)
	caps, _ := auth["__capabilities"].([]string)
	if userID == "" {
		c.Disconnect()
		return errors.New("bridge: connection without resolved user_id")
	}

	state := &clientState{
		userID:       userID,
		connectedAt:  time.Now(),
		capabilities: caps,
	}
	b.clients.Store(c.ID(), state)
	b.connectsTotal.Add(1)

	// Auto-join user room — every user always sees their per-user events.
	if err := c.Join("user:" + userID); err != nil {
		b.log.Warn("bridge: join user room failed",
			slog.String("client_id", c.ID()),
			slog.String("user_id", userID),
			slog.String("err", err.Error()))
	}

	// Echo a `connected` event to the client so it can confirm handshake.
	connEnv := sessionstore.SocketEnvelope{
		Type:       "connected",
		Kind:       "system",
		Control:    true,
		UserID:     userID,
		InstanceID: b.builder.InstanceID,
		Ts:         time.Now().UTC().Format(time.RFC3339Nano),
		Payload:    map[string]any{"capabilities": caps},
	}
	_ = c.Emit("event", connEnv)
	return nil
}

func (b *SocketIOBridge) handleJoinApp(ctx context.Context, c ports.RealtimeClient, data any) error {
	b.actionsTotal.Add(1)
	st := b.stateOf(c)
	if st == nil {
		return errors.New("join_app: unknown client")
	}
	appID, _ := extractStr(data, "app_id")
	if appID == "" {
		b.actionsRejected.Add(1)
		return errors.New("join_app: app_id required")
	}
	st.mu.Lock()
	old := st.appID
	st.appID = appID
	st.mu.Unlock()
	if old != "" && old != appID {
		_ = c.Leave("app:" + old)
	}
	if err := c.Join("app:" + appID); err != nil {
		b.actionsRejected.Add(1)
		return err
	}
	return nil
}

func (b *SocketIOBridge) handleLeaveApp(ctx context.Context, c ports.RealtimeClient, data any) error {
	b.actionsTotal.Add(1)
	st := b.stateOf(c)
	if st == nil {
		return errors.New("leave_app: unknown client")
	}
	appID, _ := extractStr(data, "app_id")
	st.mu.Lock()
	if appID == "" {
		appID = st.appID
	}
	if st.appID == appID {
		st.appID = ""
	}
	st.mu.Unlock()
	if appID != "" {
		_ = c.Leave("app:" + appID)
	}
	return nil
}

// handleJoinSession enforces the strict-isolation invariant : when a client
// joins a session, it LEAVES every other session room atomically. This is
// the rule that prevents tabs from leaking events across sessions.
func (b *SocketIOBridge) handleJoinSession(ctx context.Context, c ports.RealtimeClient, data any) error {
	b.actionsTotal.Add(1)
	st := b.stateOf(c)
	if st == nil {
		return errors.New("join_session: unknown client")
	}
	appID, _ := extractStr(data, "app_id")
	sessionID, _ := extractStr(data, "session_id")
	if sessionID == "" {
		b.actionsRejected.Add(1)
		return errors.New("join_session: session_id required")
	}

	if err := b.authorizeSession(ctx, st.userID, appID, sessionID); err != nil {
		b.actionsRejected.Add(1)
		b.log.Warn("bridge: join_session unauthorized",
			slog.String("client_id", c.ID()),
			slog.String("user_id", st.userID),
			slog.String("session_id", sessionID),
			slog.String("err", err.Error()))
		// A refused join would otherwise be SILENT — the client never gets the room,
		// so it sees no turn, no error, no spinner. Push the reason straight to it.
		b.emitJoinError(c, sessionID, appID, err)
		return err
	}

	// LEAVE every other session room first.
	for _, r := range c.Rooms() {
		if strings.HasPrefix(r, "session:") && r != "session:"+sessionID {
			_ = c.Leave(r)
		}
	}

	st.mu.Lock()
	st.sessionID = sessionID
	if appID != "" {
		st.appID = appID
	}
	st.mu.Unlock()

	if err := c.Join("session:" + sessionID); err != nil {
		b.actionsRejected.Add(1)
		return err
	}
	// Push the last known context occupancy to THIS client so its footer shows the
	// real "ctx used/window" on open — before any new turn. Done in a goroutine and
	// emitted DIRECTLY to the joining client : it must never block the socket
	// handler (a cold-session load can be heavy) and never race room membership
	// (emitting to the room right after Join sometimes missed the client). The CLI
	// already updates its footer on a context_tokens event. Best-effort.
	go b.emitContextOnJoin(c, sessionID, appID)
	return nil
}

// emitContextOnJoin re-sends the session's current context gauge straight to the
// joining client so it sees the last real occupancy without waiting for the next
// turn's context_tokens event. Off the socket loop (own goroutine) and direct to
// the client (no room race). No-op when the window/brain or occupancy is unknown
// (a fresh session that never ran a turn).
func (b *SocketIOBridge) emitContextOnJoin(c ports.RealtimeClient, sessionID, appID string) {
	if c == nil || b.BrainFor == nil || b.bus == nil || b.builder == nil {
		return
	}
	st, err := b.bus.State(sessionID)
	if err != nil || st == nil {
		return
	}
	snap := st.Snapshot()
	view := contextsvc.Resolve(snap, b.BrainFor(appID))
	// Emit as long as the REAL window is known (resolved from the brain), even
	// when used==0 (a fresh session) — so the client shows the true denominator
	// immediately instead of falling back to a guessed window. used==0 is honest
	// (no context counted until the first turn) and corrects upward on turn 1.
	if view.Window <= 0 {
		return
	}
	ev := sessionstore.Event{
		Type:      sessionstore.EventContextTokens,
		SessionID: sessionID,
		CtxTokens: &sessionstore.ContextTokensPayload{
			Total:    snap.ContextTokens,
			System:   snap.ContextSystemTokens,
			Tools:    snap.ContextToolsTokens,
			Messages: snap.ContextMessageTokens,
			Window:   view.Window,
			Limit:    view.MaxTokens,
		},
	}
	if err := c.Emit("event", b.builder.Build(&ev)); err != nil {
		b.log.Debug("bridge: context-on-join emit failed", slog.String("err", err.Error()))
	}
}

// emitJoinError pushes a client-facing error event STRAIGHT to the joining client
// when a join is refused. Without it the refusal is silent — the client never gets
// the room, so it shows no turn, no error, no spinner. Reuses the standard
// DaemonError shape so the existing client error path renders it.
func (b *SocketIOBridge) emitJoinError(c ports.RealtimeClient, sessionID, appID string, cause error) {
	if c == nil || b.builder == nil || cause == nil {
		return
	}
	retry := false
	ev := sessionstore.Event{
		Type:      sessionstore.EventError,
		SessionID: sessionID,
		AppID:     appID,
		Error: &sessionstore.ErrorPayload{
			Error:    "cannot open session",
			Message:  cause.Error(),
			Code:     "join_refused",
			Category: "auth",
			Detail:   cause.Error(),
			Retry:    &retry,
			Source:   "join",
		},
	}
	if err := c.Emit("event", b.builder.Build(&ev)); err != nil {
		b.log.Debug("bridge: join-error emit failed", slog.String("err", err.Error()))
	}
}

func (b *SocketIOBridge) handleLeaveSession(ctx context.Context, c ports.RealtimeClient, data any) error {
	b.actionsTotal.Add(1)
	st := b.stateOf(c)
	if st == nil {
		return errors.New("leave_session: unknown client")
	}
	sessionID, _ := extractStr(data, "session_id")
	st.mu.Lock()
	if sessionID == "" {
		sessionID = st.sessionID
	}
	if st.sessionID == sessionID {
		st.sessionID = ""
	}
	st.mu.Unlock()
	if sessionID != "" {
		_ = c.Leave("session:" + sessionID)
	}
	// Make sure the client still sees its per-user events.
	if st.userID != "" {
		_ = c.Join("user:" + st.userID)
	}
	return nil
}

func (b *SocketIOBridge) handleSendMessage(ctx context.Context, c ports.RealtimeClient, data any) error {
	b.actionsTotal.Add(1)
	st := b.stateOf(c)
	if st == nil {
		return errors.New("send_message: unknown client")
	}
	appID, _ := extractStr(data, "app_id")
	sessionID, _ := extractStr(data, "session_id")
	text, _ := extractStr(data, "text")
	if sessionID == "" || text == "" {
		b.actionsRejected.Add(1)
		return errors.New("send_message: session_id and text required")
	}
	if err := b.authorizeSession(ctx, st.userID, appID, sessionID); err != nil {
		b.actionsRejected.Add(1)
		return err
	}
	ev := sessionstore.Event{
		Type:      sessionstore.EventUserMessage,
		SessionID: sessionID,
		AppID:     appID,
		UserID:    st.userID,
		Message: &sessionstore.MessagePayload{
			Role:    "user",
			Content: text,
		},
	}
	if _, err := b.bus.Append(ctx, ev); err != nil {
		b.actionsRejected.Add(1)
		return fmt.Errorf("send_message: append: %w", err)
	}
	return nil
}

func (b *SocketIOBridge) handleAbortTurn(ctx context.Context, c ports.RealtimeClient, data any) error {
	b.actionsTotal.Add(1)
	st := b.stateOf(c)
	if st == nil {
		return errors.New("abort_turn: unknown client")
	}
	appID, _ := extractStr(data, "app_id")
	sessionID, _ := extractStr(data, "session_id")
	if sessionID == "" {
		b.actionsRejected.Add(1)
		return errors.New("abort_turn: session_id required")
	}
	if err := b.authorizeSession(ctx, st.userID, appID, sessionID); err != nil {
		b.actionsRejected.Add(1)
		return err
	}
	ev := sessionstore.Event{
		Type:      sessionstore.EventSessionInterrupt,
		SessionID: sessionID,
		AppID:     appID,
		UserID:    st.userID,
	}
	if _, err := b.bus.Append(ctx, ev); err != nil {
		b.actionsRejected.Add(1)
		return fmt.Errorf("abort_turn: append: %w", err)
	}
	return nil
}

func (b *SocketIOBridge) handleResolveApproval(ctx context.Context, c ports.RealtimeClient, data any) error {
	b.actionsTotal.Add(1)
	st := b.stateOf(c)
	if st == nil {
		return errors.New("resolve_approval: unknown client")
	}
	appID, _ := extractStr(data, "app_id")
	sessionID, _ := extractStr(data, "session_id")
	approvalID, _ := extractStr(data, "approval_id")
	action, _ := extractStr(data, "action")
	reason, _ := extractStr(data, "reason")
	if sessionID == "" || approvalID == "" || action == "" {
		b.actionsRejected.Add(1)
		return errors.New("resolve_approval: session_id, approval_id, action required")
	}
	if err := b.authorizeSession(ctx, st.userID, appID, sessionID); err != nil {
		b.actionsRejected.Add(1)
		return err
	}
	typ := sessionstore.EventApprovalGranted
	if action == "deny" || action == "denied" {
		typ = sessionstore.EventApprovalDenied
	}
	ev := sessionstore.Event{
		Type:      typ,
		SessionID: sessionID,
		AppID:     appID,
		UserID:    st.userID,
		Approval: &sessionstore.ApprovalPayload{
			ID:     approvalID,
			Status: action,
			Reason: reason,
		},
	}
	if _, err := b.bus.Append(ctx, ev); err != nil {
		b.actionsRejected.Add(1)
		return err
	}
	return nil
}

// handleReplay returns events with seq > since for the requested session.
// The user MUST be authorized for that session — replay returns nothing
// for sessions the user doesn't own.
func (b *SocketIOBridge) handleReplay(ctx context.Context, c ports.RealtimeClient, data any) error {
	b.actionsTotal.Add(1)
	st := b.stateOf(c)
	if st == nil {
		return errors.New("replay: unknown client")
	}
	appID, _ := extractStr(data, "app_id")
	sessionID, _ := extractStr(data, "session_id")
	since, _ := extractUint64(data, "since")
	if sessionID == "" {
		b.actionsRejected.Add(1)
		return errors.New("replay: session_id required")
	}
	if err := b.authorizeSession(ctx, st.userID, appID, sessionID); err != nil {
		b.actionsRejected.Add(1)
		return err
	}
	// Drain the write-behind queue to disk first : appends are buffered and
	// flushed on a timer, so reading the file straight away would miss events
	// already committed to memory but not yet flushed — exactly the events a
	// just-reconnected client is replaying to catch up on. Without this, replay
	// returns a stale snapshot and the client silently loses recent events.
	if err := b.bus.FlushPending(ctx); err != nil {
		b.log.Warn("replay: flush before read failed", slog.String("err", err.Error()))
	}
	res, err := sessionstore.ReadJSONL(b.paths.EventsFile(sessionID), sessionstore.JSONLBestEffort, "")
	if err != nil {
		return fmt.Errorf("replay: read: %w", err)
	}
	count := 0
	for i := range res.Events {
		ev := &res.Events[i]
		if ev.Seq <= since {
			continue
		}
		// Strict isolation : double-check the session_id on the event itself.
		if ev.SessionID != sessionID {
			continue
		}
		env := b.builder.Build(ev)
		_ = c.Emit("event", env)
		count++
	}
	_ = c.Emit("replay_done", map[string]any{
		"session_id": sessionID,
		"since":      since,
		"delivered":  count,
	})
	return nil
}

func (b *SocketIOBridge) handleLatestSeq(ctx context.Context, c ports.RealtimeClient, data any) error {
	b.actionsTotal.Add(1)
	st := b.stateOf(c)
	if st == nil {
		return errors.New("latest_seq: unknown client")
	}
	sessionID, _ := extractStr(data, "session_id")
	if sessionID == "" {
		sessionID = st.sessionID
	}
	if sessionID == "" {
		_ = c.Emit("latest_seq", map[string]any{"seq": 0})
		return nil
	}
	if err := b.authorizeSession(ctx, st.userID, "", sessionID); err != nil {
		b.actionsRejected.Add(1)
		return err
	}
	state, err := b.bus.State(sessionID)
	if err != nil {
		return err
	}
	state.RLock()
	seq := state.LastSeq
	state.RUnlock()
	_ = c.Emit("latest_seq", map[string]any{"session_id": sessionID, "seq": seq})
	return nil
}

func (b *SocketIOBridge) handlePing(ctx context.Context, c ports.RealtimeClient, data any) error {
	_ = c.Emit("pong", map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano)})
	return nil
}

// authorizeSession verifies that userID may access (appID, sessionID). It
// mirrors the HTTP requireOwnedSession contract so the realtime surface can't
// be used to bypass it :
//
//   - no user_id            → reject (must be authenticated ; the handshake
//     auth validator already gates the connection, this is defence in depth).
//   - session doesn't exist → reject as not-found. Critically this CLOSES the
//     "session squatting" hole : the old code allowed joining a session whose
//     owner was still "" (which, because State() cold-loads an empty state for
//     any id, meant ANY not-yet-created id). A client could join a victim's
//     future session id and then receive the real owner's events once it
//     materialised. Existence is decided by FirstSeq != 0 — identical to the
//     HTTP path's 404 rule.
//   - owner mismatch        → reject. (Ownerless-but-existing sessions stay
//     accessible — owner == "" with FirstSeq != 0 is a system/legacy session.)
func (b *SocketIOBridge) authorizeSession(ctx context.Context, userID, appID, sessionID string) error {
	if userID == "" {
		return errors.New("unauthorized: no user_id")
	}
	state, err := b.bus.State(sessionID)
	if err != nil {
		return fmt.Errorf("authorize: %w", err)
	}
	state.RLock()
	owner := state.UserID
	first := state.FirstSeq
	state.RUnlock()
	if first == 0 {
		// Never received an event → does not exist. Reject rather than let a
		// client squat the id before its real owner creates it.
		return errors.New("unauthorized: session not found")
	}
	if owner != "" && owner != userID {
		return errors.New("unauthorized: session belongs to another user")
	}
	return nil
}

func (b *SocketIOBridge) stateOf(c ports.RealtimeClient) *clientState {
	if v, ok := b.clients.Load(c.ID()); ok {
		return v.(*clientState)
	}
	// Heal a live socket whose per-connection state is missing — e.g. one
	// restored by socket.io's connection-state-recovery WITHOUT re-firing the
	// `connection` event, so handleConnect never ran. Rebuild from the handshake
	// identity (still carried on the socket) instead of rejecting every
	// join/replay/send on a connected-but-stateless socket. appID/sessionID are
	// left empty on purpose : the client re-emits join_session on reconnect,
	// which sets them. Returns nil only when there's genuinely no identity.
	auth := c.Auth()
	userID, _ := auth["__user_id"].(string)
	if userID == "" {
		return nil
	}
	caps, _ := auth["__capabilities"].([]string)
	fresh := &clientState{userID: userID, connectedAt: time.Now(), capabilities: caps}
	actual, loaded := b.clients.LoadOrStore(c.ID(), fresh)
	if !loaded {
		_ = c.Join("user:" + userID) // restore the per-user room the lost connect would have joined
	}
	return actual.(*clientState)
}

// onDisconnect cleans up per-socket state. Called explicitly via the realtime
// server's disconnect event when wired.
func (b *SocketIOBridge) onDisconnect(clientID string) {
	if _, ok := b.clients.LoadAndDelete(clientID); ok {
		b.disconnectsTotal.Add(1)
	}
}

type BridgeStats struct {
	Connects        uint64
	Disconnects     uint64
	AuthRejected    uint64
	Emits           uint64
	EmitErrors      uint64
	DropsNoRouting  uint64
	Actions         uint64
	ActionsRejected uint64
	LiveClients     int
}

func (b *SocketIOBridge) Stats() BridgeStats {
	live := 0
	b.clients.Range(func(_, _ any) bool { live++; return true })
	return BridgeStats{
		Connects:        b.connectsTotal.Load(),
		Disconnects:     b.disconnectsTotal.Load(),
		AuthRejected:    b.authRejected.Load(),
		Emits:           b.emitsTotal.Load(),
		EmitErrors:      b.emitErrors.Load(),
		DropsNoRouting:  b.dropsNoRouting.Load(),
		Actions:         b.actionsTotal.Load(),
		ActionsRejected: b.actionsRejected.Load(),
		LiveClients:     live,
	}
}

func extractStr(data any, key string) (string, bool) {
	m, ok := data.(map[string]any)
	if !ok {
		return "", false
	}
	s, ok := m[key].(string)
	return s, ok
}

func extractUint64(data any, key string) (uint64, bool) {
	m, ok := data.(map[string]any)
	if !ok {
		return 0, false
	}
	switch v := m[key].(type) {
	case float64:
		return uint64(v), true
	case int:
		return uint64(v), true
	case int64:
		return uint64(v), true
	case uint64:
		return v, true
	case string:
		var n uint64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}
