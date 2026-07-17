package channels

import (
	"fmt"
	"strings"
)

type ModuleConfig struct {
	Providers           map[string]ProviderConfig `yaml:"providers"`
	DefaultAgent        string                    `yaml:"default_agent"`
	MaxTurns            int                       `yaml:"max_turns"`
	Timeout             float64                   `yaml:"timeout"`
	HistoryLimit        int                       `yaml:"history_limit"`
	SecretFilterEnabled *bool                     `yaml:"secret_filter_enabled"`
}

type ProviderConfig struct {
	Adapter       string           `yaml:"adapter"`
	Config        map[string]any   `yaml:"config"`
	Activation    ActivationConfig `yaml:"activation"`
	Enabled       *bool            `yaml:"enabled"`
	MaxConcurrent int              `yaml:"max_concurrent"`
}

type ActivationConfig struct {
	Agent string `yaml:"agent"`
	Owner      string            `yaml:"owner"`
	Session    string            `yaml:"session"`
	Message    string            `yaml:"message"`
	Context    string            `yaml:"context"`
	Model      string            `yaml:"model"`
	Reports    bool              `yaml:"reports"`
	Attachments []AttachmentRef   `yaml:"attachments"`
	ExposeData bool              `yaml:"expose_data"`
	Filter     []FilterCondition `yaml:"filter"`
	Prepare    []PrepareStep     `yaml:"prepare"`
	Route      *RouteConfig      `yaml:"route"`
	Reply      string            `yaml:"reply"`
	Deliver *DeliverConfig `yaml:"deliver"`
}

type AttachmentRef struct {
	Hash string `yaml:"hash" json:"hash"`
	Mime string `yaml:"mime" json:"mime"`
	Size int64  `yaml:"size" json:"size"`
}

type DeliverConfig struct {
	Adapter string            `yaml:"adapter"`
	Ref     map[string]string `yaml:"ref"`
}

type FilterCondition struct {
	Field     string   `yaml:"field"`
	Equals    any      `yaml:"equals"`
	NotEquals any      `yaml:"not_equals"`
	Contains  *string  `yaml:"contains"`
	Gt        *float64 `yaml:"gt"`
	Lt        *float64 `yaml:"lt"`
}

type PrepareStep struct {
	Action string         `yaml:"action"`
	Params map[string]any `yaml:"params"`
	As     string         `yaml:"as"`
}

type RouteConfig struct {
	Field string      `yaml:"field"`
	Rules []RouteRule `yaml:"rules"`
}

type RouteRule struct {
	Match   *string `yaml:"match"`
	Default bool    `yaml:"default"`
	Agent   string  `yaml:"agent"`
}

const (
	ReplyNone     = "none"
	ReplyAuto     = "auto"
	ReplyExplicit = "explicit"
	ReplyStream   = "stream"
)

const (
	SessionPerEvent = "per_event"
	SessionShared   = "shared"
)

const (
	defMaxTurns      = 30
	defTimeout       = 120.0
	defHistoryLimit  = 200
	defMaxConcurrent = 5
)

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

func (m *ModuleConfig) FilterSecrets() bool {
	return m.SecretFilterEnabled == nil || *m.SecretFilterEnabled
}

func (p ProviderConfig) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

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
		case "", ReplyNone, ReplyAuto, ReplyExplicit, ReplyStream:
		default:
			return fmt.Errorf("provider %q: invalid reply %q (auto|none|explicit|stream)", name, p.Activation.Reply)
		}
		if r := p.Activation.Route; r != nil && strings.TrimSpace(r.Field) == "" {
			return fmt.Errorf("provider %q: route.field is required when route is set", name)
		}
		if d := p.Activation.Deliver; d != nil {
			if strings.TrimSpace(d.Adapter) == "" {
				return fmt.Errorf("provider %q: deliver.adapter is required when deliver is set", name)
			}
			if len(d.Ref) == 0 {
				return fmt.Errorf("provider %q: deliver.ref is required when deliver is set", name)
			}
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
