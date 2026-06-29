package credentials

import (
	"context"
	"testing"
)

func TestResolver_LookupCacheInvalidateIsolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	r := NewResolver(s)

	// Absent provider → miss (and the verdict is cached).
	if _, _, ok := r.Lookup(ctx, "u", "openai"); ok {
		t.Fatal("expected miss for an absent provider")
	}

	// Create a key; the cache still holds the stale "absent" verdict until invalidate.
	if _, err := s.Create(ctx, "u", CreateInput{
		ProviderName: "openai",
		Fields:       map[string]string{"api_key": "sk-live-123456789"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, ok := r.Lookup(ctx, "u", "openai"); ok {
		t.Fatal("expected stale miss before Invalidate")
	}

	r.Invalidate("u")
	key, _, ok := r.Lookup(ctx, "u", "openai")
	if !ok || key != "sk-live-123456789" {
		t.Fatalf("after invalidate: ok=%v key=%q", ok, key)
	}

	// base_url is surfaced for custom OpenAI-compatible endpoints.
	if _, err := s.Create(ctx, "u", CreateInput{
		ProviderName: "custom_llm",
		Fields:       map[string]string{"api_key": "k-abcdefghij", "base_url": "https://api.x/v1"},
	}); err != nil {
		t.Fatalf("create2: %v", err)
	}
	r.Invalidate("u")
	k, b, ok := r.Lookup(ctx, "u", "custom_llm")
	if !ok || k != "k-abcdefghij" || b != "https://api.x/v1" {
		t.Fatalf("custom: ok=%v k=%q b=%q", ok, k, b)
	}

	// A connection_string credential (no api_key) is not a usable LLM key → miss.
	if _, err := s.Create(ctx, "u", CreateInput{
		ProviderName: "postgres",
		ProviderType: "connection_string",
		Fields:       map[string]string{"connection_string": "postgres://x:y@h/d"},
	}); err != nil {
		t.Fatalf("create3: %v", err)
	}
	r.Invalidate("u")
	if _, _, ok := r.Lookup(ctx, "u", "postgres"); ok {
		t.Fatal("connection_string should not resolve as an api key")
	}

	// Cross-user isolation: another user sees nothing.
	if _, _, ok := r.Lookup(ctx, "other", "openai"); ok {
		t.Fatal("cross-user cache leak")
	}

	// nil resolver is safe.
	var nilR *Resolver
	if _, _, ok := nilR.Lookup(ctx, "u", "openai"); ok {
		t.Fatal("nil resolver must return ok=false")
	}
	nilR.Invalidate("u") // must not panic
}
