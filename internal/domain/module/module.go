package module

import (
	"context"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

type Platform string

const (
	PlatformLinux   Platform = "linux"
	PlatformMacOS   Platform = "darwin"
	PlatformWindows Platform = "windows"
)

type Manifest struct {
	ID                   string         `json:"id"`
	Version              string         `json:"version"`
	Description          string         `json:"description"`
	SupportedPlatforms   []Platform     `json:"supported_platforms,omitempty"`
	Tools                []tool.Spec    `json:"tools"`
	Dependencies         []string       `json:"dependencies,omitempty"`
	DeclaredPermissions  []string       `json:"declared_permissions,omitempty"`
	ProvidesServices     []string       `json:"provides_services,omitempty"`
	ConsumesServices     []string       `json:"consumes_services,omitempty"`
	ConfigSchema         map[string]any `json:"config_schema,omitempty"`
	CompatibleMiddleware []string       `json:"compatible_middleware,omitempty"`
}

type Module interface {
	Manifest() Manifest
	Init(ctx context.Context, cfg map[string]any) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Invoke(ctx context.Context, toolName string, params []byte) (tool.Result, error)
}

type Pauser interface {
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
}

type Reloader interface {
	UpdateConfig(ctx context.Context, cfg map[string]any) error
}

type PromptScope struct {
	AppID   string
	AgentID string
	Role    string
}

type PromptSection struct {
	Title    string
	Content  string
	Priority int
}

type PromptContributor interface {
	PromptSections(scope PromptScope) []PromptSection

	DynamicToolPrompts(scope PromptScope) map[string]string
}

type LiveTooler interface {
	LiveTools(ctx context.Context) []tool.Spec
}

type EventEmitter interface {
	EmitEvent(ctx context.Context, event interface{}) error

	DeclaredEvents() []map[string]string
}
