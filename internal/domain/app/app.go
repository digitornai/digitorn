// Package app defines the compiled AppDefinition produced by the YAML compiler.
package app

import "github.com/digitornai/digitorn/internal/domain/agent"

// Meta holds high-level app metadata shown to users.
type Meta struct {
	AppID         string   `yaml:"app_id" json:"app_id"`
	Name          string   `yaml:"name" json:"name"`
	ShortName     string   `yaml:"short_name,omitempty" json:"short_name,omitempty"`
	Version       string   `yaml:"version" json:"version"`
	SchemaVersion string   `yaml:"schema_version,omitempty" json:"schema_version,omitempty"`
	Description   string   `yaml:"description,omitempty" json:"description,omitempty"`
	Author        string   `yaml:"author,omitempty" json:"author,omitempty"`
	Tags          []string `yaml:"tags,omitempty" json:"tags,omitempty"`
	Icon          string   `yaml:"icon,omitempty" json:"icon,omitempty"`
	Color         string   `yaml:"color,omitempty" json:"color,omitempty"`
	Category      string   `yaml:"category,omitempty" json:"category,omitempty"`
}

// ModuleBlock is the per-module configuration declared inside an app.
type ModuleBlock struct {
	Enabled bool           `yaml:"enabled" json:"enabled"`
	Config  map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
}

// ChannelBlock declares a side-channel I/O (slack, email, webhook, etc.).
type ChannelBlock struct {
	Type   string         `yaml:"type" json:"type"`
	Config map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
}

// ToolsBlock groups module + channel declarations.
type ToolsBlock struct {
	Modules  map[string]ModuleBlock  `yaml:"modules,omitempty" json:"modules,omitempty"`
	Channels map[string]ChannelBlock `yaml:"channels,omitempty" json:"channels,omitempty"`
}

// MiddlewareEntry references one middleware in the execution pipeline.
type MiddlewareEntry struct {
	Name    string         `yaml:"name" json:"name"`
	Enabled bool           `yaml:"enabled" json:"enabled"`
	Config  map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
}

// HookAction describes the action triggered by a hook.
type HookAction struct {
	Type   string         `yaml:"type" json:"type"`
	Module string         `yaml:"module,omitempty" json:"module,omitempty"`
	Action string         `yaml:"action,omitempty" json:"action,omitempty"`
	Params map[string]any `yaml:"params,omitempty" json:"params,omitempty"`
}

// Hook ties an event trigger to an action.
type Hook struct {
	ID      string     `yaml:"id" json:"id"`
	Trigger string     `yaml:"trigger" json:"trigger"`
	Action  HookAction `yaml:"action" json:"action"`
}

// Execution describes the runtime configuration for the app.
type Execution struct {
	Mode           string            `yaml:"mode,omitempty" json:"mode,omitempty"`
	DefaultChannel string            `yaml:"default_channel,omitempty" json:"default_channel,omitempty"`
	Middleware     []MiddlewareEntry `yaml:"middleware,omitempty" json:"middleware,omitempty"`
	Hooks          []Hook            `yaml:"hooks,omitempty" json:"hooks,omitempty"`
}

// Security describes the security profile of the app.
type Security struct {
	DefaultPolicy string            `yaml:"default_policy,omitempty" json:"default_policy,omitempty"`
	Grants        []CapabilityGrant `yaml:"grants,omitempty" json:"grants,omitempty"`
}

// CapabilityGrant grants explicit access to a module/tool.
type CapabilityGrant struct {
	Module string   `yaml:"module" json:"module"`
	Tools  []string `yaml:"tools,omitempty" json:"tools,omitempty"`
	Reason string   `yaml:"reason,omitempty" json:"reason,omitempty"`
}

// Definition is a fully-compiled, validated app ready for the runtime.
type Definition struct {
	App       Meta               `yaml:"app" json:"app"`
	Agents    []agent.Definition `yaml:"agents" json:"agents"`
	Execution Execution          `yaml:"execution,omitempty" json:"execution,omitempty"`
	Tools     ToolsBlock         `yaml:"tools,omitempty" json:"tools,omitempty"`
	Security  Security           `yaml:"security,omitempty" json:"security,omitempty"`
}
