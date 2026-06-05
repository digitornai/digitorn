package tool

import (
	"context"
	"io"
)

// Identity is the caller context attached to a tool dispatch. It travels
// in the request context from the runtime engine down to the module Invoke,
// across the in-process bus and the gRPC worker boundary alike. The
// tool-call middleware pipeline reads it to scope per-session state
// (dedup / cache) and to key app-global state (circuit breaker / budget).
type Identity struct {
	AppID     string
	SessionID string
	UserID    string
	AgentID   string
	ModuleID  string
	ToolName  string

	// Attempt is the 1-based retry attempt set by the retry middleware so
	// inner layers (audit, breaker) see which try they are on.
	Attempt int
}

type identityKey struct{}

// WithIdentity returns a context carrying id. Successive calls overwrite the
// previous identity, so a layer that knows more (module/tool name) can refine
// what an outer layer set (app/session/user).
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFromContext extracts the identity. ok is false when none was set —
// the caller decides whether that is a hard error or a benign anonymous call.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

type backgroundKey struct{}

// WithBackground marks ctx as an asynchronous (background_run) dispatch rather
// than a foreground turn. A module that holds per-session interactive state —
// e.g. the bash module's persistent shell — reads this to run the call in an
// independent, separately-cancellable process instead of serializing it on the
// shared session resource.
func WithBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

// IsBackground reports whether ctx was marked by WithBackground.
func IsBackground(ctx context.Context) bool {
	v, _ := ctx.Value(backgroundKey{}).(bool)
	return v
}

type liveSinkKey struct{}

// WithLiveSink attaches a writer that a long-running tool streams its output
// into AS IT RUNS, so a background task's caller (the BackgroundManager) can
// surface a live tail while the task is still going — instead of only seeing
// the output once it finishes. The sink must be safe for concurrent Write while
// another goroutine reads it. A module that doesn't stream simply ignores it.
func WithLiveSink(ctx context.Context, w io.Writer) context.Context {
	if w == nil {
		return ctx
	}
	return context.WithValue(ctx, liveSinkKey{}, w)
}

// LiveSinkFromContext returns the live-output sink, or nil when none is set.
func LiveSinkFromContext(ctx context.Context) io.Writer {
	w, _ := ctx.Value(liveSinkKey{}).(io.Writer)
	return w
}

// FileChangeNotifier is signalled by a module right after it mutates a file in
// the session workdir (filesystem write/edit). The daemon's implementation
// coalesces these signals per session and pushes a live workspace-changes event
// to the client. FileChanged MUST return immediately — it is called on the
// tool's hot path and may never block on git or I/O.
type FileChangeNotifier interface {
	FileChanged(sessionID, workdir string)
}

type fileChangeNotifierKey struct{}

// WithFileChangeNotifier attaches the workspace file-change notifier to ctx so
// a mutating module reads it back via FileChangeNotifierFromContext. Absent
// (CLI / setup / tests) means no live push — the module simply skips the call.
func WithFileChangeNotifier(ctx context.Context, n FileChangeNotifier) context.Context {
	if n == nil {
		return ctx
	}
	return context.WithValue(ctx, fileChangeNotifierKey{}, n)
}

// FileChangeNotifierFromContext returns the notifier carried on ctx, if any.
func FileChangeNotifierFromContext(ctx context.Context) (FileChangeNotifier, bool) {
	n, ok := ctx.Value(fileChangeNotifierKey{}).(FileChangeNotifier)
	return n, ok
}
