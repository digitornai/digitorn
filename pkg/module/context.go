package module

import (
	"context"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

type Caller interface {
	Call(ctx context.Context, module, toolName string, params []byte) (tool.Result, error)
}

type Embedder interface {
	EmbedModel(ctx context.Context, model, role string, texts []string) (vectors [][]float32, dim int, err error)
}

type Reranker interface {
	Rerank(ctx context.Context, model, query string, docs []string) (scores []float32, err error)
}

type ctxKey int

const (
	keySessionID ctxKey = iota
	keyUserID
	keyAppID
	keyAppDir
	keyAgentID
	keyWorkspace
	keyCaller
	keyConstraints
	keyEmbedder
	keyModuleConfig
	keyReranker
	keyAuthContext
	keyListingAuth
)

func WithSessionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keySessionID, id)
}

func SessionID(ctx context.Context) string {
	if v, _ := ctx.Value(keySessionID).(string); v != "" {
		return v
	}
	id, _ := tool.IdentityFromContext(ctx)
	return id.SessionID
}

func WithUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyUserID, id)
}

func UserID(ctx context.Context) string {
	if v, _ := ctx.Value(keyUserID).(string); v != "" {
		return v
	}
	id, _ := tool.IdentityFromContext(ctx)
	return id.UserID
}

func WithAppID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyAppID, id)
}

func AppID(ctx context.Context) string {
	if v, _ := ctx.Value(keyAppID).(string); v != "" {
		return v
	}
	id, _ := tool.IdentityFromContext(ctx)
	return id.AppID
}

func WithAppDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, keyAppDir, dir)
}

func AppDir(ctx context.Context) string {
	v, _ := ctx.Value(keyAppDir).(string)
	return v
}

func WithAgentID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyAgentID, id)
}

func AgentID(ctx context.Context) string {
	if v, _ := ctx.Value(keyAgentID).(string); v != "" {
		return v
	}
	id, _ := tool.IdentityFromContext(ctx)
	return id.AgentID
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

func WithEmbedder(ctx context.Context, e Embedder) context.Context {
	return context.WithValue(ctx, keyEmbedder, e)
}

func EmbedderFrom(ctx context.Context) Embedder {
	v, _ := ctx.Value(keyEmbedder).(Embedder)
	return v
}

func WithReranker(ctx context.Context, r Reranker) context.Context {
	return context.WithValue(ctx, keyReranker, r)
}

func RerankerFrom(ctx context.Context) Reranker {
	v, _ := ctx.Value(keyReranker).(Reranker)
	return v
}

func WithModuleConfig(ctx context.Context, cfg map[string]any) context.Context {
	return context.WithValue(ctx, keyModuleConfig, cfg)
}

func ModuleConfigFrom(ctx context.Context) map[string]any {
	v, _ := ctx.Value(keyModuleConfig).(map[string]any)
	return v
}

type AuthContext struct {
	Token        string
	TokenType    string
	EnvTokenVar  string
	ExpiresAt    int64
	Provider     string
	RefreshToken string
	Scope        string
	ClientID     string
	ClientSecret string
}

func WithAuthContext(ctx context.Context, ac AuthContext) context.Context {
	return context.WithValue(ctx, keyAuthContext, ac)
}

func AuthContextFrom(ctx context.Context) (AuthContext, bool) {
	v, ok := ctx.Value(keyAuthContext).(AuthContext)
	return v, ok
}

func WithListingAuth(ctx context.Context, byServer map[string]AuthContext) context.Context {
	return context.WithValue(ctx, keyListingAuth, byServer)
}

func ListingAuthFrom(ctx context.Context) map[string]AuthContext {
	v, _ := ctx.Value(keyListingAuth).(map[string]AuthContext)
	return v
}
