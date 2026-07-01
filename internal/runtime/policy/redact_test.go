package policy_test

import (
	"reflect"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime/policy"
)

func TestRedactParams_EmptyInput(t *testing.T) {
	if got := policy.RedactParams(nil, nil); got != nil {
		t.Errorf("nil input → got %v, want nil", got)
	}
	if got := policy.RedactParams(map[string]any{}, nil); len(got) != 0 {
		t.Errorf("empty input → got len %d, want 0", len(got))
	}
}

func TestRedactParams_SensitiveKeysRedacted(t *testing.T) {
	in := map[string]any{
		"command":      "echo hi",
		"password":     "p@ssw0rd",
		"api_key":      "sk-abc-123",
		"API_KEY":      "DEF-456",
		"x-api-key":    "ghi-789",
		"auth_token":   "tok",
		"normal_field": 42,
	}
	out := policy.RedactParams(in, nil)
	for k, want := range map[string]any{
		"command":      "echo hi",
		"password":     policy.RedactedPlaceholder,
		"api_key":      policy.RedactedPlaceholder,
		"API_KEY":      policy.RedactedPlaceholder,
		"x-api-key":    policy.RedactedPlaceholder,
		"auth_token":   policy.RedactedPlaceholder,
		"normal_field": 42,
	} {
		if got := out[k]; !reflect.DeepEqual(got, want) {
			t.Errorf("key=%q : got %v, want %v", k, got, want)
		}
	}
}

func TestRedactParams_DoesNotMutateInput(t *testing.T) {
	in := map[string]any{"password": "secret-123", "x": 1}
	_ = policy.RedactParams(in, nil)
	if in["password"] != "secret-123" {
		t.Errorf("input mutated : password = %v", in["password"])
	}
}

func TestRedactParams_NestedMapsRecursed(t *testing.T) {
	in := map[string]any{
		"connection": map[string]any{
			"host":     "db.example.com",
			"password": "secret",
		},
		"safe": "value",
	}
	out := policy.RedactParams(in, nil)
	conn, ok := out["connection"].(map[string]any)
	if !ok {
		t.Fatalf("connection not nested map : %T", out["connection"])
	}
	if conn["host"] != "db.example.com" {
		t.Errorf("non-sensitive nested key was changed : host=%v", conn["host"])
	}
	if conn["password"] != policy.RedactedPlaceholder {
		t.Errorf("nested password not redacted : %v", conn["password"])
	}
	if out["safe"] != "value" {
		t.Errorf("safe value changed : %v", out["safe"])
	}
}

func TestRedactParams_CustomPatterns(t *testing.T) {
	in := map[string]any{
		"my_internal_id": "abc",
		"password":       "x",
	}
	out := policy.RedactParams(in, []string{"internal_id"})
	if out["my_internal_id"] != policy.RedactedPlaceholder {
		t.Errorf("custom pattern not applied")
	}
	if out["password"] != "x" {
		t.Errorf("default pattern still applied when custom provided : %v", out["password"])
	}
}

func TestRedactParams_ScalarArrayValuesUntouched(t *testing.T) {
	// A slice of scalars is preserved verbatim : key-based redaction can only
	// act on a keyed value, and a bare string element has no key to match.
	in := map[string]any{
		"items":    []any{"a", "b"},
		"password": "p",
	}
	out := policy.RedactParams(in, nil)
	items, ok := out["items"].([]any)
	if !ok || len(items) != 2 || items[0] != "a" || items[1] != "b" {
		t.Fatalf("scalar array mangled : %v", out["items"])
	}
	if out["password"] != policy.RedactedPlaceholder {
		t.Errorf("top-level password not redacted : %v", out["password"])
	}
}

func TestRedactParams_SecretInsideArrayRedacted(t *testing.T) {
	// A sensitive key nested inside an array (a map element, or a deeper
	// array) MUST be redacted — the old map-only recursion leaked it verbatim
	// into the audit row.
	in := map[string]any{
		"creds": []any{
			map[string]any{"name": "primary", "api_key": "sk-secret-1"},
			map[string]any{"name": "backup", "password": "hunter2"},
		},
		"matrix": []any{
			[]any{map[string]any{"token": "deep-secret"}},
		},
		"safe": "ok",
	}
	out := policy.RedactParams(in, nil)
	creds, ok := out["creds"].([]any)
	if !ok || len(creds) != 2 {
		t.Fatalf("creds mangled : %v", out["creds"])
	}
	c0 := creds[0].(map[string]any)
	if c0["name"] != "primary" {
		t.Errorf("non-sensitive nested key changed : %v", c0["name"])
	}
	if c0["api_key"] != policy.RedactedPlaceholder {
		t.Errorf("api_key in array not redacted : %v", c0["api_key"])
	}
	if creds[1].(map[string]any)["password"] != policy.RedactedPlaceholder {
		t.Errorf("password in array not redacted")
	}
	deep := out["matrix"].([]any)[0].([]any)[0].(map[string]any)
	if deep["token"] != policy.RedactedPlaceholder {
		t.Errorf("token nested array-of-array not redacted : %v", deep["token"])
	}
	if out["safe"] != "ok" {
		t.Errorf("safe value changed : %v", out["safe"])
	}
}

func TestRedactParams_DoesNotMutateNestedArray(t *testing.T) {
	in := map[string]any{"creds": []any{map[string]any{"api_key": "sk-x"}}}
	_ = policy.RedactParams(in, nil)
	if got := in["creds"].([]any)[0].(map[string]any)["api_key"]; got != "sk-x" {
		t.Errorf("input array mutated : %v", got)
	}
}
