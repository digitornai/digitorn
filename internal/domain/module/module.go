// Package module defines the contract every Digitorn module must satisfy.
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

// Manifest is the public surface of a module — its identity, capabilities, and dependencies.
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

// Module is the contract every pluggable module satisfies. The five lifecycle
// methods drive the FSM (LOADED → STARTING → ACTIVE → PAUSED → STOPPING →
// DISABLED). UpdateConfig is called for hot config reloads while ACTIVE.
type Module interface {
	Manifest() Manifest
	Init(ctx context.Context, cfg map[string]any) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Invoke(ctx context.Context, toolName string, params []byte) (tool.Result, error)
}

// Pauser is an optional interface for modules that support pause/resume.
type Pauser interface {
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
}

// Reloader is an optional interface for modules that accept hot config updates.
type Reloader interface {
	UpdateConfig(ctx context.Context, cfg map[string]any) error
}

// PromptScope is the per-build context a PromptContributor reads to tailor
// its contributions. It is app/agent scoped (matching the per-agent prompt
// cache key), so contributions are recomputed on a cache miss, not per turn.
type PromptScope struct {
	AppID   string
	AgentID string
	Role    string
}

// PromptSection is one block a module injects into the agent's system prompt.
// Title is optional ; when set it renders as a "# Title" heading above Content.
// Priority orders sections (lower renders first) — mirrors the reference
// daemon's get_prompt_sections() priority field.
type PromptSection struct {
	Title    string
	Content  string
	Priority int
}

// PromptContributor is the OPTIONAL interface a module implements to inject
// content into the system prompts of agents AUTHORIZED for it. The framework
// gathers contributions only for modules in the agent's authorized toolset
// (the anti-leak invariant), so an unauthorized agent never sees them — the
// module writes ZERO wiring code beyond these methods.
//
// This is the faithful port of the reference daemon's module
// get_prompt_sections() + get_dynamic_tool_prompts() mechanism.
type PromptContributor interface {
	// PromptSections returns the module-level sections to inject (e.g. usage
	// guidance, operating rules). Empty/nil = nothing.
	PromptSections(scope PromptScope) []PromptSection

	// DynamicToolPrompts returns per-tool usage prompts keyed by FQN
	// ("filesystem.read"), overlaid on top of the static tool.Spec.ToolPrompt
	// (dynamic wins). Lets modules with runtime-discovered tools (e.g. MCP)
	// attach guidance no manifest could declare. Empty/nil = nothing.
	DynamicToolPrompts(scope PromptScope) map[string]string
}

// LiveTooler is the optional interface a module implements to report tools it
// discovers at runtime (MCP server tools). The caller's app config + identity
// ride in ctx for per-app materialization.
type LiveTooler interface {
	LiveTools(ctx context.Context) []tool.Spec
}

// EventEmitter is the OPTIONAL interface a module implements to publish events
// on the EventBus. Modules that emit events (e.g. audio detecting an incoming
// call, clipboard detecting a change) implement this to push events that other
// components (background service, hooks) can subscribe to.
type EventEmitter interface {
	// EmitEvent publishes an event on the EventBus attached to ctx.
	// The event parameter is typed as interface{} to avoid import cycles;
	// the concrete type is ports.Event. Returns an error if the bus is
	// unavailable or closed.
	EmitEvent(ctx context.Context, event interface{}) error

	// DeclaredEvents returns the list of event topics this module may emit.
	// Used by discovery to wire subscribers. Each entry is a map with
	// "topic", "type", and optional "source" keys.
	DeclaredEvents() []map[string]string
}
