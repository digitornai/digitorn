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
