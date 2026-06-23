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

type AuthValidator interface {
	Validate(ctx context.Context, token string, metadata map[string]any) (*AuthIdentity, error)
}

type AuthIdentity struct {
	UserID       string
	Capabilities []string
	Roles        []string
}

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

type SocketIOBridge struct {
	rt      ports.RealtimeServer
	bus     *sessionstore.Bus
	builder *sessionstore.EnvelopeBuilder
	auth    AuthValidator
	log     *slog.Logger
	paths   sessionstore.Paths

	BrainFor func(appID string) schema.Brain

	SessionWindowBrain func(snap sessionstore.SessionSnapshot) schema.Brain

	PreWarmContext func(sessionID, appID string)

	clients sync.Map

	sub        *sessionstore.Subscription
	subStarted atomic.Bool

	connectsTotal    atomic.Uint64
	disconnectsTotal atomic.Uint64
	authRejected     atomic.Uint64
	emitsTotal       atomic.Uint64
	emitErrors       atomic.Uint64
	dropsNoRouting   atomic.Uint64
	actionsTotal     atomic.Uint64
	actionsRejected  atomic.Uint64
}

type clientState struct {
	mu           sync.Mutex
	userID       string
	appID        string
	sessionID    string
	connectedAt  time.Time
	capabilities []string
}

const bridgeNamespace = "/events"

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

func (b *SocketIOBridge) Stop(ctx context.Context) error {
	if b.sub != nil {
		b.sub.Cancel()
		b.sub = nil
	}
	return nil
}

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

	if _, runID, isSub := sessionstore.SubAgentSession(ev.SessionID); isSub {
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

	if err := c.Join("user:" + userID); err != nil {
		b.log.Warn("bridge: join user room failed",
			slog.String("client_id", c.ID()),
			slog.String("user_id", userID),
			slog.String("err", err.Error()))
	}

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
		b.emitJoinError(c, sessionID, appID, err)
		return err
	}

	// When joining a child (sub-agent) session, keep the root session room so
	// the client continues to receive agent_result / agent_progress events that
	// are routed to the root session. Only leave other session rooms when
	// switching between root sessions.
	_, _, isSubSession := sessionstore.SubAgentSession(sessionID)
	if !isSubSession {
		for _, r := range c.Rooms() {
			if strings.HasPrefix(r, "session:") && r != "session:"+sessionID {
				_ = c.Leave(r)
			}
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
	go b.emitContextOnJoin(c, sessionID, appID)
	return nil
}

func (b *SocketIOBridge) emitContextOnJoin(c ports.RealtimeClient, sessionID, appID string) {
	if c == nil || b.BrainFor == nil || b.bus == nil || b.builder == nil {
		return
	}
	st, err := b.bus.State(sessionID)
	if err != nil || st == nil {
		return
	}
	snap := st.Snapshot()

	if snap.ContextTokens == 0 && b.PreWarmContext != nil {
		go b.PreWarmContext(sessionID, appID)
	}

	brain := b.BrainFor(appID)
	if b.SessionWindowBrain != nil {
		brain = b.SessionWindowBrain(snap)
	}
	view := contextsvc.Resolve(snap, brain)
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
	// Sub-agent sessions (ID contains "::agent::") may have FirstSeq=0 when they
	// are freshly spawned and their events haven't been committed yet. Allow the
	// join so the client can receive live events as the agent produces them.
	isSubAgent := strings.Contains(sessionID, "::agent::")
	if first == 0 && !isSubAgent {
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
	auth := c.Auth()
	userID, _ := auth["__user_id"].(string)
	if userID == "" {
		return nil
	}
	caps, _ := auth["__capabilities"].([]string)
	fresh := &clientState{userID: userID, connectedAt: time.Now(), capabilities: caps}
	actual, loaded := b.clients.LoadOrStore(c.ID(), fresh)
	if !loaded {
		_ = c.Join("user:" + userID)
	}
	return actual.(*clientState)
}

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
