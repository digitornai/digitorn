// Package channels is the Go re-think of the old daemon's `channels` subsystem:
// the activation pipeline that turns an inbound external event into an agentic
// session launch (filter → prepare → route → session → build → reply). It is a
// FAITHFUL port of the Python module's documented semantics — same config keys,
// same operators, same template scope, same session strategies — re-implemented
// pure and heavily tested. It depends on nothing from the daemon; the resolved
// Activation is handed to the daemon client (BG-3) for the actual invocation.
package channels

import (
	"fmt"
	"strings"
)

// ModuleConfig mirrors the Python ChannelsModuleConfig (module.py:112).
type ModuleConfig struct {
	Providers           map[string]ProviderConfig `yaml:"providers"`
	DefaultAgent        string                    `yaml:"default_agent"`
	MaxTurns            int                       `yaml:"max_turns"`
	Timeout             float64                   `yaml:"timeout"`
	HistoryLimit        int                       `yaml:"history_limit"`
	SecretFilterEnabled *bool                     `yaml:"secret_filter_enabled"`
}

// ProviderConfig mirrors the Python ProviderConfig (module.py:103).
type ProviderConfig struct {
	Adapter       string           `yaml:"adapter"`
	Config        map[string]any   `yaml:"config"`
	Activation    ActivationConfig `yaml:"activation"`
	Enabled       *bool            `yaml:"enabled"`
	MaxConcurrent int              `yaml:"max_concurrent"`
}

// ActivationConfig mirrors the Python ActivationConfig (module.py:90).
type ActivationConfig struct {
	Agent string `yaml:"agent"`
	// Owner is the end-user the launched session belongs to (a template over the
	// event, e.g. "{{event.payload.from.id}}"). Empty → derived from the event's
	// sender, namespaced by provider. The background service forwards it so the
	// daemon owns the session under the real user (X-Act-As-User).
	Owner      string            `yaml:"owner"`
	Session    string            `yaml:"session"` // "" | per_event | shared | <template>
	Message    string            `yaml:"message"`
	Context    string            `yaml:"context"`
	ExposeData bool              `yaml:"expose_data"`
	Filter     []FilterCondition `yaml:"filter"`
	Prepare    []PrepareStep     `yaml:"prepare"`
	Route      *RouteConfig      `yaml:"route"`
	Reply      string            `yaml:"reply"` // auto | none | explicit
}

// FilterCondition mirrors the Python FilterCondition (module.py:67). A nil field
// means "operator not set". ALL set operators must pass (AND).
type FilterCondition struct {
	Field     string   `yaml:"field"`
	Equals    any      `yaml:"equals"`
	NotEquals any      `yaml:"not_equals"`
	Contains  *string  `yaml:"contains"`
	Gt        *float64 `yaml:"gt"`
	Lt        *float64 `yaml:"lt"`
}

// PrepareStep mirrors the Python PrepareStep (module.py:60): a "module.action"
// call whose result is bound under As for later templating.
type PrepareStep struct {
	Action string         `yaml:"action"`
	Params map[string]any `yaml:"params"`
	As     string         `yaml:"as"`
}

// RouteConfig / RouteRule mirror the Python route models (module.py:77,84).
type RouteConfig struct {
	Field string      `yaml:"field"`
	Rules []RouteRule `yaml:"rules"`
}

type RouteRule struct {
	Match   *string `yaml:"match"`
	Default bool    `yaml:"default"`
	Agent   string  `yaml:"agent"`
}

// Reply modes.
const (
	ReplyNone     = "none"
	ReplyAuto     = "auto"
	ReplyExplicit = "explicit"
)

// Session strategies.
const (
	SessionPerEvent = "per_event"
	SessionShared   = "shared"
)

// Module-level defaults (module.py field defaults).
const (
	defMaxTurns      = 30
	defTimeout       = 120.0
	defHistoryLimit  = 200
	defMaxConcurrent = 5
)

// Normalize fills defaults in-place so the rest of the pipeline reads concrete
// values. Faithful to the Python field defaults.
func (m *ModuleConfig) Normalize() {
	if m.MaxTurns == 0 {
		m.MaxTurns = defMaxTurns
	}
	if m.Timeout == 0 {
		m.Timeout = defTimeout
	}
	if m.HistoryLimit == 0 {
		m.HistoryLimit = defHistoryLimit
	}
	if m.SecretFilterEnabled == nil {
		t := true
		m.SecretFilterEnabled = &t
	}
	for name, p := range m.Providers {
		if p.Enabled == nil {
			t := true
			p.Enabled = &t
		}
		if p.MaxConcurrent == 0 {
			p.MaxConcurrent = defMaxConcurrent
		}
		if p.Activation.Session == "" {
			p.Activation.Session = SessionPerEvent
		}
		if p.Activation.Reply == "" {
			p.Activation.Reply = ReplyNone
		}
		m.Providers[name] = p
	}
}

// FilterSecrets reports the effective secret-filter setting (default true).
func (m *ModuleConfig) FilterSecrets() bool {
	return m.SecretFilterEnabled == nil || *m.SecretFilterEnabled
}

// IsEnabled reports whether a provider is active (default true).
func (p ProviderConfig) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// Validate checks bounds and required fields, matching the Python Pydantic
// constraints. Returns the first violation.
func (m *ModuleConfig) Validate() error {
	if err := boundsInt("max_turns", m.MaxTurns, 1, 200); err != nil {
		return err
	}
	if err := boundsFloat("timeout", m.Timeout, 5.0, 3600.0); err != nil {
		return err
	}
	if err := boundsInt("history_limit", m.HistoryLimit, 0, 10000); err != nil {
		return err
	}
	for name, p := range m.Providers {
		if strings.TrimSpace(p.Adapter) == "" {
			return fmt.Errorf("provider %q: adapter is required", name)
		}
		if err := boundsInt(fmt.Sprintf("provider %q max_concurrent", name), p.MaxConcurrent, 1, 100); err != nil {
			return err
		}
		switch p.Activation.Reply {
		case "", ReplyNone, ReplyAuto, ReplyExplicit:
		default:
			return fmt.Errorf("provider %q: invalid reply %q (auto|none|explicit)", name, p.Activation.Reply)
		}
		if r := p.Activation.Route; r != nil && strings.TrimSpace(r.Field) == "" {
			return fmt.Errorf("provider %q: route.field is required when route is set", name)
		}
	}
	return nil
}

func boundsInt(name string, v, lo, hi int) error {
	if v < lo || v > hi {
		return fmt.Errorf("%s=%d out of range [%d, %d]", name, v, lo, hi)
	}
	return nil
}

func boundsFloat(name string, v, lo, hi float64) error {
	if v < lo || v > hi {
		return fmt.Errorf("%s=%g out of range [%g, %g]", name, v, lo, hi)
	}
	return nil
}
