package contextcompact

import "testing"

// TestContextWindowFor : longest-prefix match on the model name, conservative
// fallback for the unknown. This is the max_tokens:0 auto-detect path.
func TestContextWindowFor(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"gpt-4o-mini", 128000},
		{"gpt-4o", 128000},
		{"gpt-4.1", 1000000},
		{"gpt-5-mini", 256000},
		{"claude-sonnet-4-5", 200000}, // longest-prefix beats bare "claude"
		{"claude-3-5-sonnet", 200000},
		{"deepseek-chat", 65536},
		{"gemini-1.5-pro", 1000000},
		{"some-unlisted-model", DefaultContextWindow},
		{"", DefaultContextWindow},
	}
	for _, c := range cases {
		if got := ContextWindowFor("openai", c.model); got != c.want {
			t.Errorf("ContextWindowFor(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}
