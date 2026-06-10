package channels

import "testing"

// TestResolveOwner : per-user ownership is opt-in — an explicit owner template
// renders the end-user id (namespacing is the app's choice) ; an unset owner yields
// "" so the launcher (service) owns the session (back-compat).
func TestResolveOwner(t *testing.T) {
	ev := Event{Provider: "tg", Adapter: "telegram", Source: "12345", Payload: map[string]any{"user": "alice"}}
	scope := buildScope(ev)

	if got := resolveOwner("u-{{event.payload.user}}", ev, scope); got != "u-alice" {
		t.Fatalf("template owner = %q, want u-alice", got)
	}
	if got := resolveOwner("{{event.provider}}:{{event.source}}", ev, scope); got != "tg:12345" {
		t.Fatalf("namespaced template owner = %q, want tg:12345", got)
	}
	if got := resolveOwner("", ev, scope); got != "" {
		t.Fatalf("unset owner must be empty (service-owned), got %q", got)
	}
}
