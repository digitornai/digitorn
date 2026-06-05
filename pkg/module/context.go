package module

import (
	"context"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

// Caller is the minimal contract a module needs to call another module — the
// daemon's ServiceBus satisfies it.
type Caller interface {
	Call(ctx context.Context, module, toolName string, params []byte) (tool.Result, error)
}

type ctxKey int

const (
	keySessionID ctxKey = iota
	keyUserID
	keyAppID
	keyAgentID
	keyWorkspace
	keyCaller
	keyConstraints
)

func WithSessionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keySessionID, id)
}

func SessionID(ctx context.Context) string {
	v, _ := ctx.Value(keySessionID).(string)
	return v
}

func WithUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyUserID, id)
}

func UserID(ctx context.Context) string {
	v, _ := ctx.Value(keyUserID).(string)
	return v
}

func WithAppID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyAppID, id)
}

func AppID(ctx context.Context) string {
	v, _ := ctx.Value(keyAppID).(string)
	return v
}

func WithAgentID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyAgentID, id)
}

func AgentID(ctx context.Context) string {
	v, _ := ctx.Value(keyAgentID).(string)
	return v
}

func WithWorkspace(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, keyWorkspace, path)
}

func Workspace(ctx context.Context) string {
	v, _ := ctx.Value(keyWorkspace).(string)
	return v
}

func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, keyCaller, c)
}

func CallerFrom(ctx context.Context) Caller {
	v, _ := ctx.Value(keyCaller).(Caller)
	return v
}

func WithConstraints(ctx context.Context, c map[string]any) context.Context {
	return context.WithValue(ctx, keyConstraints, c)
}

func Constraints(ctx context.Context) map[string]any {
	v, _ := ctx.Value(keyConstraints).(map[string]any)
	return v
}
