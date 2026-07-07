package flow

import "testing"

func TestInterpolateSecret(t *testing.T) {
	fc := newContext(nil, func(key string) (string, bool) {
		if key == "GLPI_URL" {
			return "https://glpi.example", true
		}
		return "", false
	})
	got := fc.interpolate("{{secret.GLPI_URL}}/apirest.php/Ticket/42")
	want := "https://glpi.example/apirest.php/Ticket/42"
	if got != want {
		t.Fatalf("interpolate secret: got %q want %q", got, want)
	}
}

func TestParseJSONObject_Robust(t *testing.T) {
	cases := []struct {
		in   string
		key  string
		want string
	}{
		{`{"category":"refund"}`, "category", "refund"},
		{"```json\n{\"category\": \"refund\"}\n```", "category", "refund"},
		{"```\n{\"category\":\"tech\"}\n```", "category", "tech"},
		{"Here is the result: {\"category\":\"other\"} hope that helps", "category", "other"},
		{"```json {\"category\": \"refund\", \"note\":\"has } brace\"} ```", "category", "refund"},
		{"no json here", "category", ""},
		{"", "category", ""},
	}
	for _, tc := range cases {
		m := parseJSONObject(tc.in)
		got, _ := m[tc.key].(string)
		if got != tc.want {
			t.Errorf("parseJSONObject(%q)[%q] = %q, want %q", tc.in, tc.key, got, tc.want)
		}
	}
}

func TestTypedOutputGuard(t *testing.T) {
	fc := newContext(nil, nil)
	fc.recordAgent("triage", `{"reply":"Bonjour, votre imprimante a été réinitialisée.","category":"network","confidence":0.92}`)

	if got := fc.interpolate("{{triage.output}}"); got != "Bonjour, votre imprimante a été réinitialisée." {
		t.Errorf("bare .output = %q, want the unwrapped reply (not raw JSON)", got)
	}
	if got := fc.interpolate("{{triage.output.category}}"); got != "network" {
		t.Errorf(".output.category = %q, want network (structured access preserved)", got)
	}
	if got := fc.lastText(); got != "Bonjour, votre imprimante a été réinitialisée." {
		t.Errorf("lastText = %q, want the unwrapped reply", got)
	}
}

func TestTypedOutputGuard_PlainText(t *testing.T) {
	fc := newContext(nil, nil)
	fc.recordAgent("writer", "Just a plain answer, no JSON.")
	if got := fc.interpolate("{{writer.output}}"); got != "Just a plain answer, no JSON." {
		t.Errorf("plain .output = %q, want the raw text unchanged", got)
	}
}

func TestTypedOutputGuard_NoTextKey(t *testing.T) {
	fc := newContext(nil, nil)
	fc.recordAgent("classify", `{"category":"network","confidence":0.9}`)
	got := fc.interpolate("{{classify.output}}")
	if got != `{"category":"network","confidence":0.9}` {
		t.Errorf("object without a text key: .output = %q, want raw JSON preserved", got)
	}
}

func TestInterpolateJSONModifier(t *testing.T) {
	fc := newContext(map[string]any{"payload": map[string]any{"subject": `VPN "cassé" depuis ce matin`}}, nil)
	got := fc.interpolate(`{"input":{"name":{{event.payload.subject | json}}}}`)
	want := `{"input":{"name":"VPN \"cassé\" depuis ce matin"}}`
	if got != want {
		t.Errorf("json modifier: got %s want %s", got, want)
	}
	if out := fc.interpolate(`{{event.payload.missing | json}}`); out != "null" {
		t.Errorf("missing path = %q, want null", out)
	}
}
