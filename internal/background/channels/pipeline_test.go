package channels

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func ptr[T any](v T) *T { return &v }

// fakeInvoker records prepare calls and returns a canned result keyed by action.
type fakeInvoker struct {
	results map[string]any
	calls   []string
	err     error
}

func (f *fakeInvoker) Invoke(_ context.Context, action string, params map[string]any) (any, error) {
	f.calls = append(f.calls, action)
	if f.err != nil {
		return nil, f.err
	}
	if r, ok := f.results[action]; ok {
		return r, nil
	}
	return params, nil
}

func ev() Event {
	return Event{
		EventID:  "e1",
		Provider: "gh",
		Adapter:  "webhook",
		Source:   "1.2.3.4",
		Payload: map[string]any{
			"status":   "active",
			"priority": float64(8),
			"user_id":  "u42",
			"pr":       map[string]any{"number": float64(7)},
			"tags":     []any{"a", "b"},
		},
	}
}

// ── template ────────────────────────────────────────────────────────────────

func TestRender_Dotpath_ListIndex_Coercion(t *testing.T) {
	scope := buildScope(ev())
	cases := map[string]string{
		"{{event.payload.status}}":    "active",
		"{{event.payload.priority}}":  "8",   // integral float → "8" not "8.0"
		"{{event.payload.pr.number}}": "7",   // nested
		"{{event.payload.tags.0}}":    "a",   // list index
		"{{event.data.user_id}}":      "u42", // data alias
		"{{ event.provider }}":        "gh",  // whitespace-tolerant
		"{{event.payload.missing}}":   "",    // unresolved → empty
		"hi {{event.source}}!":        "hi 1.2.3.4!",
	}
	for tmpl, want := range cases {
		if got := Render(tmpl, scope); got != want {
			t.Errorf("Render(%q) = %q, want %q", tmpl, got, want)
		}
	}
}

func TestRender_BlocksSecretAndEnvAtRuntime(t *testing.T) {
	scope := map[string]any{"secret": map[string]any{"KEY": "leak"}, "env": map[string]any{"X": "y"}}
	if got := Render("a={{secret.KEY}} b={{env.X}}", scope); got != "a= b=" {
		t.Fatalf("runtime secret/env not blocked: %q", got)
	}
}

func TestRender_SinglePass_NoInjection(t *testing.T) {
	// A value that itself looks like a template must NOT be re-expanded.
	scope := map[string]any{"event": map[string]any{"message": "{{event.source}}"}}
	if got := Render("{{event.message}}", scope); got != "{{event.source}}" {
		t.Fatalf("single-pass violated: %q", got)
	}
}

func TestRender_CapsOutput(t *testing.T) {
	big := strings.Repeat("x", maxRenderBytes+100)
	if got := Render(big, nil); len(got) != maxRenderBytes {
		t.Fatalf("output not capped: len=%d", len(got))
	}
}

// ── filter ──────────────────────────────────────────────────────────────────

func TestFilter_AllOperators(t *testing.T) {
	scope := buildScope(ev())
	pass := []FilterCondition{
		{Field: "event.payload.status", Equals: "active"},
		{Field: "event.payload.status", NotEquals: "closed"},
		{Field: "event.payload.user_id", Contains: ptr("42")},
		{Field: "event.payload.priority", Gt: ptr(5.0)},
		{Field: "event.payload.priority", Lt: ptr(10.0)},
	}
	if reason, ok := evalFilter(pass, scope); !ok {
		t.Fatalf("all should pass, failed on %q", reason)
	}
	fail := []struct {
		name string
		c    FilterCondition
	}{
		{"equals", FilterCondition{Field: "event.payload.status", Equals: "closed"}},
		{"not_equals", FilterCondition{Field: "event.payload.status", NotEquals: "active"}},
		{"contains", FilterCondition{Field: "event.payload.user_id", Contains: ptr("zzz")}},
		{"gt", FilterCondition{Field: "event.payload.priority", Gt: ptr(8.0)}},
		{"lt", FilterCondition{Field: "event.payload.priority", Lt: ptr(8.0)}},
		{"missing", FilterCondition{Field: "event.payload.nope", Equals: "x"}},
	}
	for _, f := range fail {
		if _, ok := evalFilter([]FilterCondition{f.c}, scope); ok {
			t.Errorf("%s should fail", f.name)
		}
	}
}

func TestProcess_FilteredOut(t *testing.T) {
	ac := ActivationConfig{Filter: []FilterCondition{{Field: "event.payload.status", Equals: "closed"}}}
	a, err := Process(context.Background(), ev(), ac, ModuleConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Filtered || a.FilterReason != "event.payload.status" {
		t.Fatalf("expected filtered, got %+v", a)
	}
}

// ── route ───────────────────────────────────────────────────────────────────

func TestRoute_FirstMatchThenDefault(t *testing.T) {
	scope := buildScope(ev())
	r := &RouteConfig{Field: "event.payload.status", Rules: []RouteRule{
		{Match: ptr("closed"), Agent: "closer"},
		{Match: ptr("active"), Agent: "worker"},
		{Default: true, Agent: "fallback"},
	}}
	if got := resolveRoute(r, scope); got != "worker" {
		t.Fatalf("route = %q, want worker", got)
	}
	r2 := &RouteConfig{Field: "event.payload.status", Rules: []RouteRule{
		{Match: ptr("nope"), Agent: "x"},
		{Default: true, Agent: "fallback"},
	}}
	if got := resolveRoute(r2, scope); got != "fallback" {
		t.Fatalf("route = %q, want fallback", got)
	}
	r3 := &RouteConfig{Field: "event.payload.status", Rules: []RouteRule{{Match: ptr("nope"), Agent: "x"}}}
	if got := resolveRoute(r3, scope); got != "" {
		t.Fatalf("no match + no default should be empty, got %q", got)
	}
}

func TestProcess_RouteOverridesAgent_ElseDefaultAgent(t *testing.T) {
	ac := ActivationConfig{
		Agent: "static",
		Route: &RouteConfig{Field: "event.payload.status", Rules: []RouteRule{{Match: ptr("nope"), Agent: "x"}}},
	}
	a, _ := Process(context.Background(), ev(), ac, ModuleConfig{DefaultAgent: "def"}, nil)
	if a.Agent != "def" {
		t.Fatalf("route miss should fall to default_agent, got %q", a.Agent)
	}
}

// ── session strategies ──────────────────────────────────────────────────────

func TestSession_Strategies(t *testing.T) {
	e := ev()
	scope := buildScope(e)

	if id, strat := resolveSession("per_event", e, scope); id != "" || strat != SessionPerEvent {
		t.Fatalf("per_event should defer (empty id), got %q/%q", id, strat)
	}
	if id, _ := resolveSession("", e, scope); id != "" {
		t.Fatalf("empty strategy = per_event, got %q", id)
	}
	if id, strat := resolveSession("shared", e, scope); id != "ch-gh-1.2.3.4" || strat != SessionShared {
		t.Fatalf("shared = %q/%q", id, strat)
	}
	id, strat := resolveSession("ticket-{{event.payload.user_id}}", e, scope)
	if id != "ticket-u42" || strat != "template" {
		t.Fatalf("template = %q/%q", id, strat)
	}
}

func TestSession_TemplateSanitized(t *testing.T) {
	scope := buildScope(ev())
	id, _ := resolveSession("a/b c:{{event.source}}", ev(), scope)
	// '/', ' ', ':' → '-'; dots kept (1.2.3.4)
	if id != "a-b-c-1.2.3.4" {
		t.Fatalf("sanitized = %q", id)
	}
}

// ── prepare ─────────────────────────────────────────────────────────────────

func TestProcess_PrepareBindsResultIntoScope(t *testing.T) {
	inv := &fakeInvoker{results: map[string]any{
		"database.lookup": map[string]any{"role": "admin"},
	}}
	ac := ActivationConfig{
		Prepare: []PrepareStep{{Action: "database.lookup", Params: map[string]any{"id": "{{event.payload.user_id}}"}, As: "user"}},
		Route:   &RouteConfig{Field: "user.role", Rules: []RouteRule{{Match: ptr("admin"), Agent: "admin_agent"}, {Default: true, Agent: "x"}}},
		Message: "role={{user.role}}",
	}
	a, err := Process(context.Background(), ev(), ac, ModuleConfig{}, inv)
	if err != nil {
		t.Fatal(err)
	}
	if a.Agent != "admin_agent" {
		t.Fatalf("prepare→route failed, agent=%q", a.Agent)
	}
	if a.Message != "role=admin" {
		t.Fatalf("prepare result not in scope, msg=%q", a.Message)
	}
	if len(inv.calls) != 1 || inv.calls[0] != "database.lookup" {
		t.Fatalf("invoker calls = %v", inv.calls)
	}
}

func TestProcess_PrepareError_Propagates(t *testing.T) {
	inv := &fakeInvoker{err: errors.New("db down")}
	ac := ActivationConfig{Prepare: []PrepareStep{{Action: "database.lookup"}}}
	if _, err := Process(context.Background(), ev(), ac, ModuleConfig{}, inv); err == nil {
		t.Fatal("prepare error should propagate")
	}
}

func TestProcess_PrepareWithoutInvoker_Errors(t *testing.T) {
	ac := ActivationConfig{Prepare: []PrepareStep{{Action: "x.y"}}}
	if _, err := Process(context.Background(), ev(), ac, ModuleConfig{}, nil); err == nil {
		t.Fatal("prepare without invoker should error")
	}
}

// ── message build ───────────────────────────────────────────────────────────

func TestBuildMessage_FallbackChain(t *testing.T) {
	scope := buildScope(ev())
	if m := buildMessage(ActivationConfig{Message: "PR {{event.payload.pr.number}}"}, ev(), scope); m != "PR 7" {
		t.Fatalf("templated msg = %q", m)
	}
	e2 := Event{Provider: "p", Message: "raw text"}
	if m := buildMessage(ActivationConfig{}, e2, buildScope(e2)); m != "raw text" {
		t.Fatalf("event.message fallback = %q", m)
	}
	e3 := Event{Provider: "p", Payload: map[string]any{"k": "v"}}
	if m := buildMessage(ActivationConfig{}, e3, buildScope(e3)); !strings.Contains(m, `"k":"v"`) {
		t.Fatalf("json fallback = %q", m)
	}
	e4 := Event{Provider: "cron"}
	if m := buildMessage(ActivationConfig{}, e4, buildScope(e4)); m != "Event from cron" {
		t.Fatalf("generic fallback = %q", m)
	}
}

// ── config: defaults, validation, YAML round-trip ───────────────────────────

func TestConfig_DefaultsAndValidate(t *testing.T) {
	m := ModuleConfig{Providers: map[string]ProviderConfig{
		"p": {Adapter: "webhook"},
	}}
	m.Normalize()
	if m.MaxTurns != 30 || m.Timeout != 120 || m.HistoryLimit != 200 || !m.FilterSecrets() {
		t.Fatalf("module defaults wrong: %+v", m)
	}
	p := m.Providers["p"]
	if !p.IsEnabled() || p.MaxConcurrent != 5 || p.Activation.Session != "per_event" || p.Activation.Reply != "none" {
		t.Fatalf("provider defaults wrong: %+v", p)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestConfig_ValidationRejects(t *testing.T) {
	bad := []ModuleConfig{
		{MaxTurns: 999, Providers: map[string]ProviderConfig{"p": {Adapter: "x"}}},
		{Providers: map[string]ProviderConfig{"p": {Adapter: ""}}},                                              // missing adapter
		{Providers: map[string]ProviderConfig{"p": {Adapter: "x", Activation: ActivationConfig{Reply: "huh"}}}}, // bad reply
	}
	for i, m := range bad {
		m.Normalize()
		if err := m.Validate(); err == nil {
			t.Errorf("case %d should fail validation", i)
		}
	}
}

func TestConfig_YAMLRoundTrip(t *testing.T) {
	src := `
default_agent: main
max_turns: 30
providers:
  github_webhook:
    adapter: webhook
    config:
      inbound_path: /hook/github
      signature_header: X-Hub-Signature-256
    activation:
      agent: reviewer
      session: "pr-{{event.payload.pr.number}}"
      message: "PR {{event.payload.pr.number}}"
      filter:
        - field: event.payload.action
          equals: opened
        - field: event.payload.pr.number
          gt: 0
      route:
        field: event.payload.repo
        rules:
          - match: core
            agent: senior
          - default: true
            agent: general
      reply: auto
`
	var m ModuleConfig
	if err := yaml.Unmarshal([]byte(src), &m); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	m.Normalize()
	if err := m.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	p := m.Providers["github_webhook"]
	if p.Adapter != "webhook" || p.Activation.Agent != "reviewer" || p.Activation.Reply != "auto" {
		t.Fatalf("parsed provider wrong: %+v", p.Activation)
	}
	if len(p.Activation.Filter) != 2 || p.Activation.Filter[0].Equals != "opened" {
		t.Fatalf("filters parsed wrong: %+v", p.Activation.Filter)
	}
	if p.Activation.Filter[1].Gt == nil || *p.Activation.Filter[1].Gt != 0 {
		t.Fatalf("gt parsed wrong: %+v", p.Activation.Filter[1])
	}
	if p.Activation.Route == nil || len(p.Activation.Route.Rules) != 2 {
		t.Fatalf("route parsed wrong")
	}

	// End-to-end through Process.
	e := Event{Provider: "github_webhook", Payload: map[string]any{
		"action": "opened", "pr": map[string]any{"number": float64(12)}, "repo": "core",
	}}
	a, err := Process(context.Background(), e, p.Activation, m, nil)
	if err != nil || a.Filtered {
		t.Fatalf("should activate: %+v err=%v", a, err)
	}
	if a.Agent != "senior" || a.Session != "pr-12" || a.Message != "PR 12" || a.Reply != "auto" {
		t.Fatalf("activation = %+v", a)
	}
}

// ── security ────────────────────────────────────────────────────────────────

func TestSanitizePayload_StripsAndCaps(t *testing.T) {
	p := map[string]any{
		"ok":          "value",
		"__proto__":   "evil",
		"constructor": "evil",
		"a$$b":        "evil",
		"big":         strings.Repeat("x", maxStringLen+50),
		"nested":      map[string]any{"__class__": "evil", "fine": 1},
	}
	out := SanitizePayload(p)
	if _, bad := out["__proto__"]; bad {
		t.Fatal("__proto__ not stripped")
	}
	if _, bad := out["constructor"]; bad {
		t.Fatal("constructor not stripped")
	}
	if _, bad := out["a$$b"]; bad {
		t.Fatal("$$ key not stripped")
	}
	if len(out["big"].(string)) != maxStringLen {
		t.Fatalf("string not capped: %d", len(out["big"].(string)))
	}
	nested := out["nested"].(map[string]any)
	if _, bad := nested["__class__"]; bad {
		t.Fatal("nested dangerous key not stripped")
	}
	if nested["fine"] != 1 {
		t.Fatal("legit nested value lost")
	}
}

func TestSanitizePayload_DepthLimit(t *testing.T) {
	// Build 15 levels deep; beyond maxSanitizeDepth must be dropped.
	deep := map[string]any{"v": "leaf"}
	for i := 0; i < 15; i++ {
		deep = map[string]any{"n": deep}
	}
	out := SanitizePayload(deep)
	cur := any(out)
	depth := 0
	for {
		m, ok := cur.(map[string]any)
		if !ok {
			break
		}
		next, ok := m["n"]
		if !ok || next == nil {
			break
		}
		cur = next
		depth++
	}
	if depth > maxSanitizeDepth+1 {
		t.Fatalf("depth not capped: %d", depth)
	}
}

func TestFilterSecrets(t *testing.T) {
	cases := []string{
		"key sk-ant-abcdefghij0123456789XYZ done",
		"openai sk-proj0123456789abcdefghij end",
		"slack xoxb-1234567890-abcdEFGH tail",
		"gh ghp_0123456789abcdefghijABCD x",
		"aws AKIA0123456789ABCDEF y",
		"auth Bearer abcdef0123456789 z",
		"dig dk_0123456789abcdef w",
	}
	for _, in := range cases {
		out := FilterSecrets(in)
		if !strings.Contains(out, redaction) {
			t.Errorf("not redacted: %q → %q", in, out)
		}
	}
	if FilterSecrets("no secrets here") != "no secrets here" {
		t.Fatal("clean text altered")
	}
}

func TestFilterSecretsIn_Recursive(t *testing.T) {
	in := map[string]any{
		"a": "sk-ant-abcdefghij0123456789XYZ",
		"b": []any{"AKIA0123456789ABCDEF", "fine"},
		"c": map[string]any{"d": "Bearer abcdef0123456789"},
	}
	out := FilterSecretsIn(in).(map[string]any)
	if strings.Contains(out["a"].(string), "sk-ant") {
		t.Fatal("a not filtered")
	}
	if strings.Contains(out["b"].([]any)[0].(string), "AKIA") {
		t.Fatal("list secret not filtered")
	}
	if strings.Contains(out["c"].(map[string]any)["d"].(string), "Bearer ab") {
		t.Fatal("nested secret not filtered")
	}
}

func TestProcess_AttachesTriggerEventForFlow(t *testing.T) {
	e := Event{
		Provider: "glpi",
		Adapter:  "webhook",
		Source:   "10.0.0.1",
		Payload: map[string]any{
			"id":     float64(4242),
			"status": "new",
			"name":   "VPN down",
		},
	}
	act, err := Process(context.Background(), e, ActivationConfig{
		Message: "{{event.payload.name}}",
	}, ModuleConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if act.TriggerEvent == nil {
		t.Fatal("expected TriggerEvent on activation")
	}
	if act.TriggerEvent["provider"] != "glpi" {
		t.Fatalf("provider = %v", act.TriggerEvent["provider"])
	}
	payload := act.TriggerEvent["payload"].(map[string]any)
	if payload["id"] != float64(4242) {
		t.Fatalf("payload.id = %v", payload["id"])
	}
	ls := act.ToLaunchSpec("glpi-support")
	if ls.TriggerEvent == nil || ls.TriggerEvent["adapter"] != "webhook" {
		t.Fatalf("LaunchSpec lost TriggerEvent: %+v", ls.TriggerEvent)
	}
}
