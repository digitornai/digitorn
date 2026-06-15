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
	Model      string            `yaml:"model"` // per-session model override for the entry agent
	// Reports, when true, gives the agent a dated output folder under its workdir
	// (attachments/<stamp>/) and instructs it — at EVERY fire (injected into the
	// per-turn message, so it survives on a persistent session) — to write any file
	// or report it produces there, preserved and downloadable via the workspace
	// routes. Opt-in : apps that produce no files leave it off.
	Reports    bool              `yaml:"reports"`
	// Attachments are INPUT media the schedule carries to EVERY fire (the CV in a
	// "match jobs to my CV every morning" schedule). They are content-addressed
	// blobs already in the app's blob store, so the same ref rides each wake and
	// the daemon resolves it into vision/document content for the model.
	Attachments []AttachmentRef   `yaml:"attachments"`
	ExposeData bool              `yaml:"expose_data"`
	Filter     []FilterCondition `yaml:"filter"`
	Prepare    []PrepareStep     `yaml:"prepare"`
	Route      *RouteConfig      `yaml:"route"`
	Reply      string            `yaml:"reply"` // auto | none | explicit
	// Deliver is the PROACTIVE-push destination: where to send the result when the
	// triggering event carries no inbound reply handle (a cron tick, a CI webhook).
	// When set, the reply (reply:auto → the agent's answer) or the rendered Message
	// (reply:none → a raw, no-LLM announcement) is delivered HERE instead of back to
	// the originator. Decouples "where it goes" from "where the event came from", so
	// any adapter with Send (Discord, Telegram, …) is a push target — zero new code.
	Deliver *DeliverConfig `yaml:"deliver"`
}

// AttachmentRef is a content-addressed blob already stored in the app's blob
// store (hash + mime + size) — the wire shape the daemon's message attachments
// use. Carried verbatim from the schedule to each fire's wake message.
type AttachmentRef struct {
	Hash string `yaml:"hash" json:"hash"`
	Mime string `yaml:"mime" json:"mime"`
	Size int64  `yaml:"size" json:"size"`
}

// DeliverConfig addresses a channel for a proactive push: which transport (Adapter,
// e.g. "discord") and the transport handle (Ref, e.g. {provider, channel_id}). Both
// the adapter name and every Ref value are templated over the event scope, so a
// webhook payload can carry the target channel.
type DeliverConfig struct {
	Adapter string            `yaml:"adapter"`
	Ref     map[string]string `yaml:"ref"`
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
	ReplyAuto     = "auto" // final answer only
	ReplyExplicit = "explicit"
	ReplyStream   = "stream" // relay the whole agentic loop live (messages + tool activity)
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
