package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Event is one inbound external delivery, transport-agnostic. Adapters (BG-5+)
// build these; the pipeline consumes them. It maps onto the Python runtime
// template scope (event.*).
type Event struct {
	EventID   string
	Provider  string // provider instance name (event.provider / provider_id)
	Adapter   string // adapter type (webhook, cron, …)
	Source    string // sender: ip / email / user_id / phone
	Message   string // text content, if any
	Payload   map[string]any
	Metadata  map[string]any
	Timestamp string
}

// Activation is the resolved decision for one event: either filtered out, or a
// concrete launch (which agent, which session, what message). It is pure data —
// the wiring layer maps it onto a daemon invocation.
type Activation struct {
	Filtered     bool
	FilterReason string // the field whose condition failed
	Agent        string
	Session      string // resolved id; EMPTY for per_event (the caller derives a
	// deterministic per-delivery id → idempotent retries, the durable fix over
	// Python's random-uuid double-fire).
	SessionStrategy string
	Owner           string // end-user the session belongs to (resolved); "" = launcher owns it
	Message         string
	Context         string // extra system-prompt context (rendered)
	Reply           string // auto | none | explicit
	ExposeData      bool
}

// PrepareInvoker runs a prepare step's "module.action" and returns its result.
// In the isolated background service this is injected (a fake in tests; a real
// transport later) so the pipeline itself stays pure and daemon-free.
type PrepareInvoker interface {
	Invoke(ctx context.Context, action string, params map[string]any) (any, error)
}

// Process runs the activation pipeline for one event:
// filter → prepare → route → session → build. It never performs I/O except via
// the injected PrepareInvoker. Returns a filtered Activation when a filter
// condition fails (not an error).
func Process(ctx context.Context, ev Event, ac ActivationConfig, mod ModuleConfig, inv PrepareInvoker) (Activation, error) {
	scope := buildScope(ev)

	// 1. filter (AND of all conditions; first failure drops the event).
	if reason, ok := evalFilter(ac.Filter, scope); !ok {
		return Activation{Filtered: true, FilterReason: reason}, nil
	}

	// 2. prepare (sequential enrichment; each result bound under its alias).
	for _, step := range ac.Prepare {
		if inv == nil {
			return Activation{}, fmt.Errorf("prepare step %q requires an invoker", step.Action)
		}
		res, err := inv.Invoke(ctx, step.Action, renderParams(step.Params, scope))
		if err != nil {
			return Activation{}, fmt.Errorf("prepare %q: %w", step.Action, err)
		}
		key := step.As
		if key == "" {
			key = afterDot(step.Action)
		}
		scope[key] = res
	}

	// 3. route (overrides the static agent; empty result falls back to default).
	agent := ac.Agent
	if ac.Route != nil {
		agent = resolveRoute(ac.Route, scope)
	}
	if agent == "" {
		agent = mod.DefaultAgent
	}

	// 4. session strategy + owner (the end-user the session belongs to).
	session, strat := resolveSession(ac.Session, ev, scope)
	owner := resolveOwner(ac.Owner, ev, scope)

	// 5. message + context.
	return Activation{
		Agent:           agent,
		Session:         session,
		SessionStrategy: strat,
		Owner:           owner,
		Message:         buildMessage(ac, ev, scope),
		Context:         Render(ac.Context, scope),
		Reply:           replyOr(ac.Reply),
		ExposeData:      ac.ExposeData,
	}, nil
}

// resolveOwner derives the end-user a launched session belongs to. Per-user
// ownership is OPT-IN : an app declares an `owner` template (e.g.
// "{{event.payload.from.id}}", or "{{event.provider}}:{{event.source}}" for the
// channel's natural sender). When unset, "" is returned → the launcher (service)
// owns the session, exactly as before (back-compat ; no impersonation grant needed).
func resolveOwner(owner string, _ Event, scope map[string]any) string {
	s := strings.TrimSpace(owner)
	if s == "" {
		return ""
	}
	return sanitizeOwner(Render(s, scope))
}

func sanitizeOwner(s string) string {
	var b strings.Builder
	for _, r := range s {
		if isAlnum(r) || r == '-' || r == '_' || r == '.' || r == ':' || r == '@' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := b.String()
	if len(out) > 128 {
		out = out[:128]
	}
	return out
}

func buildScope(ev Event) map[string]any {
	payload := ev.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	meta := ev.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	return map[string]any{
		"event": map[string]any{
			"event_id":  ev.EventID,
			"provider":  ev.Provider,
			"adapter":   ev.Adapter,
			"source":    ev.Source,
			"message":   ev.Message,
			"timestamp": ev.Timestamp,
			"payload":   payload,
			"data":      payload, // alias
			"metadata":  meta,
		},
	}
}

// evalFilter returns ("", true) if all conditions pass, else (failingField, false).
func evalFilter(conds []FilterCondition, scope map[string]any) (string, bool) {
	for _, c := range conds {
		v, _ := resolveDotpath(scope, c.Field)
		if !condPasses(c, v) {
			return c.Field, false
		}
	}
	return "", true
}

func condPasses(c FilterCondition, v any) bool {
	if c.Equals != nil && stringify(v) != stringify(c.Equals) {
		return false
	}
	if c.NotEquals != nil && stringify(v) == stringify(c.NotEquals) {
		return false
	}
	if c.Contains != nil && !strings.Contains(stringify(v), *c.Contains) {
		return false
	}
	if c.Gt != nil {
		f, ok := toFloat(v)
		if !ok || f <= *c.Gt {
			return false
		}
	}
	if c.Lt != nil {
		f, ok := toFloat(v)
		if !ok || f >= *c.Lt {
			return false
		}
	}
	return true
}

// resolveRoute returns the agent for the first matching rule, else the default
// rule's agent, else "" (→ module default_agent).
func resolveRoute(r *RouteConfig, scope map[string]any) string {
	v, _ := resolveDotpath(scope, r.Field)
	val := stringify(v)
	def := ""
	for _, rule := range r.Rules {
		if rule.Match != nil && *rule.Match == val {
			return rule.Agent
		}
		if rule.Default {
			def = rule.Agent
		}
	}
	return def
}

// resolveSession maps a strategy to (id, normalizedStrategy). per_event returns
// an empty id by design: the caller derives a deterministic per-delivery id so a
// crash-retry hits the SAME session (idempotent), fixing the Python double-fire.
func resolveSession(strategy string, ev Event, scope map[string]any) (string, string) {
	s := strings.TrimSpace(strategy)
	switch {
	case s == "" || s == SessionPerEvent:
		return "", SessionPerEvent
	case s == SessionShared:
		return fmt.Sprintf("ch-%s-%s", ev.Provider, ev.Source), SessionShared
	default:
		return sanitizeSessionID(Render(s, scope)), "template"
	}
}

func sanitizeSessionID(s string) string {
	var b strings.Builder
	for _, r := range s {
		if isAlnum(r) || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := b.String()
	if len(out) > 128 {
		out = out[:128]
	}
	if out == "" {
		return "ch-evt-" + uuid.NewString()[:12]
	}
	return out
}

// buildMessage renders the configured message, with the Python fallback chain:
// event.message → JSON payload → generic text.
func buildMessage(ac ActivationConfig, ev Event, scope map[string]any) string {
	if ac.Message != "" {
		return Render(ac.Message, scope)
	}
	if ev.Message != "" {
		return ev.Message
	}
	if len(ev.Payload) > 0 {
		if b, err := json.Marshal(ev.Payload); err == nil {
			return string(b)
		}
	}
	return fmt.Sprintf("Event from %s", ev.Provider)
}

func renderParams(params map[string]any, scope map[string]any) map[string]any {
	out := make(map[string]any, len(params))
	for k, v := range params {
		out[k] = renderValue(v, scope)
	}
	return out
}

func renderValue(v any, scope map[string]any) any {
	switch x := v.(type) {
	case string:
		return Render(x, scope)
	case map[string]any:
		return renderParams(x, scope)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = renderValue(e, scope)
		}
		return out
	default:
		return v
	}
}

func replyOr(r string) string {
	if r == "" {
		return ReplyNone
	}
	return r
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func isAlnum(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func afterDot(s string) string {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return s[i+1:]
	}
	return s
}
