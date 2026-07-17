package tool

import (
	"context"
	"io"
)

type Identity struct {
	AppID     string
	SessionID string
	UserID    string
	AgentID   string
	ModuleID  string
	ToolName  string

	Attempt int
}

type identityKey struct{}

func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

type backgroundKey struct{}

func WithBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

func IsBackground(ctx context.Context) bool {
	v, _ := ctx.Value(backgroundKey{}).(bool)
	return v
}

type liveSinkKey struct{}

func WithLiveSink(ctx context.Context, w io.Writer) context.Context {
	if w == nil {
		return ctx
	}
	return context.WithValue(ctx, liveSinkKey{}, w)
}

func LiveSinkFromContext(ctx context.Context) io.Writer {
	w, _ := ctx.Value(liveSinkKey{}).(io.Writer)
	return w
}

type stdinPipeKey struct{}

func WithStdinPipe(ctx context.Context, r io.Reader) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, stdinPipeKey{}, r)
}

func StdinPipeFromContext(ctx context.Context) io.Reader {
	r, _ := ctx.Value(stdinPipeKey{}).(io.Reader)
	return r
}

type FileChangeNotifier interface {
	FileChanged(sessionID, workdir string, paths ...string)
}

type fileChangeNotifierKey struct{}

func WithFileChangeNotifier(ctx context.Context, n FileChangeNotifier) context.Context {
	if n == nil {
		return ctx
	}
	return context.WithValue(ctx, fileChangeNotifierKey{}, n)
}

func FileChangeNotifierFromContext(ctx context.Context) (FileChangeNotifier, bool) {
	n, ok := ctx.Value(fileChangeNotifierKey{}).(FileChangeNotifier)
	return n, ok
}

type eventBusKey struct{}

func WithEventBus(ctx context.Context, bus interface{}) context.Context {
	if bus == nil {
		return ctx
	}
	return context.WithValue(ctx, eventBusKey{}, bus)
}

func EventBusFromContext(ctx context.Context) (interface{}, bool) {
	b, ok := ctx.Value(eventBusKey{}).(interface{})
	return b, ok
}
