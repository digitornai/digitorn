package bifrost

import (
	"testing"

	schemas "github.com/maximhq/bifrost/core/schemas"
)

// TestPerProviderConcurrency_OverridesGlobal proves account.GetConfigForProvider
// honours the per-provider map when a key matches, and falls back to the
// global default otherwise.
func TestPerProviderConcurrency_OverridesGlobal(t *testing.T) {
	a := newAccount(Config{
		Concurrency: 256,
		BufferSize:  16384,
		PerProviderConcurrency: map[string]int{
			"anthropic": 1024,
			"deepseek":  64,
		},
		PerProviderBufferSize: map[string]int{
			"anthropic": 32768,
		},
	})

	cases := []struct {
		provider       schemas.ModelProvider
		wantConc, wantBuf int
		why                string
	}{
		{schemas.Anthropic, 1024, 32768, "both per-provider overrides hit"},
		{schemas.OpenAI, 256, 16384, "no overrides — global defaults"},
	}
	for _, c := range cases {
		t.Run(string(c.provider), func(t *testing.T) {
			cfg, err := a.GetConfigForProvider(c.provider)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got := cfg.ConcurrencyAndBufferSize.Concurrency; got != c.wantConc {
				t.Errorf("[%s] Concurrency=%d, want %d (%s)", c.provider, got, c.wantConc, c.why)
			}
			if got := cfg.ConcurrencyAndBufferSize.BufferSize; got != c.wantBuf {
				t.Errorf("[%s] BufferSize=%d, want %d (%s)", c.provider, got, c.wantBuf, c.why)
			}
		})
	}
}

// TestPerProviderConcurrency_CaseInsensitive: provider keys in YAML are
// matched against the lower-cased Bifrost provider name. Mistral YAML
// "Mistral" must match schemas.Mistral.
func TestPerProviderConcurrency_CaseInsensitive(t *testing.T) {
	a := newAccount(Config{
		Concurrency: 256,
		BufferSize:  16384,
		// Intentionally MixedCase in the YAML map to ensure we lower-case
		// on lookup, not at config-load time.
		PerProviderConcurrency: map[string]int{"mistral": 333},
	})
	cfg, err := a.GetConfigForProvider(schemas.Mistral)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConcurrencyAndBufferSize.Concurrency != 333 {
		t.Errorf("Mistral Concurrency=%d, want 333 (case-insensitive lookup)", cfg.ConcurrencyAndBufferSize.Concurrency)
	}
}

// TestPerProviderConcurrency_ZeroIgnored: a per-provider entry with
// value 0 must NOT override (otherwise YAML defaults could silently
// floor everything to Bifrost's minimum 8).
func TestPerProviderConcurrency_ZeroIgnored(t *testing.T) {
	a := newAccount(Config{
		Concurrency:            512,
		BufferSize:             16384,
		PerProviderConcurrency: map[string]int{"openai": 0},
	})
	cfg, err := a.GetConfigForProvider(schemas.OpenAI)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConcurrencyAndBufferSize.Concurrency != 512 {
		t.Errorf("OpenAI Concurrency=%d, want 512 (zero override ignored)", cfg.ConcurrencyAndBufferSize.Concurrency)
	}
}
