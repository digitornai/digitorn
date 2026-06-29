package server

import "testing"

// TestResolveConfigSecrets proves the daemon swaps `{{secret.X}}` placeholders
// in a channel config for the stored value before pushing the trigger to the
// background — the core of the "paste your token, done" flow. Unset secrets stay
// as placeholders (background env fallback) and the original config is untouched.
func TestResolveConfigSecrets(t *testing.T) {
	d := &Daemon{}
	d.secrets = newSecretStore()
	d.secrets.set("paul", "discord-test", "discord_bot_token", "REAL-TOKEN-123")

	cfg := map[string]any{
		"bot_token": "{{secret.discord_bot_token}}",
		"nested":    map[string]any{"x": "{{secret.missing_key}}"},
		"list":      []any{"plain", "Bot {{secret.discord_bot_token}}"},
	}

	out, ok := d.resolveConfigSecrets("discord-test", cfg).(map[string]any)
	if !ok {
		t.Fatal("expected a map result")
	}
	if out["bot_token"] != "REAL-TOKEN-123" {
		t.Fatalf("token not resolved: %v", out["bot_token"])
	}
	if nx := out["nested"].(map[string]any)["x"]; nx != "{{secret.missing_key}}" {
		t.Fatalf("unset secret should stay a placeholder, got %v", nx)
	}
	if lv := out["list"].([]any)[1]; lv != "Bot REAL-TOKEN-123" {
		t.Fatalf("secret inside a longer string not resolved: %v", lv)
	}
	if cfg["bot_token"] != "{{secret.discord_bot_token}}" {
		t.Fatal("original config was mutated")
	}

	// Isolation: another app's missing secret resolves to nothing.
	other := d.resolveConfigSecrets("other-app", map[string]any{"t": "{{secret.discord_bot_token}}"}).(map[string]any)
	if other["t"] != "{{secret.discord_bot_token}}" {
		t.Fatalf("cross-app leak: %v", other["t"])
	}
}
