package app

import "github.com/digitornai/digitorn/internal/domain/agent"

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

type ModuleBlock struct {
	Enabled bool           `yaml:"enabled" json:"enabled"`
	Config  map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
}

type ChannelBlock struct {
	Type   string         `yaml:"type" json:"type"`
	Config map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
}

type ToolsBlock struct {
	Modules  map[string]ModuleBlock  `yaml:"modules,omitempty" json:"modules,omitempty"`
	Channels map[string]ChannelBlock `yaml:"channels,omitempty" json:"channels,omitempty"`
}

type MiddlewareEntry struct {
	Name    string         `yaml:"name" json:"name"`
	Enabled bool           `yaml:"enabled" json:"enabled"`
	Config  map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
}

type HookAction struct {
	Type   string         `yaml:"type" json:"type"`
	Module string         `yaml:"module,omitempty" json:"module,omitempty"`
	Action string         `yaml:"action,omitempty" json:"action,omitempty"`
	Params map[string]any `yaml:"params,omitempty" json:"params,omitempty"`
}

type Hook struct {
	ID      string     `yaml:"id" json:"id"`
	Trigger string     `yaml:"trigger" json:"trigger"`
	Action  HookAction `yaml:"action" json:"action"`
}

type Execution struct {
	Mode           string            `yaml:"mode,omitempty" json:"mode,omitempty"`
	DefaultChannel string            `yaml:"default_channel,omitempty" json:"default_channel,omitempty"`
	Middleware     []MiddlewareEntry `yaml:"middleware,omitempty" json:"middleware,omitempty"`
	Hooks          []Hook            `yaml:"hooks,omitempty" json:"hooks,omitempty"`
}

type Security struct {
	DefaultPolicy string            `yaml:"default_policy,omitempty" json:"default_policy,omitempty"`
	Grants        []CapabilityGrant `yaml:"grants,omitempty" json:"grants,omitempty"`
}

type CapabilityGrant struct {
	Module string   `yaml:"module" json:"module"`
	Tools  []string `yaml:"tools,omitempty" json:"tools,omitempty"`
	Reason string   `yaml:"reason,omitempty" json:"reason,omitempty"`
}

type Definition struct {
	App       Meta               `yaml:"app" json:"app"`
	Agents    []agent.Definition `yaml:"agents" json:"agents"`
	Execution Execution          `yaml:"execution,omitempty" json:"execution,omitempty"`
	Tools     ToolsBlock         `yaml:"tools,omitempty" json:"tools,omitempty"`
	Security  Security           `yaml:"security,omitempty" json:"security,omitempty"`
}
