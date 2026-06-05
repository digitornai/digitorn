package client

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	sio "github.com/zishang520/socket.io/clients/socket/v3"
)

// ConnState mirrors the four conn states the TUI cares about. The wire library
// has more granular states ; we collapse them here so the consumer only deals
// with what is meaningful to a human.
type ConnState int

const (
	ConnStateConnecting ConnState = iota
	ConnStateConnected
	ConnStateReconnecting
	ConnStateDisconnected
)

func (s ConnState) String() string {
	switch s {
	case ConnStateConnecting:
		return "connecting"
	case ConnStateConnected:
		return "connected"
	case ConnStateReconnecting:
		return "reconnecting"
	case ConnStateDisconnected:
		return "disconnected"
	}
	return "?"
}

// Envelope is the wire shape emitted by the daemon on `event`. Field names
// match SocketEnvelope on the daemon side (json tags are byte-identical to
// the legacy Python contract — do NOT rename).
type Envelope struct {
	EventID    string         `json:"event_id,omitempty"`
	Type       string         `json:"type"`
	Kind       string         `json:"kind"`
	Seq        uint64         `json:"seq"`
	AppID      string         `json:"app_id,omitempty"`
	SessionID  string         `json:"session_id,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
	Ts         string         `json:"ts"`
	Control    bool           `json:"control"`
	UserID     string         `json:"user_id,omitempty"`
	InstanceID string         `json:"instance_id"`

	// AgentRunID / RootSessionID are set ONLY on a sub-agent's OWN turn event
	// that the daemon bridge fanned out to its root session room. They let the
	// TUI attribute the event to the right sub-agent (run_id) and recognise it
	// as belonging to the active root session (root_session_id) rather than a
	// genuinely different session. Absent on every normal event.
	AgentRunID    string `json:"agent_run_id,omitempty"`
	RootSessionID string `json:"root_session_id,omitempty"`

	// CorrelationID ties a transient event to a parent : a run_parallel child's
	// tool_progress carries the parent run_parallel call_id here so the TUI
	// updates the right chip. Absent on plain events.
	CorrelationID string `json:"correlation_id,omitempty"`

	// LiveOutputTokens is the running token estimate carried on an
	// assistant_delta (CTX-7.5) — the TUI shows it as a live, incrementing
	// counter next to the working indicator. 0 on every other event.
	LiveOutputTokens int `json:"live_output_tokens,omitempty"`
}

// Realtime wraps the zishang520 Socket.IO v3 client with our handshake (JWT +
// user_id), our namespace (/events), and a state-tracker. The TUI consumes it
// via OnEnvelope / OnState callbacks ; concurrency is the consumer's problem.
//
// Reconnection is delegated to the underlying library (exponential backoff,
// jitter, infinite retries by default). On reconnect we automatically rejoin
// the last session room and request a replay since the highest seq we saw.
type Realtime struct {
	baseURL string
	token   string
	userID  string

	mgr  *sio.Manager
	sock *sio.Socket

	mu       sync.Mutex
	state    ConnState
	appID    string
	sessID   string
	lastSeq  atomic.Uint64
	closed   atomic.Bool
	onEnv    func(Envelope)
	onState  func(ConnState, error)
	stopOnce sync.Once
}

// RealtimeOptions configures the connection. BaseURL must include scheme +
// host + port — e.g. http://127.0.0.1:28002. The namespace `/events` is
// appended internally.
type RealtimeOptions struct {
	BaseURL string
	Token   string
	UserID  string
}

// NewRealtime constructs a non-connected client. Call Connect to handshake.
func NewRealtime(opts RealtimeOptions) (*Realtime, error) {
	if opts.BaseURL == "" {
		return nil, errors.New("realtime: BaseURL is required")
	}
	u, err := url.Parse(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("realtime: parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("realtime: unsupported scheme %q", u.Scheme)
	}
	if opts.UserID == "" {
		return nil, errors.New("realtime: UserID is required")
	}
	return &Realtime{
		baseURL: opts.BaseURL,
		token:   opts.Token,
		userID:  opts.UserID,
		state:   ConnStateConnecting,
	}, nil
}

// OnEnvelope registers the handler called for every daemon-emitted event on
// the `/events` namespace. The handler runs on the socket.io callback
// goroutine — keep it short or hand off to a channel.
func (r *Realtime) OnEnvelope(fn func(Envelope)) {
	r.mu.Lock()
	r.onEnv = fn
	r.mu.Unlock()
}

// OnState registers a handler called whenever the connection state changes.
// err is non-nil on transitions caused by a failure (handshake reject,
// transport drop, etc.) and nil for clean transitions.
func (r *Realtime) OnState(fn func(ConnState, error)) {
	r.mu.Lock()
	r.onState = fn
	r.mu.Unlock()
}

// Connect performs the handshake. Blocks until the transport "connect" event
// fires (auth accepted by daemon) OR ctx is cancelled OR a timeout elapses.
// Reconnection is configured automatically — once Connect succeeds, drops are
// recovered transparently and OnState is the only signal you get.
func (r *Realtime) Connect(ctx context.Context) error {
	if r.closed.Load() {
		return errors.New("realtime: already closed")
	}

	Debugf("realtime: connecting baseURL=%s userID=%s tokenLen=%d", r.baseURL, r.userID, len(r.token))

	mgrOpts := sio.DefaultManagerOptions()
	mgrOpts.SetAutoConnect(false)
	mgrOpts.SetReconnection(true)
	mgrOpts.SetReconnectionDelay(500)
	mgrOpts.SetReconnectionDelayMax(5_000)
	mgrOpts.SetTimeout(10 * time.Second)
	r.mgr = sio.NewManager(r.baseURL, mgrOpts)

	sockOpts := sio.DefaultSocketOptions()
	sockOpts.SetAuth(map[string]any{
		"token":   r.token,
		"user_id": r.userID,
	})
	r.sock = r.mgr.Socket("/events", sockOpts)

	connected := make(chan struct{}, 1)
	connErr := make(chan error, 1)

	_ = r.sock.On("connect", func(...any) {
		Debugf("realtime: connect event fired (auth ok)")
		r.setState(ConnStateConnected, nil)
		select {
		case connected <- struct{}{}:
		default:
		}
		// On reconnect, re-emit join_session + replay so we don't miss events
		// that fired while we were offline.
		r.mu.Lock()
		sid := r.sessID
		appID := r.appID
		since := r.lastSeq.Load()
		r.mu.Unlock()
		if sid != "" {
			_ = r.sock.Emit("join_session", map[string]any{
				"app_id":     appID,
				"session_id": sid,
			})
			if since > 0 {
				_ = r.sock.Emit("replay", map[string]any{
					"app_id":     appID,
					"session_id": sid,
					"since":      since,
				})
			}
		}
	})

	_ = r.sock.On("connect_error", func(args ...any) {
		err := errors.New("connect_error")
		if len(args) > 0 {
			if e, ok := args[0].(error); ok {
				err = e
			} else {
				err = fmt.Errorf("connect_error: %v", args[0])
			}
		}
		Debugf("realtime: connect_error : %v", err)
		r.setState(ConnStateReconnecting, err)
		select {
		case connErr <- err:
		default:
		}
	})

	_ = r.sock.On("disconnect", func(args ...any) {
		reason := ""
		if len(args) > 0 {
			if s, ok := args[0].(string); ok {
				reason = s
			}
		}
		// "io client disconnect" = we closed it on purpose, no reconnect.
		if reason == "io client disconnect" || r.closed.Load() {
			r.setState(ConnStateDisconnected, nil)
		} else {
			r.setState(ConnStateReconnecting, fmt.Errorf("disconnected: %s", reason))
		}
	})

	_ = r.sock.On("event", func(args ...any) {
		if len(args) == 0 {
			Debugf("realtime: event arg empty")
			return
		}
		Debugf("realtime: raw arg[0] type=%T value=%+v", args[0], args[0])
		env := decodeEnvelope(args[0])
		Debugf("realtime: decoded type=%s seq=%d session=%s payload=%+v", env.Type, env.Seq, env.SessionID, env.Payload)
		// Track highest seq for replay-on-reconnect — but ONLY for events of the
		// joined (root) session. Sub-agent events fanned out from an isolated
		// sub-session (AgentRunID set) carry that sub-session's INDEPENDENT seq
		// counter ; letting them bump lastSeq would make the reconnect replay
		// `since` jump past un-replayed root events and silently lose them.
		if env.Seq > 0 && env.AgentRunID == "" {
			if env.Seq > r.lastSeq.Load() {
				r.lastSeq.Store(env.Seq)
			}
		}
		r.mu.Lock()
		cb := r.onEnv
		r.mu.Unlock()
		if cb != nil {
			cb(env)
		}
	})

	// replay_done marks the end of a session's backlog (join/replay). Surface it
	// as a synthetic envelope through the same callback so the TUI can drop the
	// "loading session" spinner once the full history has streamed in.
	_ = r.sock.On("replay_done", func(args ...any) {
		sid := ""
		if len(args) > 0 {
			if m, ok := args[0].(map[string]any); ok {
				sid, _ = m["session_id"].(string)
			}
		}
		r.mu.Lock()
		cb := r.onEnv
		r.mu.Unlock()
		if cb != nil {
			cb(Envelope{Type: "replay_done", SessionID: sid})
		}
	})

	r.sock.Connect()

	select {
	case <-connected:
		return nil
	case err := <-connErr:
		return fmt.Errorf("realtime: %w", err)
	case <-ctx.Done():
		_ = r.Close()
		return fmt.Errorf("realtime: %w", ctx.Err())
	case <-time.After(15 * time.Second):
		_ = r.Close()
		return errors.New("realtime: handshake timeout after 15s")
	}
}

// JoinSession subscribes to the given session's events. Idempotent — calling
// twice with the same session is a no-op. Calling with a different session
// implicitly leaves the previous one (daemon-side guarantee).
func (r *Realtime) JoinSession(appID, sessionID string) error {
	if sessionID == "" {
		return errors.New("realtime: session_id required")
	}
	r.mu.Lock()
	r.appID = appID
	r.sessID = sessionID
	r.lastSeq.Store(0)
	r.mu.Unlock()
	if r.sock == nil {
		return errors.New("realtime: not connected")
	}
	Debugf("realtime: emit join_session app=%s session=%s", appID, sessionID)
	err := r.sock.Emit("join_session", map[string]any{
		"app_id":     appID,
		"session_id": sessionID,
	})
	if err != nil {
		Debugf("realtime: join_session emit err=%v", err)
		return err
	}
	// Also emit a replay since=0 so we catch any events that fired between
	// session creation (REST) and our socket joining the room.
	Debugf("realtime: emit replay since=0 (catch-up)")
	return r.sock.Emit("replay", map[string]any{
		"app_id":     appID,
		"session_id": sessionID,
		"since":      0,
	})
}

// keys returns the sorted keys of a payload map for logging.
func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Replay asks the daemon to redeliver events with seq > since for the current
// session. Useful for catch-up after a reconnect or to fetch history without
// hitting REST. Events arrive via the normal `event` channel.
func (r *Realtime) Replay(since uint64) error {
	r.mu.Lock()
	sid := r.sessID
	appID := r.appID
	r.mu.Unlock()
	if sid == "" {
		return errors.New("realtime: no active session")
	}
	if r.sock == nil {
		return errors.New("realtime: not connected")
	}
	return r.sock.Emit("replay", map[string]any{
		"app_id":     appID,
		"session_id": sid,
		"since":      since,
	})
}

// State returns the current connection state. Cheap, atomic-free for the
// caller — internal mutex protects the read.
func (r *Realtime) State() ConnState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// LastSeq returns the highest envelope seq we've seen on the current session.
// Used by the consumer to deduplicate on replay.
func (r *Realtime) LastSeq() uint64 { return r.lastSeq.Load() }

// Close disconnects the socket and releases resources. Safe to call multiple
// times — only the first call has effect.
func (r *Realtime) Close() error {
	r.stopOnce.Do(func() {
		r.closed.Store(true)
		if r.sock != nil {
			r.sock.Disconnect()
		}
	})
	return nil
}

func (r *Realtime) setState(s ConnState, err error) {
	r.mu.Lock()
	r.state = s
	cb := r.onState
	r.mu.Unlock()
	if cb != nil {
		cb(s, err)
	}
}

// decodeEnvelope coerces the socket.io payload (always map[string]any post-JSON
// unmarshal) into our typed Envelope. Best-effort — unknown fields are ignored,
// missing fields stay at zero.
func decodeEnvelope(raw any) Envelope {
	m, ok := raw.(map[string]any)
	if !ok {
		return Envelope{}
	}
	env := Envelope{}
	if v, ok := m["event_id"].(string); ok {
		env.EventID = v
	}
	if v, ok := m["type"].(string); ok {
		env.Type = v
	}
	if v, ok := m["kind"].(string); ok {
		env.Kind = v
	}
	switch v := m["seq"].(type) {
	case float64:
		env.Seq = uint64(v)
	case uint64:
		env.Seq = v
	case int64:
		env.Seq = uint64(v)
	}
	if v, ok := m["app_id"].(string); ok {
		env.AppID = v
	}
	if v, ok := m["session_id"].(string); ok {
		env.SessionID = v
	}
	if v, ok := m["payload"].(map[string]any); ok {
		env.Payload = v
	}
	if v, ok := m["ts"].(string); ok {
		env.Ts = v
	}
	if v, ok := m["control"].(bool); ok {
		env.Control = v
	}
	if v, ok := m["user_id"].(string); ok {
		env.UserID = v
	}
	if v, ok := m["instance_id"].(string); ok {
		env.InstanceID = v
	}
	if v, ok := m["agent_run_id"].(string); ok {
		env.AgentRunID = v
	}
	if v, ok := m["root_session_id"].(string); ok {
		env.RootSessionID = v
	}
	if v, ok := m["correlation_id"].(string); ok {
		env.CorrelationID = v
	}
	switch v := m["live_output_tokens"].(type) {
	case float64:
		env.LiveOutputTokens = int(v)
	case int64:
		env.LiveOutputTokens = int(v)
	case int:
		env.LiveOutputTokens = v
	}
	return env
}
